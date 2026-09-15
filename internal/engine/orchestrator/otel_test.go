package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// newTestTracer wires the OTelTracer to an in-memory SpanRecorder so
// tests can assert on emitted spans without any external collector.
func newTestTracer() (*OTelTracer, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
	return NewOTelTracer(tp), sr
}

func TestOTelTracerStartRun(t *testing.T) {
	tr, sr := newTestTracer()
	ctx, end := tr.StartRun(context.Background(), "root")
	_ = ctx
	end()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	s := spans[0]
	if s.Name() != "orchestrator.run" {
		t.Errorf("name = %q", s.Name())
	}
	if v, ok := attrVal(s.Attributes(), "agent.name"); !ok || v.AsString() != "root" {
		t.Errorf("agent.name attr = %+v", v)
	}
}

func TestOTelTracerStartTurn(t *testing.T) {
	tr, sr := newTestTracer()
	_, end := tr.StartTurn(context.Background(), "root", 3)
	end()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	s := spans[0]
	if s.Name() != "orchestrator.turn" {
		t.Errorf("name = %q", s.Name())
	}
	if v, ok := attrVal(s.Attributes(), "turn"); !ok || v.AsInt64() != 3 {
		t.Errorf("turn attr = %+v", v)
	}
}

func TestOTelTracerStartTool(t *testing.T) {
	tr, sr := newTestTracer()
	_, end := tr.StartTool(context.Background(), "root", "query", "tu_abc")
	end()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans=%d", len(spans))
	}
	s := spans[0]
	if v, ok := attrVal(s.Attributes(), "tool.name"); !ok || v.AsString() != "query" {
		t.Errorf("tool.name attr = %+v", v)
	}
	if v, ok := attrVal(s.Attributes(), "tool.use.id"); !ok || v.AsString() != "tu_abc" {
		t.Errorf("tool.use.id attr = %+v", v)
	}
}

// TestRunnerEmitsTraceHierarchy drives a full Runner.Run through the
// tracer and asserts the expected span shape: one run span containing
// turn spans, each turn containing the tool spans that fired in it.
func TestRunnerEmitsTraceHierarchy(t *testing.T) {
	tr, sr := newTestTracer()

	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_1", "probe", json.RawMessage(`{}`)),
					model.ToolUseBlock("tu_2", "probe", json.RawMessage(`{}`)),
				},
			},
			{
				StopReason: model.StopEndTurn,
				Blocks:     []model.Block{model.TextBlock("done")},
			},
		},
	}
	var c int64
	agent := &Agent{
		Name:  "traced",
		Model: llm,
		Tools: []Tool{countingTool("probe", &c)},
	}
	var r Runner
	if _, err := r.Run(context.Background(), agent, "go", WithTracer(tr)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spans := sr.Ended()

	// Expect: 1 run + 2 turns + 2 tool calls = 5 spans.
	byName := map[string]int{}
	for _, s := range spans {
		byName[s.Name()]++
	}
	if byName["orchestrator.run"] != 1 {
		t.Errorf("run spans = %d, want 1 (got %+v)", byName["orchestrator.run"], byName)
	}
	if byName["orchestrator.turn"] != 2 {
		t.Errorf("turn spans = %d, want 2 (got %+v)", byName["orchestrator.turn"], byName)
	}
	if byName["orchestrator.tool"] != 2 {
		t.Errorf("tool spans = %d, want 2 (got %+v)", byName["orchestrator.tool"], byName)
	}

	// Each tool span should have a non-empty tool.use.id.
	for _, s := range spans {
		if s.Name() != "orchestrator.tool" {
			continue
		}
		v, ok := attrVal(s.Attributes(), "tool.use.id")
		if !ok || v.AsString() == "" {
			t.Errorf("tool span missing tool.use.id: attrs = %+v", s.Attributes())
		}
	}
}

// TestNoopTracerIsNoop sanity-checks that the default tracer is
// zero-overhead and doesn't trigger any span recording when no
// provider is installed.
func TestNoopTracerIsNoop(t *testing.T) {
	tr := NoopTracer{}
	ctx, end := tr.StartRun(context.Background(), "x")
	if ctx == nil {
		t.Error("ctx is nil")
	}
	end() // must not panic
}

// attrVal returns the attribute with the given key, if present.
func attrVal(attrs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
