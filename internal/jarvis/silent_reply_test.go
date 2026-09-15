package jarvis

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis/llm"
)

// TestResolveReply_ReproducesSilentRatingTurn reproduces the exact failure
// from the audit log: a rating tool ran successfully (no error) and the
// model returned empty text. Before the fix, the turn produced no reply at
// all and TTS never ran, so the user heard nothing. resolveReply must now
// fill in the tool's own short acknowledgement so the turn still speaks.
func TestResolveReply_ReproducesSilentRatingTurn(t *testing.T) {
	cases := []struct {
		name  string
		calls []llm.ToolCallStat
		want  string
	}{
		{
			name:  "jarvis_thumbs_up, 223ms, no error",
			calls: []llm.ToolCallStat{{Name: "jarvis_thumbs_up", TookMs: 223, Result: "Noted, sir."}},
			want:  "Noted, sir.",
		},
		{
			name:  "jarvis_note, 167ms, no error",
			calls: []llm.ToolCallStat{{Name: "jarvis_note", TookMs: 167, Result: "Noted, sir."}},
			want:  "Noted, sir.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, usedFallback := resolveReply("", c.calls)
			if !usedFallback {
				t.Fatalf("expected the fallback to fire, the turn would otherwise say nothing")
			}
			if reply != c.want {
				t.Errorf("reply = %q, want %q", reply, c.want)
			}
			if reply == "" {
				t.Fatal("reply must not be empty, a silent turn is exactly the bug being fixed")
			}
		})
	}
}

// TestResolveReply_NonEmptyReplyUntouched guards the normal case: when the
// model DOES return text, resolveReply must not overwrite it, even if a
// tool also ran and even if the grounding guard already rewrote it.
func TestResolveReply_NonEmptyReplyUntouched(t *testing.T) {
	reply, usedFallback := resolveReply("Functioning nominally, sir.",
		[]llm.ToolCallStat{{Name: "jarvis_thumbs_up", Result: "Noted, sir."}})
	if usedFallback {
		t.Error("must not use the fallback when the model already returned text")
	}
	if reply != "Functioning nominally, sir." {
		t.Errorf("reply = %q, want the model's own text unchanged", reply)
	}
}

// TestResolveReply_NoSuccessfulToolStaysEmpty covers a turn with no tool
// calls, and a turn where every tool call errored: resolveReply must leave
// the reply empty rather than inventing something to say. This is a
// pre-existing, separate behavior this fix does not touch.
func TestResolveReply_NoSuccessfulToolStaysEmpty(t *testing.T) {
	cases := []struct {
		name  string
		calls []llm.ToolCallStat
	}{
		{"no tool calls at all", nil},
		{"only an errored call", []llm.ToolCallStat{{Name: "weather", Error: "wttr.in status=500"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, usedFallback := resolveReply("", c.calls)
			if usedFallback || reply != "" {
				t.Errorf("resolveReply(%q, %v) = (%q, %v), want (\"\", false)", "", c.calls, reply, usedFallback)
			}
		})
	}
}

// TestFallbackForSilentReply_UsesMostRecentSuccess checks that an errored
// trailing call doesn't clobber a real result from an earlier successful
// call, and that among multiple successes the most recent one wins.
func TestFallbackForSilentReply_UsesMostRecentSuccess(t *testing.T) {
	calls := []llm.ToolCallStat{
		{Name: "current_time", Result: "8:35 PM (Monday, July 20, 2026, EDT)"},
		{Name: "weather", Error: "wttr.in timeout"},
	}
	if got := fallbackForSilentReply(calls); got != "8:35 PM (Monday, July 20, 2026, EDT)" {
		t.Errorf("got %q, want the earlier successful call's result", got)
	}

	calls = []llm.ToolCallStat{
		{Name: "job_start", Result: "Working on it, sir. I'll call this one amber-otter."},
		{Name: "job_status", Result: "amber-otter (draft budget) is running"},
	}
	if got := fallbackForSilentReply(calls); got != "amber-otter (draft budget) is running" {
		t.Errorf("got %q, want the most recent successful call's result", got)
	}
}

// TestSpeakableAsIs_RejectsUnsafePayloads is the crux of the design: many
// tools return large or structured payloads that must NOT be read verbatim
// even when the model itself falls silent. Speaking a raw blob would be
// worse than the silence this fix closes.
func TestSpeakableAsIs_RejectsUnsafePayloads(t *testing.T) {
	unsafe := []string{
		// mcp_find_tool's raw JSON array of candidate tools.
		`[{"server":"nytimes","name":"nyt_search","description":"Search NYT articles"}]`,
		// tasks_list's multi-line numbered list.
		"1. [p1] finish the cron migration (open)\n2. [p2] review the index pipeline (open)",
		// day_summary's multi-line dossier.
		"Time: 8:35 PM, Monday, July 20, 2026\nWeather (Boston, MA): Clear 72F 40% 5mph\nTasks: no open tasks.",
		// A long aida_query paragraph past the safety threshold.
		strings.Repeat("This is a long synthesized answer with citations. ", 6),
	}
	for _, s := range unsafe {
		if speakableAsIs(s) {
			t.Errorf("speakableAsIs(%q) = true, want false (unsafe to speak verbatim)", truncate(s, 60))
		}
	}

	safe := []string{
		"Noted, sir.",
		"Added task #183: review the index pipeline",
		"Completed task #162: finish the cron migration",
		"Current conditions in Boston, MA: Partly cloudy +57°F 64% wind 8mph",
	}
	for _, s := range safe {
		if !speakableAsIs(s) {
			t.Errorf("speakableAsIs(%q) = false, want true (short single-line prose)", s)
		}
	}
}

// TestFallbackForSilentReply_FallsBackToGenericAck confirms that when the
// only successful tool's result is unsafe to speak (per speakableAsIs),
// the turn still produces speakable output via the generic acknowledgement
// rather than either staying silent or reading the raw payload.
func TestFallbackForSilentReply_FallsBackToGenericAck(t *testing.T) {
	calls := []llm.ToolCallStat{
		{Name: "mcp_find_tool", Result: `[{"server":"nytimes","name":"nyt_search"}]`},
	}
	got := fallbackForSilentReply(calls)
	if got == "" {
		t.Fatal("must not stay silent when a tool succeeded")
	}
	if got == calls[0].Result {
		t.Fatal("must not speak the raw JSON payload verbatim")
	}
	if got != silentReplyGenericFallback {
		t.Errorf("got %q, want the generic fallback %q", got, silentReplyGenericFallback)
	}
}
