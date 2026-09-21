package cli

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/eval"
)

func TestAnswerIsUseless(t *testing.T) {
	useless := []string{
		"I don't have access to information about what you ate today (source: acme-widgets subagent-response).",
		"I do not have access to your weight data.",
		"I cannot find any matches for that query in the available sources.",
		"I cannot answer your question about csv-viewer because the data source returned an error.",
		"I don't know what time you went to bed last night.",
		"The search returned no results from the available sources.",
		"The available data source only has access to software development project context.",
		"The data source available to me only has scope for code-related queries.",
		"",
	}
	useful := []string{
		"Your latest workout was a 5-mile run on April 5 at an 8:32/mile pace.",
		"The csv-viewer exposes 3 sample assets and supports tab-separated values.",
		"Top vendors by spend Q1: Umbrella $432, DCU $211, Stripe $109.",
		"Excalidraw is a monorepo with a clear separation between core library and app.",
		"Sources: workouts: yesterday, 5 miles, 38 minutes",
	}
	for _, a := range useless {
		if !answerIsUseless(a) {
			t.Errorf("expected useless: %q", a)
		}
	}
	for _, a := range useful {
		if answerIsUseless(a) {
			t.Errorf("expected useful: %q", a)
		}
	}
}

// TestShouldWriteRoutingHint covers the Fix 4 gate: a real incident had
// the PRM-lite quality scorer rate a wrong answer 4/5 (it judges
// fluency/completeness, not factual correctness) while the deterministic
// citation reviewer had already failed the same run - the quality-only
// gate let a reinforcing routing_hint lesson through anyway. Reviewer
// failure must now block the hint; reviewer absence (no records) must
// not, since that's the common no-reviewers-ran case (offline mode, etc.)
// rather than a signal of anything wrong.
func TestShouldWriteRoutingHint(t *testing.T) {
	sources := []string{"snowflake"}

	tests := []struct {
		name    string
		quality int
		sources []string
		records []eval.ReviewRecord
		want    bool
	}{
		{
			name:    "quality 4, citation reviewer failed -> blocked",
			quality: 4,
			sources: sources,
			records: []eval.ReviewRecord{{Reviewer: "citation", Verdict: eval.VerdictFail}},
			want:    false,
		},
		{
			name:    "quality 4, all reviewers pass -> written",
			quality: 4,
			sources: sources,
			records: []eval.ReviewRecord{
				{Reviewer: "citation", Verdict: eval.VerdictPass},
				{Reviewer: "completeness", Verdict: eval.VerdictPass},
			},
			want: true,
		},
		{
			name:    "quality 4, a warn but no fail -> written",
			quality: 4,
			sources: sources,
			records: []eval.ReviewRecord{{Reviewer: "scope", Verdict: eval.VerdictWarn}},
			want:    true,
		},
		{
			name:    "quality 3 -> blocked regardless of reviewers",
			quality: 3,
			sources: sources,
			records: nil,
			want:    false,
		},
		{
			name:    "quality 4, no reviewer records at all -> written (no signal to block on)",
			quality: 4,
			sources: sources,
			records: nil,
			want:    true,
		},
		{
			name:    "quality 5, no sources -> blocked",
			quality: 5,
			sources: nil,
			records: nil,
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldWriteRoutingHint(tc.quality, tc.sources, tc.records)
			if got != tc.want {
				t.Errorf("shouldWriteRoutingHint(%d, %v, %v) = %v, want %v",
					tc.quality, tc.sources, tc.records, got, tc.want)
			}
		})
	}
}
