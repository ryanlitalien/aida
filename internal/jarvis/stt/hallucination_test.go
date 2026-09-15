package stt

import "testing"

// TestIsHallucination_Bracketed covers the original bracket/paren tag
// filter - the behavior that lived in both internal/jarvis/listener and
// internal/jarvis/lmd before they were de-duplicated into this package.
func TestIsHallucination_Bracketed(t *testing.T) {
	cases := []string{
		"[BLANK_AUDIO]",
		"[blank_audio]",
		"[silence]",
		"[SILENCE]",
		"(upbeat music)",
		"(electronic music)",
		"(rustling)",
		"[no audio]",
		"[music]",
		"[applause]",
		"[laughter]",
		"  [BLANK_AUDIO]  ", // surrounding whitespace
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if !IsHallucination(c) {
				t.Errorf("IsHallucination(%q) = false, want true", c)
			}
		})
	}
}

// TestIsHallucination_BareWords covers the new bug fix: whisper's
// well-known bare-word/short-phrase transcriptions of digital silence, room
// tone, or a brief noise burst. Each is exercised as the FULL transcript,
// in a few punctuation/case variants, since normalizeTranscript is supposed
// to fold those together.
func TestIsHallucination_BareWords(t *testing.T) {
	cases := []string{
		"you", "You", " you ", "YOU",
		"thank you", "Thank you", "thank you.", "THANK YOU.",
		"thanks for watching", "thanks for watching!", "Thanks For Watching!",
		"bye", "Bye.", "BYE!",
		"so", "So.",
		"um", "Um,",
		"uh", "Uh.",
		"okay", "Okay.", "OKAY!",
		"oh", "Oh.",
		"please subscribe", "Please subscribe.", "please subscribe!",
		".",
		"♪", "♫", "♪ ♪ ♪", " ♫♪ ",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if !IsHallucination(c) {
				t.Errorf("IsHallucination(%q) = false, want true", c)
			}
		})
	}
}

// TestIsHallucination_RealSentencesPassThrough is the critical regression
// guard: a real question that merely CONTAINS one of the bare-hallucination
// words as a substring must NOT be filtered. A naive strings.Contains
// implementation would incorrectly reject every one of these.
func TestIsHallucination_RealSentencesPassThrough(t *testing.T) {
	cases := []string{
		"what did you say about the deploy",
		"so what's next",
		"tell him thank you for me",
		"is that okay with you",
		"uh, what's the weather like today",
		"can you bye the way check my tasks", // contrived, but contains "bye"
		"oh, what time is my next meeting",
		"please subscribe to my calendar and check for conflicts",
		"thanks for watching my presentation earlier, can you summarize it",
		"so, um, what's on my plate today",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if IsHallucination(c) {
				t.Errorf("IsHallucination(%q) = true, want false (real speech, not just a substring match)", c)
			}
		})
	}
}

// TestIsHallucination_RealSpeech covers ordinary, unambiguous real queries
// with no overlap at all with the hallucination lists.
func TestIsHallucination_RealSpeech(t *testing.T) {
	cases := []string{
		"what's on my plate today",
		"mark task 162 done",
		"what's the weather in boston",
		"add a task to wire up the cron job",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if IsHallucination(c) {
				t.Errorf("IsHallucination(%q) = true, want false", c)
			}
		})
	}
}

func TestNormalizeTranscript(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Thank You.", "thank you"},
		{"  BYE!  ", "bye"},
		{"so, what's next?", "so, what's next"}, // internal punctuation preserved
		{".", ""},
		{"", ""},
		{"   ", ""},
	}
	for _, c := range cases {
		if got := normalizeTranscript(c.in); got != c.want {
			t.Errorf("normalizeTranscript(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
