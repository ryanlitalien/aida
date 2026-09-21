package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

func TestExtractAutoSolveDrafts_SingleDraft(t *testing.T) {
	plan := &brain.ExecPlan{
		ID:        "20260505-abc",
		Title:     "Investigate approver flow",
		Status:    brain.ExecPlanStatusCompleted,
		UpdatedAt: time.Now().UTC(),
		Origin:    brain.ExecPlanOrigin{Type: "ingest", Ref: "notion:xyz"},
		Body: `## Goal

Investigate approver flow data display issue.

## Decision log

- 2026-05-05T18:44:14Z [auto-solve] draft answer:
The approver flow data isn't showing because of a missing JOIN.
Likely fix: add LEFT JOIN approvers ON ... in the rake template.
`,
	}
	got := extractAutoSolveDrafts(plan)
	if len(got) != 1 {
		t.Fatalf("expected 1 draft, got %d", len(got))
	}
	d := got[0]
	if d.PlanTitle != "Investigate approver flow" {
		t.Errorf("PlanTitle = %q", d.PlanTitle)
	}
	if !strings.Contains(d.Body, "missing JOIN") {
		t.Errorf("draft body should contain analysis: %q", d.Body)
	}
	if d.Failed {
		t.Errorf("draft should not be marked failed")
	}
	if d.Source != "notion:xyz" {
		t.Errorf("source = %q", d.Source)
	}
}

func TestExtractAutoSolveDrafts_FailureMarked(t *testing.T) {
	plan := &brain.ExecPlan{
		ID:    "20260505-fail",
		Title: "Broken task",
		Body: `## Decision log

- 2026-05-05T19:00:00Z [auto-solve] aida --agent failed: exit code 1
output:
something went wrong
`,
	}
	got := extractAutoSolveDrafts(plan)
	if len(got) != 1 {
		t.Fatalf("expected 1 draft, got %d", len(got))
	}
	if !got[0].Failed {
		t.Errorf("expected failure flag")
	}
	if !strings.Contains(got[0].Body, "exit code 1") {
		t.Errorf("body should retain failure detail: %q", got[0].Body)
	}
}

func TestExtractAutoSolveDrafts_IgnoresNonAutoSolveLogs(t *testing.T) {
	plan := &brain.ExecPlan{
		ID:    "20260505-mixed",
		Title: "Mixed log",
		Body: `## Decision log

- 2026-05-05T17:00:00Z [agent] turn 1: brain_search → ok
- 2026-05-05T17:00:05Z [agent] turn 2: get_layer → ok
- 2026-05-05T18:44:14Z [auto-solve] draft answer:
This is the only thing the drafts surface should show.
- 2026-05-05T18:50:00Z [user] noted: something
`,
	}
	got := extractAutoSolveDrafts(plan)
	if len(got) != 1 {
		t.Fatalf("expected 1 auto-solve draft, got %d", len(got))
	}
	if !strings.Contains(got[0].Body, "only thing the drafts surface") {
		t.Errorf("wrong body extracted: %q", got[0].Body)
	}
	// And specifically that subsequent [user] line did NOT bleed in.
	if strings.Contains(got[0].Body, "noted:") {
		t.Errorf("user log line bled into draft body: %q", got[0].Body)
	}
}

func TestExtractAutoSolveDrafts_MultipleDrafts(t *testing.T) {
	plan := &brain.ExecPlan{
		ID:    "20260505-multi",
		Title: "Multi-draft",
		Body: `## Decision log

- 2026-05-05T18:00:00Z [auto-solve] draft answer:
First draft body.
- 2026-05-05T19:00:00Z [auto-solve] draft answer:
Second draft body.
`,
	}
	got := extractAutoSolveDrafts(plan)
	if len(got) != 2 {
		t.Fatalf("expected 2 drafts, got %d", len(got))
	}
	if !strings.Contains(got[0].Body, "First draft") {
		t.Errorf("first draft body wrong: %q", got[0].Body)
	}
	if !strings.Contains(got[1].Body, "Second draft") {
		t.Errorf("second draft body wrong: %q", got[1].Body)
	}
}

func TestPlanMatchesTags_AndAcrossFilters(t *testing.T) {
	tags := []string{"acme-widgets", "owner:Aida", "p2"}
	cases := []struct {
		name    string
		filters []string
		want    bool
	}{
		{"no filters → match", nil, true},
		{"single match", []string{"acme-widgets"}, true},
		{"both match (AND)", []string{"acme-widgets", "Aida"}, true},
		{"one fails", []string{"acme-widgets", "missing"}, false},
		{"substring works", []string{"acme"}, true},
		{"case insensitive", []string{"AIDA"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planMatchesTags(tags, c.filters); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestBracketAfter(t *testing.T) {
	cases := map[string]bool{
		"- 2026-05-05T... [author] msg":  true,
		"- 2026-05-05T... no brackets":   false,
		"continuation with no log shape": false,
		"- ] backwards [":                false,
	}
	for in, want := range cases {
		if got := bracketAfter(in); got != want {
			t.Errorf("bracketAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRunTasksDrafts_FilterCompositionShape(t *testing.T) {
	// Smoke test on filter composition. Doesn't run the command
	// (needs a brain on disk); just verifies the source-hash gets
	// folded into the tag filter set as expected by the docs.
	opts := tasksDraftsOpts{
		SourceHash: "abc123",
		Tags:       []string{"acme-widgets"},
	}
	tagFilters := append([]string(nil), opts.Tags...)
	if opts.SourceHash != "" {
		tagFilters = append(tagFilters, sourceTagPrefix+opts.SourceHash)
	}
	if len(tagFilters) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(tagFilters))
	}
	if tagFilters[1] != "source-hash:abc123" {
		t.Errorf("source-hash tag = %q", tagFilters[1])
	}
}
