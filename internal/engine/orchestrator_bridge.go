package engine

import (
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
	anthropicadapter "github.com/ryanlitalien/aida/internal/engine/orchestrator/model/anthropic"
	openaiadapter "github.com/ryanlitalien/aida/internal/engine/orchestrator/model/openai"
	"github.com/ryanlitalien/aida/internal/llm"
)

// AdaptTools converts legacy AgentTools into orchestrator Tools so the
// existing BuildAgentTools pipeline can feed the orchestrator Runner.
func AdaptTools(tools []AgentTool) []orchestrator.Tool {
	out := make([]orchestrator.Tool, len(tools))
	for i, t := range tools {
		t := t
		out[i] = &orchestrator.FuncTool{
			TName:   t.Name,
			TDesc:   t.Description,
			TSchema: t.InputSchema,
			TFunc:   t.Execute,
		}
	}
	return out
}

// NewLLMAdapter creates a model.LLM for the given API key and model ID,
// selecting the provider adapter based on the model name.
func NewLLMAdapter(apiKey, modelID string) model.LLM {
	provider := llm.ResolveProvider(modelID)
	switch provider {
	case "openai":
		return openaiadapter.New(apiKey, modelID)
	default:
		return anthropicadapter.New(apiKey, modelID)
	}
}

// CostFromUsage computes a USD estimate from orchestrator token counts
// using the same pricing table as the legacy CostTracker.
func CostFromUsage(usage model.Usage, modelID string) float64 {
	pricing := llm.LookupPricing(modelID)
	return float64(usage.InputTokens)/1_000_000*pricing.InputPerM +
		float64(usage.OutputTokens)/1_000_000*pricing.OutputPerM
}
