package engine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// fakeLLM is a scripted model.LLM that answers based on which role's
// system prompt is calling, and counts calls per role. The evaluator says
// REVISE on its first call and APPROVED after, to exercise the loop.
type fakeLLM struct {
	mu          sync.Mutex
	calls       map[string]int
	evalReplies []string // optional scripted evaluator replies, by call index
}

func (f *fakeLLM) Provider() string { return "fake" }
func (f *fakeLLM) Model() string    { return "fake-1" }

func (f *fakeLLM) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	sys := req.SystemPrefix + " " + req.System
	role := "other"
	switch {
	case strings.Contains(sys, "TRIAGE"):
		role = "triage"
	case strings.Contains(sys, "EVIDENCE"):
		role = "evidence"
	case strings.Contains(sys, "ANALYSIS"):
		role = "analysis"
	case strings.Contains(sys, "RISKS"):
		role = "risks"
	case strings.Contains(sys, "SYNTHESIS"):
		role = "synthesize"
	case strings.Contains(sys, "EVALUATOR"):
		role = "evaluate"
	}

	f.mu.Lock()
	f.calls[role]++
	n := f.calls[role]
	f.mu.Unlock()

	text := role + " findings"
	switch role {
	case "synthesize":
		text = "DRAFT v" + strconv.Itoa(n)
	case "evaluate":
		switch {
		case len(f.evalReplies) > 0:
			idx := n - 1
			if idx >= len(f.evalReplies) {
				idx = len(f.evalReplies) - 1
			}
			text = f.evalReplies[idx]
		case n == 1:
			text = "needs more evidence\nVERDICT: REVISE"
		default:
			text = "well supported\nVERDICT: APPROVED"
		}
	}
	return &model.Response{
		StopReason: model.StopEndTurn,
		Blocks:     []model.Block{{Type: model.BlockText, Text: text}},
		ModelID:    "fake-1",
	}, nil
}

// TestBuildInvestigateGraph drives the whole investigate supervisor with a
// scripted LLM: triage frames, three specialists run in parallel, collect
// joins, and the synthesize⇄evaluate loop iterates once (REVISE) before
// converging (APPROVED). Asserts the topology and loop behavior without
// any real API call.
func TestBuildInvestigateGraph(t *testing.T) {
	llm := &fakeLLM{calls: map[string]int{}}
	g := BuildInvestigateGraph(llm, nil, InvestigateConfig{MaxRevisions: 2})

	res, err := g.Run(context.Background(), "why did the job fail?")
	if err != nil {
		t.Fatalf("investigate run: %v", err)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()

	// triage + each specialist run exactly once.
	for _, role := range []string{"triage", "evidence", "analysis", "risks"} {
		if llm.calls[role] != 1 {
			t.Fatalf("role %q ran %d times, want 1", role, llm.calls[role])
		}
	}
	// The evaluator's first REVISE drives a second synthesis + evaluation.
	if llm.calls["synthesize"] != 2 {
		t.Fatalf("expected 2 synthesis passes (loop), got %d", llm.calls["synthesize"])
	}
	if llm.calls["evaluate"] != 2 {
		t.Fatalf("expected 2 evaluation passes (loop), got %d", llm.calls["evaluate"])
	}
	// OutputKey projects the latest draft from shared state.
	if res.FinalOutput != "DRAFT v2" {
		t.Fatalf("final output should be the revised draft, got %q", res.FinalOutput)
	}
}

// TestInvestigateEvaluatorProseApprovedDoesNotShortCircuit guards the fix
// for the "APPROVED" substring bug: an evaluator whose critique prose
// contains "approved" but whose VERDICT line says REVISE must keep looping.
func TestInvestigateEvaluatorProseApprovedDoesNotShortCircuit(t *testing.T) {
	llm := &fakeLLM{calls: map[string]int{}, evalReplies: []string{
		"this would be approved if X were fixed\nVERDICT: REVISE",
		"now correct\nVERDICT: APPROVED",
	}}
	// MaxRevisions high so the verdict, not the revision bound, controls exit.
	g := BuildInvestigateGraph(llm, nil, InvestigateConfig{MaxRevisions: 5})
	if _, err := g.Run(context.Background(), "q"); err != nil {
		t.Fatalf("run: %v", err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if llm.calls["synthesize"] != 2 || llm.calls["evaluate"] != 2 {
		t.Fatalf("prose 'approved' short-circuited the revise loop: synth=%d eval=%d",
			llm.calls["synthesize"], llm.calls["evaluate"])
	}
}
