package stt

import "strings"

// bareHallucinations lists whisper's well-known bare-word/short-phrase
// transcriptions of silence, room tone, or a brief noise burst - none of
// them carry the "[...]" / "(...)" bracket markers IsHallucination's other
// branches catch, so they need their own list. Keyed by the FULL normalized
// transcript (see normalizeTranscript): every lookup here is whole-string
// equality, never strings.Contains, because a substring match would reject
// legitimate speech like "what did you say about the deploy" (contains
// "you") or "so what's next" (contains "so").
//
// "" is included on purpose: normalizeTranscript trims trailing sentence
// punctuation, so a transcript that was nothing but punctuation (whisper
// occasionally emits a bare ".") collapses to "" and lands here instead of
// needing its own special case.
var bareHallucinations = map[string]bool{
	"":                    true,
	"you":                 true,
	"thank you":           true,
	"thanks for watching": true,
	"bye":                 true,
	"so":                  true,
	"um":                  true,
	"uh":                  true,
	"okay":                true,
	"oh":                  true,
	"please subscribe":    true,
}

// normalizeTranscript lowercases s, trims surrounding whitespace, and trims
// trailing sentence punctuation (. , ! ? ; :) so "Thank you.", "bye!", and
// "Bye." all collapse to the same key bareHallucinations looks up. Only
// TRAILING punctuation is stripped - punctuation in the middle of a real
// sentence is left untouched, so "so, what's next?" keeps its shape and
// doesn't accidentally collapse to "so".
func normalizeTranscript(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	return strings.TrimRight(t, ".,!?;: ")
}

// IsHallucination reports whether transcript is one of whisper's well-known
// non-speech outputs on quiet or musical input: bracketed/parenthesized tags
// like "[BLANK_AUDIO]" or "(upbeat music)", bare words/short phrases like
// "you" or "thank you" (see bareHallucinations), or a transcript made up
// only of musical note glyphs.
//
// Shared by internal/jarvis/listener (desk-mic loop, where amplitude VAD
// already gates out most silence before whisper ever runs - this is a
// second line of defense) and internal/jarvis/lmd (phone push-to-talk,
// which has no VAD; this filter plus the RMS energy gate in that package
// are the only things standing between held-button silence and a wasted
// LLM + TTS call).
func IsHallucination(transcript string) bool {
	t := normalizeTranscript(transcript)

	if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
		return true
	}
	if strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")") {
		return true
	}
	// A few specific known tags that may appear with surrounding text.
	for _, marker := range []string{
		"[blank_audio]", "[silence]", "[no audio]", "[music]",
		"[applause]", "[laughter]",
	} {
		if strings.Contains(t, marker) {
			return true
		}
	}

	if bareHallucinations[t] {
		return true
	}

	return isMusicalNotesOnly(t)
}

// isMusicalNotesOnly reports whether s - once whitespace is stripped - is
// composed entirely of musical note glyphs (whisper's transcription for
// background music with no lyrics) and contains at least one.
func isMusicalNotesOnly(s string) bool {
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	if stripped == "" {
		return false
	}
	for _, r := range stripped {
		if r != '♪' && r != '♫' {
			return false
		}
	}
	return true
}
