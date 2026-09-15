package jobs

import (
	"strings"
	"testing"
)

// DeriveHandle must be deterministic, shaped adjective-noun, and pronounceable
// (only lowercase letters + one hyphen - no digits the LLM would mangle).
func TestDeriveHandle_DeterministicAndClean(t *testing.T) {
	const runID = "20260622-022838-create-a-file"
	h1 := DeriveHandle(runID)
	h2 := DeriveHandle(runID)
	if h1 != h2 {
		t.Errorf("DeriveHandle not deterministic: %q vs %q", h1, h2)
	}
	parts := strings.Split(h1, "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("handle not adjective-noun: %q", h1)
	}
	for _, r := range h1 {
		if !((r >= 'a' && r <= 'z') || r == '-') {
			t.Errorf("handle %q has non-speakable rune %q", h1, r)
		}
	}
	if DeriveHandle("a-different-run-id") == h1 {
		t.Errorf("distinct run-ids collided on %q", h1)
	}
}

// Both word lists must hold exactly 64 unique, TTS-clean entries: one hash
// byte indexes each list and 256 % 64 == 0 keeps the pick uniform.
func TestHandleWordLists(t *testing.T) {
	for name, list := range map[string][]string{
		"adjectives": handleAdjectives,
		"nouns":      handleNouns,
	} {
		if len(list) != 64 {
			t.Errorf("%s: %d entries, want 64 (uniform 256%%64 indexing)", name, len(list))
		}
		seen := map[string]bool{}
		for _, w := range list {
			if seen[w] {
				t.Errorf("%s: duplicate entry %q", name, w)
			}
			seen[w] = true
			for _, r := range w {
				if r < 'a' || r > 'z' {
					t.Errorf("%s: %q has non-speakable rune %q", name, w, r)
				}
			}
		}
	}
}

// RefMatchesHandleWord tolerates a Whisper mishear that keeps one word of
// the handle, but never matches on unrelated words.
func TestRefMatchesHandleWord(t *testing.T) {
	const runID = "20260622-022838-create-a-file"
	parts := strings.SplitN(DeriveHandle(runID), "-", 2)
	adj, noun := parts[0], parts[1]

	for _, ref := range []string{
		adj,                    // just the adjective
		noun,                   // just the noun
		adj + " other",         // mishear: noun garbled, adjective survives
		"the " + noun + " job", // embedded in a phrase
		strings.ToUpper(adj),   // case-insensitive
	} {
		if !RefMatchesHandleWord(runID, ref) {
			t.Errorf("RefMatchesHandleWord(%q) = false, want true (handle %s-%s)", ref, adj, noun)
		}
	}
	for _, ref := range []string{"", "xylophone", "pr five eighty three"} {
		if RefMatchesHandleWord(runID, ref) {
			t.Errorf("RefMatchesHandleWord(%q) = true, want false (handle %s-%s)", ref, adj, noun)
		}
	}
}

// NormalizeRef strips spoken variants to a comparable form so a handle
// round-trips regardless of how Whisper renders it.
func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"amber-otter":    "amberotter",
		"amber otter":    "amberotter",
		"Amber, Otter":   "amberotter",
		"  amber-otter ": "amberotter",
	}
	for in, want := range cases {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}
