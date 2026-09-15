package brain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeMeetilyCallFixture lays down one <callsRoot>/<folder>/ call
// directory with transcripts.json (required) and, when non-empty,
// summary.json and meeting.json. metadata.json is always written since
// real exports always carry it.
func writeMeetilyCallFixture(t *testing.T, callsRoot, folder string, segments []string, summaryMarkdown, meetingTitle string) string {
	t.Helper()

	dir := filepath.Join(callsRoot, folder)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir call dir: %v", err)
	}

	type seg struct {
		Text string `json:"text"`
	}
	var segs []seg
	for _, s := range segments {
		segs = append(segs, seg{Text: s})
	}
	transcript := map[string]interface{}{"last_updated": "2026-08-24T18:00:00Z", "segments": segs}
	writeMeetilyJSON(t, filepath.Join(dir, "transcripts.json"), transcript)

	writeMeetilyJSON(t, filepath.Join(dir, "metadata.json"), map[string]interface{}{
		"created_at":   "2026-08-24T18:00:00Z",
		"completed_at": "2026-08-24T18:30:00Z",
		"status":       "completed",
	})

	if summaryMarkdown != "" {
		writeMeetilyJSON(t, filepath.Join(dir, "summary.json"), map[string]interface{}{
			"markdown": summaryMarkdown,
		})
	}
	if meetingTitle != "" {
		writeMeetilyJSON(t, filepath.Join(dir, "meeting.json"), map[string]interface{}{
			"id":         "meeting-" + folder,
			"title":      meetingTitle,
			"created_at": "2026-08-24T18:00:00Z",
		})
	}

	// Fixture files are written with the real wall-clock mtime, which
	// would otherwise land inside harvestQuietWindow relative to the
	// fixed opts.Now most tests use and make the call look "still
	// recording". Pin every file's mtime to a fixed point in the past so
	// selection behaves deterministically regardless of when the test
	// actually runs; tests that specifically exercise the quiet window
	// re-touch mtimes themselves via touchMeetilyFile.
	fixedMtime := mustParseTime(t, "2026-08-24T18:30:00Z")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read call dir: %v", err)
	}
	for _, e := range entries {
		touchMeetilyFile(t, filepath.Join(dir, e.Name()), fixedMtime)
	}

	return dir
}

