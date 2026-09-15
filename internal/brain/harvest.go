package brain

// Multi-agent memory bridge, harvester core -- Codex/Gemini analogue of
// the Claude Code capture path (internal/cli/brain_capture.go). Unlike
// Claude Code, neither Codex nor Gemini writes memory files aida can
// mirror directly: what's on disk is a raw session transcript. So this
// side of the bridge is a distillation harvester instead of a mirror --
// an LLM extraction pass (mirroring consolidate.go's episodic ->
// semantic/procedural shape) reads each new/changed session and
// proposes 0-N durable fact/instruction/event records, written under a
// per-tool memory profile ("codex" or "gemini") via the same WriteMemory
// path every other memory record goes through.
//
// Two write paths share this file's plumbing:
//
//   - harvestDistilledSessions -- LLM-powered. One CompleteJSON call per
//     selected session (Codex rollouts, Gemini CLI session logs).
//   - harvestDirectMemories -- no LLM. Pre-distilled artifacts (an
//     Antigravity walkthrough.md, GEMINI.md) mirrored verbatim, gated by
//     the same per-tool watermark so an unchanged file isn't rewritten
//     every run.
//
// Both paths are incremental: a per-tool watermark file under
// brain/meta/ (same directory consolidate.go's trigger state lives in,
// so it survives and syncs with the brain git repo) tracks the last
// harvested UpdatedAt per session/item id. A session updated within the
// last harvestQuietWindow is treated as still in progress and skipped
// until a later run finds it quiet.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// harvestQuietWindow bounds how recently a session may have been
// updated and still be considered "still running" rather than finished.
// Sessions inside this window are excluded from every harvest pass
// until a later run finds them quiet.
const harvestQuietWindow = 10 * time.Minute

// harvestMaxMemoriesPerSession caps how many distilled memories a
// single session may contribute, regardless of what the LLM proposes --
// the distillation prompt asks for 0-5; this is the hard backstop.
const harvestMaxMemoriesPerSession = 5

// harvestDefaultMaxSessions is the per-run session cap when the caller
// doesn't set HarvestOptions.MaxSessions.
const harvestDefaultMaxSessions = 20

// DistillFunc matches (*llm.Client).CompleteJSON's signature, so a real
// client's method value (client.CompleteJSON) plugs straight in and
// tests can inject a canned responder with no network call or API key.
// Mirrors EmbedFunc's role in mine.go.
type DistillFunc func(ctx context.Context, systemPrompt, userPrompt string, schema map[string]interface{}) (string, error)

// HarvestSession is one distillable transcript, normalized across
// source tools (Codex, Gemini CLI) so the LLM distillation pass and
// watermark bookkeeping stay tool-agnostic.
type HarvestSession struct {
	// ID is stable and unique within a tool's harvest -- the watermark
	// key. Codex uses the raw session id; Gemini prefixes it
	// ("gemini-cli:<sessionId>") since Gemini also harvests non-session
	// items into the same per-tool watermark map.
	ID string
	// UpdatedAt drives both the quiet-window check and the incremental
	// watermark comparison.
	UpdatedAt time.Time
	// Scope is "global" or "project:<slug>", classifyMemoryPath's
	// convention -- memory_recall.go's memoryScopeMatch reads the tags
	// this produces.
	Scope string
	// Transcript is the already-capped, tool-noise-stripped text handed
	// to the LLM.
	Transcript string
	// SourceRef is a human-readable provenance string (a file path,
	// typically) stored on the written record's Source field.
	SourceRef string
	// SystemPromptOverride, when non-empty, replaces
	// harvestDistillSystemPrompt for this session's distill call. Codex
	// and Gemini sessions leave this empty and get the default
	// coding-agent-session prompt; Meetily sets it per run so its
	// call-transcript framing and correction glossary (see
	// harvest_meetily.go) reach the LLM.
	SystemPromptOverride string
	// SchemaOverride, when non-nil, replaces harvestDistillSchema for this
	// session's distill call -- for a source whose LLM response carries
	// more than just "memories" in the same call (Meetily's tags/
	// participants/action_items/key_points/is_new_category/proposed_tag
	// classifier fields).
	SchemaOverride map[string]interface{}
	// TagsFromResponse, when set, is called once per session with the raw
	// distill response (the same JSON harvestDistilledSessions already
	// parses for "memories") and its return value replaces the default
	// []string{tool + "-code"} base tag set applied to every memory
	// written from this session. A nil return leaves the default in
	// place. Meetily uses this to derive per-call tags (a classifier
	// result carried in the same LLM response) instead of the generic
	// per-tool tag, and to capture that response's other fields as a
	// side effect via closure.
	TagsFromResponse func(raw string) []string
}

