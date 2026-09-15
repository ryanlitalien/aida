package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

func TestTranslateUserMessageText(t *testing.T) {
	out, err := translateUserMessage([]model.Block{model.TextBlock("hello")})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d, want 1", len(out))
	}
	if out[0].OfUser == nil {
		t.Errorf("expected user message, got %+v", out[0])
	}
}

func TestTranslateUserMessageToolResultsSplit(t *testing.T) {
	// A user message carrying only tool_result blocks should map to
	// N role=tool messages, one per result.
	blocks := []model.Block{
		model.ToolResultBlock("tu_a", "42", false),
		model.ToolResultBlock("tu_b", "oops", true),
	}
	out, err := translateUserMessage(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].OfTool == nil {
		t.Errorf("expected tool message, got %+v", out[0])
	}
	if got := out[0].OfTool.ToolCallID; got != "tu_a" {
		t.Errorf("ToolCallID = %q, want tu_a", got)
	}
}

func TestTranslateUserMessageMixedTextAndToolResult(t *testing.T) {
	// A user message carrying both text and tool_result blocks should
	// emit the text first (as a user message) then the tool results.
	blocks := []model.Block{
		model.TextBlock("hi"),
		model.ToolResultBlock("tu_a", "done", false),
	}
	out, err := translateUserMessage(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2", len(out))
	}
	if out[0].OfUser == nil {
		t.Errorf("expected user text first, got %+v", out[0])
	}
	if out[1].OfTool == nil {
		t.Errorf("expected tool second, got %+v", out[1])
	}
}

func TestTranslateAssistantMessageTextAndToolCall(t *testing.T) {
	blocks := []model.Block{
		model.TextBlock("let me check"),
		model.ToolUseBlock("tu_1", "query", json.RawMessage(`{"q":"x"}`)),
	}
	out, err := translateAssistantMessage(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.OfAssistant == nil {
		t.Fatal("expected assistant message")
	}
	asst := out.OfAssistant
	if asst.Content.OfString.Value != "let me check" {
		t.Errorf("content = %q, want 'let me check'", asst.Content.OfString.Value)
	}
	if len(asst.ToolCalls) != 1 {
		t.Fatalf("tool calls len=%d, want 1", len(asst.ToolCalls))
	}
	if asst.ToolCalls[0].ID != "tu_1" {
		t.Errorf("tool call ID = %q", asst.ToolCalls[0].ID)
	}
	if asst.ToolCalls[0].Function.Name != "query" {
		t.Errorf("tool fn name = %q", asst.ToolCalls[0].Function.Name)
	}
	if asst.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Errorf("tool fn args = %q", asst.ToolCalls[0].Function.Arguments)
	}
}

func TestTranslateAssistantEmptyToolArgs(t *testing.T) {
	// Zero-length ToolInput must marshal to "{}", not "".
	blocks := []model.Block{
		model.ToolUseBlock("tu_1", "ping", nil),
	}
	out, err := translateAssistantMessage(blocks)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.OfAssistant.ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("expected {}, got %q", out.OfAssistant.ToolCalls[0].Function.Arguments)
	}
}

func TestTranslateToolsIncludesSchema(t *testing.T) {
	tools := []model.ToolDef{
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
	}
	out, err := translateTools(tools)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d", len(out))
	}
	if out[0].Function.Name != "query" {
		t.Errorf("fn name = %q", out[0].Function.Name)
	}
	if out[0].Function.Description.Value != "run a query" {
		t.Errorf("desc = %q", out[0].Function.Description.Value)
	}
	if _, ok := out[0].Function.Parameters["properties"]; !ok {
		t.Error("expected properties to be preserved")
	}
}