func writeMeetilyJSON(t *testing.T, path string, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// touchMeetilyFile sets a file's mtime, used to control which file is
// "newest" in a call folder (and, via the quiet-window tests, how recently
// the folder as a whole looks updated).
func touchMeetilyFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestListMeetilyCallMetas(t *testing.T) {
	root := t.TempDir()
	writeMeetilyCallFixture(t, root, "2026-08-24-14-09-chief-tech-advisor", []string{"hello"}, "", "")
	writeMeetilyCallFixture(t, root, "2026-08-25-10-00-david-koenig", []string{"world"}, "", "")

	// GLOSSARY.md at the root must never be treated as a call folder.
	if err := os.WriteFile(filepath.Join(root, "GLOSSARY.md"), []byte("glossary"), 0644); err != nil {
		t.Fatalf("write GLOSSARY.md: %v", err)
	}

	metas, err := listMeetilyCallMetas(root)
	if err != nil {
		t.Fatalf("listMeetilyCallMetas: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("expected 2 call metas (GLOSSARY.md excluded), got %d: %+v", len(metas), metas)
	}
}

func TestListMeetilyCallMetas_MissingRoot(t *testing.T) {
	metas, err := listMeetilyCallMetas(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected no error for missing calls root, got %v", err)
	}
	if metas != nil {
		t.Errorf("expected nil metas, got %v", metas)
	}
}

func TestListMeetilyCallMetas_SkipsFolderMissingTranscript(t *testing.T) {
	root := t.TempDir()
	// A folder with no transcripts.json at all -- e.g. a call still
	// recording -- must not be a candidate.
	incomplete := filepath.Join(root, "2026-08-25-11-00-still-recording")
	if err := os.MkdirAll(incomplete, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeMeetilyJSON(t, filepath.Join(incomplete, "metadata.json"), map[string]interface{}{"status": "recording"})

	writeMeetilyCallFixture(t, root, "2026-08-25-10-00-finished-call", []string{"hi"}, "", "")

	metas, err := listMeetilyCallMetas(root)
	if err != nil {
		t.Fatalf("listMeetilyCallMetas: %v", err)
	}
	if len(metas) != 1 || metas[0].Folder != "2026-08-25-10-00-finished-call" {
		t.Fatalf("expected only the finished call, got %+v", metas)
	}
}

func TestBuildMeetilyHarvestSession(t *testing.T) {
	root := t.TempDir()
	dir := writeMeetilyCallFixture(t, root, "2026-08-24-14-09-chief-tech-advisor",
		[]string{"Hi there.", "Let's talk about the CTA practice."},
		"# Summary\n\nDiscussed the CTA practice launch.",
		"Chief Tech Advisor Practice Launch")

	meta := meetilyCallMeta{Folder: "2026-08-24-14-09-chief-tech-advisor", Path: dir, UpdatedAt: mustParseTime(t, "2026-08-24T18:30:00Z")}
	sess, ok, err := buildMeetilyHarvestSession(meta)
	if err != nil {
		t.Fatalf("buildMeetilyHarvestSession: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if sess.ID != "2026-08-24-14-09-chief-tech-advisor" {
		t.Errorf("ID = %q", sess.ID)
	}
	if sess.Scope != "global" {
		t.Errorf("scope = %q, want global", sess.Scope)
	}
	if !contains(sess.Transcript, "Chief Tech Advisor Practice Launch") {
		t.Errorf("transcript missing title: %q", sess.Transcript)
	}
	if !contains(sess.Transcript, "2026-08-24 14:09") {
		t.Errorf("transcript missing date derived from folder name: %q", sess.Transcript)
	}
	if !contains(sess.Transcript, "Discussed the CTA practice launch.") {
		t.Errorf("transcript missing summary: %q", sess.Transcript)
	}
	if !contains(sess.Transcript, "Hi there.") || !contains(sess.Transcript, "Let's talk about the CTA practice.") {
		t.Errorf("transcript missing segment text: %q", sess.Transcript)
	}
	if sess.SourceRef != "meetily:"+dir {
		t.Errorf("SourceRef = %q", sess.SourceRef)
	}
}

func TestBuildMeetilyHarvestSession_FallsBackToEnglishCacheMarkdown(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026-08-24-14-09-some-call")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeMeetilyJSON(t, filepath.Join(dir, "transcripts.json"), map[string]interface{}{
		"segments": []map[string]string{{"text": "hello"}},
	})
	// No top-level "markdown" key -- only english_cache.markdown.
	writeMeetilyJSON(t, filepath.Join(dir, "summary.json"), map[string]interface{}{
		"english_cache": map[string]string{"markdown": "cached summary text"},
	})

	meta := meetilyCallMeta{Folder: "2026-08-24-14-09-some-call", Path: dir, UpdatedAt: mustParseTime(t, "2026-08-24T18:30:00Z")}
	sess, ok, err := buildMeetilyHarvestSession(meta)
	if err != nil || !ok {
		t.Fatalf("buildMeetilyHarvestSession: ok=%v err=%v", ok, err)
	}
	if !contains(sess.Transcript, "cached summary text") {
		t.Errorf("expected english_cache.markdown fallback in transcript: %q", sess.Transcript)
	}
}

func TestBuildMeetilyHarvestSession_TitleFallsBackToFolderSlug(t *testing.T) {
	root := t.TempDir()
	dir := writeMeetilyCallFixture(t, root, "2026-08-25-10-00-david-koenig", []string{"hello"}, "", "")

	meta := meetilyCallMeta{Folder: "2026-08-25-10-00-david-koenig", Path: dir, UpdatedAt: mustParseTime(t, "2026-08-25T14:00:00Z")}
	sess, ok, err := buildMeetilyHarvestSession(meta)
	if err != nil || !ok {
		t.Fatalf("buildMeetilyHarvestSession: ok=%v err=%v", ok, err)
	}
	if !contains(sess.Transcript, "Title: david koenig") {
		t.Errorf("expected folder-derived title, got: %q", sess.Transcript)
	}
	if !contains(sess.Transcript, "(no summary available)") {
		t.Errorf("expected placeholder summary text, got: %q", sess.Transcript)
	}
}

func TestBuildMeetilyHarvestSession_EmptyTranscriptIsNotOk(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026-08-25-10-00-empty-call")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeMeetilyJSON(t, filepath.Join(dir, "transcripts.json"), map[string]interface{}{"segments": []map[string]string{}})

	meta := meetilyCallMeta{Folder: "2026-08-25-10-00-empty-call", Path: dir, UpdatedAt: mustParseTime(t, "2026-08-25T14:00:00Z")}
	_, ok, err := buildMeetilyHarvestSession(meta)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if ok {
		t.Error("expected ok=false for a transcript with no segment text")
	}
}

func TestMeetilyDistillSystemPrompt_NoGlossaryNoVocabulary(t *testing.T) {
	got := meetilyDistillSystemPrompt("", nil)
	if !contains(got, meetilyDistillSystemPromptBase) {
		t.Error("expected base prompt to still be present")
	}
	if !contains(got, "nothing to choose from yet") {
		t.Error("expected the empty-vocabulary placeholder when vocabulary is nil")
	}
	if contains(got, "Correction glossary") {
		t.Error("expected no correction glossary section when glossary is empty")
	}
}

func TestMeetilyDistillSystemPrompt_WithGlossaryAndVocabulary(t *testing.T) {
	glossary := `ButterStack (mis: "butter sack"), Thor Odinson (mis: "Owens")`
	vocabulary := []string{"cta", "butterstack", "hoa"}
	got := meetilyDistillSystemPrompt(glossary, vocabulary)
	if !contains(got, meetilyDistillSystemPromptBase) {
		t.Error("expected base prompt to still be present")
	}
	if !contains(got, "cta, butterstack, hoa") {
		t.Errorf("expected vocabulary listed in the prompt: %q", got)
	}
	if !contains(got, glossary) {
		t.Error("expected glossary content to be included verbatim")
	}
	if !contains(got, "correct names") {
		t.Error("expected a correction instruction referencing the glossary")
	}
}

func TestReadMeetilyGlossary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "GLOSSARY.md"), []byte("# Glossary\n\nButterStack, not butter sack."), 0644); err != nil {
		t.Fatalf("write GLOSSARY.md: %v", err)
	}
	got := readMeetilyGlossary(root)
	if !contains(got, "ButterStack, not butter sack.") {
		t.Errorf("readMeetilyGlossary = %q", got)
	}
}

