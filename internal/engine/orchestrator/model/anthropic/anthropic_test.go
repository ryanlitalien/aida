package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

func TestBuildParamsBasics(t *testing.T) {
	req := model.Request{
		System: "you are a helpful agent",
		Messages: []model.Message{
			model.UserText("hello"),
		},
	}
	params, err := buildParams("claude-haiku-4-5", req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if string(params.Model) != "claude-haiku-4-5" {
		t.Errorf("model = %q, want claude-haiku-4-5", params.Model)
	}
	if params.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d, want %d", params.MaxTokens, defaultMaxTokens)
	}
	if len(params.System) != 1 || params.System[0].Text != "you are a helpful agent" {
		t.Errorf("system = %+v", params.System)
	}
	if len(params.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(params.Messages))
	}
}

func TestBuildParamsSystemPrefixCacheable(t *testing.T) {
	req := model.Request{
		SystemPrefix: "stable cacheable bit",
		System:       "variable per-question bit",
		Messages:     []model.Message{model.UserText("hi")},
	}
	params, err := buildParams("claude-haiku-4-5", req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(params.System) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(params.System))
	}
	if params.System[0].Text != "stable cacheable bit" {
		t.Errorf("prefix block text = %q", params.System[0].Text)
	}
	// CacheControl is a struct value (not pointer); the SDK marshals
	// the zero value with omitzero. The non-zero ephemeral type tag
	// is what we want - verify by JSON-marshaling and looking for
	// "cache_control".
	data, err := params.System[0].MarshalJSON()
	if err != nil {
		t.Fatalf("marshal prefix: %v", err)
	}
	if !contains(data, "cache_control") {
		t.Errorf("prefix block missing cache_control: %s", data)
	}
	if params.System[1].Text != "variable per-question bit" {
		t.Errorf("suffix block text = %q", params.System[1].Text)
	}
	data2, _ := params.System[1].MarshalJSON()
	if contains(data2, "cache_control") {
		t.Errorf("suffix block unexpectedly has cache_control: %s", data2)
	}
}

func TestBuildParamsSystemPrefixOnly(t *testing.T) {
	// Prefix without suffix should still emit one block with cache_control.
	req := model.Request{
		SystemPrefix: "only stable",
		Messages:     []model.Message{model.UserText("hi")},
	}
	params, _ := buildParams("claude-haiku-4-5", req)
	if len(params.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(params.System))
	}
	data, _ := params.System[0].MarshalJSON()
	if !contains(data, "cache_control") {
		t.Errorf("prefix block missing cache_control: %s", data)
	}
}

func TestBuildParamsLegacySystemOnly(t *testing.T) {
	// Single-block path: only System set, no SystemPrefix. No cache_control.
	req := model.Request{System: "legacy"}
	params, _ := buildParams("claude-haiku-4-5", req)
	if len(params.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(params.System))
	}
	data, _ := params.System[0].MarshalJSON()
	if contains(data, "cache_control") {
		t.Errorf("legacy single-block should not have cache_control: %s", data)
	}
}

func contains(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}

func TestBuildParamsTools(t *testing.T) {
	req := model.Request{
		Messages: []model.Message{model.UserText("x")},
		Tools: []model.ToolDef{
			{
				Name:        "query",
				Description: "run a query",
				InputSchema: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"sql": map[string]interface{}{"type": "string"},
					},
					"required": []string{"sql"},
				},
			},
		},
	}
	params, err := buildParams("claude-haiku-4-5", req)
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(params.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(params.Tools))
	}
	tool := params.Tools[0].OfTool
	if tool == nil {
		t.Fatal("tool is nil")
	}
	if tool.Name != "query" {
		t.Errorf("tool.Name = %q, want query", tool.Name)
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "sql" {
		t.Errorf("required = %v, want [sql]", tool.InputSchema.Required)
	}
}

func TestTranslateBlocksRoundTrip(t *testing.T) {
	blocks := []model.Block{
		model.TextBlock("thinking..."),
		model.ToolUseBlock("tu_1", "query", json.RawMessage(`{"q":"x"}`)),
	}
	out, err := translateBlocksToAnthropic(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(out))
	}
	if out[0].OfText == nil || out[0].OfText.Text != "thinking..." {
		t.Errorf("text block did not round-trip: %+v", out[0])
	}
	if out[1].OfToolUse == nil || out[1].OfToolUse.Name != "query" {
		t.Errorf("tool_use block did not round-trip: %+v", out[1])
	}
}