func TestNormalizeFinishReason(t *testing.T) {
	tests := map[string]model.StopReason{
		"stop":           model.StopEndTurn,
		"tool_calls":     model.StopToolUse,
		"length":         model.StopMaxTokens,
		"content_filter": model.StopOther,
		"function_call":  model.StopToolUse,
		"":               model.StopOther,
	}
	for in, want := range tests {
		if got := normalizeFinishReason(in); got != want {
			t.Errorf("normalizeFinishReason(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestBuildResponseEmpty(t *testing.T) {
	got := buildResponse(nil)
	if got.StopReason != model.StopOther {
		t.Errorf("stop = %s, want other", got.StopReason)
	}
}

// TestCompleteAgainstMockServer wires the adapter end-to-end against a
// fake OpenAI server that returns a canned ChatCompletion response.
// This exercises the full translate→request→parse→translate pipeline.
func TestCompleteAgainstMockServer(t *testing.T) {
	// Canned response: one tool call and some text.
	responseBody := `{
		"id": "chatcmpl-test",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-5-mini",
		"choices": [{
			"index": 0,
			"finish_reason": "tool_calls",
			"logprobs": null,
			"message": {
				"role": "assistant",
				"content": "let me check that",
				"refusal": "",
				"tool_calls": [{
					"id": "call_abc",
					"type": "function",
					"function": {"name": "query", "arguments": "{\"q\":\"x\"}"}
				}]
			}
		}],
		"usage": {
			"prompt_tokens": 42,
			"completion_tokens": 17,
			"total_tokens": 59,
			"prompt_tokens_details": {"cached_tokens": 10}
		}
	}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer server.Close()

	c := NewWithOptions("gpt-5-mini",
		option.WithAPIKey("sk-test"),
		option.WithBaseURL(server.URL+"/"),
	)

	resp, err := c.Complete(context.Background(), model.Request{
		System:   "you are an agent",
		Messages: []model.Message{model.UserText("do something")},
		Tools: []model.ToolDef{
			{
				Name:        "query",
				Description: "query a thing",
				InputSchema: map[string]interface{}{"type": "object"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.StopReason != model.StopToolUse {
		t.Errorf("stop = %s, want tool_use", resp.StopReason)
	}
	if resp.TextOnly() != "let me check that" {
		t.Errorf("text = %q", resp.TextOnly())
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].ToolUseID != "call_abc" || calls[0].ToolName != "query" {
		t.Errorf("tool call = %+v", calls[0])
	}
	if string(calls[0].ToolInput) != `{"q":"x"}` {
		t.Errorf("tool input = %s", calls[0].ToolInput)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 17 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if resp.Usage.CacheReadTokens != 10 {
		t.Errorf("cached tokens = %d, want 10", resp.Usage.CacheReadTokens)
	}
	if resp.ModelID != "gpt-5-mini" {
		t.Errorf("model id = %q", resp.ModelID)
	}

	// Compile-time check that Client satisfies model.LLM.
	var _ model.LLM = c
}

func TestBuildParamsSystemPromptPrepended(t *testing.T) {
	params, err := buildParams("gpt-5-mini", model.Request{
		System:   "you are a helpful agent",
		Messages: []model.Message{model.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(params.Messages) != 2 {
		t.Fatalf("expected system+user, got %d", len(params.Messages))
	}
	if params.Messages[0].OfSystem == nil {
		t.Errorf("first msg should be system, got %+v", params.Messages[0])
	}
	if params.Messages[1].OfUser == nil {
		t.Errorf("second msg should be user, got %+v", params.Messages[1])
	}
}

func TestBuildParamsSystemPrefixConcatenated(t *testing.T) {
	// OpenAI's prompt cache is automatic on prefix match - the
	// adapter just concatenates SystemPrefix + System into a single
	// system message. The prefix must come first for caching to work.
	params, err := buildParams("gpt-5-mini", model.Request{
		SystemPrefix: "stable bit",
		System:       "variable bit",
		Messages:     []model.Message{model.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(params.Messages) < 1 || params.Messages[0].OfSystem == nil {
		t.Fatalf("first message should be system, got %+v", params.Messages[0])
	}
	sys := params.Messages[0].OfSystem.Content.OfString.Value
	if sys != "stable bit\n\nvariable bit" {
		t.Errorf("system content = %q, want prefix+system concatenated", sys)
	}
}

func TestBuildParamsSystemPrefixOnly_OpenAI(t *testing.T) {
	params, _ := buildParams("gpt-5-mini", model.Request{
		SystemPrefix: "stable",
	})
	if len(params.Messages) != 1 || params.Messages[0].OfSystem == nil {
		t.Fatalf("expected one system message, got %+v", params.Messages)
	}
	sys := params.Messages[0].OfSystem.Content.OfString.Value
	if sys != "stable" {
		t.Errorf("system = %q, want %q", sys, "stable")
	}
}

// Compile-time checks.
var _ sdk.ChatCompletionMessageParamUnion
var _ model.LLM = (*Client)(nil)
var _ model.Streamer = (*Client)(nil)
