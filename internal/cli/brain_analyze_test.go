package cli

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
)

func TestProposeFromFailures(t *testing.T) {
	rows := []analyzeRow{
		{source: "sqlite", failures: 5, topType: "missing-source", topCount: 5},
		{source: "github", failures: 1, topType: "missing-citation", topCount: 1}, // below threshold
	}
	runs := []brain.EvalRun{
		{RunID: "r1", Question: "partner GMV for camp-butz", AggregateVerdict: "fail"},
		{RunID: "r2", Question: "passing one", AggregateVerdict: "pass"},
		{RunID: "r3", Question: "partner GMV for camp-butz", AggregateVerdict: "fail"}, // dup question
	}
	out := proposeFromFailures(rows, runs, 2)

	if !strings.Contains(out, "sqlite") {
		t.Error("sqlite (5 >= 2) should earn a routing-boost proposal")
	}
	if strings.Contains(out, "**github**") {
		t.Error("github (1 < 2) is below threshold and should not be proposed")
	}
	if !strings.Contains(out, "routing isn't") && !strings.Contains(out, "isn't routing to it") {
		t.Error("missing-source proposal should mention the planner not routing to it")
	}
	if !strings.Contains(out, "partner GMV for camp-butz") {
		t.Error("failing question should appear as a golden-test proposal")
	}
	if strings.Contains(out, "passing one") {
		t.Error("passing question must not be proposed")
	}
	if strings.Count(out, "partner GMV for camp-butz") != 1 {
		t.Error("duplicate failing question should be de-duplicated")
	}
}

func TestProposeFromFailuresEmpty(t *testing.T) {
	out := proposeFromFailures(nil, nil, 2)
	if !strings.Contains(out, "No source reached the proposal threshold") {
		t.Error("empty rows should note the threshold")
	}
	if !strings.Contains(out, "No failing questions") {
		t.Error("empty runs should note no golden-test proposals")
	}
}
