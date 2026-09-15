package brain

// Consolidation - Action #1a follow-on. Promotes accumulated `event`
// records into durable `fact`/`instruction` records with provenance,
// cloning the shape of CompileRoutingWisdom (compile.go) and the
// autoCompileMeta trigger (brain.go) for typed memory instead of
// routing wisdom. This is the Atlas episodic → semantic/procedural
// distillation loop: raw occurrences accumulate as events; a periodic
// LLM pass reads them plus the currently-active facts/instructions and
// proposes what should be promoted, with two hard guards borrowed from
// Atlas:
//
//   - provenance - every proposed fact/instruction/supersede must cite
//     the event id(s) it's based on. No supporting event, no write.
//   - confidence penalty - a "harsh" contradiction (a flat reversal,
//     not a natural update) writes its replacement at reduced
//     confidence (0.7) rather than full trust (1.0).
//
// Every accepted output goes through WriteMemory (memory_types.go) -
// embedding + by-key supersession come free. Supersede outputs
// additionally retire the contradicted record via SupersedeByID after
// the replacement lands.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
)

// consolidateEventLimit bounds how many recent events are sent to the
// LLM in one consolidation pass. Keeps the prompt bounded even after a
// burst of Jarvis turns or a large ingest between runs.
const consolidateEventLimit = 200

// consolidateAutoThreshold is the minimum number of new events since
// the last consolidation before the auto-trigger fires. Mirrors
// autoCompileThreshold's role for compile - conservative because
// consolidation is an LLM call and we don't want it firing on every
// query.
const consolidateAutoThreshold = 15

// consolidateFactOutput is one proposed fact or instruction. Facts and
// instructions share this exact wire shape in the LLM response (see
// consolidateSchema); they're split into separate Go fields only
// because they land in different result buckets.
type consolidateFactOutput struct {
	Body               string   `json:"body"`
	Key                string   `json:"key"`
	SupportingEventIDs []string `json:"supporting_event_ids"`
}

// consolidateSupersedeOutput proposes retiring an existing record
// (OldID) in favor of a new one. Contradiction is "harsh" (a flat
// reversal - Atlas rule: penalize confidence) or "natural" (an
// unsurprising update - full confidence).
//
// Deviation from the plan's literal JSON sketch: SupportingEventIDs is
// present here too, even though the plan's schema block omits it for
// supersedes. The plan's own guard bullet ("require non-empty
// supporting_event_ids per output; drop anything without provenance")
// reads as applying to every output kind, and the replacement record
// written for a supersede needs real provenance like any other write -
// so the field is included and enforced uniformly.
type consolidateSupersedeOutput struct {
	OldID              string   `json:"old_id"`
	NewBody            string   `json:"new_body"`
	NewKey             string   `json:"new_key"`
	Contradiction      string   `json:"contradiction"` // "harsh" | "natural"
	SupportingEventIDs []string `json:"supporting_event_ids"`
}

// consolidateResponse is the parsed shape of the LLM's JSON output.
type consolidateResponse struct {
	Facts        []consolidateFactOutput      `json:"facts"`
	Instructions []consolidateFactOutput      `json:"instructions"`
	Supersedes   []consolidateSupersedeOutput `json:"supersedes"`
}

// consolidateSchema is the JSON schema handed to CompleteJSON so the
// provider enforces the response shape.
func consolidateSchema() map[string]interface{} {
	eventIDs := map[string]interface{}{
		"type":  "array",
		"items": map[string]interface{}{"type": "string"},
	}
	factLike := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"body":                 map[string]interface{}{"type": "string"},
			"key":                  map[string]interface{}{"type": "string"},
			"supporting_event_ids": eventIDs,
		},
		"required":             []string{"body", "key", "supporting_event_ids"},
		"additionalProperties": false,
	}
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"facts": map[string]interface{}{
				"type":  "array",
				"items": factLike,
			},
			"instructions": map[string]interface{}{
				"type":  "array",
				"items": factLike,
			},
			"supersedes": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"old_id":               map[string]interface{}{"type": "string"},
						"new_body":             map[string]interface{}{"type": "string"},
						"new_key":              map[string]interface{}{"type": "string"},
						"contradiction":        map[string]interface{}{"type": "string", "enum": []string{"harsh", "natural"}},
						"supporting_event_ids": eventIDs,
					},
					"required":             []string{"old_id", "new_body", "contradiction", "supporting_event_ids"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"facts", "instructions", "supersedes"},
		"additionalProperties": false,
	}
}

