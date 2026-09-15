package cli

import "testing"

func TestMatchesTimeShortcut(t *testing.T) {
	tests := []struct {
		name     string
		question string
		want     bool
	}{
		// --- whitelist: must match ---
		{"what time is it", "What time is it?", true},
		{"whats the time", "What's the time?", true},
		{"whats the time no apostrophe", "whats the time", true},
		{"what is the time", "What is the time", true},
		{"what time is it now", "What time is it now?", true},
		{"what time is it right now", "what time is it right now", true},
		{"whats todays date apostrophes", "What's today's date?", true},
		{"whats todays date no apostrophe", "whats todays date", true},
		{"what is todays date apostrophe", "What is today's date?", true},
		{"what day is it", "What day is it?", true},
		{"what day is it today", "What day is it today?", true},
		{"whats the date", "What's the date?", true},
		{"whats the date today", "what's the date today", true},
		{"what is the date", "What is the date?", true},
		{"what is the date today", "What is the date today?", true},
		{"current time", "current time", true},
		{"time now", "time now", true},
		{"what timezone am i in", "What timezone am I in?", true},
		{"what time zone am i in", "What time zone am I in?", true},
		{"whats the day today", "What's the day today?", true},
		{"trailing period", "What time is it.", true},
		{"trailing bang", "current time!", true},
		{"extra whitespace collapses", "what   time   is   it", true},

		// --- must NOT match: extra scope/tokens ---
		{"time is the game", "what time is the game", false},
		{"time in tokyo", "what time is it in tokyo", false},
		{"date works for meeting", "what date works for the meeting", false},
		{"unrelated question", "what is the capital of France", false},
		{"time plus context", "what time is it, I have a meeting", false},
		{"empty string", "", false},
		{"partial prefix only", "what time", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesTimeShortcut(tt.question)
			if got != tt.want {
				t.Errorf("matchesTimeShortcut(%q) = %v, want %v", tt.question, got, tt.want)
			}
		})
	}
}

func TestNormalizeTimeQuestion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"What's the time?", "whats the time"},
		{"  current   time  ", "current time"},
		{"What time is it!!!", "what time is it"},
		{"WHAT DAY IS IT", "what day is it"},
	}
	for _, tt := range tests {
		got := normalizeTimeQuestion(tt.in)
		if got != tt.want {
			t.Errorf("normalizeTimeQuestion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
