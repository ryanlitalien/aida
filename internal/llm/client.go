package llm

import (
	"context"
	"fmt"
	"time"
)

const (
	// DefaultModel is Claude Haiku for fast, cost-effective inference.
	DefaultModel = "claude-haiku-4-5-20251001"

	// DefaultOllamaModel is the model to use when running offline via Ollama.
	DefaultOllamaModel = "qwen3:8b"

	// OllamaBaseURL is the default Ollama endpoint with OpenAI-compatible API.
	OllamaBaseURL = "http://localhost:11434/v1"

	// GeminiBaseURL is the Gemini API's OpenAI-compatible endpoint. No
	// trailing slash - callers append "/chat/completions".
	GeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

	// DefaultMaxTokens is the maximum tokens for a completion response.
	DefaultMaxTokens = 4096

	// DefaultTimeout is the default timeout for LLM API calls.
	DefaultTimeout = 60 * time.Second
)

// ptrFloat returns a pointer to f, for use in Request.Temperature.
func ptrFloat(f float64) *float64 { return &f }

// Client dispatches LLM calls to the appropriate Provider based on
// model resolution. It preserves the same Complete/CompleteJSON
// interface so all existing call sites work unchanged.
type Client struct {
	provider    Provider
	model       string
	offline     bool
	timeout     time.Duration
	tracker     *CostTracker
	stageModels map[string]string // "parse" → model, "synthesize" → model
	apiKey      string            // kept for creating stage-specific providers on the fly
}

// ResolveOfflineModel picks the model an --offline run should use, before
// NewClient is ever called. override (config's model.offline, when set)
// always wins. Otherwise, candidate (typically config's model.fallback) is
// kept as long as it looks local; an empty candidate, or one that resolves
// to a cloud provider (google/openai), falls back to DefaultOllamaModel.
// NewClient applies the same cloud-provider guard on its own as a second
// line of defense for callers that skip this helper, so either path is
// safe on its own - this one just also honors the model.offline override.
func ResolveOfflineModel(candidate, override string) string {
	if override != "" {
		return override
	}
	if candidate == "" {
		return DefaultOllamaModel
	}
	switch ResolveProvider(candidate) {
	case "google", "openai":
		return DefaultOllamaModel
	default:
		return candidate
	}
}

// NewClient creates a new LLM client. If offline is true, it connects to a
// local Ollama instance instead of the Anthropic API.
func NewClient(apiKey string, model string, offline bool) *Client {
	if model == "" {
		if offline {
			model = DefaultOllamaModel
		} else {
			model = DefaultModel
		}
	}

	var provider Provider
	if offline {
		// --offline means "run fully local, no API key required" - full
		// stop. A caller can still pass a model naming a cloud provider
		// (e.g. model.fallback configured to gemini-3.7-flash), but that
		// must never reach the network with no credentials attached. When
		// the model resolves to a cloud provider, ignore it and use the
		// local Ollama default instead; a genuinely local name (qwen3:8b,
		// an "ollama:" prefix, or anything unresolved) is kept as-is.
		if p := ResolveProvider(model); p == "google" || p == "openai" {
			model = DefaultOllamaModel
		}
		provider = NewOllamaProvider(OllamaBaseURL)
	} else {
		// Resolve provider from model name.
		providerName := ResolveProvider(model)
		switch providerName {
		case "openai":
			provider = NewOpenAIProvider("", "") // uses OPENAI_API_KEY env
		case "google":
			provider = NewGoogleProvider("") // uses GEMINI_API_KEY env
		default:
			provider = NewAnthropicProvider(apiKey)
		}
	}

	return &Client{
		provider: provider,
		model:    model,
		offline:  offline,
		timeout:  DefaultTimeout,
		tracker:  NewCostTracker(),
		apiKey:   apiKey,
	}
}

// SetStageModels configures per-stage model overrides. When a stage
// name is present in the map, CompleteWithStage will use that model
// (and resolve its provider) instead of the default.
func (c *Client) SetStageModels(stages map[string]string) {
	c.stageModels = stages
}

// Complete sends a simple text completion request and returns the response text.
func (c *Client) Complete(ctx context.Context, systemPrompt string, userPrompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.provider.Complete(ctx, Request{
		Model:        c.model,
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		MaxTokens:    DefaultMaxTokens,
	})
	if err != nil {
		return "", err
	}
	c.tracker.Record("", resp)
	return resp.Text, nil
}