// consolidateSystemPrompt instructs the LLM on the episodic →
// semantic/procedural distillation task. Mirrors CompileRoutingWisdom's
// systemPrompt in tone: specific, evidence-based, output-format-strict.
const consolidateSystemPrompt = `You are consolidating episodic memory into durable semantic/procedural memory for an LLM-powered personal assistant called Aida.

Below are recent EVENTS (timestamped occurrences - voice turns, ingested notes, tool runs) and the currently-active FACTS and INSTRUCTIONS already known. Facts are long-lived assertions (e.g. "user prefers US units"). Instructions are durable rules (e.g. "always run make install after Go changes").

Your job: read the events and propose what should be promoted into durable memory.

Rules:
- Every proposed fact/instruction/supersede MUST cite the event id(s) it is based on in "supporting_event_ids". Never propose something with no supporting event - it will be discarded.
- Do NOT duplicate an existing active fact or instruction. If an event merely confirms something already known, skip it.
- If an event CONTRADICTS an existing active fact/instruction, propose a "supersede" instead of a fresh fact: old_id = the id of the record being replaced, new_body/new_key = the corrected record, supporting_event_ids = the contradicting event(s). Set contradiction = "harsh" if the event flatly reverses the prior record (e.g. "actually no, I prefer X" replacing "I prefer Y"), or "natural" if it's an unsurprising update (e.g. a preference evolving over time, a fact going stale).
- Facts and instructions need a "key" for future by-key supersession - pick a short, stable, kebab-case identifier (e.g. "user-units", "build-after-go-changes"). Leave key empty only when there is truly no natural dedup key.
- Be conservative. Most events (small talk, one-off questions, routine tool runs) should produce NOTHING. Only promote genuinely durable, generalizable information.
- Output ONLY valid JSON matching the schema. No prose, no markdown.`

// consolidateEventContext and consolidateActiveContext are the
// trimmed shapes fed into the user prompt - just enough for the LLM
// to reason about provenance and dedup without paying for every
// column on MemoryRecord.
type consolidateEventContext struct {
	ID      string `json:"id"`
	Body    string `json:"body"`
	Created string `json:"created"`
}

type consolidateActiveContext struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Key  string `json:"key"`
	Body string `json:"body"`
}

// buildConsolidateUserPrompt renders the events + active-record
// dedup context as the user turn.
func buildConsolidateUserPrompt(events, active []MemoryRecord) string {
	evCtx := make([]consolidateEventContext, 0, len(events))
	for _, e := range events {
		evCtx = append(evCtx, consolidateEventContext{ID: e.ID, Body: e.Body, Created: e.Created})
	}
	activeCtx := make([]consolidateActiveContext, 0, len(active))
	for _, a := range active {
		activeCtx = append(activeCtx, consolidateActiveContext{ID: a.ID, Type: string(a.Type), Key: a.Key, Body: a.Body})
	}
	evJSON, _ := json.MarshalIndent(evCtx, "", "  ")
	activeJSON, _ := json.MarshalIndent(activeCtx, "", "  ")
	return fmt.Sprintf(
		"## Events (%d)\n\n%s\n\n## Currently active facts/instructions (%d)\n\n%s",
		len(evCtx), string(evJSON), len(activeCtx), string(activeJSON),
	)
}

// ConsolidateSupersession records one accepted supersede proposal.
// NewID is empty in a dry run (nothing was actually written yet).
type ConsolidateSupersession struct {
	OldID         string
	NewID         string
	NewBody       string
	Contradiction string
}

// ConsolidateResult summarizes one Consolidate call - used both to
// print the CLI report (real or --dry-run) and to assert on in tests.
type ConsolidateResult struct {
	DryRun              bool
	EventsConsidered    int
	FactsWritten        []MemoryRecord
	InstructionsWritten []MemoryRecord
	Superseded          []ConsolidateSupersession
	// Dropped counts outputs discarded by the provenance guard (empty
	// supporting_event_ids) or, for supersedes, an unresolvable old_id.
	Dropped int
}

