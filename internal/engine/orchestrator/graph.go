package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file implements a directed-graph workflow layer over the existing
// composite agents. It is a provider-agnostic analog of Google ADK Go
// 2.0's `workflow` package (nodes + routed edges + a scheduler) and of
// LangGraph's StateGraph, but with three deliberate departures:
//
//   - Nodes are plain Runnables (string in, *RunResult out). Every
//     Agent, SequentialAgent, ParallelAgent, LoopAgent, and nested
//     Graph is already a Runnable, so it becomes a node with zero
//     changes - no genai.Content lingua franca, no adapter tax.
//   - A Graph is itself a Runnable, so graphs nest inside composites
//     and composites nest inside graphs. This is the "unified runtime"
//     idea: one interface for single agents, composites, and graphs.
//   - Routing is expressed by a node emitting a label from its result
//     (Node.Route) and edges matching that label (Edge.When) - the same
//     shape as ADK's node-emits-route / StringRoute+Default edges, but
//     evaluated against our synchronous *RunResult.
//
// Supported today: linear chains, conditional routing, fan-out
// (concurrent successors), fan-in via Join barrier nodes, and bounded
// CYCLES. Cycles matter because the evaluator-optimizer / reflection
// pattern (generate → critique → revise until good enough) is the
// canonical self-correcting agent loop, and it is a cycle: an evaluator
// node routes "revise" back to a generator, or "done" forward. Each
// node carries a visit budget (Node.MaxVisits / Graph.DefaultMaxVisits)
// so a non-converging loop fails loudly rather than spinning. Per-node
// Retry and Timeout policies wrap a node's Runnable.
//
// A shared typed state channel (see state.go) is threaded to any node
// whose Runnable implements StatefulRunnable, with per-key reducers
// merging concurrent writes - the LangGraph state model, added
// additively so existing agents are unaffected.
//
// Deferred (see docs/plan-orchestrator-graph-layer.md): durable
// checkpoint/resume for human-in-the-loop across process restarts
// (Phase 3). Join semantics inside a cycle are undefined for now - the
// evaluator-optimizer loop has no join in the cycle, which is the shape
// we target.

// RunnableFunc adapts a plain function to a Runnable so arbitrary Go -
// including data-dependent branching that itself calls other Runnables -
// can be a graph node. This is our analog of ADK 2.0's dynamic node.
type RunnableFunc func(ctx context.Context, input string, opts ...RunOption) (*RunResult, error)

// Run implements Runnable.
func (f RunnableFunc) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	return f(ctx, input, opts...)
}

// RetryPolicy configures per-node retry with exponential backoff. It
// mirrors ADK 2.0's per-node retry and LangGraph's RetryPolicy.
type RetryPolicy struct {
	// Attempts is the total number of attempts including the first.
	// Values <= 1 mean no retry.
	Attempts int
	// Backoff is the base delay between attempts; it doubles each retry
	// (attempt 0→Backoff, 1→2×, 2→4×, …). Zero defaults to 100ms.
	Backoff time.Duration
	// MaxBackoff caps the computed delay. Zero means uncapped.
	MaxBackoff time.Duration
}

// Node is one vertex in a Graph. It wraps any Runnable - an Agent, a
// composite, a nested Graph, or a RunnableFunc - so everything already
// built plugs in unchanged.
type Node struct {
	// Name is the node's stable identifier, referenced by Edge.From /
	// Edge.To and used in traces and the aggregate HandoffPath.
	Name string

	// Runnable is what the node executes. Required.
	Runnable Runnable

	// Route optionally inspects this node's result and returns a routing
	// label. Outgoing edges whose When equals the label are taken; a
	// DefaultRoute edge is taken only when no labeled edge matched. If
	// Route is nil the node is linear: all unconditional outgoing edges
	// fire. This mirrors ADK's node-emits-route + StringRoute/Default.
	Route func(res *RunResult) string

	// RouteState is like Route but also receives the graph's shared
	// State, so a routing decision can read what upstream nodes wrote
	// (e.g. an evaluator that stored an iteration count). When set it
	// takes precedence over Route.
	RouteState func(res *RunResult, state *State) string

	// Join marks the node as a fan-in barrier: the scheduler waits until
	// every predecessor has delivered, merges their outputs into this
	// node's input, then runs it once. Without Join, a node runs once
	// per firing predecessor. Mirrors ADK's JoinNode.
	Join bool

	// Merge combines predecessor outputs when Join is true. nil joins
	// them with "\n---\n", matching ParallelAgent's defaultMerge.
	Merge func(inputs []string) string

	// MaxVisits caps how many times this node may run in one graph
	// execution. It bounds cycles: an evaluator-optimizer loop that
	// never converges fails at the cap instead of spinning forever.
	// Zero falls back to Graph.DefaultMaxVisits.
	MaxVisits int

	// Retry, if set, retries the node's Runnable on error per the policy.
	Retry *RetryPolicy

	// Timeout, if > 0, bounds each attempt of the node with a context
	// deadline. On timeout the attempt errors (and may be retried).
	Timeout time.Duration
}

