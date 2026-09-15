package eval

import "testing"

func TestFailureBoosts_PassVerdictsContributeNothing(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictPass,
			Issues:  []Issue{{Type: "missing-citation", Anchor: "(github: pr-1)"}},
		},
	}
	if got := FailureBoosts(in); len(got) != 0 {
		t.Errorf("pass verdicts should contribute zero boosts, got %v", got)
	}
}

func TestFailureBoosts_MissingCitationDemotes(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues:  []Issue{{Type: "missing-citation", Anchor: "(github: pr-1)"}},
		},
	}
	got := FailureBoosts(in)
	if got["github"] != -25 {
		t.Errorf("github boost = %d, want -25", got["github"])
	}
}

func TestFailureBoosts_MissingSourceBoosts(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues:  []Issue{{Type: "missing-source", Anchor: "(snowflake: row-1)"}},
		},
	}
	got := FailureBoosts(in)
	if got["snowflake"] != +25 {
		t.Errorf("snowflake boost = %d, want +25", got["snowflake"])
	}
}

func TestFailureBoosts_PlaceholderHasNoEffect(t *testing.T) {
	// Placeholder citations are a synth bug, not a routing signal.
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues:  []Issue{{Type: "placeholder-citation", Anchor: "(github: github)"}},
		},
	}
	if got := FailureBoosts(in); len(got) != 0 {
		t.Errorf("placeholder issues should not produce boosts, got %v", got)
	}
}

func TestFailureBoosts_AccumulatesAcrossRecords(t *testing.T) {
	// 3 separate failures all flagging github with missing-citation
	// should sum to -75 before clamping.
	in := []ReviewRecord{
		{Verdict: VerdictFail, Issues: []Issue{{Type: "missing-citation", Anchor: "(github: a)"}}},
		{Verdict: VerdictFail, Issues: []Issue{{Type: "missing-citation", Anchor: "(github: b)"}}},
		{Verdict: VerdictFail, Issues: []Issue{{Type: "missing-citation", Anchor: "(github: c)"}}},
	}
	got := FailureBoosts(in)
	// 3 × -25 = -75, clamped to -MaxAdjustment (-50)
	if got["github"] != -MaxAdjustment {
		t.Errorf("github boost = %d, want %d (clamped)", got["github"], -MaxAdjustment)
	}
}

func TestFailureBoosts_ClampPositive(t *testing.T) {
	// 4 missing-source issues all flagging chrono should clamp to +50.
	var issues []Issue
	for i := 0; i < 4; i++ {
		issues = append(issues, Issue{Type: "missing-source", Anchor: "(chrono: log-1)"})
	}
	in := []ReviewRecord{{Verdict: VerdictFail, Issues: issues}}
	got := FailureBoosts(in)
	if got["chrono"] != MaxAdjustment {
		t.Errorf("chrono boost = %d, want %d (clamped)", got["chrono"], MaxAdjustment)
	}
}

func TestFailureBoosts_MultipleSourcesAccumulateIndependently(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues: []Issue{
				{Type: "missing-citation", Anchor: "(github: a)"},
				{Type: "missing-source", Anchor: "(chrono: x)"},
			},
		},
	}
	got := FailureBoosts(in)
	if got["github"] != -25 {
		t.Errorf("github = %d, want -25", got["github"])
	}
	if got["chrono"] != +25 {
		t.Errorf("chrono = %d, want +25", got["chrono"])
	}
}

func TestFailureBoosts_UnknownIssueTypeIgnored(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues:  []Issue{{Type: "unknown-future-thing", Anchor: "(github: pr-1)"}},
		},
	}
	if got := FailureBoosts(in); len(got) != 0 {
		t.Errorf("unknown issue types should be no-ops, got %v", got)
	}
}

func TestFailureBoosts_MalformedAnchorIgnored(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues:  []Issue{{Type: "missing-citation", Anchor: "not a citation"}},
		},
	}
	if got := FailureBoosts(in); len(got) != 0 {
		t.Errorf("malformed anchors should be no-ops, got %v", got)
	}
}

func TestParseFailures_FlattensIntoAttributions(t *testing.T) {
	in := []ReviewRecord{
		{
			Verdict: VerdictFail,
			Issues: []Issue{
				{Type: "missing-citation", Severity: "error", Anchor: "(github: pr-1)"},
				{Type: "missing-source", Severity: "error", Anchor: "(snowflake: row-1)"},
				// malformed anchor - dropped
				{Type: "missing-citation", Severity: "error", Anchor: "not an anchor"},
			},
		},
		// pass record - entirely dropped
		{Verdict: VerdictPass, Issues: []Issue{{Type: "missing-citation", Anchor: "(github: pr-2)"}}},
	}
	got := ParseFailures(in)
	if len(got) != 2 {
		t.Fatalf("got %d attributions, want 2: %+v", len(got), got)
	}
	if got[0].Source != "github" || got[0].IssueType != "missing-citation" {
		t.Errorf("[0] = %+v, want github/missing-citation", got[0])
	}
	if got[1].Source != "snowflake" || got[1].IssueType != "missing-source" {
		t.Errorf("[1] = %+v, want snowflake/missing-source", got[1])
	}
}

func TestExtractSourceFromAnchor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"(github: pr-1)", "github"},
		{"(snowflake: rows.2026-04.001)", "snowflake"},
		{"(chrono: log-99)", "chrono"},
		{"", ""},
		{"not an anchor", ""},
		{"(GITHUB: pr-1)", ""}, // source names are lowercase
	}
	for _, tc := range cases {
		if got := extractSourceFromAnchor(tc.in); got != tc.want {
			t.Errorf("extractSourceFromAnchor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
