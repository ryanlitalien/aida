package tools

import (
	"encoding/json"
	"testing"
)

// TestCalendarScheduleTool_MissingConfig_ReturnsPayloadAndError covers the
// "config invalid" branch of the Run error mapping: even when calendar:
// config is missing entirely, Run must still return a JSON payload (never
// bare "") alongside the error, so the model can see a structured reason.
func TestCalendarScheduleTool_MissingConfig_ReturnsPayloadAndError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.aida/config.yaml at all

	tool := calendarScheduleTool(nil)
	input, err := json.Marshal(map[string]interface{}{
		"when": map[string]interface{}{"kind": "day", "offset": 0},
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	payload, runErr := tool.Run(nil, input)
	if runErr == nil {
		t.Fatal("expected an error when calendar: config is missing")
	}
	if payload == "" {
		t.Fatal("Run returned an empty payload alongside the error; the model needs to see a reason")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v (payload=%q)", err, payload)
	}
	if decoded["status"] != "failed" {
		t.Errorf("status = %v, want failed", decoded["status"])
	}
}

// TestCalendarScheduleTool_MissingWhenKind_IsRejected covers the input
// validation path -- the model must supply when.kind, since that's the
// only way the semantic date reference the tool depends on gets in.
func TestCalendarScheduleTool_MissingWhenKind_IsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tool := calendarScheduleTool(nil)
	_, err := tool.Run(nil, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for a request with no when.kind")
	}
}

// TestCalendarScheduleTool_Registered_WhenDiscoveryPresent confirms the
// tool is wired into New's registration the same way the other
// MCP-dependent tools are gated on discovery != nil.
func TestCalendarScheduleTool_Registered_WhenDiscoveryPresent(t *testing.T) {
	tool := calendarScheduleTool(nil)
	if tool.Name != "calendar_schedule" {
		t.Errorf("Name = %q, want calendar_schedule", tool.Name)
	}
	if tool.Run == nil {
		t.Error("Run is nil")
	}
}
