package sources

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestIsGrepRegexError(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"grep: repetition-operator operand invalid", true},
		{"grep: Unmatched ( or \\(", true},
		{"grep: trailing backslash (\\)", true},
		{"", false},
		{"some other error", false},
	}
	for _, c := range cases {
		got := isGrepRegexError(c.stderr)
		if got != c.want {
			t.Errorf("isGrepRegexError(%q) = %v, want %v", c.stderr, got, c.want)
		}
	}
}

// TestGrepAdapter_RegexErrorFallsBackToFixedString covers issue #14 Bug C:
// when the LLM generates a query like "cell metabolism (balance)" with
// unescaped parens, the BRE grep call returns
// "repetition-operator operand invalid". The adapter should retry with
// -F (fixed-string) and find the literal text.
func TestGrepAdapter_RegexErrorFallsBackToFixedString(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(fixture, []byte("cell metabolism (balance) parameters\nother line\n"), 0644); err != nil {
		t.Fatal(err)
	}

	src := config.Source{Path: dir}
	a := &GrepAdapter{}

	r, err := a.Execute(context.Background(), "cell metabolism (balance)", src)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.Status != "success" {
		t.Errorf("status = %q, want success (got summary: %s)", r.Status, r.Summary)
	}
	if len(r.Artifacts) == 0 {
		t.Errorf("expected at least one artifact, got 0")
	}
}
