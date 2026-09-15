package brain

// Meetily source for the multi-agent memory bridge harvester (harvest.go).
//
// Meetily is Ryan's local call-recording/transcription app (see
// docs/memory-bridge-multi-agent.md and CLAUDE.md's "Multi-agent memory
// bridge" section). A separate sweep exports finished calls under
// life-log/calls/<YYYY-MM-DD>-<HH-MM>-<slug>/, one folder per call:
//
//   transcripts.json  -- {"last_updated", "segments": [{"text", ...}, ...]}
//                          always present once a call folder exists.
//   summary.json      -- {"markdown": "...", "english_cache": {"markdown": "..."}}
//                          optional -- absent until Meetily finishes
//                          summarizing (or the call errored before that
//                          stage). Top-level "markdown" wins when present;
//                          "english_cache.markdown" is the fallback.
//   meeting.json      -- {"id", "title", "created_at"} -- optional.
//   metadata.json     -- {"created_at", "completed_at", "duration_seconds",
//                          ...} -- present for essentially every call folder,
//                          finished or not.
//
// A folder missing transcripts.json isn't a call worth harvesting yet (still
// recording, or errored before a transcript was written) and is skipped
// entirely -- not even counted as a candidate. GLOSSARY.md and any other
// non-directory entry at the calls root are ignored by construction (only
// directories are considered).
//
// Unlike Codex/Gemini, a Meetily call is a real conversation, not a
// coding-agent session -- there's no equivalent "memory file" to mirror, so
// like Codex/Gemini sessions this is an LLM distillation source, not a
// direct mirror like Antigravity/GEMINI.md. It writes under its own
// "meetily" memory profile and watermark file
// (meta/harvest_watermark_meetily.json), keyed by call folder name and
// gated on the newest mtime among the folder's four files -- the
// folder-based analogue of Codex's session id + updated_at and Gemini's
// file mtime.
//
// One correction that Codex/Gemini don't need: call transcripts are raw STT
// output over Ryan's real conversations, and proper nouns get garbled
// routinely (e.g. "Owens" mis-heard for a surname like "Odinson"). When
// <calls-root>/GLOSSARY.md exists, its content is folded into the distill
// system prompt (see meetilyDistillSystemPrompt) as a correction glossary,
// so distilled records use the corrected real names rather than propagating
// STT noise into the brain.
//
// Classifier pass (aida task #367): the SAME distill call that extracts
// memories also returns a lightweight classification -- tags (chosen only
// from a vocabulary built at harvest time by buildMeetilyTagVocabulary,
// see harvest_meetily_vocab.go), participants, action items, key points
// (the call's "badge" summary), and an is_new_category flag + proposed_tag
// for a call that doesn't fit any vocabulary tag. This rides the
// SchemaOverride/TagsFromResponse hooks added to HarvestSession/
// harvestDistilledSessions for exactly this purpose -- see harvest.go.
// The classification is written to call.json in the call's export folder,
// stamped onto the WriteMemory records' tags (replacing the shared write
// path's default "meetily-code" with "meetily-call" plus the chosen
// tags), and, when is_new_category, both tags the call "needs-review" and
// opens an aida task for Ryan to look at it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// meetilyTranscriptHalfBudget mirrors codexTranscriptHalfBudget/
// geminiTranscriptHalfBudget: ~30k total characters per call, head+tail.
const meetilyTranscriptHalfBudget = 15000

// meetilyCallsRootEnv overrides the default calls root
// (~/dev/life-log/calls), matching the AIDA_* env-var convention used
// elsewhere (e.g. AIDA_TTS_VOLUME, AIDA_LMD_TOKEN) for lightweight,
// no-config-schema-change overrides.
const meetilyCallsRootEnv = "AIDA_MEETILY_CALLS_ROOT"

// meetilyCallFiles are the files whose mtime contributes to a call
// folder's UpdatedAt -- whichever of these are present, the newest wins.
var meetilyCallFiles = []string{"transcripts.json", "summary.json", "meeting.json", "metadata.json"}

