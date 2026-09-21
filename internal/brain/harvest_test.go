package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := parseFlexibleRFC3339(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestCwdToProjectSlug(t *testing.T) {
	got := cwdToProjectSlug("/Users/fakehome/dev/aida")
	want := "-Users-fakehome-dev-aida"
	if got != want {
		t.Errorf("cwdToProjectSlug = %q, want %q", got, want)
	}
}

func TestCapTranscript(t *testing.T) {
	short := "hello world"
	if got := capTranscript(short, 100, 100); got != short {
		t.Errorf("short transcript should pass through unchanged, got %q", got)
	}

	long := ""
	for i := 0; i < 100; i++ {
		long += fmt.Sprintf("line-%03d ", i)
	}
	capped := capTranscript(long, 20, 20)
	if len(capped) >= len(long) {
		t.Errorf("expected capped transcript shorter than original: %d vs %d", len(capped), len(long))
	}
	if capped[:20] != long[:20] {
		t.Error("capped transcript should preserve the head")
	}
	tail := long[len(long)-20:]
	if capped[len(capped)-20:] != tail {
		t.Error("capped transcript should preserve the tail")
	}
}

func TestHarvestKebab(t *testing.T) {
	cases := map[string]string{
		"User Prefers Dark Mode": "user-prefers-dark-mode",
		"  already-kebab  ":      "already-kebab",
		"Weird!!Chars??":         "weird-chars",
		"":                       "",
		"---":                    "",
	}
	for in, want := range cases {
		if got := harvestKebab(in); got != want {
			t.Errorf("harvestKebab(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHarvestScopeTags(t *testing.T) {
	if got := harvestScopeTags("global"); len(got) != 1 || got[0] != "scope:global" {
		t.Errorf("global scope tags = %v, want [scope:global]", got)
	}
	got := harvestScopeTags("project:-Users-ryan-dev-aida")
	want := []string{"scope:project", "project:-Users-ryan-dev-aida"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("project scope tags = %v, want %v", got, want)
	}
}

// TestSelectHarvestSessions_QuietWindow verifies a session updated
// within the last 10 minutes is excluded as "still running", while an
// older one is selected.
func TestSelectHarvestSessions_QuietWindow(t *testing.T) {
	now := mustParseTime(t, "2026-08-17T12:00:00Z")
	opts := HarvestOptions{Now: now}
	wm := harvestWatermark{Sessions: map[string]string{}}

	sessions := []HarvestSession{
		{ID: "recent", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "quiet", UpdatedAt: now.Add(-1 * time.Hour)},
	}

	selected, skipped := selectHarvestSessions(sessions, wm, opts)
	if skipped != 1 {
		t.Errorf("skippedQuiet = %d, want 1", skipped)
	}
	if len(selected) != 1 || selected[0].ID != "quiet" {
		t.Errorf("selected = %v, want just [quiet]", selected)
	}
}

// TestSelectHarvestSessions_WatermarkIncremental verifies a session
// already harvested at or after its current UpdatedAt is skipped, but
// one whose UpdatedAt has advanced past the watermark is re-selected.
func TestSelectHarvestSessions_WatermarkIncremental(t *testing.T) {
	now := mustParseTime(t, "2026-08-17T12:00:00Z")
	opts := HarvestOptions{Now: now}
	wm := harvestWatermark{Sessions: map[string]string{
		"already-done":  "2026-08-01T00:00:00Z",
		"updated-since": "2026-08-01T00:00:00Z",
	}}

	sessions := []HarvestSession{
		{ID: "already-done", UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z")},
		{ID: "updated-since", UpdatedAt: mustParseTime(t, "2026-08-10T00:00:00Z")},
		{ID: "never-seen", UpdatedAt: mustParseTime(t, "2026-08-05T00:00:00Z")},
	}

	selected, _ := selectHarvestSessions(sessions, wm, opts)
	ids := map[string]bool{}
	for _, s := range selected {
		ids[s.ID] = true
	}
	if ids["already-done"] {
		t.Error("already-done should be skipped (not updated since watermark)")
	}
	if !ids["updated-since"] {
		t.Error("updated-since should be selected (newer than its watermark entry)")
	}
	if !ids["never-seen"] {
		t.Error("never-seen should be selected (no watermark entry yet)")
	}
}

// TestSelectHarvestSessions_SinceOverride verifies --since excludes
// sessions updated before the given lower bound.
func TestSelectHarvestSessions_SinceOverride(t *testing.T) {
	now := mustParseTime(t, "2026-08-17T12:00:00Z")
	opts := HarvestOptions{Now: now, Since: "2026-08-10T00:00:00Z"}
	wm := harvestWatermark{Sessions: map[string]string{}}

	sessions := []HarvestSession{
		{ID: "too-old", UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z")},
		{ID: "in-range", UpdatedAt: mustParseTime(t, "2026-08-15T00:00:00Z")},
	}

	selected, _ := selectHarvestSessions(sessions, wm, opts)
	if len(selected) != 1 || selected[0].ID != "in-range" {
		t.Errorf("selected = %v, want just [in-range]", selected)
	}
}

// TestSelectHarvestSessions_MaxSessionsCapsOldestFirst verifies the cap
// keeps the oldest-updated sessions for deterministic incremental
// progress.
func TestSelectHarvestSessions_MaxSessionsCapsOldestFirst(t *testing.T) {
	now := mustParseTime(t, "2026-08-17T12:00:00Z")
	opts := HarvestOptions{Now: now, MaxSessions: 2}
	wm := harvestWatermark{Sessions: map[string]string{}}

	sessions := []HarvestSession{
		{ID: "newest", UpdatedAt: mustParseTime(t, "2026-08-15T00:00:00Z")},
		{ID: "oldest", UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z")},
		{ID: "middle", UpdatedAt: mustParseTime(t, "2026-08-10T00:00:00Z")},
	}

	selected, _ := selectHarvestSessions(sessions, wm, opts)
	if len(selected) != 2 {
		t.Fatalf("expected 2 selected, got %d", len(selected))
	}
	if selected[0].ID != "oldest" || selected[1].ID != "middle" {
		t.Errorf("expected oldest-first order [oldest, middle], got [%s, %s]", selected[0].ID, selected[1].ID)
	}
}

// TestHarvestWatermark_RoundTrip verifies write then read reproduces
// the same session map, and a missing file degrades to empty rather
// than an error.
func TestHarvestWatermark_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	empty := readHarvestWatermark(dir, "codex")
	if len(empty.Sessions) != 0 {
		t.Errorf("expected empty watermark for missing file, got %v", empty.Sessions)
	}

	wm := harvestWatermark{Tool: "codex", Sessions: map[string]string{
		"abc": "2026-08-01T00:00:00Z",
		"def": "2026-08-02T00:00:00Z",
	}}
	if err := writeHarvestWatermark(dir, wm); err != nil {
		t.Fatalf("writeHarvestWatermark: %v", err)
	}

	got := readHarvestWatermark(dir, "codex")
	if len(got.Sessions) != 2 || got.Sessions["abc"] != "2026-08-01T00:00:00Z" || got.Sessions["def"] != "2026-08-02T00:00:00Z" {
		t.Errorf("round-tripped watermark = %v", got.Sessions)
	}

	// A different tool's watermark is independent.
	otherEmpty := readHarvestWatermark(dir, "gemini")
	if len(otherEmpty.Sessions) != 0 {
		t.Errorf("expected gemini watermark to be independent of codex's, got %v", otherEmpty.Sessions)
	}
}

// fakeDistill returns a canned distill response for every call,
// regardless of input -- the injectable-func seam under test.
func fakeDistill(resp harvestDistillResponse) DistillFunc {
	return func(_ context.Context, _, _ string, _ map[string]interface{}) (string, error) {
		data, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// TestHarvestDistilledSessions_WritesAndAdvancesWatermark verifies a
// real (non-dry-run) pass writes memory records under the tool's
// profile with the expected key/tags/scope, and advances the
// watermark so a second pass sees nothing new.
func TestHarvestDistilledSessions_WritesAndAdvancesWatermark(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	distill := fakeDistill(harvestDistillResponse{
		Memories: []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
			Body        string `json:"body"`
		}{
			{Name: "User Prefers Tabs", Description: "editor preference", Type: "fact", Body: "The user prefers tabs over spaces."},
		},
	})

	sess := HarvestSession{
		ID:         "sess-1",
		UpdatedAt:  mustParseTime(t, "2026-08-01T00:00:00Z"),
		Scope:      "project:-Users-ryan-dev-aida",
		Transcript: "User: I prefer tabs.\n\nAssistant: Noted.",
		SourceRef:  "codex:/fake/path.jsonl",
	}

	result, err := b.harvestDistilledSessions(ctx, "codex", []HarvestSession{sess}, distill, false)
	if err != nil {
		t.Fatalf("harvestDistilledSessions: %v", err)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected 1 session with 1 memory written, got %+v", result)
	}
	rec := result.Sessions[0].Written[0]
	if rec.Type != MemoryFact {
		t.Errorf("type = %s, want fact", rec.Type)
	}
	if rec.Key != "codex:sess-1:user-prefers-tabs" {
		t.Errorf("key = %q", rec.Key)
	}
	if rec.Profile != "codex" {
		t.Errorf("profile = %q, want codex", rec.Profile)
	}
	foundScopeTag := false
	foundProjectTag := false
	for _, tag := range rec.Tags {
		if tag == "scope:project" {
			foundScopeTag = true
		}
		if tag == "project:-Users-ryan-dev-aida" {
			foundProjectTag = true
		}
	}
	if !foundScopeTag || !foundProjectTag {
		t.Errorf("tags = %v, missing scope/project tag", rec.Tags)
	}

	// Watermark advanced -- a second pass over the same session (same
	// UpdatedAt) selects nothing.
	wm := readHarvestWatermark(b.Path, "codex")
	if wm.Sessions["sess-1"] != "2026-08-01T00:00:00Z" {
		t.Errorf("watermark not advanced: %v", wm.Sessions)
	}
	selected, _ := selectHarvestSessions([]HarvestSession{sess}, wm, HarvestOptions{Now: mustParseTime(t, "2026-08-17T00:00:00Z")})
	if len(selected) != 0 {
		t.Errorf("expected no candidates after watermark advance, got %v", selected)
	}
}

// TestHarvestDistilledSessions_DryRunSkipsWriteAndWatermark verifies
// --dry-run still calls distill (so the preview reflects real
// extraction) but writes nothing to the brain and does not advance the
// watermark.
func TestHarvestDistilledSessions_DryRunSkipsWriteAndWatermark(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	distill := fakeDistill(harvestDistillResponse{
		Memories: []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Type        string `json:"type"`
			Body        string `json:"body"`
		}{
			{Name: "some-fact", Description: "d", Type: "fact", Body: "body text"},
		},
	})

	sess := HarvestSession{ID: "sess-dry", UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z"), Scope: "global", SourceRef: "codex:x"}

	result, err := b.harvestDistilledSessions(ctx, "codex", []HarvestSession{sess}, distill, true)
	if err != nil {
		t.Fatalf("harvestDistilledSessions: %v", err)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 1 {
		t.Fatalf("expected a proposed (unwritten) memory in the result, got %+v", result)
	}
	if result.Sessions[0].Written[0].ID != "" {
		t.Error("dry-run record should have no ID (never actually written)")
	}

	counts, err := b.DB.MemoryCounts()
	if err != nil {
		t.Fatalf("MemoryCounts: %v", err)
	}
	for _, c := range counts {
		if c.Active != 0 {
			t.Errorf("dry-run should not persist anything, got %+v", c)
		}
	}

	wm := readHarvestWatermark(b.Path, "codex")
	if len(wm.Sessions) != 0 {
		t.Errorf("dry-run should not advance the watermark, got %v", wm.Sessions)
	}
}

// TestHarvestDistilledSessions_EmptyResponseIsNotAnError verifies the
// common "nothing worth keeping" outcome produces zero written memories
// without being treated as a failure.
func TestHarvestDistilledSessions_EmptyResponseIsNotAnError(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	distill := fakeDistill(harvestDistillResponse{})
	sess := HarvestSession{ID: "sess-empty", UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z"), Scope: "global", SourceRef: "codex:x"}

	result, err := b.harvestDistilledSessions(ctx, "codex", []HarvestSession{sess}, distill, false)
	if err != nil {
		t.Fatalf("harvestDistilledSessions: %v", err)
	}
	if len(result.Sessions) != 1 || len(result.Sessions[0].Written) != 0 {
		t.Errorf("expected 1 session with 0 memories, got %+v", result.Sessions)
	}
}

// TestHarvestDirectMemories_WritesAndGatesOnChange verifies a direct
// item writes once, is skipped unchanged on a second pass, and is
// re-written once its UpdatedAt advances (a file that changed).
func TestHarvestDirectMemories_WritesAndGatesOnChange(t *testing.T) {
	b := newTestBrain(t)
	ctx := context.Background()

	item := HarvestDirectMemory{
		ID:        "antigravity:conv-1:walkthrough",
		UpdatedAt: mustParseTime(t, "2026-08-01T00:00:00Z"),
		Type:      MemoryEvent,
		Key:       "gemini:antigravity:conv-1:walkthrough",
		Body:      "We implemented the thing.",
		Scope:     "project:-Users-ryan-dev-acme_widgets",
		Tags:      []string{"gemini-antigravity", "conversation:conv-1"},
		Source:    "gemini-antigravity:/fake/walkthrough.md",
	}

	written, err := b.harvestDirectMemories(ctx, "gemini", []HarvestDirectMemory{item}, HarvestOptions{})
	if err != nil {
		t.Fatalf("harvestDirectMemories: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected 1 written, got %d", len(written))
	}

	// Second pass, unchanged UpdatedAt -- skipped.
	written2, err := b.harvestDirectMemories(ctx, "gemini", []HarvestDirectMemory{item}, HarvestOptions{})
	if err != nil {
		t.Fatalf("harvestDirectMemories (2nd): %v", err)
	}
	if len(written2) != 0 {
		t.Errorf("expected 0 written on unchanged item, got %d", len(written2))
	}

	// File "changed" -- newer UpdatedAt should re-mirror.
	item.UpdatedAt = mustParseTime(t, "2026-08-02T00:00:00Z")
	item.Body = "We implemented the thing, then fixed a bug."
	written3, err := b.harvestDirectMemories(ctx, "gemini", []HarvestDirectMemory{item}, HarvestOptions{})
	if err != nil {
		t.Fatalf("harvestDirectMemories (3rd): %v", err)
	}
	if len(written3) != 1 {
		t.Errorf("expected 1 written after item changed, got %d", len(written3))
	}
}
