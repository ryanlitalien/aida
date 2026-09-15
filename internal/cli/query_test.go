package cli

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
)

// TestFormatNoneViableAnswer covers the router's none_viable escape
// (Fix 3): the honest "nothing here can answer this" message shown
// instead of letting execute/synthesize hallucinate an answer.
func TestFormatNoneViableAnswer(t *testing.T) {
	t.Run("includes reason and closest candidates", func(t *testing.T) {
		candidates := []engine.ScoredSource{
			{Name: "gworkspace", Source: &config.Source{Type: "tool"}, Score: 0},
			{Name: "web-search", Source: &config.Source{Type: "tool"}, Score: 5},
			{Name: "aida", Source: &config.Source{Type: "codebase"}, Score: 3},
		}
		got := formatNoneViableAnswer("no candidate answers a clock question", candidates)
		if !strings.Contains(got, "No configured source can answer this question.") {
			t.Errorf("missing lead sentence, got %q", got)
		}
		if !strings.Contains(got, "Router: no candidate answers a clock question.") {
			t.Errorf("missing router reason, got %q", got)
		}
		if !strings.Contains(got, "Closest candidates: web-search, aida, gworkspace.") {
			t.Errorf("expected candidates ordered by score desc, got %q", got)
		}
	})

	t.Run("omits closest candidates when none given", func(t *testing.T) {
		got := formatNoneViableAnswer("nothing fits", nil)
		if strings.Contains(got, "Closest candidates") {
			t.Errorf("did not expect a candidates clause, got %q", got)
		}
		if !strings.Contains(got, "Router: nothing fits.") {
			t.Errorf("missing router reason, got %q", got)
		}
	})

	t.Run("omits router clause when reason is empty", func(t *testing.T) {
		got := formatNoneViableAnswer("", nil)
		if strings.Contains(got, "Router:") {
			t.Errorf("did not expect a Router clause for empty reason, got %q", got)
		}
	})
}

func TestTopCandidateNames(t *testing.T) {
	candidates := []engine.ScoredSource{
		{Name: "low", Score: 1},
		{Name: "high", Score: 100},
		{Name: "mid", Score: 50},
		{Name: "mid2", Score: 50},
	}
	got := topCandidateNames(candidates, 3)
	want := []string{"high", "mid", "mid2"}
	if len(got) != len(want) {
		t.Fatalf("topCandidateNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("topCandidateNames()[%d] = %q, want %q (got %v)", i, got[i], want[i], got)
		}
	}

	// n larger than input: return everything, still sorted.
	all := topCandidateNames(candidates, 10)
	if len(all) != len(candidates) {
		t.Errorf("expected all %d candidates back, got %d", len(candidates), len(all))
	}

	// Input slice must not be mutated.
	orig := []engine.ScoredSource{{Name: "a", Score: 1}, {Name: "b", Score: 2}}
	origCopy := append([]engine.ScoredSource(nil), orig...)
	_ = topCandidateNames(orig, 1)
	for i := range orig {
		if orig[i] != origCopy[i] {
			t.Errorf("topCandidateNames mutated its input: %v vs original %v", orig, origCopy)
		}
	}

	if got := topCandidateNames(nil, 3); got != nil {
		t.Errorf("expected nil for empty candidates, got %v", got)
	}
}
