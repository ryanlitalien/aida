package orchestrator

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGraphDurablePauseResume runs a durable approval flow to a human
// pause, then resumes it with a BRAND-NEW Graph and Checkpointer pointing
// at the same directory - simulating a process restart. It asserts: the
// run pauses with the right prompt; state written before the pause
// survives the "restart"; the reply flows forward; and already-completed
// nodes do not re-run.
func TestGraphDurablePauseResume(t *testing.T) {
	dir := t.TempDir()
	const runID = "run-approval-1"

	var prepareRuns, finalizeRuns int32

	// build produces a fresh graph - process 1 and process 2 each get
	// their own, sharing nothing in memory.
	build := func() *Graph {
		return &Graph{
			Name: "approval-flow",
			Nodes: []Node{
				{Name: "prepare", Runnable: StateFunc(func(ctx context.Context, in string, st *State, opts ...RunOption) (*RunResult, error) {
					atomic.AddInt32(&prepareRuns, 1)
					st.Set("draft", "the-draft")
					return &RunResult{FinalOutput: "prepared", Turns: 1}, nil
				})},
				{Name: "approve", Runnable: RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
					return nil, RequestInput("approve the draft?")
				})},
				{Name: "finalize", Runnable: StateFunc(func(ctx context.Context, in string, st *State, opts ...RunOption) (*RunResult, error) {
					atomic.AddInt32(&finalizeRuns, 1)
					return &RunResult{FinalOutput: "reply=" + in + " draft=" + st.GetString("draft"), Turns: 1}, nil
				})},
			},
			Edges: []Edge{
				{From: StartNode, To: "prepare"},
				{From: "prepare", To: "approve"},
				{From: "approve", To: "finalize"},
			},
		}
	}

	// --- process 1: run until the human pause ---
	r1, err := build().RunDurable(context.Background(), runID, NewFileCheckpointer(dir), "start")
	if err != nil {
		t.Fatalf("RunDurable: %v", err)
	}
	if !r1.Paused {
		t.Fatal("expected the run to pause for approval")
	}
	if r1.Prompt != "approve the draft?" {
		t.Fatalf("wrong pause prompt: %q", r1.Prompt)
	}
	if n := atomic.LoadInt32(&prepareRuns); n != 1 {
		t.Fatalf("prepare should have run once before the pause, got %d", n)
	}

	// --- process 2: fresh graph + fresh checkpointer, same dir on disk ---
	r2, err := build().Resume(context.Background(), runID, NewFileCheckpointer(dir), "yes")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !r2.Done {
		t.Fatal("expected the run to complete after resume")
	}
	// finalize sees the human reply AND the draft restored from disk state.
	if r2.Output != "reply=yes draft=the-draft" {
		t.Fatalf("resume output wrong (state or handoff lost): %q", r2.Output)
	}
	if n := atomic.LoadInt32(&prepareRuns); n != 1 {
		t.Fatalf("prepare re-ran on resume (should resume from frontier): %d", n)
	}
	if n := atomic.LoadInt32(&finalizeRuns); n != 1 {
		t.Fatalf("finalize should have run exactly once, got %d", n)
	}
}

// TestGraphDurableGateBranch confirms a gate can route on the human's
// answer: "reject" flows to a different successor than "approve".
func TestGraphDurableGateBranch(t *testing.T) {
	cp := NewMemoryCheckpointer()
	const runID = "run-branch-1"

	build := func() *Graph {
		return &Graph{
			Name: "gated",
			Nodes: []Node{
				{Name: "gate", Runnable: RunnableFunc(func(ctx context.Context, in string, opts ...RunOption) (*RunResult, error) {
					return nil, RequestInput("approve?")
				}), Route: func(r *RunResult) string {
					if r.FinalOutput == "approve" {
						return "yes"
					}
					return "no"
				}},
				{Name: "ship", Runnable: echo("shipped")},
				{Name: "cancel", Runnable: echo("cancelled")},
			},
			Edges: []Edge{
				{From: StartNode, To: "gate"},
				{From: "gate", To: "ship", When: "yes"},
				{From: "gate", To: "cancel", When: "no"},
			},
		}
	}

	if _, err := build().RunDurable(context.Background(), runID, cp, "go"); err != nil {
		t.Fatalf("RunDurable: %v", err)
	}
	r, err := build().Resume(context.Background(), runID, cp, "reject")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// The reply "reject" routes to cancel AND flows in as its input, so
	// echo("cancelled") yields "reject>cancelled".
	if !r.Done || r.Output != "reject>cancelled" {
		t.Fatalf("gate should have branched to cancel on reject, got done=%v out=%q", r.Done, r.Output)
	}
}

