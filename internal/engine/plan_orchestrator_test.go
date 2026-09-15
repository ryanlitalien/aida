package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
)

func TestFuncRunnable(t *testing.T) {
	r := &FuncRunnable{
		Name_: "test",
		Fn: func(_ context.Context, input string) (string, error) {
			return "echo: " + input, nil
		},
	}
	result, err := r.Run(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != "echo: hello" {
		t.Errorf("got %q, want %q", result.FinalOutput, "echo: hello")
	}
	if result.HandoffPath[0] != "test" {
		t.Errorf("handoff path[0] = %q, want %q", result.HandoffPath[0], "test")
	}
}

func TestPlanToRunnableEmpty(t *testing.T) {
	r := PlanToRunnable(&ExecutionPlan{}, nil)
	result, err := r.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.FinalOutput, "no sources") {
		t.Errorf("expected no-sources message, got %q", result.FinalOutput)
	}
}

func TestPlanToRunnableSingleSource(t *testing.T) {
	plan := &ExecutionPlan{
		Phases: []Phase{{
			Name:    "lookup",
			Sources: []ScoredSource{{Name: "snow", Source: &config.Source{}, Score: 10}},
		}},
	}
	execs := map[string]SourceExecFunc{
		"snow": func(_ context.Context, q string) (string, error) {
			return "rows: 42 for " + q, nil
		},
	}
	r := PlanToRunnable(plan, execs)
	result, err := r.Run(context.Background(), "how many?")
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalOutput != "rows: 42 for how many?" {
		t.Errorf("got %q", result.FinalOutput)
	}
}

func TestPlanToRunnableParallelPhase(t *testing.T) {
	plan := &ExecutionPlan{
		Phases: []Phase{{
			Name: "diagnose",
			Sources: []ScoredSource{
				{Name: "snow", Source: &config.Source{}, Score: 10},
				{Name: "chrono", Source: &config.Source{}, Score: 8},
				{Name: "grep", Source: &config.Source{}, Score: 5},
			},
			Parallel: true,
		}},
	}

	var callCount atomic.Int32
	execs := map[string]SourceExecFunc{
		"snow": func(_ context.Context, _ string) (string, error) {
			callCount.Add(1)
			time.Sleep(50 * time.Millisecond)
			return "snow-result", nil
		},
		"chrono": func(_ context.Context, _ string) (string, error) {
			callCount.Add(1)
			time.Sleep(50 * time.Millisecond)
			return "chrono-result", nil
		},
		"grep": func(_ context.Context, _ string) (string, error) {
			callCount.Add(1)
			time.Sleep(50 * time.Millisecond)
			return "grep-result", nil
		},
	}

	start := time.Now()
	r := PlanToRunnable(plan, execs)
	result, err := r.Run(context.Background(), "investigate errors")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatal(err)
	}
	if callCount.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", callCount.Load())
	}
	// Parallel: 3x50ms should complete well under 180ms (sequential would be 150ms+).
	if elapsed > 130*time.Millisecond {
		t.Errorf("parallel phase took %v, expected < 130ms", elapsed)
	}
	// All three results should appear in the merged output.
	for _, want := range []string{"snow-result", "chrono-result", "grep-result"} {
		if !strings.Contains(result.FinalOutput, want) {
			t.Errorf("output missing %q: %q", want, result.FinalOutput)
		}
	}
}

func TestPlanToRunnableMultiPhaseSequential(t *testing.T) {
	plan := &ExecutionPlan{
		Phases: []Phase{
			{
				Name:    "diagnose",
				Sources: []ScoredSource{{Name: "snow", Source: &config.Source{}, Score: 10}},
			},
			{
				Name:           "contextualize",
				Sources:        []ScoredSource{{Name: "docs", Source: &config.Source{}, Score: 5}},
				DependsOnPrior: true,
			},
		},
	}

	var order []string
	execs := map[string]SourceExecFunc{
		"snow": func(_ context.Context, _ string) (string, error) {
			order = append(order, "snow")
			return "snow-data", nil
		},
		"docs": func(_ context.Context, input string) (string, error) {
			order = append(order, "docs")
			return "docs-context from " + input, nil
		},
	}

	r := PlanToRunnable(plan, execs)
	// SequentialAgent pipes output: phase1.FinalOutput → phase2 input.
	_, err := r.Run(context.Background(), "investigate partner errors")
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "snow" || order[1] != "docs" {
		t.Errorf("execution order = %v, want [snow docs]", order)
	}
}

func TestPlanToRunnableMissingSource(t *testing.T) {
	plan := &ExecutionPlan{
		Phases: []Phase{{
			Name:    "lookup",
			Sources: []ScoredSource{{Name: "unknown-src", Source: &config.Source{}, Score: 1}},
		}},
	}
	// No exec registered for "unknown-src".
	r := PlanToRunnable(plan, map[string]SourceExecFunc{})
	result, err := r.Run(context.Background(), "q")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.FinalOutput, "no executor registered") {
		t.Errorf("expected diagnostic, got %q", result.FinalOutput)
	}
}

// Verify FuncRunnable satisfies the Runnable interface at compile time.
var _ orchestrator.Runnable = (*FuncRunnable)(nil)
