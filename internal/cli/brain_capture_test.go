package cli

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/brain"
)

func TestClassifyMemoryPath(t *testing.T) {
	home := "/Users/fakehome"
	cases := []struct {
		name string
		path string
		want memoryPathInfo
	}{
		{
			name: "global memory file",
			path: home + "/.claude/memory/reference_minty.md",
			want: memoryPathInfo{scope: "global", name: "reference_minty", ok: true},
		},
		{
			name: "global MEMORY.md",
			path: home + "/.claude/memory/MEMORY.md",
			want: memoryPathInfo{scope: "global", name: "MEMORY", ok: true},
		},
		{
			name: "global CLAUDE.md",
			path: home + "/.claude/CLAUDE.md",
			want: memoryPathInfo{scope: "global", name: "CLAUDE", isCLAUDE: true, ok: true},
		},
		{
			name: "project memory file",
			path: home + "/.claude/projects/-Users-fakehome-dev-aida/memory/sayodevice-x-playpause-remap.md",
			want: memoryPathInfo{scope: "project", projKey: "-Users-fakehome-dev-aida", name: "sayodevice-x-playpause-remap", ok: true},
		},
		{
			name: "project-local CLAUDE.md is not captured",
			path: "/Users/fakehome/dev/aida/.claude/CLAUDE.md",
			want: memoryPathInfo{},
		},
		{
			name: "unrelated path",
			path: "/tmp/some-notes.txt",
			want: memoryPathInfo{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyMemoryPath(c.path, home)
			if got != c.want {
				t.Errorf("classifyMemoryPath(%q) = %+v, want %+v", c.path, got, c.want)
			}
		})
	}
}

func TestParseScopeOverride(t *testing.T) {
	cases := []struct {
		in        string
		wantScope string
		wantKey   string
		wantErr   bool
	}{
		{"global", "global", "", false},
		{"project:demo", "project", "demo", false},
		{"project:", "", "", true},
		{"bogus", "", "", true},
		{"", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			scope, key, err := parseScopeOverride(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if scope != c.wantScope || key != c.wantKey {
				t.Errorf("parseScopeOverride(%q) = (%q,%q), want (%q,%q)", c.in, scope, key, c.wantScope, c.wantKey)
			}
		})
	}
}

func TestMapMemoryType(t *testing.T) {
	cases := []struct {
		name         string
		sourceType   string
		typeOverride string
		want         brain.MemoryType
		wantErr      bool
	}{
		{"top-level reference -> fact", "reference", "", brain.MemoryFact, false},
		{"top-level project -> fact", "project", "", brain.MemoryFact, false},
		{"nested user -> instruction", "user", "", brain.MemoryInstruction, false},
		{"nested feedback -> instruction", "feedback", "", brain.MemoryInstruction, false},
		{"session -> event", "session", "", brain.MemoryEvent, false},
		{"missing source type -> fact", "", "", brain.MemoryFact, false},
		{"unrecognized source type -> fact default", "something-else", "", brain.MemoryFact, false},
		{"typeOverride wins over source type", "session", "instruction", brain.MemoryInstruction, false},
		{"invalid typeOverride errors", "session", "bogus", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := mapMemoryType(c.sourceType, c.typeOverride)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("mapMemoryType(%q,%q) = %q, want %q", c.sourceType, c.typeOverride, got, c.want)
			}
		})
	}
}

// TestMemFrontmatterDialects verifies memFrontmatter parses both the
// global (top-level `type:`) and project (nested `metadata.type:`)
// frontmatter dialects, and that the nested form wins when both are
// present -- matching runCapture's "Metadata.Type if non-empty else
// Type" rule.
func TestMemFrontmatterDialects(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"top-level dialect", "type: reference\ndescription: d\n", "reference"},
		{"nested dialect", "metadata:\n  type: user\n", "user"},
		{"nested wins when both present", "type: reference\nmetadata:\n  type: user\n", "user"},
		{"neither present", "description: d\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var fm memFrontmatter
			if err := yaml.Unmarshal([]byte(c.yaml), &fm); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			effective := fm.Type
			if fm.Metadata.Type != "" {
				effective = fm.Metadata.Type
			}
			if effective != c.want {
				t.Errorf("effective type = %q, want %q", effective, c.want)
			}
		})
	}
}

func TestStripSoulBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "removes marked block, keeps surrounding content",
			in:   "before\n<!-- BEGIN aida-soul -->\nsoul stuff\nmore soul\n<!-- END aida-soul -->\nafter",
			want: "before\nafter",
		},
		{
			name: "no-op when markers absent",
			in:   "just plain content\nno markers here",
			want: "just plain content\nno markers here",
		},
		{
			name: "no-op when only begin marker present",
			in:   "before\n<!-- BEGIN aida-soul -->\ndangling",
			want: "before\n<!-- BEGIN aida-soul -->\ndangling",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripSoulBlock(c.in)
			if got != c.want {
				t.Errorf("stripSoulBlock() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRunCapture_GlobalMemoryEndToEnd exercises the full runCapture
// path against a temp brain: classify -> read/parse -> map type ->
// write -- then verifies a second call on unchanged content is a
// no-op (idempotency short-circuit), not a duplicate write.
func TestRunCapture_GlobalMemoryEndToEnd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	memDir := filepath.Join(tmpHome, ".claude", "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(memDir, "reference_minty.md")
	content := "---\n" +
		"name: minty linux box\n" +
		"description: Ryan's Linux machine\n" +
		"type: reference\n" +
		"---\n\n" +
		"Ryan has a Linux box called minty."
	if err := os.WriteFile(memFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	if err := runCapture(ctx, b, memFile, "", ""); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	key := "claude:global:reference_minty"
	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	wantBody := "Ryan's Linux machine\n\nRyan has a Linux box called minty."
	if rec.Body != wantBody {
		t.Errorf("Body = %q, want %q", rec.Body, wantBody)
	}
	if rec.Profile != claudeMemoryProfile {
		t.Errorf("Profile = %q, want %q", rec.Profile, claudeMemoryProfile)
	}
	if rec.Source != "claude-code:"+memFile {
		t.Errorf("Source = %q", rec.Source)
	}
	if rec.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", rec.Confidence)
	}
	wantTags := []string{"claude-code", "scope:global"}
	if !reflect.DeepEqual(rec.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", rec.Tags, wantTags)
	}

	// Idempotency: re-running on unchanged content must not write a
	// new record -- the active record's ID must survive unchanged.
	firstID := rec.ID
	if err := runCapture(ctx, b, memFile, "", ""); err != nil {
		t.Fatalf("second runCapture: %v", err)
	}
	rec2, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey after second capture: %v", err)
	}
	if rec2.ID != firstID {
		t.Errorf("expected idempotent no-op, got new record ID %q (was %q)", rec2.ID, firstID)
	}
}

// TestRunCapture_ProjectScopeAndCrossTypeSupersede covers the nested
// `metadata.type` frontmatter dialect, project-scoped key/tag
// derivation, and the cross-type supersede step: when a memory's
// frontmatter type changes across edits (reference/fact -> user/
// instruction), the old fact must stop being active and the new
// instruction must take over the same key.
func TestRunCapture_ProjectScopeAndCrossTypeSupersede(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projMemDir := filepath.Join(tmpHome, ".claude", "projects", "-Users-fakehome-dev-aida", "memory")
	if err := os.MkdirAll(projMemDir, 0755); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(projMemDir, "sayodevice-x-playpause-remap.md")

	referenceContent := "---\n" +
		"name: sayodevice-x-playpause-remap\n" +
		"description: SayoDevice remap note\n" +
		"metadata:\n" +
		"  type: reference\n" +
		"---\n\n" +
		"Original reference body."
	if err := os.WriteFile(memFile, []byte(referenceContent), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	if err := runCapture(ctx, b, memFile, "", ""); err != nil {
		t.Fatalf("runCapture (fact): %v", err)
	}

	key := "claude:project:-Users-fakehome-dev-aida:sayodevice-x-playpause-remap"
	factRec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("expected active fact: %v", err)
	}
	wantTags := []string{"claude-code", "scope:project", "project:-Users-fakehome-dev-aida"}
	if !reflect.DeepEqual(factRec.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", factRec.Tags, wantTags)
	}

	// Reclassify the same memory as "user" (-> instruction) with new
	// content -- simulates an edit that changed metadata.type.
	userContent := "---\n" +
		"name: sayodevice-x-playpause-remap\n" +
		"description: SayoDevice remap note\n" +
		"metadata:\n" +
		"  type: user\n" +
		"---\n\n" +
		"Updated instruction body."
	if err := os.WriteFile(memFile, []byte(userContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := runCapture(ctx, b, memFile, "", ""); err != nil {
		t.Fatalf("runCapture (instruction): %v", err)
	}

	// The prior fact must no longer be active (cross-type supersede).
	if _, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("expected fact to be superseded, got err=%v", err)
	}
	instRec, err := b.DB.ActiveMemoryByKey(brain.MemoryInstruction, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("expected active instruction: %v", err)
	}
	if instRec.ID == factRec.ID {
		t.Errorf("instruction record should be a distinct write from the superseded fact")
	}
}

// TestRunCapture_ScopeAndTypeOverride verifies the --scope/--type
// overrides used by `remember` force classification even for a path
// that classifyMemoryPath alone would treat as unrelated.
func TestRunCapture_ScopeAndTypeOverride(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	notePath := filepath.Join(tmpHome, "scratch-note.md")
	if err := os.WriteFile(notePath, []byte("Some ad-hoc note body."), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	if err := runCapture(context.Background(), b, notePath, "project:demo", "instruction"); err != nil {
		t.Fatalf("runCapture: %v", err)
	}

	key := "claude:project:demo:scratch-note"
	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryInstruction, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if rec.Body != "Some ad-hoc note body." {
		t.Errorf("Body = %q", rec.Body)
	}
}

func TestRunCapture_UnrelatedPathIsNoOp(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	if err := runCapture(context.Background(), b, "/tmp/unrelated-file.txt", "", ""); err != nil {
		t.Fatalf("expected nil for unrelated path, got %v", err)
	}
}

func TestRunCapture_InvalidTypeOverrideErrors(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	memDir := filepath.Join(tmpHome, ".claude", "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(memDir, "foo.md")
	if err := os.WriteFile(memFile, []byte("body text"), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	if err := runCapture(context.Background(), b, memFile, "", "bogus"); err == nil {
		t.Error("expected error for invalid type override")
	}
}

// TestRunCapture_MalformedFrontmatterGracefulDegradation verifies that a
// memory file with unparseable YAML frontmatter (e.g., an unquoted colon
// in a description field) is still captured: the error is logged as a
// verbose warning and the body is preserved, but sourceType and description
// remain empty so the memory defaults to a fact type.
func TestRunCapture_MalformedFrontmatterGracefulDegradation(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	memDir := filepath.Join(tmpHome, ".claude", "memory")
	if err := os.MkdirAll(memDir, 0755); err != nil {
		t.Fatal(err)
	}
	memFile := filepath.Join(memDir, "broken-fm.md")

	// Frontmatter with malformed YAML: unquoted colon in the description
	// value will break the YAML parser.
	content := "---\n" +
		"name: broken-frontmatter\n" +
		"description: Fix: the thing without quotes breaks YAML\n" +
		"type: reference\n" +
		"---\n\n" +
		"The actual body content we want to preserve."
	if err := os.WriteFile(memFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	// runCapture should NOT error even though the YAML is broken
	if err := runCapture(ctx, b, memFile, "", ""); err != nil {
		t.Fatalf("runCapture with malformed frontmatter: %v", err)
	}

	key := "claude:global:broken-fm"
	// With malformed frontmatter, sourceType is empty, so mapMemoryType
	// defaults to brain.MemoryFact
	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}

	// The body should be captured intact, without the malformed frontmatter
	wantBody := "The actual body content we want to preserve."
	if rec.Body != wantBody {
		t.Errorf("Body = %q, want %q", rec.Body, wantBody)
	}

	// Verify profile and tags are correct
	if rec.Profile != claudeMemoryProfile {
		t.Errorf("Profile = %q, want %q", rec.Profile, claudeMemoryProfile)
	}
	wantTags := []string{"claude-code", "scope:global"}
	if !reflect.DeepEqual(rec.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", rec.Tags, wantTags)
	}
}

// TestRunCaptureRetimesUnchangedBody verifies that when a memory file's
// body is unchanged across captures but its mtime-derived Created differs
// from what's already stored, runCapture retimes the existing record in
// place (via brain.RetimeMemory) instead of skipping it outright or
// superseding it with a duplicate write.
func TestRunCaptureRetimesUnchangedBody(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	b, err := brain.Open(t.TempDir(), "test", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("Open brain: %v", err)
	}
	defer b.Close()

	dir := t.TempDir()
	f := filepath.Join(dir, "note.md")
	if err := os.WriteFile(f, []byte("some memory body"), 0644); err != nil {
		t.Fatal(err)
	}

	t1 := time.Date(2026, 7, 18, 1, 12, 0, 0, time.UTC)
	if err := os.Chtimes(f, t1, t1); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := runCapture(ctx, b, f, "global", ""); err != nil {
		t.Fatalf("runCapture (first): %v", err)
	}

	key := "claude:global:note"
	rec, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey: %v", err)
	}
	if rec.Created != t1.Format(time.RFC3339) {
		t.Errorf("Created = %q, want %q", rec.Created, t1.Format(time.RFC3339))
	}

	// Change only the mtime -- content is identical.
	t2 := time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC)
	if err := os.Chtimes(f, t2, t2); err != nil {
		t.Fatal(err)
	}

	if err := runCapture(ctx, b, f, "global", ""); err != nil {
		t.Fatalf("runCapture (second): %v", err)
	}

	rec2, err := b.DB.ActiveMemoryByKey(brain.MemoryFact, key, claudeMemoryProfile)
	if err != nil {
		t.Fatalf("ActiveMemoryByKey after retime: %v", err)
	}
	if rec2.ID != rec.ID {
		t.Errorf("expected retime in place, got a different record: %q (was %q)", rec2.ID, rec.ID)
	}
	if rec2.Created != t2.Format(time.RFC3339) {
		t.Errorf("Created after retime = %q, want %q", rec2.Created, t2.Format(time.RFC3339))
	}

	// No supersession churn -- still exactly one active fact, zero superseded.
	counts, err := b.DB.MemoryCounts()
	if err != nil {
		t.Fatalf("MemoryCounts: %v", err)
	}
	found := false
	for _, c := range counts {
		if c.Type == brain.MemoryFact {
			found = true
			if c.Active != 1 || c.Superseded != 0 {
				t.Errorf("fact counts = %+v, want {Active:1, Superseded:0}", c)
			}
		}
	}
	if !found {
		t.Fatalf("no MemoryFact entry in counts: %+v", counts)
	}
}
