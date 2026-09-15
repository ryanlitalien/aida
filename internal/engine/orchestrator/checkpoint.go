package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// This file adds durable, resumable graph execution - the ADK Go 2.0
// headline (a node pauses, the run persists, and resumes across process
// restarts) and LangGraph's checkpointer + interrupt/resume model.
//
// It is a second execution path alongside Graph.Run. Where Run is a
// concurrent, in-memory scheduler optimized for throughput, RunDurable
// is a sequential superstep executor optimized for durability: it
// checkpoints after every node (LangGraph's "sync" durability mode) so a
// crash or a human-in-the-loop pause resumes from the last completed
// node. Human-in-the-loop graphs don't need concurrency; they need to
// survive a restart, which is exactly the trade this path makes.
//
// The atomic-write checkpointer mirrors Aida's internal/jobs manifest
// pattern (temp file -> fsync -> rename) without importing that package,
// keeping the orchestrator extractable.

// RequestInputError is returned by a node's Runnable to pause the durable
// run and wait for human input. The durable executor checkpoints and
// returns; Graph.Resume then treats the human's reply as the paused gate
// node's output and routes it forward (handoff mode). This is our analog
// of LangGraph's interrupt().
type RequestInputError struct {
	Prompt string
}

// Error implements error.
func (e *RequestInputError) Error() string { return "graph: request input: " + e.Prompt }

// RequestInput builds the pause sentinel a node returns to request human
// input with the given prompt.
func RequestInput(prompt string) error { return &RequestInputError{Prompt: prompt} }

// activation is one pending (node, input) pair on the durable frontier.
type activation struct {
	Node  string `json:"node"`
	Input string `json:"input"`
}

// pauseInfo records where a durable run paused for human input.
type pauseInfo struct {
	Node   string `json:"node"`
	Prompt string `json:"prompt"`
}

// Checkpoint is the serializable snapshot of a durable graph run. It
// holds everything needed to resume: the pending frontier, per-node
// completion outputs, cycle visit counts, join accumulation, the shared
// State values, rolled-up telemetry, and any pause marker. It carries no
// live objects (no *Agent, no model.LLM), so it round-trips through JSON.
// State values must be JSON-serializable for durability across restarts.
type Checkpoint struct {
	RunID     string              `json:"run_id"`
	GraphName string              `json:"graph_name"`
	Frontier  []activation        `json:"frontier"`
	Completed map[string]string   `json:"completed"`
	Visits    map[string]int      `json:"visits"`
	JoinBuf   map[string][]string `json:"join_buf"`
	JoinLeft  map[string]int      `json:"join_left"`
	State     map[string]any      `json:"state"`
	Terminals map[string]string   `json:"terminals"`

	// Rolled-up telemetry (a reduced RunResult without unserializable
	// fields like FinalAgent). model.Usage is four plain ints, so it
	// serializes directly.
	Turns       int              `json:"turns"`
	Usage       model.Usage      `json:"usage"`
	ToolCalls   []ToolCallRecord `json:"tool_calls"`
	HandoffPath []string         `json:"handoff_path"`

	Pause *pauseInfo `json:"pause,omitempty"`
	Done  bool       `json:"done"`
}

// DurableResult is what RunDurable / Resume return: either a pause
// awaiting input, or a completed run with its output and telemetry.
type DurableResult struct {
	RunID  string
	Paused bool
	Prompt string // set when Paused
	Done   bool
	Output string
	Result *RunResult // rolled-up telemetry when Done
}

// Checkpointer persists and loads durable-run checkpoints keyed by runID.
type Checkpointer interface {
	Save(runID string, chk *Checkpoint) error
	Load(runID string) (*Checkpoint, error)
}

// ErrNoCheckpoint is returned by a Checkpointer.Load when no checkpoint
// exists for the runID.
var ErrNoCheckpoint = errors.New("orchestrator: no checkpoint for run")

// MemoryCheckpointer is an in-memory Checkpointer for tests and
// single-process use.
type MemoryCheckpointer struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMemoryCheckpointer builds an empty in-memory checkpointer.
func NewMemoryCheckpointer() *MemoryCheckpointer {
	return &MemoryCheckpointer{data: map[string][]byte{}}
}

// Save implements Checkpointer.
func (m *MemoryCheckpointer) Save(runID string, chk *Checkpoint) error {
	b, err := json.Marshal(chk)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[runID] = b
	return nil
}

