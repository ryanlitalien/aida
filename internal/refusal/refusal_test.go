package refusal

import "testing"

// TestLooksLikeRefusal_Positives covers real refusal prose emitted by
// LLM Call #2 (query construction) and by a delegated claude-project
// sub-agent, including the exact string from the gworkspace incident that
// motivated this check.
func TestLooksLikeRefusal_Positives(t *testing.T) {
	cases := []string{
		`I cannot build a query for the "gworkspace" tool to answer "what time is it".`,
		"I'm unable to build a query for that request.",
		"I am unable to answer this without more context.",
		"I don't have access to that data source.",
		"I do not have access to the requested information.",
		"No applicable source exists for this question.",
		"i cannot help with that request.",
		"I can't find anything relevant to that query.",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if !LooksLikeRefusal(s) {
				t.Errorf("LooksLikeRefusal(%q) = false, want true", s)
			}
		})
	}
}

// TestLooksLikeRefusal_Negatives covers legitimate short commands that must
// never be flagged as refusals -- the whole point of scoping the check to
// the head of the string is to avoid false positives on real queries.
func TestLooksLikeRefusal_Negatives(t *testing.T) {
	cases := []string{
		"SELECT * FROM charges WHERE created > '2026-01-01'",
		"gh pr list --state open",
		"current time ET New York",
		"grep -rn -E 'timezone' /Users/fakehome/dev/aida",
		"",
		"   ",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if LooksLikeRefusal(s) {
				t.Errorf("LooksLikeRefusal(%q) = true, want false", s)
			}
		})
	}
}

// TestLooksLikeCommandRefusal_Positives covers refusal prose that LLM Call
// #2 emits instead of a command -- all must still be caught by the narrow
// verb-anchored list, including the exact gworkspace incident string.
func TestLooksLikeCommandRefusal_Positives(t *testing.T) {
	cases := []string{
		`I cannot build a query for the "gworkspace" tool to answer "what time is it".`,
		"I can't build a query for that tool.",
		"I'm unable to construct a query for that request.",
		"I am unable to generate a command for this source.",
		"I cannot construct a valid SQL statement for this question.",
		"I cannot generate a query without more context.",
		"I cannot answer this with the available tool.",
		"I don't have access to that data source.",
		"No applicable query exists for this source.",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if !LooksLikeCommandRefusal(s) {
				t.Errorf("LooksLikeCommandRefusal(%q) = false, want true", s)
			}
		})
	}
}

// TestLooksLikeCommandRefusal_Negatives is the false-positive regression
// guard: generated search queries that echo the user's own phrasing carry
// "i can't" / "can't i" / "i don't know" right at the head of the string
// and must NOT be classified as command refusals -- the broad list would
// misfire on these, which is exactly why the command path uses the narrow
// verb-anchored list.
func TestLooksLikeCommandRefusal_Negatives(t *testing.T) {
	cases := []string{
		"why i can't focus in the mornings",
		"why can't i connect to the vpn",
		"i don't know lyrics meaning",
		"SELECT * FROM charges WHERE created > '2026-01-01'",
		"gh pr list --state open",
		"current time ET New York",
		"grep -rn -E 'timezone' /Users/fakehome/dev/aida",
		"",
		"   ",
	}
	for _, s := range cases {
		t.Run(s, func(t *testing.T) {
			if LooksLikeCommandRefusal(s) {
				t.Errorf("LooksLikeCommandRefusal(%q) = true, want false", s)
			}
		})
	}
}

// TestLooksLikeRefusal_DeepMarkerNotFlagged ensures a marker buried deep in
// otherwise-legitimate prose (well past the head window) doesn't trip the
// check -- refusal prose front-loads its disclaimer, so anything found only
// deep in the string is very unlikely to actually be a refusal.
func TestLooksLikeRefusal_DeepMarkerNotFlagged(t *testing.T) {
	padding := ""
	for len(padding) < headWindow {
		padding += "the quick brown fox jumps over the lazy dog, "
	}
	s := padding + "note: i cannot stress this enough, always double check."
	if LooksLikeRefusal(s) {
		t.Errorf("LooksLikeRefusal should not flag a marker buried past the head window")
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"single line", "hello world", "hello world"},
		{"multi line takes first", "first line\nsecond line", "first line"},
		{"trims whitespace", "  spaced out  \nmore", "spaced out"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FirstLine(tt.input); got != tt.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFirstLine_Truncates(t *testing.T) {
	long := ""
	for i := 0; i < 50; i++ {
		long += "0123456789"
	}
	got := FirstLine(long)
	if len(got) > firstLineMaxLen {
		t.Errorf("FirstLine output length %d exceeds max %d", len(got), firstLineMaxLen)
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("FirstLine(%q) = %q, want suffix '...'", long, got)
	}
}