// meetilyFolderPrefixRe matches a call folder's leading
// <YYYY-MM-DD>-<HH-MM>- date/time prefix, capturing the date, time, and
// remaining slug separately.
var meetilyFolderPrefixRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})-(\d{2}-\d{2})-(.*)$`)

// meetilyCallMeta is the cheap (folder, path, newest-mtime) triple listed
// before any file content is read -- the watermark filter runs against
// this, mirroring listCodexSessionMetas's two-phase design.
type meetilyCallMeta struct {
	Folder    string
	Path      string
	UpdatedAt time.Time
}

// listMeetilyCallMetas enumerates call folders directly under callsRoot.
// Missing callsRoot is not an error (Meetily export sweep may not be set
// up on this machine) -- returns an empty slice. Non-directory entries
// (GLOSSARY.md, .DS_Store, ...) and directories missing transcripts.json
// (call still recording, or errored before a transcript was written) are
// skipped -- they are never candidates at all.
func listMeetilyCallMetas(callsRoot string) ([]meetilyCallMeta, error) {
	entries, err := os.ReadDir(callsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []meetilyCallMeta
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		folderPath := filepath.Join(callsRoot, e.Name())
		if _, err := os.Stat(filepath.Join(folderPath, "transcripts.json")); err != nil {
			continue
		}

		var newest time.Time
		for _, name := range meetilyCallFiles {
			info, err := os.Stat(filepath.Join(folderPath, name))
			if err != nil {
				continue
			}
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if newest.IsZero() {
			continue
		}
		out = append(out, meetilyCallMeta{Folder: e.Name(), Path: folderPath, UpdatedAt: newest})
	}
	return out, nil
}

// meetilyTranscriptFile is the shape of one call's transcripts.json.
type meetilyTranscriptFile struct {
	Segments []struct {
		Text string `json:"text"`
	} `json:"segments"`
}

// meetilySummaryFile is the shape of one call's summary.json. Top-level
// Markdown wins; EnglishCache.Markdown is the fallback (present on some
// exports and absent on others -- both are treated as the AI-generated
// meeting summary).
type meetilySummaryFile struct {
	Markdown     string `json:"markdown"`
	EnglishCache struct {
		Markdown string `json:"markdown"`
	} `json:"english_cache"`
}

// meetilyDateFromFolder extracts a human-readable date/time from a call
// folder's <YYYY-MM-DD>-<HH-MM>- prefix, falling back to the raw folder
// name when it doesn't match the expected shape.
func meetilyDateFromFolder(folder string) string {
	if m := meetilyFolderPrefixRe.FindStringSubmatch(folder); m != nil {
		return m[1] + " " + strings.ReplaceAll(m[2], "-", ":")
	}
	return folder
}

// meetilyTitleFromFolder derives a fallback title from a call folder's
// slug (the part after the date/time prefix) when meeting.json is absent
// or has no title -- kebab-case dashes become spaces.
func meetilyTitleFromFolder(folder string) string {
	if m := meetilyFolderPrefixRe.FindStringSubmatch(folder); m != nil && m[3] != "" {
		return strings.ReplaceAll(m[3], "-", " ")
	}
	return folder
}

// meetilyCallIdentity resolves a call's meeting id, title, and human
// date, preferring meeting.json's id/title (when present and non-empty)
// and falling back to values derived from the folder name otherwise.
// Shared by buildMeetilyHarvestSession (title/date) and call.json writing/
// the new-category review task (id/title/date), so both agree on the same
// values.
func meetilyCallIdentity(meta meetilyCallMeta) (id, title, date string) {
	date = meetilyDateFromFolder(meta.Folder)
	title = meetilyTitleFromFolder(meta.Folder)
	if mdata, err := os.ReadFile(filepath.Join(meta.Path, "meeting.json")); err == nil {
		var mf struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		if json.Unmarshal(mdata, &mf) == nil {
			id = strings.TrimSpace(mf.ID)
			if t := strings.TrimSpace(mf.Title); t != "" {
				title = t
			}
		}
	}
	return id, title, date
}

// buildMeetilyHarvestSession reads one call folder's transcripts.json
// (required), summary.json and meeting.json (both optional) into a
// HarvestSession. ok is false when the transcript has no segment text
// worth distilling (normal, not an error) -- an unparseable
// transcripts.json is a real error since the caller already verified the
// file exists.
func buildMeetilyHarvestSession(meta meetilyCallMeta) (sess HarvestSession, ok bool, err error) {
	data, err := os.ReadFile(filepath.Join(meta.Path, "transcripts.json"))
	if err != nil {
		return HarvestSession{}, false, err
	}
	var tf meetilyTranscriptFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return HarvestSession{}, false, fmt.Errorf("unparseable meetily transcript %s: %w", meta.Folder, err)
	}

	var text strings.Builder
	for _, seg := range tf.Segments {
		t := strings.TrimSpace(seg.Text)
		if t == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteString(" ")
		}
		text.WriteString(t)
	}
	transcript := strings.TrimSpace(text.String())
	if transcript == "" {
		return HarvestSession{}, false, nil
	}

	_, title, date := meetilyCallIdentity(meta)

	summary := "(no summary available)"
	if sdata, err := os.ReadFile(filepath.Join(meta.Path, "summary.json")); err == nil {
		var sf meetilySummaryFile
		if json.Unmarshal(sdata, &sf) == nil {
			if md := strings.TrimSpace(sf.Markdown); md != "" {
				summary = md
			} else if md := strings.TrimSpace(sf.EnglishCache.Markdown); md != "" {
				summary = md
			}
		}
	}

	body := fmt.Sprintf("## Meeting\nTitle: %s\nDate: %s\n\n## Summary\n%s\n\n## Transcript\n%s",
		title, date, summary,
		capTranscript(transcript, meetilyTranscriptHalfBudget, meetilyTranscriptHalfBudget))

	return HarvestSession{
		ID:         meta.Folder,
		UpdatedAt:  meta.UpdatedAt,
		Scope:      "global",
		Transcript: body,
		SourceRef:  "meetily:" + meta.Path,
	}, true, nil
}

// meetilyDistillSystemPromptBase frames the distillation task for a real
// call transcript rather than a coding-agent session -- the coding-agent
// framing in harvestDistillSystemPrompt doesn't fit ("ignore routine tool
// output" makes no sense for a phone call). The same call also asks for a
// lightweight classification (tags/participants/action items/key points/
// new-category flag) -- see the "## Classification" section appended by
// meetilyDistillSystemPrompt.
const meetilyDistillSystemPromptBase = `You are distilling a personal call recording transcript into durable memory records AND a lightweight classification, for a personal AI assistant called Aida.

