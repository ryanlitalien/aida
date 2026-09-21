package sources

import "testing"

func TestStripOuterQuotes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"--oneline -n 20"`, `--oneline -n 20`},
		{`'--oneline -n 20'`, `--oneline -n 20`},
		{`--oneline -n 20`, `--oneline -n 20`},
		{`""`, ``},
		{`"`, `"`},
		{``, ``},
		{`"--author=Aida"`, `--author=Aida`},
	}
	for _, c := range cases {
		got := stripOuterQuotes(c.in)
		if got != c.want {
			t.Errorf("stripOuterQuotes(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHasRefSelector_BareNumberIsNotBranch(t *testing.T) {
	// "-n 1" splits into ["-n", "1"]. The bare "1" must NOT be treated
	// as a branch name, otherwise --all is skipped and feature-branch
	// commits become invisible.
	args := removeStripArgs(`--author="Ralph" -n 1`)
	if hasRefSelector(args) {
		t.Errorf("hasRefSelector(%q) = true, want false (bare number is not a ref)", args)
	}
}

// TestRemoveStripArgs_HandlesQuotedLLMOutput covers issue #14 Bug B:
// The LLM sometimes wraps the entire git args string in quotes. After
// the Execute path strips the outer quotes, removeStripArgs should
// correctly drop --oneline.
func TestRemoveStripArgs_HandlesQuotedLLMOutput(t *testing.T) {
	args := stripOuterQuotes(`"--oneline -n 20"`)
	got := removeStripArgs(args)
	if got != "-n 20" {
		t.Errorf("removeStripArgs after stripOuterQuotes = %q, want %q", got, "-n 20")
	}
}
