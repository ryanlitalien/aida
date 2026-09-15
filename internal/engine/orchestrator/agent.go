package orchestrator

import (
	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// Agent is a single LLM-driven worker with instructions, a model, a
// tool palette, optional handoffs, and optional guardrails. An Agent
// is a value type - Runner drives it and does not mutate it.
//
// Shape is the intersection of OpenAI Agents SDK's Agent (instructions,
// tools, handoffs, guardrails) and ADK's LlmAgent (named, transfer).
type Agent struct {
	// Name is a stable identifier used in traces, handoff tool names,
	// and multi-agent composition.
	Name string

	// Instructions is the system prompt for this agent. When
	// InstructionsPrefix is also set, the runner sends them in two
	// slots: prefix (cacheable across calls) followed by Instructions
	// (variable per-question). When prefix is empty, Instructions is
	// the entire system prompt - same shape as it always was.
	Instructions string

	// InstructionsPrefix is the cacheable, stable portion of the
	// system prompt. The anthropic adapter marks it cache_control:
	// ephemeral so repeat calls with the same prefix get billed at the
	// cache-read rate (~10% of normal input tokens). Use this for
	// content that doesn't change per question: the system template,
	// available tool list, L1 layer index. Per-question content (brain
	// context, resolved entities) belongs in Instructions, not here.
	InstructionsPrefix string

	// Model is the LLM this agent uses. Required.
	Model model.LLM

	// Tools are the tools the agent may call. Handoffs are surfaced as
	// additional tools by the Runner; they do not need to be listed here.
	Tools []Tool

	// Handoffs are the agents this agent may transfer control to.
	Handoffs []Handoff

	// InputGuardrails run against the user's initial input before any
	// LLM call. A violation aborts the run before tokens are spent.
	InputGuardrails []Guardrail

	// OutputGuardrails run against the agent's final text output.
	OutputGuardrails []Guardrail

	// MaxTurns is no longer honored by the Runner: the tool-use loop
	// has no turn cap, only the wall-clock deadline and cost ceiling
	// passed to Run via WithDeadline / WithCostLimit. The field stays
	// so callers that still set it (e.g. from an "agent.max_turns"
	// config value or an AIDA_AGENT_MAX_TURNS env var) keep compiling;
	// a value here is simply ignored.
	MaxTurns int

	// MaxToolFailures is the circuit-breaker threshold: once a tool
	// has failed this many times in one run, subsequent calls to it
	// are short-circuited with an error. Zero defaults to 3 (legacy).
	MaxToolFailures int

	// MaxTokens caps a single completion. Zero uses the adapter default.
	MaxTokens int
}

// toolMap builds a name-indexed lookup of the agent's own tools,
// excluding handoffs. Used by the Runner during tool dispatch.
func (a *Agent) toolMap() map[string]Tool {
	m := make(map[string]Tool, len(a.Tools))
	for _, t := range a.Tools {
		m[t.Name()] = t
	}
	return m
}

// handoffMap builds a name-indexed lookup of this agent's handoffs
// keyed by their effective tool name.
func (a *Agent) handoffMap() map[string]Handoff {
	m := make(map[string]Handoff, len(a.Handoffs))
	for _, h := range a.Handoffs {
		m[h.effectiveToolName()] = h
	}
	return m
}

// modelToolDefs returns the combined ToolDef list the LLM sees: real
// tools plus handoff shims.
func (a *Agent) modelToolDefs() []model.ToolDef {
	defs := make([]model.ToolDef, 0, len(a.Tools)+len(a.Handoffs))
	for _, t := range a.Tools {
		defs = append(defs, model.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	for _, h := range a.Handoffs {
		desc := h.Description
		if desc == "" && h.Target != nil {
			desc = "Transfer control to agent " + h.Target.Name
		}
		defs = append(defs, model.ToolDef{
			Name:        h.effectiveToolName(),
			Description: desc,
			InputSchema: h.toolDef(),
		})
	}
	return defs
}
