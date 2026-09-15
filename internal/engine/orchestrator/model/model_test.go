package model

import (
	"encoding/json"
	"testing"
)

func TestResponseTextOnly(t *testing.T) {
	resp := &Response{
		Blocks: []Block{
			TextBlock("hello "),
			ToolUseBlock("id1", "foo", json.RawMessage(`{}`)),
			TextBlock("world"),
		},
	}
	got := resp.TextOnly()
	if got != "hello world" {
		t.Errorf("TextOnly() = %q, want %q", got, "hello world")
	}
}

func TestResponseTextOnlyNil(t *testing.T) {
	var resp *Response
	if got := resp.TextOnly(); got != "" {
		t.Errorf("nil Response.TextOnly() = %q, want empty", got)
	}
}

func TestResponseToolCalls(t *testing.T) {
	resp := &Response{
		Blocks: []Block{
			TextBlock("thinking..."),
			ToolUseBlock("a", "query", json.RawMessage(`{"q":"x"}`)),
			ToolUseBlock("b", "search", json.RawMessage(`{"q":"y"}`)),
		},
	}
	calls := resp.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].ToolUseID != "a" || calls[1].ToolUseID != "b" {
		t.Errorf("unexpected tool_use IDs: %v", calls)
	}
}

func TestUserText(t *testing.T) {
	m := UserText("hi")
	if m.Role != RoleUser {
		t.Errorf("role = %s, want user", m.Role)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].Text != "hi" {
		t.Errorf("blocks = %+v", m.Blocks)
	}
}

func TestUserToolResults(t *testing.T) {
	r1 := ToolResultBlock("id1", "ok", false)
	r2 := ToolResultBlock("id2", "oops", true)
	m := UserToolResults(r1, r2)
	if m.Role != RoleUser {
		t.Errorf("role = %s, want user", m.Role)
	}
	if len(m.Blocks) != 2 {
		t.Fatalf("expected 2 result blocks, got %d", len(m.Blocks))
	}
	if !m.Blocks[1].IsError {
		t.Error("second block should be an error")
	}
}

func TestToolUseBlockPreservesInput(t *testing.T) {
	raw := json.RawMessage(`{"query":"SELECT 1"}`)
	b := ToolUseBlock("tu_123", "snowflake", raw)
	if string(b.ToolInput) != string(raw) {
		t.Errorf("ToolInput = %s, want %s", b.ToolInput, raw)
	}
	if b.Type != BlockToolUse {
		t.Errorf("type = %s, want tool_use", b.Type)
	}
}