// Consolidate reads recent event records plus the currently-active
// facts/instructions, asks the LLM to propose durable
// facts/instructions/supersedes (one CompleteJSON call), and applies
// the result via applyConsolidation.
//
// Deviation from the plan's literal signature
// ("Consolidate(ctx, client *llm.Client) error"): this adds a dryRun
// parameter and returns (*ConsolidateResult, error) instead of a bare
// error. Justification: the CLI's --dry-run mode needs the proposed
// facts/instructions back to print them (the plan requires "dry-run
// prints the proposed facts/instructions"), and tests need a
// structured result to assert on without a way to fake *llm.Client
// (its Provider field is unexported and NewClient has no injection
// point - confirmed by grepping the llm package and every existing
// test that touches an LLM-calling function; they all test pure
// helpers around the LLM call, never the call itself). The pure
// write/guard logic is split into applyConsolidation so it's testable
// with a hand-built consolidateResponse and no LLM at all.
func (b *Brain) Consolidate(ctx context.Context, client *llm.Client, dryRun bool) (*ConsolidateResult, error) {
	since := ""
	if meta, ok := b.readConsolidateMeta(); ok {
		since = meta.LastConsolidate
	}

	events, err := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryEvent}, Limit: consolidateEventLimit})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	if since != "" {
		recent := events[:0:0]
		for _, e := range events {
			if e.Created > since {
				recent = append(recent, e)
			}
		}
		events = recent
	}
	if len(events) == 0 {
		return &ConsolidateResult{DryRun: dryRun}, nil
	}

	active, err := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryFact, MemoryInstruction}})
	if err != nil {
		return nil, fmt.Errorf("list active facts/instructions: %w", err)
	}

	userPrompt := buildConsolidateUserPrompt(events, active)
	raw, err := client.CompleteJSON(ctx, consolidateSystemPrompt, userPrompt, consolidateSchema())
	if err != nil {
		return nil, fmt.Errorf("LLM consolidate: %w", err)
	}

	var resp consolidateResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("parse consolidate response: %w", err)
	}

	result, err := b.applyConsolidation(ctx, resp, dryRun)
	if err != nil {
		return nil, err
	}
	result.EventsConsidered = len(events)
	result.DryRun = dryRun
	return result, nil
}

// applyConsolidation is the pure(ish) write path - no LLM call, just
// the provenance guard + WriteMemory/SupersedeByID plumbing. Split out
// from Consolidate so it's unit-testable with a hand-built
// consolidateResponse.
func (b *Brain) applyConsolidation(ctx context.Context, resp consolidateResponse, dryRun bool) (*ConsolidateResult, error) {
	result := &ConsolidateResult{DryRun: dryRun}

	for _, f := range resp.Facts {
		ids := nonEmptyProvenance(f.SupportingEventIDs)
		if len(ids) == 0 {
			result.Dropped++
			continue
		}
		rec := MemoryRecord{
			Type:       MemoryFact,
			Key:        f.Key,
			Body:       f.Body,
			Provenance: ids,
			Confidence: 1.0,
		}
		if dryRun {
			result.FactsWritten = append(result.FactsWritten, rec)
			continue
		}
		written, err := b.WriteMemory(ctx, rec)
		if err != nil {
			ui.PrintVerbose("Consolidate", "fact write failed: "+err.Error())
			continue
		}
		result.FactsWritten = append(result.FactsWritten, *written)
	}

	for _, ins := range resp.Instructions {
		ids := nonEmptyProvenance(ins.SupportingEventIDs)
		if len(ids) == 0 {
			result.Dropped++
			continue
		}
		rec := MemoryRecord{
			Type:       MemoryInstruction,
			Key:        ins.Key,
			Body:       ins.Body,
			Provenance: ids,
			Confidence: 1.0,
		}
		if dryRun {
			result.InstructionsWritten = append(result.InstructionsWritten, rec)
			continue
		}
		written, err := b.WriteMemory(ctx, rec)
		if err != nil {
			ui.PrintVerbose("Consolidate", "instruction write failed: "+err.Error())
			continue
		}
		result.InstructionsWritten = append(result.InstructionsWritten, *written)
	}

	for _, s := range resp.Supersedes {
		ids := nonEmptyProvenance(s.SupportingEventIDs)
		if len(ids) == 0 || strings.TrimSpace(s.OldID) == "" || strings.TrimSpace(s.NewBody) == "" {
			result.Dropped++
			continue
		}
		oldRec, err := b.DB.GetMemory(s.OldID)
		if err != nil {
			ui.PrintVerbose("Consolidate", "supersede skipped, old_id not found: "+s.OldID)
			result.Dropped++
			continue
		}
		confidence := 1.0
		if strings.EqualFold(strings.TrimSpace(s.Contradiction), "harsh") {
			confidence = 0.7 // Atlas rule: penalize a flat reversal.
		}
		key := s.NewKey
		if key == "" {
			key = oldRec.Key // preserve by-key supersession chain by default
		}
		rec := MemoryRecord{
			Type:       oldRec.Type,
			Key:        key,
			Body:       s.NewBody,
			Profile:    oldRec.Profile,
			Provenance: ids,
			Confidence: confidence,
		}
		sup := ConsolidateSupersession{OldID: s.OldID, NewBody: s.NewBody, Contradiction: s.Contradiction}
		if dryRun {
			result.Superseded = append(result.Superseded, sup)
			continue
		}
		written, err := b.WriteMemory(ctx, rec)
		if err != nil {
			ui.PrintVerbose("Consolidate", "supersede write failed: "+err.Error())
			continue
		}
		if err := b.DB.SupersedeByID(s.OldID, written.ID); err != nil {
			ui.PrintVerbose("Consolidate", "SupersedeByID failed: "+err.Error())
		}
		sup.NewID = written.ID
		sup.NewBody = written.Body
		result.Superseded = append(result.Superseded, sup)
	}

	return result, nil
}