This is a real conversation (business or personal, not a coding-agent session), captured via speech-to-text.

## Memories

Extract ONLY durable, non-obvious memories: facts about people/companies/situations discussed, decisions made, commitments given, and notable events -- things that would be genuinely useful to recall in a future, unrelated context. Ignore small talk, filler, and anything already obvious from the summary alone.

- Produce 0 to 5 memories under "memories". Nothing worth keeping is a common and valid outcome for a call -- return an empty list rather than manufacturing memories to fill a quota.
- Each memory needs: "name" (a short, stable, kebab-case identifier for this memory), "description" (one line summary), "type" (one of "fact", "instruction", "event"), and "body" (the full detail, self-contained -- it will be read later with no other context, so spell out who/what/why).
- "fact": a long-lived assertion about a person, company, or situation (e.g. "X's daughter attends Y"). "instruction": a durable rule, preference, or commitment made on the call (e.g. "agreed to send the proposal by Friday"). "event": a timestamped occurrence worth remembering but not a standing rule or fact (e.g. "had an intro call with X about Y on this date").

## Classification

Also return, from this SAME call:
- "tags": zero or more tags for this call, chosen ONLY from the vocabulary listed below -- never invent a tag here, never return anything not in that list. Empty is valid when nothing in the vocabulary fits.
- "participants": names of the people on the call (correct per the glossary below when applicable).
- "action_items": concrete follow-ups or commitments from the call, as short strings.
- "key_points": the handful of headline takeaways from the call, as short strings -- these become the call's summary badge.
- "is_new_category": true only when NONE of the vocabulary tags fit this call's topic (e.g. a brand-new consulting client, a personal catch-up with no matching tag); otherwise false.
- "proposed_tag": a short kebab-case tag name proposing this call's topic, ONLY when is_new_category is true; empty string otherwise.

