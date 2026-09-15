package orchestrator

import (
	"encoding/json"
	"fmt"
)

// Handoff describes the ability for an agent to transfer control to
// another agent mid-run. Modeled on the OpenAI Agents SDK and on
// Google ADK's `transfer` semantic: a handoff is exposed to the model
// as a tool, and when the model calls that tool, the Runner resolves
// it to a switch rather than invoking it as a real tool.
type Handoff struct {
	// ToolName is the name the agent's LLM sees and calls to trigger
	// the handoff. If empty, the default is "handoff_to_" + Target.Name.
	ToolName string

	// Description is shown in the tool's description field.
	Description string

	// Target is the agent that receives control on handoff. Required.
	Target *Agent
}

// effectiveToolName returns the LLM-facing tool name for this handoff.
func (h Handoff) effectiveToolName() string {
	if h.ToolName != "" {
		return h.ToolName
	}
	if h.Target != nil {
		return "handoff_to_" + h.Target.Name
	}
	return "handoff"
}

// toolDef returns the LLM-visible tool definition for this handoff.
// The single string parameter "context" carries a message to the
// target agent - keeping the surface area minimal matches OpenAI
// Agents SDK's default handoff shape.
func (h Handoff) toolDef() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"context": map[string]interface{}{
				"type":        "string",
				"description": "Message or context to pass to the target agent.",
			},
		},
		"required": []string{"context"},
	}
}

// handoffInput is the unmarshaled payload of a handoff tool call.
type handoffInput struct {
	Context string `json:"context"`
}

// parseHandoffInput unmarshals the JSON input from a handoff tool call.
func parseHandoffInput(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var in handoffInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", fmt.Errorf("handoff input: %w", err)
	}
	return in.Context, nil
}