// HarvestDirectMemory is a pre-distilled record mirrored verbatim --  no
// LLM call, e.g. an Antigravity walkthrough.md or a mirrored GEMINI.md.
type HarvestDirectMemory struct {
	// ID is the watermark key, e.g. "antigravity:<conv>:<relpath>" or
	// "gemini-md".
	ID        string
	UpdatedAt time.Time
	Type      MemoryType
	Key       string
	Body      string
	Scope     string
	Tags      []string
	Source    string
}

// HarvestOptions configures one harvest pass. Zero value is valid:
// MaxSessions falls back to harvestDefaultMaxSessions and Now falls
// back to time.Now().
type HarvestOptions struct {
	// Since, when set, is an RFC3339 lower bound on session UpdatedAt --
	// a manual override for backfilling a specific window. The
	// persisted watermark still applies on top of it, so a session
	// already harvested at or after its current UpdatedAt is still
	// skipped even if it falls inside --since.
	Since string
	// MaxSessions caps how many sessions one run processes (LLM calls
	// are the cost driver here, not I/O), oldest-updated-first for
	// deterministic incremental progress. <= 0 means
	// harvestDefaultMaxSessions.
	MaxSessions int
	// DryRun runs the real distillation call(s) so the preview reflects
	// real cost, but skips every write and skips advancing the
	// watermark -- a dry run never counts as "harvested".
	DryRun bool
	// Now is the injectable clock backing the quiet-window check.
	// Zero means time.Now().
	Now time.Time
}

func (o HarvestOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now
}

func (o HarvestOptions) maxSessions() int {
	if o.MaxSessions <= 0 {
		return harvestDefaultMaxSessions
	}
	return o.MaxSessions
}

// HarvestSessionOutcome is what one distilled session produced.
type HarvestSessionOutcome struct {
	SessionID string
	Scope     string
	Written   []MemoryRecord
}

// HarvestResult summarizes one HarvestCodex/HarvestGemini call -- used
// both to render the CLI report (real or --dry-run) and to assert on in
// tests.
type HarvestResult struct {
	Tool   string
	DryRun bool
	// CandidatesTotal is every session/item known to the source, before
	// any filtering.
	CandidatesTotal int
	// Selected is how many were actually processed this run, after the
	// quiet-window, watermark, --since, and --max-sessions filters.
	Selected int
	// SkippedQuiet counts candidates excluded as still-running.
	SkippedQuiet int
	// Sessions holds one entry per LLM-distilled session that was
	// processed (including ones that yielded zero memories -- that's
	// the expected common case, not an error).
	Sessions []HarvestSessionOutcome
	// DirectMirrored holds every record written by the no-LLM mirror
	// path.
	DirectMirrored []MemoryRecord
	// Dropped counts LLM-proposed memories discarded for an empty
	// name/body or an unparseable response -- not itself an error, just
	// noise filtered before it reaches the brain.
	Dropped int
}

