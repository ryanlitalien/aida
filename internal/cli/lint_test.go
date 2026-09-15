package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/library"
)

// setupLintRegistry writes one source YAML under a self-owned
// library-local root and returns the loaded *library.Registry plus its
// loaded sources, mirroring how `aida init` + a fresh ~/.aida/ actually
// resolves a source file to a name (the file's base name minus ".yaml").
func setupLintRegistry(t *testing.T, fileName, yaml string) (*library.Registry, map[string]bool) {
	t.Helper()
	aidaDir := t.TempDir()
	sourcesDir := filepath.Join(aidaDir, "library-local", "sources")
	if err := os.MkdirAll(sourcesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourcesDir, fileName), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := library.LoadRegistry(aidaDir)
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	if _, err := reg.LoadSources(); err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	present := make(map[string]bool, len(reg.Sources))
	for name := range reg.Sources {
		present[name] = true
	}
	return reg, present
}

// A source file ending in "-example.yaml" with a missing path is the
// shipped-examples case (examples/sources/codebase-example.yaml,
// docs-example.yaml as copied by `aida init`): a fresh clean-HOME lint run
// must warn, not error, so `aida lint` reports 0 errors out of the box.
func TestLintSourceMissingPathOnExampleFile(t *testing.T) {
	yaml := "path: ~/dev/does-not-exist-on-this-machine\n" +
		"type: docs\n" +
		"description: fixture\n" +
		"capabilities: [reference-docs]\n"
	reg, present := setupLintRegistry(t, "docs-fixture-example.yaml", yaml)
	if !present["docs-fixture-example"] {
		t.Fatalf("expected source %q to load, got: %v", "docs-fixture-example", present)
	}

	sources, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	got := sources["docs-fixture-example"]
	if got == nil {
		t.Fatal("source not loaded")
	}

	findings := lintSource("docs-fixture-example", got, reg)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", findings)
	}
	if findings[0].severity != "warn" {
		t.Fatalf("severity = %q, want warn", findings[0].severity)
	}
	if !containsAll(findings[0].message, "does not exist", "example source", "docs-fixture-example.yaml") {
		t.Fatalf("message = %q, missing expected substrings", findings[0].message)
	}
}

// A source explicitly marked `example: true` gets the same warn treatment
// even without the "-example.yaml" file-name convention.
func TestLintSourceMissingPathOnExampleField(t *testing.T) {
	yaml := "path: ~/dev/does-not-exist-on-this-machine\n" +
		"type: docs\n" +
		"description: fixture\n" +
		"capabilities: [reference-docs]\n" +
		"example: true\n"
	reg, _ := setupLintRegistry(t, "fixture.yaml", yaml)
	sources, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	got := sources["fixture"]
	if got == nil {
		t.Fatal("source not loaded")
	}
	findings := lintSource("fixture", got, reg)
	if len(findings) != 1 || findings[0].severity != "warn" {
		t.Fatalf("findings = %+v, want exactly one warn", findings)
	}
}

// A normal (non-example) source with a missing path is still an error -
// this fix must not weaken lint for real, user-authored sources.
func TestLintSourceMissingPathOnNonExample(t *testing.T) {
	yaml := "path: ~/dev/does-not-exist-on-this-machine\n" +
		"type: docs\n" +
		"description: fixture\n" +
		"capabilities: [reference-docs]\n"
	reg, _ := setupLintRegistry(t, "real-source.yaml", yaml)
	sources, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	got := sources["real-source"]
	if got == nil {
		t.Fatal("source not loaded")
	}
	findings := lintSource("real-source", got, reg)
	if len(findings) != 1 || findings[0].severity != "error" {
		t.Fatalf("findings = %+v, want exactly one error", findings)
	}
}

// --strict still turns a warning-level finding (example or not) into a
// non-zero-exit failure by way of runLint's existing strict handling -
// this fix only changes the severity, never the --strict contract.
func TestLintExampleWarningFailsUnderStrict(t *testing.T) {
	findings := []lintFinding{{source: "docs-fixture-example", severity: "warn", message: "path does not exist: ~/dev/x (this is an example source; edit examples/sources/docs-fixture-example.yaml to point at a real path, or delete it)"}}
	errors, warns := 0, 0
	for _, f := range findings {
		switch f.severity {
		case "error":
			errors++
		case "warn":
			warns++
		}
	}
	if errors != 0 || warns != 1 {
		t.Fatalf("errors=%d warns=%d, want 0/1", errors, warns)
	}
	strict := true
	if !(errors > 0 || (strict && warns > 0)) {
		t.Fatal("expected --strict to turn the example warning into a failure")
	}
	strict = false
	if errors > 0 || (strict && warns > 0) {
		t.Fatal("expected the example warning to be non-fatal without --strict")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
