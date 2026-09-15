package sources

import "testing"

// TestSubagentRefusal verifies that a claude-project sub-agent's declined
// response is turned into an "empty" status with no artifacts (so the
// synthesizer can't cite the refusal prose as a fact), while a genuine
// answer passes through untouched.
func TestSubagentRefusal(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		refused bool
	}{
		{
			name:    "explicit refusal",
			text:    `I cannot build a query for the "gworkspace" tool to answer "what time is it".`,
			refused: true,
		},
		{
			name:    "no access variant",
			text:    "I don't have access to that data source from inside this project.",
			refused: true,
		},
		{
			name:    "genuine answer untouched",
			text:    "Your last workout was a 5-mile tempo run on 2026-07-01, average pace 7:32/mi.",
			refused: false,
		},
		{
			name:    "short factual answer untouched",
			text:    "It is currently 3:14 PM Eastern Time.",
			refused: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, refused := subagentRefusal(tt.text)
			if refused != tt.refused {
				t.Fatalf("subagentRefusal(%q) refused = %v, want %v", tt.text, refused, tt.refused)
			}
			if !tt.refused {
				return
			}
			if result.Status != "empty" {
				t.Errorf("subagentRefusal(%q) Status = %q, want %q", tt.text, result.Status, "empty")
			}
			if len(result.Artifacts) != 0 {
				t.Errorf("subagentRefusal(%q) Artifacts = %v, want none", tt.text, result.Artifacts)
			}
			if result.Source != "claude-project" {
				t.Errorf("subagentRefusal(%q) Source = %q, want %q", tt.text, result.Source, "claude-project")
			}
			wantPrefix := "sub-agent declined: "
			if len(result.Summary) < len(wantPrefix) || result.Summary[:len(wantPrefix)] != wantPrefix {
				t.Errorf("subagentRefusal(%q) Summary = %q, missing prefix %q", tt.text, result.Summary, wantPrefix)
			}
		})
	}
}
