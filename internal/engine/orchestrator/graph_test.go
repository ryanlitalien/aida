package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// echo builds a RunnableFunc node that appends a tag to its input, so a
// path through the graph is visible in the final output.
func echo(tag string) Runnable {
	return RunnableFunc(func(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
		out := input
		if out != "" {
			out += ">"
		}
		return &RunResult{FinalOutput: out + tag, Turns: 1}, nil
	})
}

// TestGraphConditionalRouting drives a classifier node that routes to one
// of two specialists by emitting a label, then to a shared terminal. Only
// the routed branch should run.
func TestGraphConditionalRouting(t *testing.T) {
	classify := RunnableFunc(func(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
		return &RunResult{FinalOutput: input, Turns: 1}, nil
	})

	g := &Graph{
		Name: "router",
		Nodes: []Node{
			{Name: "classify", Runnable: classify, Route: func(r *RunResult) string {
				if strings.Contains(r.FinalOutput, "refund") {
					return "billing"
				}
				return "tech"
			}},
			{Name: "billing", Runnable: echo("billing")},
			{Name: "tech", Runnable: echo("tech")},
		},
		Edges: []Edge{
			{From: StartNode, To: "classify"},
			{From: "classify", To: "billing", When: "billing"},
			{From: "classify", To: "tech", When: "tech"},
		},
	}

	res, err := g.Run(context.Background(), "please process my refund", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.FinalOutput; got != "please process my refund>billing" {
		t.Fatalf("routed to wrong branch: %q", got)
	}
	// The classify + billing nodes ran (2 turns); tech did not.
	if res.Turns != 2 {
		t.Fatalf("expected 2 turns (classify+billing), got %d", res.Turns)
	}
}

// TestGraphFanOutFanIn splits to two workers concurrently and joins their
// outputs at a barrier node.
func TestGraphFanOutFanIn(t *testing.T) {
	g := &Graph{
		Name: "diamond",
		Nodes: []Node{
			{Name: "split", Runnable: echo("split")},
			{Name: "a", Runnable: echo("a")},
			{Name: "b", Runnable: echo("b")},
			{Name: "join", Runnable: echo("done"), Join: true, Merge: func(in []string) string {
				return strings.Join(in, "+")
			}},
		},
		Edges: []Edge{
			{From: StartNode, To: "split"},
			{From: "split", To: "a"},
			{From: "split", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
		},
	}

	res, err := g.Run(context.Background(), "", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// join receives "split>a" and "split>b" (order nondeterministic), merges
	// with "+", then appends ">done". Assert both branches are present.
	if !strings.Contains(res.FinalOutput, "split>a") || !strings.Contains(res.FinalOutput, "split>b") {
		t.Fatalf("join missing a branch: %q", res.FinalOutput)
	}
	if !strings.HasSuffix(res.FinalOutput, ">done") {
		t.Fatalf("join node did not run last: %q", res.FinalOutput)
	}
	// split + a + b + join = 4 node runs.
	if res.Turns != 4 {
		t.Fatalf("expected 4 turns, got %d", res.Turns)
	}
}

// TestGraphEvaluatorOptimizerLoop proves cycles *work*, not just that they
// are guarded: an evaluator routes "revise" back to a generator until the
// draft is good enough, then "done" forward. This is the canonical
// self-correcting agent loop and requires a cyclic graph.
func TestGraphEvaluatorOptimizerLoop(t *testing.T) {
	var genCount int32
	generate := RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
		atomic.AddInt32(&genCount, 1)
		return &RunResult{FinalOutput: in + "*", Turns: 1}, nil // add one "star" per revision
	})
	evaluate := RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
		return &RunResult{FinalOutput: in, Turns: 1}, nil
	})

	g := &Graph{
		Name: "eval-opt",
		Nodes: []Node{
			{Name: "generate", Runnable: generate, MaxVisits: 10},
			{Name: "evaluate", Runnable: evaluate, Route: func(r *RunResult) string {
				if strings.Count(r.FinalOutput, "*") >= 3 {
					return "done"
				}
				return "revise"
			}},
			{Name: "final", Runnable: echo("final")},
		},
		Edges: []Edge{
			{From: StartNode, To: "generate"},
			{From: "generate", To: "evaluate"},
			{From: "evaluate", To: "generate", When: "revise"}, // back-edge = cycle
			{From: "evaluate", To: "final", When: "done"},
		},
	}

	res, err := g.Run(context.Background(), "", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := atomic.LoadInt32(&genCount); n != 3 {
		t.Fatalf("expected generator to run 3x (converge at 3 stars), got %d", n)
	}
	if res.FinalOutput != "***>final" {
		t.Fatalf("expected converged draft to flow to final, got %q", res.FinalOutput)
	}
}