func TestReadMeetilyGlossary_Missing(t *testing.T) {
	got := readMeetilyGlossary(t.TempDir())
	if got != "" {
		t.Errorf("expected empty string for missing GLOSSARY.md, got %q", got)
	}
}

// TestHarvestMeetily_EndToEnd exercises the full orchestrator against a
// fixture tree: list -> filter -> read call -> distill (with glossary
// injected) -> write -> watermark, then a second run picking up nothing
// new.
func TestHarvestMeetily_EndToEnd(t *testing.T) {
	root := t.TempDir()
	writeMeetilyCallFixture(t, root, "2026-08-24-14-09-chief-tech-advisor",
		[]string{"We agreed to launch the CTA practice next month."},
		"Discussed CTA practice launch.", "CTA Practice Launch")
	if err := os.WriteFile(filepath.Join(root, "GLOSSARY.md"), []byte("CTA = ChiefTechAdvisor, Kevin's LLC."), 0644); err != nil {
		t.Fatalf("write GLOSSARY.md: %v", err)
	}

	b := newTestBrain(t)
	ctx := context.Background()

	var capturedSystemPrompt string
	distill := func(_ context.Context, systemPrompt, _ string, _ map[string]interface{}) (string, error) {
		capturedSystemPrompt = systemPrompt
		resp := harvestDistillResponse{Memories: []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
			Body        string `json:"body"`
		}{
			{Name: "cta-practice-launch", Description: "d", Type: "event", Body: "Agreed to launch the CTA practice."},
		}}
		data, err := json.Marshal(resp)
		return string(data), err
	}

	result, err := b.harvestMeetily(ctx, root, distill, nil, HarvestOptions{Now: mustParseTime(t, "2026-08-25T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestMeetily: %v", err)
	}
	if result.CandidatesTotal != 1 {
		t.Errorf("CandidatesTotal = %d, want 1", result.CandidatesTotal)
	}
	if result.Selected != 1 {
		t.Errorf("Selected = %d, want 1", result.Selected)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected 1 session with 1 memory, got %+v", result.Sessions)
	}
	rec := result.Sessions[0].Written[0]
	if rec.Profile != "meetily" {
		t.Errorf("profile = %q, want meetily", rec.Profile)
	}
	if rec.Key != "meetily:2026-08-24-14-09-chief-tech-advisor:cta-practice-launch" {
		t.Errorf("key = %q", rec.Key)
	}
	if !contains(capturedSystemPrompt, "CTA = ChiefTechAdvisor, Kevin's LLC.") {
		t.Errorf("glossary content did not reach the distill system prompt: %q", capturedSystemPrompt)
	}

	// Second run: watermark advanced, nothing new.
	result2, err := b.harvestMeetily(ctx, root, distill, nil, HarvestOptions{Now: mustParseTime(t, "2026-08-25T00:00:00Z")})
	if err != nil {
		t.Fatalf("harvestMeetily (2nd): %v", err)
	}
	if result2.Selected != 0 {
		t.Errorf("second run Selected = %d, want 0", result2.Selected)
	}
}

// TestHarvestMeetily_SkipsStillRecordingCall verifies a call folder whose
// newest file changed within the last 10 minutes (harvestQuietWindow) is
// treated as still in progress and excluded.
func TestHarvestMeetily_SkipsStillRecordingCall(t *testing.T) {
	root := t.TempDir()
	now := mustParseTime(t, "2026-08-25T12:00:00Z")
	dir := writeMeetilyCallFixture(t, root, "2026-08-25-11-58-live-call", []string{"still talking"}, "", "")
	touchMeetilyFile(t, filepath.Join(dir, "transcripts.json"), now.Add(-2*time.Minute))
	touchMeetilyFile(t, filepath.Join(dir, "metadata.json"), now.Add(-2*time.Minute))

	b := newTestBrain(t)
	ctx := context.Background()
	distill := fakeDistillOne("x", "y")

	result, err := b.harvestMeetily(ctx, root, distill, nil, HarvestOptions{Now: now})
	if err != nil {
		t.Fatalf("harvestMeetily: %v", err)
	}
	if result.SkippedQuiet != 1 {
		t.Errorf("SkippedQuiet = %d, want 1", result.SkippedQuiet)
	}
	if result.Selected != 0 {
		t.Errorf("Selected = %d, want 0", result.Selected)
	}
}