// Edge is a directed connection between two nodes by name.
type Edge struct {
	// From is the source node name, or StartNode for an entry edge.
	From string
	// To is the target node name.
	To string
	// When is the routing label this edge requires. "" is unconditional.
	// DefaultRoute fires only when the source emitted a label that no
	// other edge matched.
	When string
}

const (
	// StartNode is the sentinel source for entry edges. The graph input
	// is delivered to every node that StartNode points to.
	StartNode = "__start__"

	// DefaultRoute on Edge.When fires when the source node emitted a
	// label that matched no labeled outgoing edge.
	DefaultRoute = "__default__"

	// defaultMaxVisits bounds cycles when neither Node.MaxVisits nor
	// Graph.DefaultMaxVisits is set.
	defaultMaxVisits = 25
)

// Graph is a directed multi-agent workflow: named Nodes connected by
// routed Edges. It closes the expressiveness gap that Sequential /
// Parallel / Loop leave open - arbitrary graphs (including cycles),
// conditional routing, and fan-out/fan-in - while remaining a Runnable,
// so it composes with them in both directions.
type Graph struct {
	// Name labels the graph in traces and the aggregate HandoffPath.
	Name string

	// Nodes are the graph's vertices. Names must be unique.
	Nodes []Node

	// Edges connect nodes by name. At least one edge must originate at
	// StartNode.
	Edges []Edge

	// MaxConcurrency caps concurrently-running nodes. 0 means unlimited.
	MaxConcurrency int

	// DefaultMaxVisits is the per-node visit cap when Node.MaxVisits is
	// 0. 0 defaults to defaultMaxVisits (25). Bounds cycles.
	DefaultMaxVisits int

	// MaxActivations is a global hard backstop on total node runs across
	// the whole graph - a catch-all for pathological graphs that the
	// per-node budget somehow misses. 0 defaults to 1000.
	MaxActivations int

	// StateReducers registers per-key reducers for the run's shared
	// State (see state.go). Keys without a reducer overwrite on write.
	// nil means every key uses LastWrite semantics.
	StateReducers map[string]Reducer

	// InitialState seeds the shared State before the graph runs. Values
	// are set through their reducers.
	InitialState map[string]any

	// OutputKey, if set, projects the graph's FinalOutput from this
	// State key instead of joining terminal node outputs. Use it when
	// the meaningful result is accumulated in state rather than returned
	// by a single terminal node.
	OutputKey string
}

// visitCap returns the effective per-run visit budget for a node.
func (g *Graph) visitCap(n Node) int {
	if n.MaxVisits > 0 {
		return n.MaxVisits
	}
	if g.DefaultMaxVisits > 0 {
		return g.DefaultMaxVisits
	}
	return defaultMaxVisits
}

// index builds the lookups the scheduler needs and validates structure.
func (g *Graph) index() (map[string]Node, map[string][]Edge, map[string]int, error) {
	byName := make(map[string]Node, len(g.Nodes))
	for _, n := range g.Nodes {
		if n.Name == "" {
			return nil, nil, nil, fmt.Errorf("graph %q: node with empty name", g.Name)
		}
		if n.Name == StartNode {
			return nil, nil, nil, fmt.Errorf("graph %q: node name %q is reserved", g.Name, StartNode)
		}
		if _, dup := byName[n.Name]; dup {
			return nil, nil, nil, fmt.Errorf("graph %q: duplicate node %q", g.Name, n.Name)
		}
		if n.Runnable == nil {
			return nil, nil, nil, fmt.Errorf("graph %q: node %q has nil Runnable", g.Name, n.Name)
		}
		byName[n.Name] = n
	}

	succ := make(map[string][]Edge)
	predCount := make(map[string]int)
	var startEdges int
	for _, e := range g.Edges {
		if e.From != StartNode {
			if _, ok := byName[e.From]; !ok {
				return nil, nil, nil, fmt.Errorf("graph %q: edge from unknown node %q", g.Name, e.From)
			}
		} else {
			startEdges++
		}
		if _, ok := byName[e.To]; !ok {
			return nil, nil, nil, fmt.Errorf("graph %q: edge to unknown node %q", g.Name, e.To)
		}
		// A Join barrier waits for every incoming edge to deliver, but a
		// conditional edge (When != "") may never fire - which would leave
		// the join stuck below its threshold and silently skip its whole
		// downstream. Reject that at construction rather than lose a subtree
		// at runtime. (Dead-path propagation for conditional fan-in is a
		// larger design change; until then, keep join predecessors
		// unconditional.)
		if e.When != "" && byName[e.To].Join {
			return nil, nil, nil, fmt.Errorf("graph %q: conditional edge %q->%q into Join node not supported (join predecessors must be unconditional)", g.Name, e.From, e.To)
		}
		succ[e.From] = append(succ[e.From], e)
		predCount[e.To]++
	}
	if startEdges == 0 {
		return nil, nil, nil, fmt.Errorf("graph %q: no edge originates at StartNode", g.Name)
	}
	return byName, succ, predCount, nil
}

