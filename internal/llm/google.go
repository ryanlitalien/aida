package llm

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// GoogleProvider talks to the Gemini API through its OpenAI-compatible
// chat completions endpoint. Like OllamaProvider this is a thin wrapper
// over OpenAIProvider with the base URL and credential swapped - Gemini's
// compat layer supports response_format json_schema, so CompleteJSON keeps
// working without a second code path.
type GoogleProvider struct {
	inner  *OpenAIProvider
	hasKey bool
}

// NewGoogleProvider creates a provider pointed at the Gemini OpenAI-compat
// endpoint. Passing "" for baseURL uses GeminiBaseURL. The credential is
// read from GEMINI_API_KEY, falling back to GOOGLE_API_KEY.
func NewGoogleProvider(baseURL string) *GoogleProvider {
	if baseURL == "" {
		baseURL = GeminiBaseURL
	}
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		key = os.Getenv("GOOGLE_API_KEY")
	}
	return &GoogleProvider{
		inner:  NewOpenAIProvider(key, baseURL),
		hasKey: key != "",
	}
}

func (p *GoogleProvider) Name() string { return "google" }

func (p *GoogleProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	req.Model = stripGoogleModelPrefix(req.Model)
	resp, err := p.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.ProviderName = "google"
	return resp, nil
}

func (p *GoogleProvider) CompleteJSON(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	req.Model = stripGoogleModelPrefix(req.Model)
	resp, err := p.inner.CompleteJSON(ctx, req, schema)
	if err != nil {
		return nil, err
	}
	resp.ProviderName = "google"
	return resp, nil
}

// check fails fast when no Gemini credential is present. Without this the
// wrapped OpenAIProvider would silently fall back to OPENAI_API_KEY and send
// an OpenAI key to Google, producing a 401 that reads like a Gemini outage.
func (p *GoogleProvider) check() error {
	if !p.hasKey {
		return fmt.Errorf("google: no credential - set GEMINI_API_KEY or GOOGLE_API_KEY")
	}
	return nil
}

// stripGoogleModelPrefix removes a "google:" / "gemini:" qualifier so the bare
// model ID reaches the API. ResolveProvider accepts the prefixed form, so a
// config can legitimately say "google:gemini-3.7-flash".
func stripGoogleModelPrefix(model string) string {
	idx := strings.Index(model, ":")
	if idx <= 0 {
		return model
	}
	switch strings.ToLower(model[:idx]) {
	case "google", "gemini":
		return model[idx+1:]
	}
	return model
}
