package dispatch

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/roster"
)

// TestDecide runs an exhaustive origin x route-set table over the sync-vs-
// background policy. Decide is pure, so entries are built as plain literals
// rather than loaded from the testdata roster.
func TestDecide(t *testing.T) {
	subagentPM := &roster.Entry{Name: "product-manager", Kind: roster.KindSubagent}
	subagentDevops := &roster.Entry{Name: "devops", Kind: roster.KindSubagent}
	jobEntry := &roster.Entry{Name: "night-owl", Kind: roster.KindJob}
	asyncEntry := &roster.Entry{Name: "slow-agent", Kind: roster.KindSubagent, Mode: "async"}
	sourceEntry := &roster.Entry{Name: "handbook", Kind: roster.KindSource}
	mcpEntry := &roster.Entry{Name: "notion", Kind: roster.KindMCP}

	tests := []struct {
		name    string
		entries []*roster.Entry
		origin  string
		want    bool
	}{
		{"job-kind entry is background from voice", []*roster.Entry{jobEntry}, "voice", true},
		{"job-kind entry is background from cli", []*roster.Entry{jobEntry}, "cli", true},
		{"async-mode entry is background from voice", []*roster.Entry{asyncEntry}, "voice", true},
		{"async-mode entry is background from cli", []*roster.Entry{asyncEntry}, "cli", true},
		{"voice fan-out (2 entries) is background", []*roster.Entry{subagentPM, subagentDevops}, "voice", true},
		{"voice single subagent is background", []*roster.Entry{subagentPM}, "voice", true},
		{"voice single source is synchronous", []*roster.Entry{sourceEntry}, "voice", false},
		{"voice single mcp is synchronous", []*roster.Entry{mcpEntry}, "voice", false},
		{"cli single subagent is synchronous", []*roster.Entry{subagentPM}, "cli", false},
		{"cli fan-out subagents is synchronous", []*roster.Entry{subagentPM, subagentDevops}, "cli", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.entries, tt.origin)
			if got.Background != tt.want {
				t.Errorf("Decide(entries=%v, origin=%q) = %+v, want Background=%v", entryNames(tt.entries), tt.origin, got, tt.want)
			}
			if got.Reason == "" {
				t.Errorf("Decide(entries=%v, origin=%q) returned an empty Reason", entryNames(tt.entries), tt.origin)
			}
		})
	}
}

func entryNames(entries []*roster.Entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}
