package jarvis

import (
	"testing"
	"unicode/utf8"
)

// truncate must never split a multi-byte rune - replies carry °/€/… and an
// invalid-UTF-8 fragment would flow into prompts, JSON, and durable memory.
func TestTruncateRuneSafe(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("under-limit string changed: %q", got)
	}
	// "a€b": the euro sign is 3 bytes (positions 1-3). Cutting at n=2 lands
	// mid-rune and must back off to the boundary after "a".
	if got := truncate("a€bcdef", 2); got != "a…" {
		t.Errorf("mid-rune cut = %q, want %q", got, "a…")
	}
	for _, n := range []int{1, 2, 3, 4, 5} {
		if got := truncate("°°°°", n); !utf8.ValidString(got) {
			t.Errorf("truncate(°°°°, %d) = %q is invalid UTF-8", n, got)
		}
	}
}