// TestGraphDurableStraightThrough confirms a graph with no pauses runs to
// completion through the durable path and matches the concurrent Run.
func TestGraphDurableStraightThrough(t *testing.T) {
	build := func() *Graph {
		return &Graph{
			Name: "chain",
			Nodes: []Node{
				{Name: "a", Runnable: echo("a")},
				{Name: "b", Runnable: echo("b")},
				{Name: "c", Runnable: echo("c")},
			},
			Edges: []Edge{
				{From: StartNode, To: "a"},
				{From: "a", To: "b"},
				{From: "b", To: "c"},
			},
		}
	}
	r, err := build().RunDurable(context.Background(), "run-chain", NewMemoryCheckpointer(), "")
	if err != nil {
		t.Fatalf("RunDurable: %v", err)
	}
	if !r.Done || r.Output != "a>b>c" {
		t.Fatalf("durable chain wrong: done=%v out=%q", r.Done, r.Output)
	}
	if r.Result == nil || r.Result.Turns != 3 {
		t.Fatalf("telemetry rollup wrong: %+v", r.Result)
	}
}

// TestFileCheckpointerRoundTrip confirms the atomic file checkpointer
// persists and reloads a checkpoint, and reports ErrNoCheckpoint for a
// missing run.
func TestFileCheckpointerRoundTrip(t *testing.T) {
	cp := NewFileCheckpointer(t.TempDir())
	if _, err := cp.Load("missing"); err != ErrNoCheckpoint {
		t.Fatalf("expected ErrNoCheckpoint, got %v", err)
	}
	chk := &Checkpoint{RunID: "r1", GraphName: "g", Completed: map[string]string{"a": "out"}, State: map[string]any{"k": "v"}}
	if err := cp.Save("r1", chk); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := cp.Load("r1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Completed["a"] != "out" || got.State["k"] != "v" {
		t.Fatalf("round-trip lost data: %+v", got)
	}
}

// TestGraphDurableInitialStateReducer guards that RunDurable seeds
// InitialState through the reducers exactly like the concurrent Graph.Run,
// so the two paths don't diverge for reducer-backed keys.
func TestGraphDurableInitialStateReducer(t *testing.T) {
	build := func() *Graph {
		return &Graph{
			Name:          "seeded",
			StateReducers: map[string]Reducer{"log": AppendSlice},
			InitialState:  map[string]any{"log": "seed"},
			OutputKey:     "log",
			Nodes:         []Node{{Name: "n", Runnable: echo("n")}},
			Edges:         []Edge{{From: StartNode, To: "n"}},
		}
	}
	memRun, err := build().Run(context.Background(), "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	dur, err := build().RunDurable(context.Background(), "r", NewMemoryCheckpointer(), "")
	if err != nil {
		t.Fatalf("RunDurable: %v", err)
	}
	if dur.Output != memRun.FinalOutput {
		t.Fatalf("durable InitialState diverges from Run: durable=%q run=%q", dur.Output, memRun.FinalOutput)
	}
	if !strings.Contains(dur.Output, "seed") {
		t.Fatalf("reducer not applied to InitialState on durable path: %q", dur.Output)
	}
}
