package lessons

import (
	"strings"
	"testing"
)

func TestBuildRoutingHint_FullMetadata(t *testing.T) {
	hint := BuildRoutingHint(
		[]string{"snowflake", "chrono"},
		"query",
		"query",
		[]string{"merchant", "volume", "revenue"},
	)

	if hint == "" {
		t.Fatal("expected non-empty routing hint")
	}
	if !strings.Contains(hint, "snowflake") {
		t.Error("hint should contain source name 'snowflake'")
	}
	if !strings.Contains(hint, "chrono") {
		t.Error("hint should contain source name 'chrono'")
	}
	if !strings.Contains(hint, "query") {
		t.Error("hint should contain action 'query'")
	}
	if !strings.Contains(hint, "merchant") {
		t.Error("hint should contain keyword 'merchant'")
	}

	expected := "route to [snowflake, chrono], effective for query queries about merchant, volume, revenue"
	if hint != expected {
		t.Errorf("unexpected hint:\n  got:  %s\n  want: %s", hint, expected)
	}
}

func TestBuildRoutingHint_NoKeywords(t *testing.T) {
	hint := BuildRoutingHint(
		[]string{"notion"},
		"search",
		"search",
		nil,
	)
	expected := "route to [notion], effective for search queries"
	if hint != expected {
		t.Errorf("unexpected hint:\n  got:  %s\n  want: %s", hint, expected)
	}
}

func TestBuildRoutingHint_EmptyAction(t *testing.T) {
	hint := BuildRoutingHint(
		[]string{"grep"},
		"",
		"",
		[]string{"function", "error"},
	)
	if !strings.Contains(hint, "general") {
		t.Error("hint should default to 'general' when action is empty")
	}
	expected := "route to [grep], effective for general queries about function, error"
	if hint != expected {
		t.Errorf("unexpected hint:\n  got:  %s\n  want: %s", hint, expected)
	}
}

func TestBuildRoutingHint_NoSources(t *testing.T) {
	hint := BuildRoutingHint(nil, "query", "query", []string{"test"})
	if hint != "" {
		t.Errorf("expected empty hint with no sources, got: %s", hint)
	}
}

func TestBuildRoutingHint_ManyKeywords_Truncated(t *testing.T) {
	keywords := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"}
	hint := BuildRoutingHint([]string{"src"}, "lookup", "lookup", keywords)

	// Should only include first 5 keywords
	if strings.Contains(hint, "zeta") {
		t.Error("hint should truncate keywords to 5, but found 'zeta' (6th keyword)")
	}
	if !strings.Contains(hint, "epsilon") {
		t.Error("hint should include the 5th keyword 'epsilon'")
	}
}

func TestBuildRoutingHint_SingleSource(t *testing.T) {
	hint := BuildRoutingHint(
		[]string{"snowflake"},
		"lookup",
		"lookup",
		[]string{"partner", "ari"},
	)
	expected := "route to [snowflake], effective for lookup queries about partner, ari"
	if hint != expected {
		t.Errorf("unexpected hint:\n  got:  %s\n  want: %s", hint, expected)
	}
}
