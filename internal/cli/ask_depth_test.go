package cli

import "testing"

// TestDispatchDepthExceeded covers the one-hop guard: depth 0/1 (unset,
// zero, or one hop already taken) is allowed; depth 2+ is refused. An
// unparseable value is treated as depth 0 so a malformed inherited env
// var never wedges `aida ask` shut.
func TestDispatchDepthExceeded(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"unset", "", false},
		{"zero", "0", false},
		{"one hop allowed", "1", false},
		{"two hops refused", "2", true},
		{"three hops refused", "3", true},
		{"unparseable treated as zero", "not-a-number", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatchDepthExceeded(tc.raw); got != tc.want {
				t.Errorf("dispatchDepthExceeded(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}
