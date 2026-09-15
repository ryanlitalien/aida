package jarvis

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

func TestSubstantiveForPromotion(t *testing.T) {
	cases := []struct {
		name string
		rec  audit.Record
		want bool
	}{
		{
			name: "normal tool turn",
			rec:  audit.Record{Reply: "Done, sir.", ToolCalls: []audit.ToolCall{{Name: "minecraft_ask"}}},
			want: true,
		},
		{
			name: "conversational reply, no tools",
			rec:  audit.Record{Reply: "Functioning nominally, sir."},
			want: true,
		},
		{
			name: "errored turn",
			rec:  audit.Record{Reply: "", Error: "tts: boom"},
			want: false,
		},
		{
			name: "empty reply",
			rec:  audit.Record{Reply: "   "},
			want: false,
		},
		{
			name: "wake-only",
			rec:  audit.Record{Reply: "Yes, sir?", WakeOnly: true},
			want: false,
		},
		{
			name: "rating-only turn",
			rec:  audit.Record{Reply: "Noted, sir.", ToolCalls: []audit.ToolCall{{Name: "jarvis_thumbs_down"}}},
			want: false,
		},
		{
			name: "mixed rating + real tool",
			rec:  audit.Record{Reply: "Noted and done.", ToolCalls: []audit.ToolCall{{Name: "jarvis_thumbs_up"}, {Name: "tasks_add"}}},
			want: true,
		},
		{
			// A zero-tool clarification question carries no fact worth
			// recalling - and once stored it anchors the model into
			// re-asking on the next similar query (the "which firm" loop).
			name: "zero-tool clarification question",
			rec:  audit.Record{Reply: "I'd need to know which firm you're asking about, sir. Which one is it?"},
			want: false,
		},
		{
			// A question-shaped reply that USED a tool still carries the
			// tool's facts - keep it.
			name: "tool turn ending in a question",
			rec:  audit.Record{Reply: "Task added, sir. Shall I tag it as well?", ToolCalls: []audit.ToolCall{{Name: "tasks_add"}}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := substantiveForPromotion(tc.rec); got != tc.want {
				t.Errorf("substantiveForPromotion = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildTurnSummary(t *testing.T) {
	rec := audit.Record{
		StartedAt: "2026-06-24T23:07:39Z",
		Query:     "build the sixth layer down",
		Reply:     "Layer six complete: y equals 144, light blue stained glass.",
		ToolCalls: []audit.ToolCall{{Name: "minecraft_ask"}, {Name: "jarvis_thumbs_up"}},
	}
	s := buildTurnSummary(rec)
	// Date prefix (date only, not full RFC3339).
	if !strings.HasPrefix(s, "[2026-06-24] ") {
		t.Errorf("missing date-only prefix: %q", s)
	}
	// Carries the query, the reply (the "what"), and the non-rating tool.
	for _, want := range []string{"build the sixth layer down", "light blue stained glass", "minecraft_ask"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
	// Rating tools are excluded from the tool list.
	if strings.Contains(s, "jarvis_thumbs_up") {
		t.Errorf("rating tool leaked into summary:\n%s", s)
	}
}

// A conversational turn with no tools summarizes without a tool clause.
func TestBuildTurnSummary_NoTools(t *testing.T) {
	rec := audit.Record{
		Timestamp: "2026-06-24T23:00:00Z",
		Query:     "how are you",
		Reply:     "Functioning nominally, sir.",
	}
	s := buildTurnSummary(rec)
	if strings.Contains(s, "used") {
		t.Errorf("no-tool turn should not claim tool use:\n%s", s)
	}
	if !strings.Contains(s, "Functioning nominally") {
		t.Errorf("summary missing reply:\n%s", s)
	}
}