// parseFlexibleRFC3339 accepts both fractional-second (RFC3339Nano) and
// plain RFC3339 timestamps -- the source formats mix both.
func parseFlexibleRFC3339(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// cwdToProjectSlug converts an absolute working-directory path into the
// same slug Claude Code uses for ~/.claude/projects/<slug> -- every
// path separator replaced with "-" (e.g. "/Users/fakehome/dev/aida" ->
// "-Users-fakehome-dev-aida"). Shared by every harvest source that derives
// project scope from a stored cwd/workspace path.
func cwdToProjectSlug(cwd string) string {
	return strings.ReplaceAll(filepath.Clean(cwd), string(filepath.Separator), "-")
}

// capTranscript bounds transcript size to control LLM cost: keeps the
// first headChars and the last tailChars, dropping the (usually
// least-informative, mid-session tool-call noise) middle of a long
// transcript. A transcript already within budget is returned unchanged.
func capTranscript(s string, headChars, tailChars int) string {
	if len(s) <= headChars+tailChars {
		return s
	}
	head := s[:headChars]
	tail := s[len(s)-tailChars:]
	return head + "\n\n...[truncated]...\n\n" + tail
}

// harvestKebabInvalidRe matches runs of characters not valid in a kebab
// key segment.
var harvestKebabInvalidRe = regexp.MustCompile(`[^a-z0-9]+`)

// harvestKebab sanitizes an LLM-provided memory name into a safe key
// segment: lowercase, non-alphanumeric runs collapsed to a single "-",
// leading/trailing "-" trimmed.
func harvestKebab(s string) string {
	s = harvestKebabInvalidRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	return strings.Trim(s, "-")
}

// harvestScopeTags returns the tag set matching classifyMemoryPath's
// scope convention (memory_recall.go's memoryScopeMatch reads these
// exact tags): "scope:global" alone for global scope, or
// ["scope:project", "project:<slug>"] for "project:<slug>".
func harvestScopeTags(scope string) []string {
	if proj, ok := strings.CutPrefix(scope, "project:"); ok && proj != "" {
		return []string{"scope:project", "project:" + proj}
	}
	return []string{"scope:global"}
}

// harvestWatermark is the per-tool incremental-progress state, stored
// under brain/meta so it survives and syncs with the brain git repo --
// the harvest analogue of consolidate.go's autoConsolidateMeta.
type harvestWatermark struct {
	Tool string `json:"tool"`
	// Sessions maps a session/item id to the UpdatedAt it was last
	// harvested at (RFC3339). A candidate is skipped once its own
	// UpdatedAt is no newer than this.
	Sessions map[string]string `json:"sessions"`
}

func harvestWatermarkPath(brainPath, tool string) string {
	return filepath.Join(brainPath, "meta", "harvest_watermark_"+tool+".json")
}

// readHarvestWatermark loads the trigger-state file for tool. A
// missing or unparseable file is treated as "nothing harvested yet",
// not an error.
func readHarvestWatermark(brainPath, tool string) harvestWatermark {
	wm := harvestWatermark{Tool: tool, Sessions: map[string]string{}}
	data, err := os.ReadFile(harvestWatermarkPath(brainPath, tool))
	if err != nil {
		return wm
	}
	if err := json.Unmarshal(data, &wm); err != nil {
		return harvestWatermark{Tool: tool, Sessions: map[string]string{}}
	}
	if wm.Sessions == nil {
		wm.Sessions = map[string]string{}
	}
	return wm
}

func writeHarvestWatermark(brainPath string, wm harvestWatermark) error {
	if err := os.MkdirAll(filepath.Join(brainPath, "meta"), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wm, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(harvestWatermarkPath(brainPath, wm.Tool), data, 0644)
}

// selectHarvestSessions filters candidate sessions down to the ones a
// harvest run should process: not still running (quiet window), newer
// than both the persisted watermark and any --since override, sorted
// oldest-updated-first for deterministic incremental progress, and
// capped to opts.maxSessions(). Pure and tool-agnostic -- exercised
// directly in tests, and reused by both the Codex and Gemini sources on
// cheap "id + timestamp only" stand-ins before either pays the cost of
// reading a full transcript.
func selectHarvestSessions(sessions []HarvestSession, wm harvestWatermark, opts HarvestOptions) (selected []HarvestSession, skippedQuiet int) {
	quietCutoff := opts.now().Add(-harvestQuietWindow)
	var sinceT time.Time
	if opts.Since != "" {
		if t, err := parseFlexibleRFC3339(opts.Since); err == nil {
			sinceT = t
		}
	}

	var candidates []HarvestSession
	for _, s := range sessions {
		if !s.UpdatedAt.Before(quietCutoff) {
			skippedQuiet++
			continue
		}
		if !sinceT.IsZero() && s.UpdatedAt.Before(sinceT) {
			continue
		}
		if last, ok := wm.Sessions[s.ID]; ok {
			if lastT, err := parseFlexibleRFC3339(last); err == nil && !s.UpdatedAt.After(lastT) {
				continue // already harvested at or after this point
			}
		}
		candidates = append(candidates, s)
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt) })
	if max := opts.maxSessions(); len(candidates) > max {
		candidates = candidates[:max]
	}
	return candidates, skippedQuiet
}

