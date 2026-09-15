package orchestrator

import "context"

// Tracer is the observability seam the Runner writes to. Real
// implementations emit OpenTelemetry spans; tests can supply a capturing
// tracer to assert the event stream; production deployments can swap in
// NoopTracer when tracing is off.
//
// Each Start* method returns the (possibly wrapped) context and a
// function that must be called to end the span. Tracers should tolerate
// the end-func being called multiple times.
type Tracer interface {
	// StartRun begins the top-level Run span.
	StartRun(ctx context.Context, agentName string) (context.Context, func())
	// StartTurn begins a span for a single LLM completion inside an
	// agent's tool-use loop. turn is 1-indexed and monotonic across
	// handoffs in the same Run.
	StartTurn(ctx context.Context, agentName string, turn int) (context.Context, func())
	// StartTool begins a span for a single tool invocation. toolUseID
	// is the model-issued correlation ID that ties a tool_use block
	// to its eventual tool_result.
	StartTool(ctx context.Context, agentName, toolName, toolUseID string) (context.Context, func())
	// StartNode begins a span for a single Graph node activation. visit
	// is 1-indexed and increments each time a node re-runs within a
	// cyclic graph, so loop iterations are distinguishable in a trace.
	StartNode(ctx context.Context, graphName, nodeName string, visit int) (context.Context, func())
}

// NoopTracer is the zero-overhead default.
type NoopTracer struct{}

// StartRun implements Tracer.
func (NoopTracer) StartRun(ctx context.Context, _ string) (context.Context, func()) {
	return ctx, func() {}
}

// StartTurn implements Tracer.
func (NoopTracer) StartTurn(ctx context.Context, _ string, _ int) (context.Context, func()) {
	return ctx, func() {}
}

// StartTool implements Tracer.
func (NoopTracer) StartTool(ctx context.Context, _, _, _ string) (context.Context, func()) {
	return ctx, func() {}
}

// StartNode implements Tracer.
func (NoopTracer) StartNode(ctx context.Context, _, _ string, _ int) (context.Context, func()) {
	return ctx, func() {}
}
