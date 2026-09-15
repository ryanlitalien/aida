package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Runnable is anything the Runner or a composite can execute by
// handing it a string input and getting back a RunResult. *Agent is
// a Runnable via its Run method; SequentialAgent / ParallelAgent /
// LoopAgent are the composite Runnables.
type Runnable interface {
	Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error)
}

// Run on *Agent delegates to a default Runner. This lets an Agent be
// used anywhere a Runnable is expected - in particular, as a child of
// a composite.
func (a *Agent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	var r Runner
	return r.Run(ctx, a, input, opts...)
}

// ---------------------------------------------------------------------
// SequentialAgent: ADK's SequentialAgent. Runs children in order;
// each child's FinalOutput becomes the next child's input.
// ---------------------------------------------------------------------

// SequentialAgent runs child Runnables one after another. The output
// of child N becomes the input of child N+1, modeling ADK's
// SequentialAgent.
type SequentialAgent struct {
	Name     string
	Children []Runnable
}

// Run implements Runnable.
func (s *SequentialAgent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	if len(s.Children) == 0 {
		return nil, errors.New("sequential: no children")
	}
	aggregate := &RunResult{HandoffPath: []string{s.Name}}
	current := input
	for i, child := range s.Children {
		res, err := child.Run(ctx, current, opts...)
		if err != nil {
			return nil, fmt.Errorf("sequential[%d]: %w", i, err)
		}
		mergeInto(aggregate, res)
		current = res.FinalOutput
	}
	return aggregate, nil
}

// ---------------------------------------------------------------------
// ParallelAgent: ADK's ParallelAgent. Runs children concurrently with
// the same input. A caller-supplied Merge function combines the
// outputs; the default concatenates with a "---" separator.
// ---------------------------------------------------------------------

// ParallelAgent runs child Runnables concurrently and merges their
// outputs. If any child returns an error, the run fails and the
// first error seen is returned (other children's goroutines run to
// completion; if you want hard cancellation on first error, wrap
// children with an input guardrail that short-circuits on context
// cancellation).
type ParallelAgent struct {
	Name     string
	Children []Runnable
	// Merge combines the child results into a single string. If nil,
	// the default joins each child's FinalOutput with "\n---\n".
	Merge func(children []*RunResult) string
}

// Run implements Runnable.
func (p *ParallelAgent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	if len(p.Children) == 0 {
		return nil, errors.New("parallel: no children")
	}
	results := make([]*RunResult, len(p.Children))
	errs := make([]error, len(p.Children))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	for i, child := range p.Children {
		i, child := i, child
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := child.Run(ctx, input, opts...)
			results[i] = res
			errs[i] = err
			if err != nil {
				cancel()
			}
		}()
	}
	wg.Wait()

	for _, e := range errs {
		if e != nil {
			return nil, fmt.Errorf("parallel: %w", e)
		}
	}

	aggregate := &RunResult{HandoffPath: []string{p.Name}}
	for _, res := range results {
		mergeInto(aggregate, res)
	}
	merge := p.Merge
	if merge == nil {
		merge = defaultMerge
	}
	aggregate.FinalOutput = merge(results)
	if len(results) > 0 {
		aggregate.FinalAgent = results[0].FinalAgent
	}
	return aggregate, nil
}

func defaultMerge(children []*RunResult) string {
	parts := make([]string, 0, len(children))
	for _, c := range children {
		parts = append(parts, c.FinalOutput)
	}
	return strings.Join(parts, "\n---\n")
}

// ---------------------------------------------------------------------
// LoopAgent: ADK's LoopAgent. Runs a child repeatedly. The callback
// ShouldStop decides when to break; MaxIters is a hard cap.
// ---------------------------------------------------------------------

// LoopAgent runs Child repeatedly until ShouldStop returns true or
// MaxIters is reached. Each iteration takes the previous iteration's
// FinalOutput as its input.
type LoopAgent struct {
	Name     string
	Child    Runnable
	MaxIters int
	// ShouldStop is called after each iteration with the latest
	// RunResult. Returning true ends the loop. If nil, the loop runs
	// to MaxIters.
	ShouldStop func(res *RunResult) bool
}

// Run implements Runnable.
func (l *LoopAgent) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	if l.Child == nil {
		return nil, errors.New("loop: child is nil")
	}
	maxIters := l.MaxIters
	if maxIters <= 0 {
		return nil, errors.New("loop: MaxIters must be positive")
	}
	aggregate := &RunResult{HandoffPath: []string{l.Name}}
	current := input
	var last *RunResult
	for i := 0; i < maxIters; i++ {
		res, err := l.Child.Run(ctx, current, opts...)
		if err != nil {
			return nil, fmt.Errorf("loop[%d]: %w", i, err)
		}
		mergeInto(aggregate, res)
		last = res
		current = res.FinalOutput
		if l.ShouldStop != nil && l.ShouldStop(res) {
			break
		}
	}
	if last != nil {
		aggregate.FinalOutput = last.FinalOutput
		aggregate.FinalAgent = last.FinalAgent
	}
	return aggregate, nil
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// mergeInto accumulates a child RunResult into the aggregate result.
// Composites use it to roll up tool calls, usage, turns, and the
// handoff path.
func mergeInto(agg, child *RunResult) {
	if agg == nil || child == nil {
		return
	}
	agg.Turns += child.Turns
	agg.Usage.InputTokens += child.Usage.InputTokens
	agg.Usage.OutputTokens += child.Usage.OutputTokens
	agg.Usage.CacheReadTokens += child.Usage.CacheReadTokens
	agg.Usage.CacheWriteTokens += child.Usage.CacheWriteTokens
	agg.CostUSD += child.CostUSD
	agg.ToolCalls = append(agg.ToolCalls, child.ToolCalls...)
	agg.HandoffPath = append(agg.HandoffPath, child.HandoffPath...)
	agg.History = append(agg.History, child.History...)
	// FinalOutput/FinalAgent/Termination/Result are set by the caller
	// based on its own composition rule -- the last child to run wins,
	// same as FinalOutput always has.
	agg.FinalOutput = child.FinalOutput
	agg.FinalAgent = child.FinalAgent
	agg.Termination = child.Termination
	agg.Result = child.Result
}
