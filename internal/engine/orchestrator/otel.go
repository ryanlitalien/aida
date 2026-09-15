package orchestrator

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope used for orchestrator spans.
// Users can filter on this name when viewing traces in Jaeger / Tempo /
// any OTel-compatible UI.
const TracerName = "github.com/ryanlitalien/aida/internal/engine/orchestrator"

// OTelTracer is the OpenTelemetry implementation of the Tracer
// interface. Spans are emitted at three levels:
//
//   - run: one span per Runner.Run call, tagged with agent.name.
//   - turn: one span per LLM completion inside a single agent's loop,
//     tagged with agent.name and turn.
//   - tool: one span per tool invocation, tagged with agent.name,
//     tool.name, and tool.use.id.
//
// Span attributes follow conventional snake_case naming ("agent.name",
// "tool.name") to match the emerging ecosystem of LLM tracing
// visualizers (Langfuse, Honeycomb, etc.).
type OTelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer constructs an OTelTracer from a global TracerProvider.
// Pass a nil provider to pick up the process-wide default, which is
// what most applications want.
func NewOTelTracer(provider trace.TracerProvider) *OTelTracer {
	if provider == nil {
		provider = otel.GetTracerProvider()
	}
	return &OTelTracer{tracer: provider.Tracer(TracerName)}
}

// StartRun implements Tracer.
func (t *OTelTracer) StartRun(ctx context.Context, agentName string) (context.Context, func()) {
	ctx, span := t.tracer.Start(ctx, "orchestrator.run",
		trace.WithAttributes(attribute.String("agent.name", agentName)),
	)
	return ctx, func() { span.End() }
}

// StartTurn implements Tracer.
func (t *OTelTracer) StartTurn(ctx context.Context, agentName string, turn int) (context.Context, func()) {
	ctx, span := t.tracer.Start(ctx, "orchestrator.turn",
		trace.WithAttributes(
			attribute.String("agent.name", agentName),
			attribute.Int("turn", turn),
		),
	)
	return ctx, func() { span.End() }
}

// StartTool implements Tracer.
func (t *OTelTracer) StartTool(ctx context.Context, agentName, toolName, toolUseID string) (context.Context, func()) {
	ctx, span := t.tracer.Start(ctx, "orchestrator.tool",
		trace.WithAttributes(
			attribute.String("agent.name", agentName),
			attribute.String("tool.name", toolName),
			attribute.String("tool.use.id", toolUseID),
		),
	)
	return ctx, func() { span.End() }
}

// StartNode implements Tracer. It emits an "orchestrator.graph.node"
// span so a graph's structure (which nodes ran, and which loop
// iteration) is visible above the agent's own run/turn/tool spans.
func (t *OTelTracer) StartNode(ctx context.Context, graphName, nodeName string, visit int) (context.Context, func()) {
	ctx, span := t.tracer.Start(ctx, "orchestrator.graph.node",
		trace.WithAttributes(
			attribute.String("graph.name", graphName),
			attribute.String("node.name", nodeName),
			attribute.Int("node.visit", visit),
		),
	)
	return ctx, func() { span.End() }
}
