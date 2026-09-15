package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestConnectorIntent(t *testing.T) {
	cases := []struct {
		q       string
		want    bool
		wantGrp string
	}{
		{"what's my next calendar event", true, "calendar"},
		{"what's my next meeting", true, "calendar"},
		{"any unread emails from Bob", true, "gmail"},
		{"check my inbox", true, "gmail"},
		{"send a dm to Sarah on slack", true, "slack"},
		{"find the budget google sheet", true, "drive"},
		{"look up the deal in attio", true, "attio"},
		{"search my notion workspace", true, "notion"},
		{"show airtable records", true, "airtable"},

		// Non-connector: must NOT fire.
		{"how do I rebase in git", false, ""},
		{"what's the weather", false, ""},
		{"summarize this repo", false, ""},
		{"deploy the viewer", false, ""},
		{"what's the resonant frequency of titanium", false, ""},
		{"explain the data pipeline", false, ""}, // "pipeline" intentionally not a keyword
		{"", false, ""},
	}
	for _, c := range cases {
		ok, grp := connectorIntent(c.q)
		if ok != c.want || (c.want && grp != c.wantGrp) {
			t.Errorf("connectorIntent(%q) = (%v,%q), want (%v,%q)", c.q, ok, grp, c.want, c.wantGrp)
		}
	}
}

func TestOnlyWeakSources(t *testing.T) {
	cases := []struct {
		names []string
		want  bool
	}{
		{[]string{"web-search"}, true},
		{nil, false},                                    // no sources → handled by the no-source path
		{[]string{}, false},                             // ditto
		{[]string{"butter-stack"}, false},               // a real source
		{[]string{"web-search", "butter-stack"}, false}, // mixed → a real source is present
		{[]string{"git-dev"}, false},
	}
	for _, c := range cases {
		if got := onlyWeakSources(c.names); got != c.want {
			t.Errorf("onlyWeakSources(%v) = %v, want %v", c.names, got, c.want)
		}
	}
}

func TestClaudeFallbackEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-test")
	// Widened set (config.ScrubAnthropicCreds): confirm it reaches this
	// wrapper too, not just the two original vars.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "oauth-test")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	env := claudeFallbackEnv()
	guard := false
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK":
			t.Errorf("credential %q should have been scrubbed", k)
		}
		if kv == "AIDA_NO_CLAUDE_FALLBACK=1" {
			guard = true
		}
	}
	if !guard {
		t.Error("AIDA_NO_CLAUDE_FALLBACK=1 should be set in the fallback env")
	}
}

// maybeClaudeFallback must short-circuit (no claude spawn) for the recursion
// guard and for non-connector questions. These paths return before
// runClaudeFallback, so the tests never shell out.
func TestMaybeClaudeFallback_ShortCircuits(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{} // no profiles → fallback defaults ON

	t.Run("recursion guard", func(t *testing.T) {
		t.Setenv("AIDA_NO_CLAUDE_FALLBACK", "1")
		if _, ok := maybeClaudeFallback(ctx, "any unread emails", cfg); ok {
			t.Error("recursion guard should short-circuit even for connector questions")
		}
	})

	t.Run("non-connector question", func(t *testing.T) {
		t.Setenv("AIDA_NO_CLAUDE_FALLBACK", "")
		if _, ok := maybeClaudeFallback(ctx, "how do I rebase in git", cfg); ok {
			t.Error("non-connector question should not trigger the fallback")
		}
	})
}
