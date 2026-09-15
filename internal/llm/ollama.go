package llm

import "context"

// OllamaProvider talks to a local Ollama instance via its
// OpenAI-compatible chat completions endpoint. This is a thin wrapper
// over OpenAIProvider with the base URL pointed at localhost:11434.
type OllamaProvider struct {
	inner *OpenAIProvider
}

// NewOllamaProvider creates a provider pointed at the Ollama endpoint.
func NewOllamaProvider(baseURL string) *OllamaProvider {
	return &OllamaProvider{
		inner: NewOpenAIProvider("ollama", baseURL),
	}
}

func (p *OllamaProvider) Name() string { return "ollama" }

func (p *OllamaProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	resp, err := p.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.ProviderName = "ollama"
	return resp, nil
}

func (p *OllamaProvider) CompleteJSON(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error) {
	// Ollama's OpenAI-compat endpoint may not support json_schema
	// response_format reliably. Try it - if the model doesn't honor
	// the schema, the caller's JSON parse will catch it and the
	// pipeline degrades gracefully.
	resp, err := p.inner.CompleteJSON(ctx, req, schema)
	if err != nil {
		return nil, err
	}
	resp.ProviderName = "ollama"
	return resp, nil
}
