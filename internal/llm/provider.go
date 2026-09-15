package llm

import "context"

// Provider is the abstraction layer between aida and LLM APIs.
// Each implementation knows how to talk to one wire protocol (Anthropic
// messages API, OpenAI chat completions, Google generateContent, etc.).
// Inspired by pi-mono's pi-ai ApiProvider dispatch pattern.
type Provider interface {
	// Name returns the provider identifier ("anthropic", "openai", "google", "ollama").
	Name() string

	// Complete sends a non-streaming request and returns the text response.
	Complete(ctx context.Context, req Request) (*Response, error)

	// CompleteJSON sends a request with JSON schema enforcement.
	// Providers that don't support native schema enforcement should
	// fall back to prompting for JSON and parsing the response.
	CompleteJSON(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error)
}

// Request is the provider-agnostic input to an LLM call.
type Request struct {
	Model        string
	SystemPrompt string
	UserPrompt   string
	MaxTokens    int
	Temperature  *float64 // nil = provider default; 0 = deterministic
}

// Response is the provider-agnostic output from an LLM call.
type Response struct {
	Text         string
	InputTokens  int
	OutputTokens int
	CacheReads   int
	CacheWrites  int
	Model        string  // actual model used (may differ from requested if aliased)
	ProviderName string  // "anthropic", "openai", "google", "ollama"
	Cost         float64 // USD, computed from token counts + model pricing
	DurationMs   int64
}