// harvestDistillSystemPrompt instructs the LLM on the session ->
// durable-memory extraction task. Mirrors consolidateSystemPrompt's
// tone and provenance-mindedness, adapted from "events" to "one raw
// session transcript from another coding agent".
const harvestDistillSystemPrompt = `You are distilling a coding-agent session transcript into durable memory records for a personal AI assistant called Aida.

Extract ONLY durable, non-obvious memories: user preferences, decisions, project facts, and corrections that would be genuinely useful to recall in a future, unrelated session. Ignore routine tool output, one-off questions with no lasting value, and anything already obvious from context.

Rules:
- Produce 0 to 5 memories. Nothing worth keeping is a common and valid outcome for a session -- return an empty list rather than manufacturing memories to fill a quota.
- Sessions often change direction midway. Capture only where the session ENDED UP: if a decision, preference, or plan was later reversed or abandoned within the transcript, record the final state only (or nothing at all) -- never a superseded intermediate position.
- Each memory needs: "name" (a short, stable, kebab-case identifier for this memory), "description" (one line summary), "type" (one of "fact", "instruction", "event"), and "body" (the full detail, self-contained -- it will be read later with no other context, so spell out what/why).
- "fact": a long-lived assertion (e.g. "user prefers US units"). "instruction": a durable rule or preference the user stated (e.g. "always run make install after Go changes"). "event": a timestamped occurrence worth remembering but not a standing rule or fact (e.g. "shipped v2 of the export pipeline on this date").
- Output ONLY valid JSON matching the schema. No prose, no markdown.`

// harvestDistillSchema is the JSON schema handed to CompleteJSON so the
// provider enforces the response shape.
func harvestDistillSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"memories": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"name":        map[string]interface{}{"type": "string"},
						"description": map[string]interface{}{"type": "string"},
						"type":        map[string]interface{}{"type": "string", "enum": []string{"fact", "instruction", "event"}},
						"body":        map[string]interface{}{"type": "string"},
					},
					"required":             []string{"name", "description", "type", "body"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"memories"},
		"additionalProperties": false,
	}
}

func harvestDistillUserPrompt(sess HarvestSession) string {
	return fmt.Sprintf("## Session\n\nsource: %s\nscope: %s\n\n%s", sess.SourceRef, sess.Scope, sess.Transcript)
}

// harvestDistillResponse is the parsed shape of the LLM's JSON output.
type harvestDistillResponse struct {
	Memories []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Body        string `json:"body"`
	} `json:"memories"`
}

