package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClassifyFolder_ClaudeMdBecomesToolSource(t *testing.T) {
	// A folder with CLAUDE.md has a curated interface. Even if it also
	// contains CSV files, the scanner should classify it as `tool` so
	// the ExecAdapter passes LLM-generated commands through to whatever
	// the project documentation says is the right way to query it.
	dir := t.TempDir()
	wdir := filepath.Join(dir, "workouts")
	if err := os.MkdirAll(wdir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"consolidated_workouts.csv", "consolidated_sleep.csv", "consolidated_weight.csv"} {
		if err := os.WriteFile(filepath.Join(wdir, f), []byte("col1,col2\n1,2\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wdir, "CLAUDE.md"), []byte("# Workouts\n\nUse `tp_get_workouts --days N` to query recent workouts.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d := classifyFolder(wdir)
	if d == nil {
		t.Fatal("expected a discovery, got nil")
	}
	if d.Type != "tool" {
		t.Errorf("type: got %q, want tool", d.Type)
	}
	if d.Kind != "data" {
		t.Errorf("kind: got %q, want data", d.Kind)
	}
	if d.ContextFile != "CLAUDE.md" {
		t.Errorf("context: got %q, want CLAUDE.md", d.ContextFile)
	}
	if !contains(d.Capabilities, "fitness-tracking") {
		t.Errorf("expected fitness-tracking capability, got %v", d.Capabilities)
	}
	if !contains(d.Entities, "workouts") {
		t.Errorf("expected workouts entity, got %v", d.Entities)
	}
}

func TestClassifyFolder_BareCSVFolderStaysCSV(t *testing.T) {
	// A folder with CSVs but NO CLAUDE.md has no documented interface,
	// so aida's built-in CSV adapter should handle it directly.
	dir := t.TempDir()
	ddir := filepath.Join(dir, "rawdata")
	if err := os.MkdirAll(ddir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ddir, "raw.csv"), []byte("a,b\n1,2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	d := classifyFolder(ddir)
	if d == nil || d.Type != "csv" {
		t.Errorf("expected csv type, got %+v", d)
	}
}

func TestClassifyFolder_EmptyFolderIsNil(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "nothing")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	if got := classifyFolder(empty); got != nil {
		t.Errorf("expected nil for empty folder, got %+v", got)
	}
}

func TestClassifyFolder_CodebaseNotData(t *testing.T) {
	dir := t.TempDir()
	codeDir := filepath.Join(dir, "myapp")
	if err := os.MkdirAll(codeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeDir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codeDir, "go.mod"), []byte("module x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	d := classifyFolder(codeDir)
	if d == nil {
		t.Fatal("expected discovery")
	}
	if d.Kind != "code" {
		t.Errorf("kind: got %q, want code", d.Kind)
	}
}

func TestScanForSources_Integration(t *testing.T) {
	dir := t.TempDir()
	// Build a synthetic layout mirroring the user's ~/dev structure.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "workouts"), 0755))
	must(os.WriteFile(filepath.Join(dir, "workouts", "consolidated_workouts.csv"), []byte("a,b\n1,2\n"), 0644))
	must(os.WriteFile(filepath.Join(dir, "workouts", "CLAUDE.md"), []byte("Workout tracking data.\n"), 0644))

	must(os.MkdirAll(filepath.Join(dir, "some-go-app"), 0755))
	must(os.WriteFile(filepath.Join(dir, "some-go-app", "main.go"), []byte("package main\n"), 0644))

	must(os.MkdirAll(filepath.Join(dir, ".git"), 0755)) // must be ignored
	must(os.MkdirAll(filepath.Join(dir, "node_modules"), 0755))

	results, err := ScanForSources([]string{dir})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Expect at least: workouts (data), some-go-app (code). Ignored dirs must not appear.
	var sawWorkouts, sawCode, sawIgnored bool
	for _, r := range results {
		switch filepath.Base(r.Path) {
		case "workouts":
			sawWorkouts = true
			if r.Kind != "data" || r.Type != "tool" {
				t.Errorf("workouts: got kind=%s type=%s, want data/tool", r.Kind, r.Type)
			}
		case "some-go-app":
			sawCode = true
		case ".git", "node_modules":
			sawIgnored = true
		}
	}
	if !sawWorkouts {
		t.Errorf("expected to discover workouts folder, got %+v", results)
	}
	if !sawCode {
		t.Errorf("expected to discover some-go-app as code")
	}
	if sawIgnored {
		t.Errorf("should not discover .git or node_modules")
	}
}

// --- Fix 3+10 tests: source name self-registration and partner cross-link ---

func TestSplitNameTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"csv-viewer", []string{"viewer"}}, // "csv" is 3 chars, dropped
		{"mcp-perforce", []string{"perforce"}},
		{"GeminiWatermarkTool", []string{"gemini", "watermark", "tool"}},
		{"late-cli", nil}, // "late" is 4 chars, kept; "cli" is 3, dropped
		{"thrive", []string{"thrive"}},
		{"butter-stack", []string{"butter", "stack"}},
		{"3d-viewer", []string{"viewer"}},
		{"", nil},
		{"agent37-skills-collection-main", []string{"agent37", "skills", "collection", "main"}},
	}
	for _, c := range cases {
		got := splitNameTokens(c.in)
		// Special case: "late-cli" should produce just ["late"] since cli is short
		if c.in == "late-cli" {
			if len(got) != 1 || got[0] != "late" {
				t.Errorf("splitNameTokens(%q) = %v, want [late]", c.in, got)
			}
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("splitNameTokens(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitNameTokens(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestInferCapabilitiesAndEntities_SourceNameInEntities(t *testing.T) {
	caps, ents := inferCapabilitiesAndEntities("csv-viewer", []string{"viewer"}, "codebase", nil)
	if !contains(ents, "csv-viewer") {
		t.Errorf("expected entities to contain 'csv-viewer', got %v", ents)
	}
	if !contains(ents, "viewer") {
		t.Errorf("expected entities to contain 'viewer' token, got %v", ents)
	}
	if !contains(caps, "code-reference") {
		t.Errorf("expected codebase capability still present, got %v", caps)
	}
}

func TestInferCapabilitiesAndEntities_GenericTokensBlocklisted(t *testing.T) {
	// data-tool: tokens=["data","tool"], both on blocklist, neither should
	// land in entities. The full slug "data-tool" still does.
	_, ents := inferCapabilitiesAndEntities("data-tool", []string{"data", "tool"}, "tool", nil)
	if !contains(ents, "data-tool") {
		t.Errorf("expected full slug 'data-tool' in entities, got %v", ents)
	}
	if contains(ents, "data") {
		t.Errorf("did not expect generic 'data' token, got %v", ents)
	}
	if contains(ents, "tool") {
		t.Errorf("did not expect generic 'tool' token, got %v", ents)
	}
}

func TestInferCapabilitiesAndEntities_CamelCaseSplit(t *testing.T) {
	_, ents := inferCapabilitiesAndEntities("geminiwatermarktool", []string{"gemini", "watermark", "tool"}, "codebase", nil)
	if !contains(ents, "geminiwatermarktool") {
		t.Errorf("expected slug in entities, got %v", ents)
	}
	if !contains(ents, "gemini") {
		t.Errorf("expected 'gemini' in entities, got %v", ents)
	}
	if !contains(ents, "watermark") {
		t.Errorf("expected 'watermark' in entities, got %v", ents)
	}
	if contains(ents, "tool") {
		t.Errorf("did not expect generic 'tool' token, got %v", ents)
	}
}

func TestInferCapabilitiesAndEntities_PreservesExistingBehavior(t *testing.T) {
	_, ents := inferCapabilitiesAndEntities("workouts", []string{"workouts"}, "tool", nil)
	if !contains(ents, "workouts") {
		t.Errorf("expected workouts entity (existing behavior), got %v", ents)
	}
	if !contains(ents, "fitness") {
		t.Errorf("expected fitness entity (existing keyword map), got %v", ents)
	}
}

func TestClassifyFolder_NameTokensRegistered(t *testing.T) {
	dir := t.TempDir()
	cdir := filepath.Join(dir, "csv-viewer")
	if err := os.MkdirAll(cdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "main.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cdir, "go.mod"), []byte("module csv\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d := classifyFolder(cdir)
	if d == nil {
		t.Fatal("expected discovery")
	}
	if !contains(d.Entities, "csv-viewer") {
		t.Errorf("expected slug 'csv-viewer' in entities, got %v", d.Entities)
	}
	if !contains(d.Entities, "viewer") {
		t.Errorf("expected token 'viewer' in entities, got %v", d.Entities)
	}
}

func TestWriteDiscovered_CreatesFilesAndRoute(t *testing.T) {
	// Set up a source folder.
	src := t.TempDir()
	wdir := filepath.Join(src, "workouts")
	if err := os.MkdirAll(wdir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "consolidated_workouts.csv"), []byte("a,b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "CLAUDE.md"), []byte("Workouts\n"), 0644); err != nil {
		t.Fatal(err)
	}
	discovered, err := ScanForSources([]string{src})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 1 {
		t.Fatalf("expected 1 discovery, got %d", len(discovered))
	}

	// Set up a fresh library root.
	root := filepath.Join(t.TempDir(), "lib")
	if err := ScaffoldRoot(root, "test"); err != nil {
		t.Fatal(err)
	}
	n, err := WriteDiscovered(root, discovered)
	if err != nil {
		t.Fatalf("WriteDiscovered: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 written, got %d", n)
	}

	// Check the source YAML was written.
	if _, err := os.Stat(filepath.Join(root, "sources", "workouts.yaml")); err != nil {
		t.Errorf("missing source yaml: %v", err)
	}
	// Check the layer was written.
	if _, err := os.Stat(filepath.Join(root, "layers", "sources", "workouts.md")); err != nil {
		t.Errorf("missing layer markdown: %v", err)
	}
	// Manifest should list both.
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Sources["workouts"]; !ok {
		t.Errorf("manifest missing source entry: %+v", m.Sources)
	}
	if _, ok := m.Layers["sources/workouts"]; !ok {
		t.Errorf("manifest missing layer entry: %+v", m.Layers)
	}
	// Routes should include a match_cwd for the source path.
	routes, err := LoadRoutes(root)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range routes {
		if r.MatchCwd == wdir+"/**" {
			found = true
			if !contains(r.Sources, "workouts") {
				t.Errorf("route has wrong sources: %v", r.Sources)
			}
		}
	}
	if !found {
		t.Errorf("no match_cwd route for workouts: %+v", routes)
	}
}
