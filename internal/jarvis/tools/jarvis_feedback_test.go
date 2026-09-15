package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

// openTestBrain spins up a brain backed by a temp dir with no Voyage key -
// embeddings are simply skipped, but the SQL round-trip still works.
func openTestBrain(t *testing.T) *brain.Brain {
	t.Helper()
	dir := t.TempDir()
	b, err := brain.Open(dir, "test", "VOYAGE_API_KEY_UNSET_FOR_TESTS", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestJarvisThumbsDown_PersistsRow(t *testing.T) {
	b := openTestBrain(t)

	lastTurn := func() *audit.Record {
		return &audit.Record{
			StartedAt:  "2026-05-24T10:00:00Z",
			Transcript: "Hey Jarvis, what time is it",
			Query:      "what time is it",
			Reply:      "It is five PM, sir.",
			ToolCalls:  []audit.ToolCall{{Name: "current_time", TookMs: 5}},
		}
	}

	tool := jarvisThumbsDownTool(b, lastTurn)
	in, _ := json.Marshal(feedbackInput{Reason: "should have said the date too"})
	out, err := tool.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "Noted") {
		t.Errorf("expected ack, got %q", out)
	}

	if got := b.DB.JarvisLessonCount(); got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
	}
	by, _ := b.DB.JarvisLessonCountByFeedback()
	if by["down"] != 1 {
		t.Errorf("by feedback: %+v", by)
	}
}

func TestJarvisThumbsDown_NoTurnToRate(t *testing.T) {
	b := openTestBrain(t)
	lastTurn := func() *audit.Record { return nil }
	tool := jarvisThumbsDownTool(b, lastTurn)
	in, _ := json.Marshal(feedbackInput{Reason: "anything"})
	out, err := tool.Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "don't recall") {
		t.Errorf("expected 'don't recall' fallback, got %q", out)
	}
	if got := b.DB.JarvisLessonCount(); got != 0 {
		t.Errorf("expected 0 rows when no prior turn, got %d", got)
	}
}

func TestJarvisThumbsDown_RejectsEmptyReason(t *testing.T) {
	b := openTestBrain(t)
	lastTurn := func() *audit.Record { return &audit.Record{Query: "x"} }
	tool := jarvisThumbsDownTool(b, lastTurn)
	in, _ := json.Marshal(feedbackInput{Reason: ""})
	if _, err := tool.Run(context.Background(), in); err == nil {
		t.Errorf("expected error for empty reason, got nil")
	}
}

func TestJarvisNote_StoresAsStyleDirective(t *testing.T) {
	b := openTestBrain(t)
	lastTurn := func() *audit.Record {
		return &audit.Record{
			StartedAt:  "2026-05-24T10:00:00Z",
			Transcript: "Hey Jarvis, what's the weather",
			Query:      "what's the weather",
			Reply:      "Fifty degrees, sir.",
			ToolCalls:  []audit.ToolCall{{Name: "weather", TookMs: 800}},
		}
	}
	tool := jarvisNoteTool(b, lastTurn)
	in, _ := json.Marshal(noteInput{Directive: "use Fahrenheit"})
	if _, err := tool.Run(context.Background(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := b.DB.JarvisLessonCount(); got != 1 {
		t.Fatalf("count: %d", got)
	}
}

func TestFirstEngineRunID(t *testing.T) {
	cases := []struct {
		name  string
		calls []audit.ToolCall
		want  string
	}{
		{"empty", nil, ""},
		{"no engine call", []audit.ToolCall{{Name: "weather"}, {Name: "current_time"}}, ""},
		{
			"engine call mid-list",
			[]audit.ToolCall{{Name: "weather"}, {Name: "aida_query", EngineRunID: "abc-123"}},
			"abc-123",
		},
		{
			"first engine wins",
			[]audit.ToolCall{
				{Name: "aida_query", EngineRunID: "first"},
				{Name: "aida_query", EngineRunID: "second"},
			},
			"first",
		},
	}
	for _, c := range cases {
		if got := firstEngineRunID(c.calls); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestEngineRunIDSidechannel(t *testing.T) {
	// Reset between subtests so prior test bleeds don't affect this one.
	SetLastEngineRunID("")
	if id := TakeLastEngineRunID(); id != "" {
		t.Fatalf("expected empty after reset, got %q", id)
	}
	SetLastEngineRunID("run-42")
	if id := TakeLastEngineRunID(); id != "run-42" {
		t.Errorf("Take: want run-42, got %q", id)
	}
	if id := TakeLastEngineRunID(); id != "" {
		t.Errorf("second Take should be empty, got %q", id)
	}
}

func TestEngineRunIDPattern(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"some output\n[aida-run-id:abc-123]\nmore", "abc-123"},
		{"[aida-run-id:single-line]", "single-line"},
		{"no sentinel here", ""},
		{"[aida-run-id:ts-2026-05-24T10-30-00Z-a1b2]", "ts-2026-05-24T10-30-00Z-a1b2"},
	}
	for _, c := range cases {
		m := engineRunIDPattern.FindStringSubmatch(c.in)
		got := ""
		if len(m) == 2 {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("input %q: got %q want %q", c.in, got, c.want)
		}
	}
}

func TestIsRatingTool(t *testing.T) {
	for _, n := range []string{"jarvis_thumbs_up", "jarvis_thumbs_down", "jarvis_note"} {
		if !IsRatingTool(n) {
			t.Errorf("%s should be a rating tool", n)
		}
	}
	for _, n := range []string{"weather", "current_time", "aida_query"} {
		if IsRatingTool(n) {
			t.Errorf("%s should NOT be a rating tool", n)
		}
	}
}
