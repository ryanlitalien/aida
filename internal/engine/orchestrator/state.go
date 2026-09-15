package orchestrator

import (
	"context"
	"fmt"
	"sync"
)

// This file adds a shared, typed state channel threaded through a Graph
// run - the analog of LangGraph's StateGraph state object, which the
// industry treats as the central concept for reliable multi-node
// workflows. It is fully additive: nodes opt in by implementing
// StatefulRunnable; every existing Agent and composite keeps working
// unchanged (the scheduler falls back to plain Run for them).

// Reducer merges an incoming value into the existing value for a state
// key. It models LangGraph's channel reducers: when two parallel branches
// write the same key, the Reducer combines them deterministically instead
// of one clobbering the other. existing is nil on the first write.
type Reducer func(existing, incoming any) any

// LastWrite is the default reducer: the incoming value replaces the
// existing one. Used for any key without a registered reducer.
func LastWrite(_, incoming any) any { return incoming }

// AppendSlice reduces by appending incoming to an []any accumulator -
// the reducer for collecting outputs from a fan-out into one key.
func AppendSlice(existing, incoming any) any {
	var acc []any
	if existing != nil {
		acc, _ = existing.([]any)
	}
	return append(acc, incoming)
}

// State is the shared, concurrency-safe typed store threaded through a
// Graph run. Nodes that implement StatefulRunnable read and write it;
// per-key Reducers merge concurrent writes from parallel branches so
// fan-in on state is automatic and deterministic, independent of Join
// control flow.
type State struct {
	mu       sync.RWMutex
	values   map[string]any
	reducers map[string]Reducer
}

// NewState builds a State with the given per-key reducers. Keys without a
// reducer use LastWrite (overwrite) semantics. reducers may be nil.
func NewState(reducers map[string]Reducer) *State {
	return &State{values: map[string]any{}, reducers: reducers}
}

// Get returns the current value for key and whether it was present.
func (s *State) Get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.values[key]
	return v, ok
}

// GetString returns the value formatted as a string (empty if absent).
func (s *State) GetString(key string) string {
	v, ok := s.Get(key)
	if !ok || v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprint(v)
}

// Set writes value for key, applying the key's Reducer (LastWrite if
// none). Safe for concurrent use from fan-out branches.
func (s *State) Set(key string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.reducers[key]
	if r == nil {
		r = LastWrite
	}
	s.values[key] = r(s.values[key], value)
}

// Snapshot returns a shallow copy of the current values, for
// checkpointing (see checkpoint.go) and inspection.
func (s *State) Snapshot() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]any, len(s.values))
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

// restore replaces state values directly, bypassing reducers. Used when
// rehydrating from a checkpoint, where the stored values were already
// reduced and must not be reduced again.
func (s *State) restore(values map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range values {
		s.values[k] = v
	}
}

// StatefulRunnable is an optional interface a node's Runnable may
// implement to read and write the graph's shared State. The scheduler
// calls RunState (passing the shared State) for nodes that implement it,
// and falls back to plain Run for everything else.
type StatefulRunnable interface {
	Runnable
	RunState(ctx context.Context, input string, state *State, opts ...RunOption) (*RunResult, error)
}

// StateFunc adapts a stateful function to a StatefulRunnable node.
type StateFunc func(ctx context.Context, input string, state *State, opts ...RunOption) (*RunResult, error)

// RunState implements StatefulRunnable.
func (f StateFunc) RunState(ctx context.Context, input string, state *State, opts ...RunOption) (*RunResult, error) {
	return f(ctx, input, state, opts...)
}

// Run implements Runnable so a StateFunc is usable outside a graph too;
// state is nil in that case, which the function must tolerate. Inside a
// Graph the scheduler always supplies the shared State via RunState.
func (f StateFunc) Run(ctx context.Context, input string, opts ...RunOption) (*RunResult, error) {
	return f(ctx, input, nil, opts...)
}