// CompleteJSON sends a completion request with JSON schema enforcement.
// The response is guaranteed to conform to the provided JSON schema.
func (c *Client) CompleteJSON(ctx context.Context, systemPrompt string, userPrompt string, schema map[string]interface{}) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.provider.CompleteJSON(ctx, Request{
		Model:        c.model,
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		MaxTokens:    DefaultMaxTokens,
	}, schema)
	if err != nil {
		return "", err
	}
	c.tracker.Record("", resp)
	return resp.Text, nil
}

// CompleteWithStage uses a stage-specific model if one is configured
// in StageModels. Falls back to the default model otherwise.
// Stage names: "parse", "route", "execute", "synthesize", "quality".
func (c *Client) CompleteWithStage(ctx context.Context, stage, systemPrompt, userPrompt string) (string, error) {
	model, prov := c.resolveStage(stage)
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := Request{
		Model:        model,
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		MaxTokens:    DefaultMaxTokens,
	}
	if stage == "execute" {
		req.Temperature = ptrFloat(0)
	}
	resp, err := prov.Complete(ctx, req)
	if err != nil {
		return "", err
	}
	c.tracker.Record(stage, resp)
	return resp.Text, nil
}

// CompleteJSONWithStage is the JSON-schema variant of CompleteWithStage.
func (c *Client) CompleteJSONWithStage(ctx context.Context, stage, systemPrompt, userPrompt string, schema map[string]interface{}) (string, error) {
	model, prov := c.resolveStage(stage)
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req := Request{
		Model:        model,
		SystemPrompt: systemPrompt,
		UserPrompt:   userPrompt,
		MaxTokens:    DefaultMaxTokens,
	}
	if stage == "execute" {
		req.Temperature = ptrFloat(0)
	}
	resp, err := prov.CompleteJSON(ctx, req, schema)
	if err != nil {
		return "", err
	}
	c.tracker.Record(stage, resp)
	return resp.Text, nil
}

// resolveStage returns the model and provider for a pipeline stage.
// If the stage has a configured override that maps to a different
// provider than the default, a new provider is created on the fly.
func (c *Client) resolveStage(stage string) (string, Provider) {
	if c.stageModels == nil {
		return c.model, c.provider
	}
	stageModel, ok := c.stageModels[stage]
	if !ok || stageModel == "" || stageModel == c.model {
		return c.model, c.provider
	}
	// Different model - check if it needs a different provider.
	stageProvider := ResolveProvider(stageModel)
	if stageProvider == c.provider.Name() {
		// Same provider, different model - just swap the model name.
		return stageModel, c.provider
	}
	// Different provider - create one. This is lightweight (no
	// persistent connection) so creating per-call is fine.
	switch stageProvider {
	case "openai":
		return stageModel, NewOpenAIProvider("", "")
	case "google":
		return stageModel, NewGoogleProvider("")
	case "ollama":
		return stageModel, NewOllamaProvider(OllamaBaseURL)
	default:
		return stageModel, c.provider
	}
}

// SetTimeout overrides the default timeout for LLM calls.
func (c *Client) SetTimeout(d time.Duration) {
	c.timeout = d
}

// Model returns the model name this client is configured to use.
func (c *Client) Model() string {
	return c.model
}

// IsOffline returns whether the client is using a local Ollama instance.
func (c *Client) IsOffline() bool {
	return c.offline
}

// CostSummary returns a human-readable cost breakdown for this session.
func (c *Client) CostSummary() string {
	return c.tracker.Summary()
}

// TotalCost returns the total USD spent in this session.
func (c *Client) TotalCost() float64 {
	return c.tracker.TotalUSD()
}

// CostCalls returns a copy of all recorded LLM call costs.
func (c *Client) CostCalls() []CallCost {
	return c.tracker.Calls()
}

// Provider returns the underlying provider name.
func (c *Client) ProviderName() string {
	if c.provider == nil {
		return "none"
	}
	return c.provider.Name()
}

// extractText is kept for backward compatibility with any internal callers.
func extractText(msg interface{}) string {
	// This is now handled by each provider's buildResponse method.
	return fmt.Sprintf("%v", msg)
}