// nonEmptyProvenance drops blank entries and reports the survivors.
// The Atlas provenance guard requires a non-empty result; callers
// treat an empty return as "drop this output."
func nonEmptyProvenance(ids []string) []string {
	var out []string
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}

// autoConsolidateMeta tracks when the last brain consolidate ran -
// the typed-memory analogue of autoCompileMeta (brain.go).
type autoConsolidateMeta struct {
	LastConsolidate string `json:"last_consolidate"`
	EventCount      int    `json:"event_count"`
}

// consolidateMetaPath is the on-disk trigger-state file, relative to
// the brain root's meta/ directory (created by Brain.ensureDirs).
func consolidateMetaPath(brainPath string) string {
	return filepath.Join(brainPath, "meta", "last_consolidate.json")
}

// readConsolidateMeta loads the trigger-state file. ok is false when
// the file is missing or unparseable (treated as "never consolidated").
func (b *Brain) readConsolidateMeta() (autoConsolidateMeta, bool) {
	data, err := os.ReadFile(consolidateMetaPath(b.Path))
	if err != nil {
		return autoConsolidateMeta{}, false
	}
	var meta autoConsolidateMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return autoConsolidateMeta{}, false
	}
	return meta, true
}

// countEventsSince returns the number of active event records created
// after the given timestamp (RFC3339, lexically comparable - same
// trick as LessonCountSince). Empty since counts every event.
func (b *Brain) countEventsSince(since string) int {
	events, err := b.DB.ListMemory(MemoryListOpts{Types: []MemoryType{MemoryEvent}})
	if err != nil {
		return 0
	}
	if since == "" {
		return len(events)
	}
	count := 0
	for _, e := range events {
		if e.Created > since {
			count++
		}
	}
	return count
}

// ShouldAutoConsolidate returns true if enough events have accumulated
// since the last consolidation to justify running one. Mirrors
// ShouldAutoCompile's shape exactly, substituting events for lessons.
func (b *Brain) ShouldAutoConsolidate() bool {
	meta, ok := b.readConsolidateMeta()
	if !ok {
		return b.countEventsSince("") >= consolidateAutoThreshold
	}
	return b.countEventsSince(meta.LastConsolidate) >= consolidateAutoThreshold
}

// MarkConsolidated records the current timestamp and event count so
// ShouldAutoConsolidate knows when the last consolidation happened.
func (b *Brain) MarkConsolidated() error {
	meta := autoConsolidateMeta{
		LastConsolidate: time.Now().UTC().Format(time.RFC3339),
		EventCount:      b.countEventsSince(""),
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(b.Path, "meta"), 0755); err != nil {
		return err
	}
	return os.WriteFile(consolidateMetaPath(b.Path), data, 0644)
}