The transcript is raw speech-to-text output and routinely garbles proper nouns. Only correct a garbled name using the glossary below, or when you are otherwise highly confident of the real name from context -- never invent a correction you're not sure of.

Output ONLY valid JSON matching the schema. No prose, no markdown.`

// meetilyDistillSystemPrompt composes the base prompt with the run's tag
// vocabulary (always present, even if empty -- the LLM needs to know
// there's nothing to choose from) and, when available, a correction
// glossary block instructing the LLM to fix STT-garbled proper nouns
// before writing them into a memory or participant name. glossary is the
// full content of <calls-root>/GLOSSARY.md; an empty glossary (missing
// file) omits that section entirely.
func meetilyDistillSystemPrompt(glossary string, vocabulary []string) string {
	prompt := meetilyDistillSystemPromptBase

	prompt += "\n\n## Tag vocabulary\n\n"
	if len(vocabulary) == 0 {
		prompt += `(empty -- there is nothing to choose from yet; "tags" must be an empty array. If this call has an obvious topic, set is_new_category true with a proposed_tag.)`
	} else {
		prompt += strings.Join(vocabulary, ", ")
	}

	if glossary = strings.TrimSpace(glossary); glossary != "" {
		prompt += "\n\n## Correction glossary\n\n" +
			"Transcripts are STT output and routinely garble proper nouns. Use this glossary to correct names before writing any memory or participant " +
			"(e.g. \"Owens\" -> Thor Odinson). Every distilled record MUST use the corrected real names, never the garbled transcript form.\n\n" +
			glossary
	}

	return prompt
}

// meetilyDistillSchema extends the generic memories-only shape
// (harvestDistillSchema) with the classification fields above -- all
// returned by the SAME distill call via HarvestSession.SchemaOverride.
func meetilyDistillSchema() map[string]interface{} {
	stringArray := map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}
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
			"tags":            stringArray,
			"participants":    stringArray,
			"action_items":    stringArray,
			"key_points":      stringArray,
			"is_new_category": map[string]interface{}{"type": "boolean"},
			"proposed_tag":    map[string]interface{}{"type": "string"},
		},
		"required":             []string{"memories", "tags", "participants", "action_items", "key_points", "is_new_category", "proposed_tag"},
		"additionalProperties": false,
	}
}

// meetilyClassifierData is the classification half of the distill
// response -- everything besides "memories". Captured per session via
// HarvestSession.TagsFromResponse (see harvestMeetily) and used both to
// derive the base memory tags and, after harvestDistilledSessions
// returns, to write call.json and (when IsNewCategory) open a review
// task.
type meetilyClassifierData struct {
	Tags          []string `json:"tags"`
	Participants  []string `json:"participants"`
	ActionItems   []string `json:"action_items"`
	KeyPoints     []string `json:"key_points"`
	IsNewCategory bool     `json:"is_new_category"`
	ProposedTag   string   `json:"proposed_tag"`
}

// parseMeetilyClassifierData unmarshals the classification fields out of
// a raw distill response -- the same JSON harvestDistilledSessions
// already parses for "memories". An unparseable response degrades to the
// zero value (no tags, not a new category) rather than an error, matching
// the rest of the harvester's "log and skip" posture for malformed LLM
// output.
func parseMeetilyClassifierData(raw string) meetilyClassifierData {
	var data meetilyClassifierData
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return meetilyClassifierData{}
	}
	data.ProposedTag = strings.TrimSpace(data.ProposedTag)
	return data
}

// filterTagsAgainstVocabulary keeps only the tags that are actually in
// vocabulary (case-insensitive match, deduplicated), rewriting each to
// the vocabulary's own casing so a tag never drifts from its canonical
// form. This is the enforcement half of "chosen ONLY from the provided
// vocabulary" -- the prompt asks nicely, this makes it a hard guarantee
// regardless of what the LLM actually returns.
func filterTagsAgainstVocabulary(tags, vocabulary []string) []string {
	canon := make(map[string]string, len(vocabulary))
	for _, v := range vocabulary {
		canon[strings.ToLower(strings.TrimSpace(v))] = v
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		v, ok := canon[strings.ToLower(strings.TrimSpace(t))]
		if !ok || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// readMeetilyGlossary reads <callsRoot>/GLOSSARY.md, returning "" (no
// error) when it doesn't exist -- the glossary is an optional convenience,
// not a requirement.
func readMeetilyGlossary(callsRoot string) string {
	data, err := os.ReadFile(filepath.Join(callsRoot, "GLOSSARY.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// meetilyDefaultCallsRoot resolves the default calls root:
// meetilyCallsRootEnv when set, else ~/dev/life-log/calls.
func meetilyDefaultCallsRoot() (string, error) {
	if env := strings.TrimSpace(os.Getenv(meetilyCallsRootEnv)); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "dev", "life-log", "calls"), nil
}

// meetilyCallJSON is the shape written to call.json in each call's export
// folder, next to summary.json -- the persisted, human-inspectable form
// of this call's classification.
type meetilyCallJSON struct {
	MeetingID     string   `json:"meeting_id,omitempty"`
	Title         string   `json:"title"`
	Date          string   `json:"date"`
	Tags          []string `json:"tags"`
	Participants  []string `json:"participants"`
	ActionItems   []string `json:"action_items"`
	KeyPoints     []string `json:"key_points"`
	IsNewCategory bool     `json:"is_new_category"`
	ProposedTag   string   `json:"proposed_tag,omitempty"`
}

// writeMeetilyCallClassification writes call.json into the call's export
// folder. Idempotent: always a full overwrite of the same path, so
// re-running with the same classification (or a corrected one) converges
// rather than accumulating stale content.
func writeMeetilyCallClassification(meta meetilyCallMeta, data meetilyClassifierData) error {
	id, title, date := meetilyCallIdentity(meta)
	out := meetilyCallJSON{
		MeetingID:     id,
		Title:         title,
		Date:          date,
		Tags:          data.Tags,
		Participants:  data.Participants,
		ActionItems:   data.ActionItems,
		KeyPoints:     data.KeyPoints,
		IsNewCategory: data.IsNewCategory,
		ProposedTag:   data.ProposedTag,
	}
	payload, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(meta.Path, "call.json"), payload, 0644)
}

// meetilyBulletList renders items as a "- " bulleted block, or "(none)"
// when empty -- used in the new-category review task body.
func meetilyBulletList(items []string) string {
	if len(items) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, it := range items {
		sb.WriteString("- ")
		sb.WriteString(it)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// createMeetilyReviewTask opens an aida task for a call the classifier
// couldn't fit into any existing tag vocabulary -- Ryan decides whether
// proposed_tag should become a real vocabulary entry (a new client,
// project, or standing topic) or the call was genuinely one-off.
//
// TODO(meetily-notify): once serve wiring for this pass lands, push a
// notification through internal/jarvis/notify (the same queue
// watchJobsForNotifications drains for job state transitions in
// internal/cli/serve.go) so Ryan hears about a new call category on the
// next wake instead of only discovering it via `aida tasks`.
func (b *Brain) createMeetilyReviewTask(meta meetilyCallMeta, data meetilyClassifierData) error {
	_, title, date := meetilyCallIdentity(meta)
	proposed := data.ProposedTag
	if proposed == "" {
		proposed = "uncategorized"
	}
	taskTitle := fmt.Sprintf("Review new call category: %s (%s, %s)", proposed, title, date)
	body := fmt.Sprintf(
		"Meetily harvest flagged this call as not fitting any existing tag vocabulary.\n\n"+
			"Call: %s\nDate: %s\nProposed tag: %s\n\nKey points:\n%s",
		title, date, proposed, meetilyBulletList(data.KeyPoints))
	_, err := b.AddTask(taskTitle, []string{"calls", "needs-review"}, body)
	return err
}

// HarvestMeetily distills new/changed exported Meetily call recordings
// into typed memory records under the "meetily" profile, incrementally via
// a persisted watermark. distill is typically client.CompleteJSON.
func (b *Brain) HarvestMeetily(ctx context.Context, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	root, err := meetilyDefaultCallsRoot()
	if err != nil {
		return nil, err
	}
	libEntities, err := meetilyLibrarySourceEntities()
	if err != nil {
		ui.PrintVerbose("Harvest", "meetily: library source entities unavailable: "+err.Error())
		libEntities = nil
	}
	vocabulary := b.buildMeetilyTagVocabulary(libEntities)
	return b.harvestMeetily(ctx, root, distill, vocabulary, opts)
}

// harvestMeetily is the injectable core: callsRoot is overridable so tests
// point at a fixture tree instead of the real life-log calls directory,
// and vocabulary is passed in already-built so tests supply a literal
// list instead of touching brain.db/~/.aida/library.yaml. Two-phase like
// harvestCodex: list cheap metadata, filter via selectHarvestSessions,
// then read only the surviving call folders.
func (b *Brain) harvestMeetily(ctx context.Context, callsRoot string, distill DistillFunc, vocabulary []string, opts HarvestOptions) (*HarvestResult, error) {
	metas, err := listMeetilyCallMetas(callsRoot)
	if err != nil {
		return nil, fmt.Errorf("list meetily calls: %w", err)
	}

	wm := readHarvestWatermark(b.Path, "meetily")
	stand := make([]HarvestSession, len(metas))
	metaByFolder := make(map[string]meetilyCallMeta, len(metas))
	for i, m := range metas {
		stand[i] = HarvestSession{ID: m.Folder, UpdatedAt: m.UpdatedAt}
		metaByFolder[m.Folder] = m
	}
	selectedStand, skippedQuiet := selectHarvestSessions(stand, wm, opts)

	systemPrompt := meetilyDistillSystemPrompt(readMeetilyGlossary(callsRoot), vocabulary)
	schema := meetilyDistillSchema()

	var sessions []HarvestSession
	for _, s := range selectedStand {
		sess, ok, err := buildMeetilyHarvestSession(metaByFolder[s.ID])
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		sess.SystemPromptOverride = systemPrompt
		sess.SchemaOverride = schema
		sessions = append(sessions, sess)
	}

	// classData is filled in per session by each session's
	// TagsFromResponse closure, called once by harvestDistilledSessions
	// right after that session's (single) distill call succeeds. Reading
	// it back after the call below lets the same LLM response drive both
	// the memory tags (via the closure's return value) and call.json /
	// the new-category review task (via this map) with no second LLM
	// call.
	classData := make(map[string]meetilyClassifierData, len(sessions))
	for i := range sessions {
		id := sessions[i].ID
		sessions[i].TagsFromResponse = func(raw string) []string {
			data := parseMeetilyClassifierData(raw)
			data.Tags = filterTagsAgainstVocabulary(data.Tags, vocabulary)
			classData[id] = data

			tags := make([]string, 0, len(data.Tags)+2)
			tags = append(tags, "meetily-call")
			tags = append(tags, data.Tags...)
			if data.IsNewCategory {
				tags = append(tags, "needs-review")
			}
			return tags
		}
	}

	result, err := b.harvestDistilledSessions(ctx, "meetily", sessions, distill, opts.DryRun)
	if err != nil {
		return nil, err
	}
	result.CandidatesTotal = len(metas)
	result.SkippedQuiet = skippedQuiet
	result.Selected = len(sessions)

	if !opts.DryRun {
		for _, outcome := range result.Sessions {
			data, ok := classData[outcome.SessionID]
			if !ok {
				continue
			}
			meta := metaByFolder[outcome.SessionID]
			if err := writeMeetilyCallClassification(meta, data); err != nil {
				ui.PrintVerbose("Harvest", "meetily call "+outcome.SessionID+": write call.json failed: "+err.Error())
			}
			if data.IsNewCategory {
				if err := b.createMeetilyReviewTask(meta, data); err != nil {
					ui.PrintVerbose("Harvest", "meetily call "+outcome.SessionID+": create review task failed: "+err.Error())
				}
			}
		}
	}

	return result, nil
}