// TestGraphMaxVisitsError confirms a non-converging cycle fails loudly at
// the per-node visit budget rather than spinning forever.
func TestGraphMaxVisitsError(t *testing.T) {
	g := &Graph{
		Name: "runaway",
		Nodes: []Node{
			{Name: "generate", Runnable: echo("g"), MaxVisits: 3},
			{Name: "evaluate", Runnable: echo("e"), MaxVisits: 100,
				Route: func(r *RunResult) string { return "revise" }}, // never converges
		},
		Edges: []Edge{
			{From: StartNode, To: "generate"},
			{From: "generate", To: "evaluate"},
			{From: "evaluate", To: "generate", When: "revise"},
		},
	}
	_, err := g.Run(context.Background(), "", nil...)
	if err == nil {
		t.Fatal("expected MaxVisits error on non-converging cycle")
	}
	if !strings.Contains(err.Error(), "MaxVisits") {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestGraphGlobalActivationBackstop confirms the global MaxActivations cap
// catches pathological graphs.
func TestGraphGlobalActivationBackstop(t *testing.T) {
	g := &Graph{
		Name:           "loop",
		MaxActivations: 5,
		Nodes: []Node{
			{Name: "a", Runnable: echo("a"), MaxVisits: 1000},
			{Name: "b", Runnable: echo("b"), MaxVisits: 1000},
		},
		Edges: []Edge{
			{From: StartNode, To: "a"},
			{From: "a", To: "b"},
			{From: "b", To: "a"},
		},
	}
	if _, err := g.Run(context.Background(), "", nil...); err == nil {
		t.Fatal("expected MaxActivations error")
	}
}

// TestGraphNodeRetry confirms a flaky node is retried per its RetryPolicy.
func TestGraphNodeRetry(t *testing.T) {
	var attempts int32
	flaky := RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
		if atomic.AddInt32(&attempts, 1) < 3 {
			return nil, errors.New("transient")
		}
		return &RunResult{FinalOutput: "ok", Turns: 1}, nil
	})
	g := &Graph{
		Name: "retry",
		Nodes: []Node{
			{Name: "flaky", Runnable: flaky, Retry: &RetryPolicy{Attempts: 3, Backoff: time.Millisecond}},
		},
		Edges: []Edge{{From: StartNode, To: "flaky"}},
	}
	res, err := g.Run(context.Background(), "", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalOutput != "ok" {
		t.Fatalf("got %q", res.FinalOutput)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Fatalf("expected 3 attempts, got %d", n)
	}
}

// TestGraphNodeTimeout confirms a per-node timeout bounds a slow node.
func TestGraphNodeTimeout(t *testing.T) {
	slow := RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			return &RunResult{FinalOutput: "too slow"}, nil
		}
	})
	g := &Graph{
		Name:  "timeout",
		Nodes: []Node{{Name: "slow", Runnable: slow, Timeout: 20 * time.Millisecond}},
		Edges: []Edge{{From: StartNode, To: "slow"}},
	}
	if _, err := g.Run(context.Background(), "", nil...); err == nil {
		t.Fatal("expected timeout error")
	}
}