// Load implements Checkpointer.
func (m *MemoryCheckpointer) Load(runID string) (*Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[runID]
	if !ok {
		return nil, ErrNoCheckpoint
	}
	var chk Checkpoint
	if err := json.Unmarshal(b, &chk); err != nil {
		return nil, err
	}
	return &chk, nil
}

// FileCheckpointer persists checkpoints under Dir/<runID>/checkpoint.json
// with an atomic write (temp file -> fsync -> rename), mirroring the
// crash-safe pattern Aida's internal/jobs uses for run manifests. The
// caller supplies Dir (e.g. ~/.aida/graphs/<profile>) so this package
// stays free of Aida config coupling.
type FileCheckpointer struct {
	Dir string
}

// NewFileCheckpointer builds a FileCheckpointer rooted at dir.
func NewFileCheckpointer(dir string) *FileCheckpointer { return &FileCheckpointer{Dir: dir} }

func (f *FileCheckpointer) path(runID string) string {
	return filepath.Join(f.Dir, runID, "checkpoint.json")
}

// Save implements Checkpointer with an atomic temp-file-then-rename write.
func (f *FileCheckpointer) Save(runID string, chk *Checkpoint) error {
	p := f.path(runID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(chk, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "checkpoint-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// Load implements Checkpointer.
func (f *FileCheckpointer) Load(runID string) (*Checkpoint, error) {
	b, err := os.ReadFile(f.path(runID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoCheckpoint
		}
		return nil, err
	}
	var chk Checkpoint
	if err := json.Unmarshal(b, &chk); err != nil {
		return nil, err
	}
	return &chk, nil
}

// RunDurable starts a durable, resumable execution of the graph under
// runID, checkpointing progress to cp after every node. If a node returns
// a RequestInputError, the run persists and returns with Paused set;
// call Resume(runID, humanInput) to continue.
func (g *Graph) RunDurable(ctx context.Context, runID string, cp Checkpointer, input string, opts ...RunOption) (*DurableResult, error) {
	byName, succ, predCount, err := g.index()
	if err != nil {
		return nil, err
	}

	chk := &Checkpoint{
		RunID:     runID,
		GraphName: g.Name,
		Completed: map[string]string{},
		Visits:    map[string]int{},
		JoinBuf:   map[string][]string{},
		JoinLeft:  map[string]int{},
		State:     map[string]any{},
		Terminals: map[string]string{},
	}
	// Seed shared state through the reducers so the durable path matches
	// Graph.Run's in-memory seeding for reducer-backed keys.
	seed := NewState(g.StateReducers)
	for k, v := range g.InitialState {
		seed.Set(k, v)
	}
	chk.State = seed.Snapshot()
	for name, n := range byName {
		if n.Join {
			chk.JoinLeft[name] = predCount[name]
		}
	}
	// Seed the frontier from the start edges.
	for _, e := range succ[StartNode] {
		chk.Frontier = append(chk.Frontier, activation{Node: e.To, Input: input})
	}

	return g.driveDurable(ctx, cp, byName, succ, chk, opts)
}

// Resume reloads a paused run's checkpoint and continues it, delivering
// humanInput to the node that paused.
func (g *Graph) Resume(ctx context.Context, runID string, cp Checkpointer, humanInput string, opts ...RunOption) (*DurableResult, error) {
	byName, succ, _, err := g.index()
	if err != nil {
		return nil, err
	}
	chk, err := cp.Load(runID)
	if err != nil {
		return nil, err
	}
	if chk.Done {
		return nil, fmt.Errorf("orchestrator: run %q already complete", runID)
	}
	if chk.Pause == nil {
		return nil, fmt.Errorf("orchestrator: run %q is not paused", runID)
	}

	// Handoff mode (ADK's term): the human's reply becomes the paused
	// gate node's output and flows to its successors. The gate does not
	// re-run - otherwise a node that pauses unconditionally would pause
	// again forever. The gate's Route still applies, so a gate can
	// branch on the answer ("approve" vs "reject").
	paused := chk.Pause.Node
	node := byName[paused]
	chk.Pause = nil
	chk.Completed[paused] = humanInput

	state := NewState(g.StateReducers)
	state.restore(chk.State)
	res := &RunResult{FinalOutput: humanInput}

	outs := succ[paused]
	if len(outs) == 0 {
		chk.Terminals[paused] = humanInput
	}
	for _, e := range firingSuccessors(node, res, state, outs) {
		chk.Frontier = append(chk.Frontier, activation{Node: e.To, Input: humanInput})
	}
	return g.driveDurable(ctx, cp, byName, succ, chk, opts)
}

// driveDurable is the sequential superstep loop shared by RunDurable and
// Resume. It drains the frontier one node at a time, checkpointing after
// each, until the frontier empties, a node pauses, or an error occurs.
func (g *Graph) driveDurable(
	ctx context.Context,
	cp Checkpointer,
	byName map[string]Node,
	succ map[string][]Edge,
	chk *Checkpoint,
	opts []RunOption,
) (*DurableResult, error) {
	state := NewState(g.StateReducers)
	state.restore(chk.State)

	for len(chk.Frontier) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		act := chk.Frontier[0]
		chk.Frontier = chk.Frontier[1:]
		node := byName[act.Node]
		in := act.Input

		// Join barrier: accumulate inputs until all predecessors arrive.
		if node.Join {
			chk.JoinBuf[act.Node] = append(chk.JoinBuf[act.Node], in)
			chk.JoinLeft[act.Node]--
			if chk.JoinLeft[act.Node] > 0 {
				if err := cp.Save(chk.RunID, chk); err != nil {
					return nil, err
				}
				continue
			}
			in = mergeInputs(node, chk.JoinBuf[act.Node])
		}

		// Cycle bound.
		chk.Visits[act.Node]++
		if chk.Visits[act.Node] > g.visitCap(node) {
			return nil, fmt.Errorf("graph %q: node %q exceeded MaxVisits (%d) - non-converging cycle?", g.Name, act.Node, g.visitCap(node))
		}

		res, rerr := runNode(ctx, node, in, state, opts)
		if rerr != nil {
			var reqErr *RequestInputError
			if errors.As(rerr, &reqErr) {
				// Pause: persist and wait for human input.
				chk.Pause = &pauseInfo{Node: act.Node, Prompt: reqErr.Prompt}
				chk.State = state.Snapshot()
				if err := cp.Save(chk.RunID, chk); err != nil {
					return nil, err
				}
				return &DurableResult{RunID: chk.RunID, Paused: true, Prompt: reqErr.Prompt}, nil
			}
			return nil, fmt.Errorf("graph %q node %q: %w", g.Name, act.Node, rerr)
		}

		// Record completion + roll up telemetry.
		chk.Completed[act.Node] = res.FinalOutput
		accumulate(chk, res)

		outs := succ[act.Node]
		if len(outs) == 0 {
			chk.Terminals[act.Node] = res.FinalOutput
		}

		// Route to successors (shared with Graph.Run via firingSuccessors).
		for _, e := range firingSuccessors(node, res, state, outs) {
			chk.Frontier = append(chk.Frontier, activation{Node: e.To, Input: res.FinalOutput})
		}

		// Checkpoint after each node (sync durability).
		chk.State = state.Snapshot()
		if err := cp.Save(chk.RunID, chk); err != nil {
			return nil, err
		}
	}

	// Frontier drained - the run is complete.
	chk.Done = true
	chk.State = state.Snapshot()
	output := joinTerminals(chk.Terminals)
	if g.OutputKey != "" {
		output = state.GetString(g.OutputKey)
	}
	if err := cp.Save(chk.RunID, chk); err != nil {
		return nil, err
	}
	return &DurableResult{RunID: chk.RunID, Done: true, Output: output, Result: resultFromCheckpoint(chk, output)}, nil
}

// accumulate rolls a node's result into the checkpoint's running totals.
func accumulate(chk *Checkpoint, res *RunResult) {
	if res == nil {
		return
	}
	chk.Turns += res.Turns
	chk.Usage.InputTokens += res.Usage.InputTokens
	chk.Usage.OutputTokens += res.Usage.OutputTokens
	chk.Usage.CacheReadTokens += res.Usage.CacheReadTokens
	chk.Usage.CacheWriteTokens += res.Usage.CacheWriteTokens
	chk.ToolCalls = append(chk.ToolCalls, res.ToolCalls...)
	chk.HandoffPath = append(chk.HandoffPath, res.HandoffPath...)
}

// resultFromCheckpoint reconstructs a RunResult from the checkpoint's
// rolled-up telemetry (FinalAgent and History are not persisted).
func resultFromCheckpoint(chk *Checkpoint, output string) *RunResult {
	return &RunResult{
		FinalOutput: output,
		Turns:       chk.Turns,
		ToolCalls:   chk.ToolCalls,
		Usage:       chk.Usage,
		HandoffPath: chk.HandoffPath,
	}
}