func TestTranslateBlocksToolResult(t *testing.T) {
	blocks := []model.Block{
		model.ToolResultBlock("tu_1", "error: boom", true),
	}
	out, err := translateBlocksToAnthropic(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 1 || out[0].OfToolResult == nil {
		t.Fatalf("expected tool_result block, got %+v", out)
	}
	tr := out[0].OfToolResult
	if tr.ToolUseID != "tu_1" {
		t.Errorf("ToolUseID = %q", tr.ToolUseID)
	}
	if tr.IsError.Value != true {
		t.Errorf("IsError should be true, got %+v", tr.IsError)
	}
	if len(tr.Content) != 1 || tr.Content[0].OfText == nil {
		t.Fatalf("content = %+v", tr.Content)
	}
	if tr.Content[0].OfText.Text != "error: boom" {
		t.Errorf("content text = %q", tr.Content[0].OfText.Text)
	}
}

func TestExtractRequired(t *testing.T) {
	tests := []struct {
		name   string
		schema map[string]interface{}
		want   []string
	}{
		{"nil schema", nil, nil},
		{"missing required", map[string]interface{}{}, nil},
		{"[]string", map[string]interface{}{"required": []string{"a", "b"}}, []string{"a", "b"}},
		{"[]interface{}", map[string]interface{}{"required": []interface{}{"a", "b"}}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRequired(tt.schema)
			if len(got) != len(tt.want) {
				t.Fatalf("len(got) = %d, len(want) = %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuildResponseEmpty(t *testing.T) {
	got := buildResponse(nil)
	if got == nil {
		t.Fatal("buildResponse(nil) returned nil")
	}
	if got.StopReason != model.StopOther {
		t.Errorf("stop_reason = %s, want other", got.StopReason)
	}
}

func TestBuildResponseTextAndToolUse(t *testing.T) {
	msg := &sdk.Message{
		StopReason: sdk.StopReason("tool_use"),
		Model:      sdk.Model("claude-haiku-4-5"),
		Usage: sdk.Usage{
			InputTokens:  42,
			OutputTokens: 17,
		},
		Content: []sdk.ContentBlockUnion{
			{Type: "text", Text: "let me check"},
			{Type: "tool_use", ID: "tu_abc", Name: "query", Input: json.RawMessage(`{"q":"x"}`)},
		},
	}
	resp := buildResponse(msg)
	if resp.StopReason != model.StopToolUse {
		t.Errorf("stop = %s, want tool_use", resp.StopReason)
	}
	if resp.ModelID != "claude-haiku-4-5" {
		t.Errorf("model = %q", resp.ModelID)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 17 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ToolName != "query" || calls[0].ToolUseID != "tu_abc" {
		t.Errorf("tool call = %+v", calls[0])
	}
	if resp.TextOnly() != "let me check" {
		t.Errorf("text = %q", resp.TextOnly())
	}
}

func TestNormalizeStopReason(t *testing.T) {
	tests := map[string]model.StopReason{
		"end_turn":           model.StopEndTurn,
		"tool_use":           model.StopToolUse,
		"max_tokens":         model.StopMaxTokens,
		"stop_sequence":      model.StopSequence,
		"":                   model.StopOther,
		"pause_turn":         model.StopOther,
		"refusal_or_unknown": model.StopOther,
	}
	for in, want := range tests {
		if got := normalizeStopReason(in); got != want {
			t.Errorf("normalizeStopReason(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestProviderAndModel(t *testing.T) {
	c := New("sk-test", "claude-haiku-4-5")
	if c.Provider() != "anthropic" {
		t.Errorf("provider = %q", c.Provider())
	}
	if c.Model() != "claude-haiku-4-5" {
		t.Errorf("model = %q", c.Model())
	}
}

// Compile-time checks.
var _ model.LLM = (*Client)(nil)
var _ model.Streamer = (*Client)(nil)