// Run implements Runnable. It walks the graph from StartNode, running
// nodes as their inputs arrive, fanning out to matching successors
// concurrently, barriering at Join nodes, and following back-edges to
// re-run nodes (bounded by each node's visit budget), until every path
// reaches a terminal node. Usage, turns, and tool calls roll up into the
// returned aggregate via mergeInto; FinalOutput is the (name-ordered)
// join of all terminal node outputs.
func (g *Graph) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	byName, succ, predCount, err := g.index()
	if err != nil {
		return nil, err
	}

	// Resolve the tracer from the run options, reusing the Runner's
	// option plumbing so a graph and its nodes share one tracer.
	ropts := runOptions{tracer: NoopTracer{}}
	for _, o := range opts {
		o(&ropts)
	}
	tracer := ropts.tracer

	// Shared typed state channel, threaded to any node whose Runnable
	// implements StatefulRunnable. Seeded with InitialState through the
	// registered reducers.
	state := NewState(g.StateReducers)
	for k, v := range g.InitialState {
		state.Set(k, v)
	}

	maxAct := g.MaxActivations
	if maxAct <= 0 {
		maxAct = 1000
	}

	ctx, endRun := tracer.StartRun(ctx, g.Name)
	defer endRun()

	var (
		mu        sync.Mutex
		aggregate = &RunResult{HandoffPath: []string{g.Name}}
		firstErr  error
		terminals = map[string]string{} // node name -> final output
		joinBuf   = map[string][]string{}
		joinLeft  = map[string]int{}
		visits    = map[string]int{}
		acts      int
		wg        sync.WaitGroup
	)
	for name, n := range byName {
		if n.Join {
			joinLeft[name] = predCount[name]
		}
	}

	// sem bounds concurrency when MaxConcurrency > 0.
	var sem chan struct{}
	if g.MaxConcurrency > 0 {
		sem = make(chan struct{}, g.MaxConcurrency)
	}

	var activate func(nodeName, in string)
	activate = func(nodeName, in string) {
		node := byName[nodeName]

		// Join nodes buffer inputs until every predecessor has arrived.
		if node.Join {
			mu.Lock()
			joinBuf[nodeName] = append(joinBuf[nodeName], in)
			joinLeft[nodeName]--
			ready := joinLeft[nodeName] <= 0
			if !ready {
				mu.Unlock()
				return
			}
			in = mergeInputs(node, joinBuf[nodeName])
			mu.Unlock()
		}

		mu.Lock()
		if firstErr != nil {
			mu.Unlock()
			return
		}
		visits[nodeName]++
		visit := visits[nodeName]
		if visit > g.visitCap(node) {
			firstErr = fmt.Errorf("graph %q: node %q exceeded MaxVisits (%d) - non-converging cycle?", g.Name, nodeName, g.visitCap(node))
			mu.Unlock()
			return
		}
		acts++
		if acts > maxAct {
			firstErr = fmt.Errorf("graph %q: exceeded MaxActivations (%d)", g.Name, maxAct)
			mu.Unlock()
			return
		}
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}
				defer func() { <-sem }()
			}

			nodeCtx, endNode := tracer.StartNode(ctx, g.Name, nodeName, visit)
			res, rerr := runNode(nodeCtx, node, in, state, opts)
			endNode()

			mu.Lock()
			if rerr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("graph %q node %q: %w", g.Name, nodeName, rerr)
				}
				mu.Unlock()
				return
			}
			mergeInto(aggregate, res)
			outs := succ[nodeName]
			if len(outs) == 0 {
				terminals[nodeName] = res.FinalOutput
			}
			mu.Unlock()

			for _, e := range firingSuccessors(node, res, state, outs) {
				activate(e.To, res.FinalOutput)
			}
		}()
	}

	for _, e := range succ[StartNode] {
		activate(e.To, input)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	// Final output: when OutputKey is set, project it from shared state;
	// otherwise join terminal node outputs in name order (single-terminal
	// graphs, the common case, yield exactly that node's output).
	if g.OutputKey != "" {
		aggregate.FinalOutput = state.GetString(g.OutputKey)
		return aggregate, nil
	}
	aggregate.FinalOutput = joinTerminals(terminals)
	return aggregate, nil
}

