package orchestrator

import (
	"context"
	"encoding/json"
)

// Tool is an executable capability an Agent can call. The shape is
// intentionally close to aida's internal/engine.AgentTool - the
// orchestrator is provider-agnostic about tools, so callers can wrap
// anything that satisfies the interface.
type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]interface{}
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// FuncTool is a convenience Tool implementation for in-process tools
// backed by a single function.
type FuncTool struct {
	TName   string
	TDesc   string
	TSchema map[string]interface{}
	TFunc   func(ctx context.Context, input json.RawMessage) (string, error)
}

// Name implements Tool.
func (t *FuncTool) Name() string { return t.TName }

// Description implements Tool.
func (t *FuncTool) Description() string { return t.TDesc }

// InputSchema implements Tool.
func (t *FuncTool) InputSchema() map[string]interface{} { return t.TSchema }

// Run implements Tool.
func (t *FuncTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	return t.TFunc(ctx, input)
}