// TestGraphStateReducerMerge fans out to three stateful workers that each
// write their tag into one "results" key with the AppendSlice reducer, so
// parallel writes accumulate instead of clobbering. OutputKey projects the
// merged slice as the graph's result.
func TestGraphStateReducerMerge(t *testing.T) {
	worker := func(tag string) StateFunc {
		return StateFunc(func(ctx context.Context, in string, st *State, opts ...RunOption) (*RunResult, error) {
			st.Set("results", tag)
			return &RunResult{FinalOutput: tag, Turns: 1}, nil
		})
	}
	g := &Graph{
		Name:          "collect",
		StateReducers: map[string]Reducer{"results": AppendSlice},
		OutputKey:     "results",
		Nodes: []Node{
			{Name: "split", Runnable: echo("split")},
			{Name: "a", Runnable: worker("a")},
			{Name: "b", Runnable: worker("b")},
			{Name: "c", Runnable: worker("c")},
			{Name: "join", Runnable: echo("done"), Join: true},
		},
		Edges: []Edge{
			{From: StartNode, To: "split"},
			{From: "split", To: "a"},
			{From: "split", To: "b"},
			{From: "split", To: "c"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "c", To: "join"},
		},
	}
	res, err := g.Run(context.Background(), "", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// OutputKey projects []any{a,b,c} (order nondeterministic) as a string.
	for _, tag := range []string{"a", "b", "c"} {
		if !strings.Contains(res.FinalOutput, tag) {
			t.Fatalf("reducer dropped a parallel write %q: %q", tag, res.FinalOutput)
		}
	}
}

// TestGraphStateRoute confirms a RouteState decision can read what an
// upstream node wrote to shared state.
func TestGraphStateRoute(t *testing.T) {
	decide := StateFunc(func(ctx context.Context, in string, st *State, opts ...RunOption) (*RunResult, error) {
		st.Set("intent", "billing")
		return &RunResult{FinalOutput: in, Turns: 1}, nil
	})
	g := &Graph{
		Name: "state-route",
		Nodes: []Node{
			{Name: "decide", Runnable: decide, RouteState: func(r *RunResult, st *State) string {
				return st.GetString("intent")
			}},
			{Name: "billing", Runnable: echo("billing")},
			{Name: "tech", Runnable: echo("tech")},
		},
		Edges: []Edge{
			{From: StartNode, To: "decide"},
			{From: "decide", To: "billing", When: "billing"},
			{From: "decide", To: "tech", When: "tech"},
		},
	}
	res, err := g.Run(context.Background(), "", nil...)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.FinalOutput != "billing" {
		t.Fatalf("state-driven route went wrong: %q", res.FinalOutput)
	}
}

// TestStateReducerDefaults confirms LastWrite is the default and Snapshot
// copies values.
func TestStateReducerDefaults(t *testing.T) {
	st := NewState(nil)
	st.Set("k", "first")
	st.Set("k", "second") // LastWrite default: overwrite
	if got := st.GetString("k"); got != "second" {
		t.Fatalf("LastWrite default failed: %q", got)
	}
	snap := st.Snapshot()
	if snap["k"] != "second" {
		t.Fatalf("snapshot missing value: %v", snap)
	}
	st.Set("k", "third")
	if snap["k"] != "second" {
		t.Fatal("snapshot must be a copy, not a live view")
	}
}

// TestGraphConditionalEdgeIntoJoinRejected guards the fix for a Join whose
// predecessor edge is conditional: such a join could silently never fire,
// so construction must reject it.
func TestGraphConditionalEdgeIntoJoinRejected(t *testing.T) {
	g := &Graph{
		Name: "bad-join",
		Nodes: []Node{
			{Name: "a", Runnable: echo("a"), Route: func(r *RunResult) string { return "x" }},
			{Name: "j", Runnable: echo("j"), Join: true},
		},
		Edges: []Edge{
			{From: StartNode, To: "a"},
			{From: "a", To: "j", When: "x"}, // conditional edge into a Join
		},
	}
	_, err := g.Run(context.Background(), "", nil...)
	if err == nil || !strings.Contains(err.Error(), "Join") {
		t.Fatalf("expected conditional-into-Join rejection, got %v", err)
	}
}