// runNode executes a node's Runnable, applying its Retry and Timeout
// policies. If the Runnable implements StatefulRunnable, the shared State
// is passed via RunState; otherwise the plain Run path is used. Retries
// use exponential backoff and respect ctx cancellation during the wait.
func runNode(ctx context.Context, node Node, in string, state *State, opts []RunOption) (*RunResult, error) {
	call := func(runCtx context.Context) (*RunResult, error) {
		if sr, ok := node.Runnable.(StatefulRunnable); ok && state != nil {
			return sr.RunState(runCtx, in, state, opts...)
		}
		return node.Runnable.Run(runCtx, in, opts...)
	}

	attempts := 1
	if node.Retry != nil && node.Retry.Attempts > 1 {
		attempts = node.Retry.Attempts
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		runCtx := ctx
		var cancel context.CancelFunc
		if node.Timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, node.Timeout)
		}
		res, err := call(runCtx)
		if cancel != nil {
			cancel()
		}
		if err == nil {
			return res, nil
		}
		// A pause request is a control signal, not a failure - never
		// retry it, surface it immediately so the durable executor can
		// checkpoint and wait for human input.
		var reqErr *RequestInputError
		if errors.As(err, &reqErr) {
			return nil, err
		}
		lastErr = err
		if attempt < attempts-1 {
			if werr := waitBackoff(ctx, node.Retry, attempt); werr != nil {
				return nil, werr
			}
		}
	}
	return nil, lastErr
}

// waitBackoff sleeps for the exponential backoff delay of the given
// attempt, returning early with ctx.Err() if the context is cancelled.
func waitBackoff(ctx context.Context, rp *RetryPolicy, attempt int) error {
	base := 100 * time.Millisecond
	if rp != nil && rp.Backoff > 0 {
		base = rp.Backoff
	}
	d := base
	for i := 0; i < attempt; i++ {
		next := d * 2
		if next < d {
			// int64 overflow - keep the last valid (large) delay rather
			// than wrapping to a negative duration that fires instantly.
			break
		}
		d = next
		if rp != nil && rp.MaxBackoff > 0 && d >= rp.MaxBackoff {
			break
		}
	}
	if rp != nil && rp.MaxBackoff > 0 && d > rp.MaxBackoff {
		d = rp.MaxBackoff
	}
	if d <= 0 {
		d = base
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// firingEdges selects which outgoing edges fire given the label the
// source node emitted (label == "" and hasRoute == false means a linear
// node). Unconditional edges (When == "") always fire. When the node
// routed, labeled edges matching the label fire; DefaultRoute edges fire
// only if no labeled edge matched.
func firingEdges(edges []Edge, label string, hasRoute bool) []Edge {
	if !hasRoute {
		var out []Edge
		for _, e := range edges {
			if e.When == "" {
				out = append(out, e)
			}
		}
		return out
	}
	var unconditional, matched, defaults []Edge
	for _, e := range edges {
		switch e.When {
		case "":
			unconditional = append(unconditional, e)
		case DefaultRoute:
			defaults = append(defaults, e)
		default:
			if e.When == label {
				matched = append(matched, e)
			}
		}
	}
	out := append(unconditional, matched...)
	if len(matched) == 0 {
		out = append(out, defaults...)
	}
	return out
}

// firingSuccessors computes the outgoing edges a node fires after it
// completes, deriving the routing label from RouteState/Route. Shared by
// the concurrent (Graph.Run) and durable (driveDurable/Resume) executors
// so routing can never diverge between the two paths.
func firingSuccessors(node Node, res *RunResult, state *State, outs []Edge) []Edge {
	label := ""
	hasRoute := node.RouteState != nil || node.Route != nil
	switch {
	case node.RouteState != nil:
		label = node.RouteState(res, state)
	case node.Route != nil:
		label = node.Route(res)
	}
	return firingEdges(outs, label, hasRoute)
}

// joinTerminals joins terminal node outputs in name order - the default
// FinalOutput rule shared by Graph.Run and the durable executor, so both
// produce identical output for a multi-terminal graph.
func joinTerminals(terminals map[string]string) string {
	names := make([]string, 0, len(terminals))
	for name := range terminals {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, terminals[name])
	}
	return strings.Join(parts, "\n---\n")
}

// mergeInputs combines a Join node's buffered predecessor outputs.
func mergeInputs(node Node, inputs []string) string {
	if node.Merge != nil {
		return node.Merge(inputs)
	}
	return strings.Join(inputs, "\n---\n")
}
