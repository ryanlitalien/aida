package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// lintSoul: parse failure = error (the file exists but silently reaches no
// prompt), missing/empty = warn, healthy = no findings.
func TestLintSoul(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".aida")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// No file → warn.
	fs := lintSoul()
	if len(fs) != 1 || fs[0].severity != "warn" {
		t.Fatalf("missing soul: got %+v, want one warn", fs)
	}

	// Broken YAML (the real-world landmine: unquoted item with ": ") → error.
	broken := "name: Ryan\npreferences:\n  - Use US units: miles, pounds\n"
	if err := os.WriteFile(filepath.Join(dir, "soul.yaml"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	fs = lintSoul()
	if len(fs) != 1 || fs[0].severity != "error" {
		t.Fatalf("broken soul: got %+v, want one error", fs)
	}

	// Healthy → clean.
	good := "name: Ryan\nrole: engineer\ncontext: |\n  I work at ButterStack.\n"
	if err := os.WriteFile(filepath.Join(dir, "soul.yaml"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if fs = lintSoul(); len(fs) != 0 {
		t.Fatalf("healthy soul: got %+v, want none", fs)
	}
}
