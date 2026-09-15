package llm

import "strings"

// ResolveProvider maps a model ID to its provider name. Uses prefix
// matching and a built-in catalog. Phase 2 will add a full ModelRegistry
// with pricing, context windows, and user overrides.
func ResolveProvider(model string) string {
	lower := strings.ToLower(model)

	// Explicit provider prefix: "openai:gpt-4o", "google:gemini-2.5-flash"
	if idx := strings.Index(lower, ":"); idx > 0 {
		prefix := lower[:idx]
		switch prefix {
		case "openai", "groq", "cerebras", "together", "openrouter":
			return "openai"
		case "google", "gemini":
			return "google"
		case "ollama":
			return "ollama"
		case "anthropic":
			return "anthropic"
		}
	}

	// Infer from model name.
	switch {
	case strings.HasPrefix(lower, "claude"):
		return "anthropic"
	case strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "o1") || strings.HasPrefix(lower, "o3") || strings.HasPrefix(lower, "o4"):
		return "openai"
	case strings.HasPrefix(lower, "gemini"):
		return "google"
	case strings.HasPrefix(lower, "llama") || strings.HasPrefix(lower, "qwen") ||
		strings.HasPrefix(lower, "mistral") || strings.HasPrefix(lower, "phi"):
		return "ollama" // assume local unless provider prefix says otherwise
	}

	return "anthropic" // default
}

// ModelPricing holds per-million-token costs for a model.
type ModelPricing struct {
	InputPerM  float64 // USD per 1M input tokens
	OutputPerM float64 // USD per 1M output tokens
}

// BuiltinPricing maps common model prefixes to their pricing.
// Used by CostTracker to compute per-call USD costs.
var BuiltinPricing = map[string]ModelPricing{
	// Anthropic. Everything Opus-tier from 4.6 on prices at 5.00/25.00.
	"claude-fable-5":  {InputPerM: 10.00, OutputPerM: 50.00},
	"claude-opus-5":   {InputPerM: 5.00, OutputPerM: 25.00},
	"claude-opus-4-8": {InputPerM: 5.00, OutputPerM: 25.00},
	"claude-opus-4-7": {InputPerM: 5.00, OutputPerM: 25.00},
	"claude-opus-4-6": {InputPerM: 5.00, OutputPerM: 25.00},
	// Sticker rate; introductory 2.00/10.00 runs through 2026-08-31, so
	// this over-reports slightly until then.
	"claude-sonnet-5":   {InputPerM: 3.00, OutputPerM: 15.00},
	"claude-sonnet-4-6": {InputPerM: 3.00, OutputPerM: 15.00},
	"claude-haiku-4-5":  {InputPerM: 1.00, OutputPerM: 5.00},
	// OpenAI GPT-5.6 family (post 2026-07-30 price cut). The bare
	// "gpt-5.6" ID is an alias that routes to Sol.
	"gpt-5.6":       {InputPerM: 5.00, OutputPerM: 30.00},
	"gpt-5.6-sol":   {InputPerM: 5.00, OutputPerM: 30.00},
	"gpt-5.6-terra": {InputPerM: 2.00, OutputPerM: 12.00},
	"gpt-5.6-luna":  {InputPerM: 0.20, OutputPerM: 1.20},
	// Legacy OpenAI, kept for older configs.
	"gpt-4o":      {InputPerM: 2.50, OutputPerM: 10.00},
	"gpt-4o-mini": {InputPerM: 0.15, OutputPerM: 0.60},
	// Legacy Google, kept for older configs.
	"gemini-2.5-flash": {InputPerM: 0.15, OutputPerM: 0.60},
	"gemini-2.5-pro":   {InputPerM: 1.25, OutputPerM: 10.00},
	// Paid-tier rates. Gemini 3.7 Flash is also on the free tier, so a
	// free-tier key bills nothing and this over-reports rather than
	// surprising anyone. Introductory pricing runs through 2026-12-31,
	// after which input/output double to 1.50/7.50.
	"gemini-3.7-flash": {InputPerM: 0.75, OutputPerM: 3.75},
}

// LookupPricing finds the pricing for a model by trying progressively
// shorter prefixes. Returns zero pricing if not found (e.g. Ollama).
func LookupPricing(model string) ModelPricing {
	lower := strings.ToLower(model)
	// Strip provider prefix if present.
	if idx := strings.Index(lower, ":"); idx > 0 {
		lower = lower[idx+1:]
	}
	// Try exact match first.
	if p, ok := BuiltinPricing[lower]; ok {
		return p
	}
	// Try progressively shorter prefixes.
	for len(lower) > 5 {
		idx := strings.LastIndexAny(lower, "-_.")
		if idx < 0 {
			break
		}
		lower = lower[:idx]
		if p, ok := BuiltinPricing[lower]; ok {
			return p
		}
	}
	return ModelPricing{} // free (Ollama, unknown)
}