// harvestDistilledSessions runs the LLM distillation pass over an
// already-selected list of sessions (selection -- watermark/since/quiet/
// cap -- is the caller's job; see selectHarvestSessions), writes the
// accepted memories under the given profile (unless dryRun), and
// advances the tool's watermark per successfully processed session.
// dryRun skips every write and the watermark advance entirely, so a
// real re-run still sees the same sessions as candidates.
func (b *Brain) harvestDistilledSessions(ctx context.Context, tool string, selected []HarvestSession, distill DistillFunc, dryRun bool) (*HarvestResult, error) {
	wm := readHarvestWatermark(b.Path, tool)
	result := &HarvestResult{Tool: tool, DryRun: dryRun, Selected: len(selected)}
	changed := false

	for _, sess := range selected {
		systemPrompt := harvestDistillSystemPrompt
		if sess.SystemPromptOverride != "" {
			systemPrompt = sess.SystemPromptOverride
		}
		schema := harvestDistillSchema()
		if sess.SchemaOverride != nil {
			schema = sess.SchemaOverride
		}
		raw, err := distill(ctx, systemPrompt, harvestDistillUserPrompt(sess), schema)
		if err != nil {
			ui.PrintVerbose("Harvest", fmt.Sprintf("%s session %s: distill failed: %v", tool, sess.ID, err))
			continue
		}

		baseTags := []string{tool + "-code"}
		if sess.TagsFromResponse != nil {
			if custom := sess.TagsFromResponse(raw); custom != nil {
				baseTags = custom
			}
		}

		var resp harvestDistillResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			ui.PrintVerbose("Harvest", fmt.Sprintf("%s session %s: parse distill response failed: %v", tool, sess.ID, err))
			continue
		}

		mems := resp.Memories
		if len(mems) > harvestMaxMemoriesPerSession {
			mems = mems[:harvestMaxMemoriesPerSession]
		}

		outcome := HarvestSessionOutcome{SessionID: sess.ID, Scope: sess.Scope}
		for _, m := range mems {
			name := harvestKebab(m.Name)
			body := strings.TrimSpace(m.Body)
			if name == "" || body == "" {
				result.Dropped++
				continue
			}
			memType := MemoryType(strings.ToLower(strings.TrimSpace(m.Type)))
			if !IsValidMemoryType(memType) {
				memType = MemoryFact
			}
			if desc := strings.TrimSpace(m.Description); desc != "" {
				body = desc + "\n\n" + body
			}

			tags := append(append([]string{}, baseTags...), harvestScopeTags(sess.Scope)...)
			tags = append(tags, "session:"+sess.ID)

			rec := MemoryRecord{
				Type:    memType,
				Key:     tool + ":" + sess.ID + ":" + name,
				Body:    body,
				Tags:    tags,
				Source:  sess.SourceRef,
				Profile: tool,
				Created: sess.UpdatedAt.UTC().Format(time.RFC3339),
			}

			if dryRun {
				outcome.Written = append(outcome.Written, rec)
				continue
			}
			written, err := b.WriteMemory(ctx, rec)
			if err != nil {
				ui.PrintVerbose("Harvest", fmt.Sprintf("%s session %s: write memory failed: %v", tool, sess.ID, err))
				continue
			}
			outcome.Written = append(outcome.Written, *written)
		}
		result.Sessions = append(result.Sessions, outcome)

		if !dryRun {
			// RFC3339Nano, not RFC3339: watermark equality must survive a
			// round trip against a source UpdatedAt that carries
			// sub-second precision (e.g. a file mtime), or a truncated-
			// to-the-second watermark reads as "older" forever and every
			// run re-processes the same session.
			wm.Sessions[sess.ID] = sess.UpdatedAt.UTC().Format(time.RFC3339Nano)
			changed = true
		}
	}

	if !dryRun && changed {
		if err := writeHarvestWatermark(b.Path, wm); err != nil {
			ui.PrintVerbose("Harvest", tool+": write watermark failed: "+err.Error())
		}
	}

	return result, nil
}

// harvestDirectMemories writes pre-distilled records verbatim -- no LLM
// call -- gated by the same per-tool watermark as
// harvestDistilledSessions so a static artifact (an Antigravity
// walkthrough.md, GEMINI.md) mirrors again only when it actually
// changes.
func (b *Brain) harvestDirectMemories(ctx context.Context, tool string, items []HarvestDirectMemory, opts HarvestOptions) ([]MemoryRecord, error) {
	wm := readHarvestWatermark(b.Path, tool)
	var written []MemoryRecord
	changed := false

	for _, item := range items {
		if last, ok := wm.Sessions[item.ID]; ok {
			if lastT, err := parseFlexibleRFC3339(last); err == nil && !item.UpdatedAt.After(lastT) {
				continue
			}
		}

		tags := append([]string{tool + "-code"}, harvestScopeTags(item.Scope)...)
		tags = append(tags, item.Tags...)

		rec := MemoryRecord{
			Type:    item.Type,
			Key:     item.Key,
			Body:    item.Body,
			Tags:    tags,
			Source:  item.Source,
			Profile: tool,
			Created: item.UpdatedAt.UTC().Format(time.RFC3339),
		}

		if opts.DryRun {
			written = append(written, rec)
			continue
		}
		w, err := b.WriteMemory(ctx, rec)
		if err != nil {
			ui.PrintVerbose("Harvest", tool+": direct memory write failed: "+err.Error())
			continue
		}
		written = append(written, *w)
		wm.Sessions[item.ID] = item.UpdatedAt.UTC().Format(time.RFC3339Nano) // see harvestDistilledSessions for why Nano
		changed = true
	}

	if !opts.DryRun && changed {
		if err := writeHarvestWatermark(b.Path, wm); err != nil {
			ui.PrintVerbose("Harvest", tool+": write watermark failed: "+err.Error())
		}
	}

	return written, nil
}
