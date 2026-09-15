package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"time"
)

const (
	openaiMaxRetries = 2
	openaiBaseDelay  = 500 * time.Millisecond
	openaiMaxDelay   = 8 * time.Second
)

// OpenAIProvider implements Provider for the OpenAI chat completions API.
// Also works with OpenAI-compatible endpoints (Groq, Cerebras, Together,
// OpenRouter) by setting a custom BaseURL.
type OpenAIProvider struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewOpenAIProvider creates a provider for OpenAI or compatible APIs.
// If apiKey is "", falls back to OPENAI_API_KEY env var at call time.
// If baseURL is "", uses https://api.openai.com/v1.
func NewOpenAIProvider(apiKey, baseURL string) *OpenAIProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{
		apiKey:  apiKey,
		baseURL: baseURL,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *OpenAIProvider) Name() string { return "openai" }

func (p *OpenAIProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	return p.do(ctx, req, nil)
}

func (p *OpenAIProvider) CompleteJSON(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error) {
	return p.do(ctx, req, schema)
}

// openaiRequest is the chat completions request body.
type openaiRequest struct {
	Model          string            `json:"model"`
	Messages       []openaiMessage   `json:"messages"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	ResponseFormat *openaiRespFormat `json:"response_format,omitempty"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openaiRespFormat struct {
	Type       string            `json:"type"`
	JSONSchema *openaiJSONSchema `json:"json_schema,omitempty"`
}

type openaiJSONSchema struct {
	Name   string                 `json:"name"`
	Schema map[string]interface{} `json:"schema"`
	Strict bool                   `json:"strict"`
}

// openaiResponse is the chat completions response.
type openaiResponse struct {
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage"`
	Model   string         `json:"model"`
}

type openaiChoice struct {
	Message openaiMessage `json:"message"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func isTransientHTTPError(statusCode int) bool {
	return statusCode == 408 || statusCode == 429 || statusCode == 409 || statusCode >= 500
}

func openaiRetryDelay(attempt int) time.Duration {
	delay := time.Duration(float64(openaiBaseDelay) * math.Pow(2, float64(attempt)))
	if delay > openaiMaxDelay {
		delay = openaiMaxDelay
	}
	// Add jitter: subtract up to 25% of the delay
	if jitter := int64(delay / 4); jitter > 0 {
		delay -= time.Duration(rand.Int63n(jitter))
	}
	return delay
}

func (p *OpenAIProvider) do(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error) {
	start := time.Now()

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	messages := make([]openaiMessage, 0, 2)
	if req.SystemPrompt != "" {
		messages = append(messages, openaiMessage{Role: "system", Content: req.SystemPrompt})
	}
	messages = append(messages, openaiMessage{Role: "user", Content: req.UserPrompt})

	body := openaiRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
	}

	if schema != nil {
		body.ResponseFormat = &openaiRespFormat{
			Type: "json_schema",
			JSONSchema: &openaiJSONSchema{
				Name:   "response",
				Schema: schema,
				Strict: true,
			},
		}
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= openaiMaxRetries; attempt++ {
		// Wait before retrying (skip delay on first attempt)
		if attempt > 0 {
			delay := openaiRetryDelay(attempt - 1)
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("openai retry cancelled: %w", lastErr)
			case <-time.After(delay):
			}
		}

		// Re-create the request for each attempt (body reader is consumed)
		httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("openai request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+p.resolveKey())

		httpResp, err := p.http.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("openai http: %w", err)
			continue // network error - retry
		}

		respBody, err := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("openai read: %w", err)
			continue // read error - retry
		}

		if httpResp.StatusCode != 200 {
			lastErr = fmt.Errorf("openai status %d: %s", httpResp.StatusCode, truncateBytes(respBody, 500))
			if !isTransientHTTPError(httpResp.StatusCode) {
				return nil, lastErr // non-transient - fail immediately
			}
			continue // transient - retry
		}

		var oaiResp openaiResponse
		if err := json.Unmarshal(respBody, &oaiResp); err != nil {
			return nil, fmt.Errorf("openai parse: %w", err)
		}

		text := ""
		if len(oaiResp.Choices) > 0 {
			text = oaiResp.Choices[0].Message.Content
		}

		return &Response{
			Text:         text,
			InputTokens:  oaiResp.Usage.PromptTokens,
			OutputTokens: oaiResp.Usage.CompletionTokens,
			Model:        oaiResp.Model,
			ProviderName: "openai",
			DurationMs:   time.Since(start).Milliseconds(),
		}, nil
	}

	return nil, fmt.Errorf("openai retries exhausted: %w", lastErr)
}

func (p *OpenAIProvider) resolveKey() string {
	if p.apiKey != "" {
		return p.apiKey
	}
	return os.Getenv("OPENAI_API_KEY")
}

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
