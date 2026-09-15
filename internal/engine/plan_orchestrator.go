package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/sources"
)

// FuncRunnable adapts a plain function into an orchestrator.Runnable.
// This is the glue between Aida's deterministic planner output and the
// orchestrator's composite primitives - each source query can be wrapped
// as a FuncRunnable without requiring an LLM-driven Agent.
type FuncRunnable struct {
	Name_ string
	Fn    func(ctx context.Context, input string) (string, error)
}

// Run implements orchestrator.Runnable.
func (f *FuncRunnable) Run(ctx context.Context, input string, _ ...orchestrator.RunOption) (*orchestrator.RunResult, error) {
	output, err := f.Fn(ctx, input)
	if err != nil {
		return nil, err
	}
	return &orchestrator.RunResult{
		FinalOutput: output,
		HandoffPath: []string{f.Name_},
	}, nil
}

// SourceExecFunc is a function that executes a query against a named
// source and returns the text result. Callers supply these by closing
// over the LLM client, source config, and any context needed for query
// generation.
type SourceExecFunc func(ctx context.Context, question string) (string, error)

// PlanToRunnable converts an ExecutionPlan into an orchestrator composite
// graph. Each phase becomes a child Runnable; phases are composed
// sequentially (matching the plan's DependsOnPrior semantics). Within a
// phase, parallel sources fan out via ParallelAgent and sequential
// sources run via SequentialAgent.
//
// sourceExecs maps source names to their execution functions. Sources in
// the plan that are missing from the map are skipped with a warning in
// their output.
func PlanToRunnable(plan *ExecutionPlan, sourceExecs map[string]SourceExecFunc) orchestrator.Runnable {
	if len(plan.Phases) == 0 {
		return &FuncRunnable{
			Name_: "empty-plan",
			Fn:    func(_ context.Context, _ string) (string, error) { return "(no sources planned)", nil },
		}
	}

	var phaseRunnables []orchestrator.Runnable

	for _, phase := range plan.Phases {
		var children []orchestrator.Runnable

		for _, scored := range phase.Sources {
			name := scored.Name
			execFn, ok := sourceExecs[name]
			if !ok {
				// Source not in the exec map - produce a diagnostic.
				execFn = func(_ context.Context, _ string) (string, error) {
					return fmt.Sprintf("(source %q: no executor registered)", name), nil
				}
			}
			children = append(children, &FuncRunnable{
				Name_: name,
				Fn:    execFn,
			})
		}

		if len(children) == 0 {
			continue
		}

		var phaseRunnable orchestrator.Runnable
		if len(children) == 1 {
			phaseRunnable = children[0]
		} else if phase.Parallel {
			phaseRunnable = &orchestrator.ParallelAgent{
				Name:     phase.Name,
				Children: children,
			}
		} else {
			phaseRunnable = &orchestrator.SequentialAgent{
				Name:     phase.Name,
				Children: children,
			}
		}

		phaseRunnables = append(phaseRunnables, phaseRunnable)
	}

	if len(phaseRunnables) == 0 {
		return &FuncRunnable{
			Name_: "empty-plan",
			Fn:    func(_ context.Context, _ string) (string, error) { return "(no sources planned)", nil },
		}
	}
	if len(phaseRunnables) == 1 {
		return phaseRunnables[0]
	}

	return &orchestrator.SequentialAgent{
		Name:     "plan",
		Children: phaseRunnables,
	}
}

// formatSourceResult renders a SourceResult as a readable text block
// suitable for Runnable output or downstream synthesis.
func formatSourceResult(r sources.SourceResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] status=%s\n", r.Source, r.Status)
	if r.Summary != "" {
		b.WriteString(r.Summary)
		b.WriteByte('\n')
	}
	for _, a := range r.Artifacts {
		fmt.Fprintf(&b, "- [%s] %s", a.Type, a.ID)
		if a.Snippet != "" {
			fmt.Fprintf(&b, ": %s", a.Snippet)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
