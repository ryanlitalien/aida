package library

import (
	"os"
	"path/filepath"
	"testing"
)

// writeManifest is a test helper that writes a per-root library.yaml.
func writeManifest(t *testing.T, rootPath string, body string) {
	t.Helper()
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, ManifestFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRegistry_SingleRoot(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	writeManifest(t, rootA, `
version: 1
name: a
layers:
  global:
    file: layers/global.md
sources:
  grep:
    file: sources/grep.yaml
`)

	cfg := &LibraryConfig{
		Version: 1,
		Roots:   []RootRef{{Name: "a", Path: rootA}},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Roots) != 1 {
		t.Errorf("expected 1 root, got %d", len(reg.Roots))
	}
	if _, ok := reg.Layers["global"]; !ok {
		t.Errorf("expected 'global' layer, got %v", reg.Layers)
	}
	if !reg.Layers["global"].Available {
		t.Errorf("global layer should be available (no required tools)")
	}
}

func TestBuildRegistry_OutputStyles(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	writeManifest(t, rootA, `
version: 1
name: a
output-styles:
  concise:
    file: output-styles/concise.md
`)

	cfg := &LibraryConfig{
		Version: 1,
		Roots:   []RootRef{{Name: "a", Path: rootA}},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	style, ok := reg.OutputStyles["concise"]
	if !ok {
		t.Fatalf("expected 'concise' output style, got %v", reg.OutputStyles)
	}
	if !style.Available {
		t.Errorf("concise output style should be available (no required tools)")
	}
	wantAbsFile := filepath.Join(rootA, "output-styles", "concise.md")
	if style.AbsFile != wantAbsFile {
		t.Errorf("AbsFile = %q, want %q", style.AbsFile, wantAbsFile)
	}
}

func TestBuildRegistry_MissingOptionalRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := &LibraryConfig{
		Roots: []RootRef{
			{Name: "missing", Path: filepath.Join(dir, "nope"), Optional: true},
		},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.MissingRoots) != 0 {
		t.Errorf("optional missing root should NOT be reported, got %v", reg.MissingRoots)
	}
}

func TestBuildRegistry_MissingRequiredRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := &LibraryConfig{
		Roots: []RootRef{
			{Name: "must-have", Path: filepath.Join(dir, "nope")},
		},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.MissingRoots) != 1 || reg.MissingRoots[0].Name != "must-have" {
		t.Errorf("expected missing required root reported, got %v", reg.MissingRoots)
	}
}

func TestBuildRegistry_OverrideOnNameCollision(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	writeManifest(t, rootA, `
version: 1
name: a
layers:
  global:
    file: layers/a-global.md
`)
	writeManifest(t, rootB, `
version: 1
name: b
layers:
  global:
    file: layers/b-global.md
`)
	cfg := &LibraryConfig{
		Roots: []RootRef{
			{Name: "a", Path: rootA},
			{Name: "b", Path: rootB},
		},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	got := reg.Layers["global"]
	if got == nil {
		t.Fatal("expected global layer")
	}
	if got.Root.Ref.Name != "b" {
		t.Errorf("expected later root 'b' to override 'a', got root=%q", got.Root.Ref.Name)
	}
}

func TestBuildRegistry_ToolFiltering(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	// Use a tool name that will not exist on any test machine.
	writeManifest(t, rootA, `
version: 1
name: a
layers:
  needs-tool:
    file: layers/x.md
    requires_tool: this-tool-definitely-does-not-exist-zzz
  no-tool:
    file: layers/y.md
`)
	cfg := &LibraryConfig{
		Roots: []RootRef{{Name: "a", Path: rootA}},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if reg.Layers["needs-tool"].Available {
		t.Errorf("layer requiring nonexistent tool should be unavailable")
	}
	if !reg.Layers["no-tool"].Available {
		t.Errorf("layer with no tool requirement should be available")
	}
	if len(reg.Layers["needs-tool"].MissingTools) != 1 {
		t.Errorf("expected 1 missing tool, got %v", reg.Layers["needs-tool"].MissingTools)
	}
}

func TestEnsureDefaultRegistered(t *testing.T) {
	dir := t.TempDir()
	cfg, err := EnsureDefaultRegistered(dir)
	if err != nil {
		t.Fatalf("EnsureDefaultRegistered: %v", err)
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0].Name != "defaults" {
		t.Errorf("expected single 'defaults' root, got %+v", cfg.Roots)
	}
	// Idempotent: second call should not change anything.
	cfg2, err := EnsureDefaultRegistered(dir)
	if err != nil {
		t.Fatalf("EnsureDefaultRegistered (2nd): %v", err)
	}
	if len(cfg2.Roots) != 1 {
		t.Errorf("registry should be idempotent, got %+v", cfg2.Roots)
	}
}

func TestScaffoldRoot_CreatesLayout(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	if err := ScaffoldRoot(root, "test"); err != nil {
		t.Fatalf("ScaffoldRoot: %v", err)
	}
	for _, sub := range []string{
		"library.yaml",
		"routes.yaml",
		"layers/global.md",
		"layers/domains",
		"skills",
		"sources",
	} {
		if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
			t.Errorf("missing %s: %v", sub, err)
		}
	}
	// Manifest should parse and have name set.
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "test" {
		t.Errorf("expected manifest name 'test', got %q", m.Name)
	}
}
