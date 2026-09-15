package llm

import (
	"context"
	"fmt"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider implements Provider for the Anthropic messages API.
type AnthropicProvider struct {
	client anthropic.Client
}

// NewAnthropicProvider creates a provider that talks to Anthropic's API.
func NewAnthropicProvider(apiKey string) *AnthropicProvider {
	ac := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &AnthropicProvider{client: ac}
}

func (p *AnthropicProvider) Name() string { return "anthropic" }

func (p *AnthropicProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserPrompt)),
		},
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic completion failed: %w", err)
	}

	return p.buildResponse(msg, start), nil
}

func (p *AnthropicProvider) CompleteJSON(ctx context.Context, req Request, schema map[string]interface{}) (*Response, error) {
	start := time.Now()
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(maxTokens),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.UserPrompt)),
		},
	}
	if req.SystemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: req.SystemPrompt},
		}
	}
	if req.Temperature != nil {
		params.Temperature = anthropic.Float(*req.Temperature)
	}
	params.OutputConfig = anthropic.OutputConfigParam{
		Format: anthropic.JSONOutputFormatParam{
			Schema: schema,
		},
	}

	msg, err := p.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic json completion failed: %w", err)
	}

	return p.buildResponse(msg, start), nil
}

func (p *AnthropicProvider) buildResponse(msg *anthropic.Message, start time.Time) *Response {
	resp := &Response{
		Text:         extractAnthropicText(msg),
		ProviderName: "anthropic",
		DurationMs:   time.Since(start).Milliseconds(),
	}
	if msg != nil && msg.Usage.InputTokens > 0 {
		resp.InputTokens = int(msg.Usage.InputTokens)
		resp.OutputTokens = int(msg.Usage.OutputTokens)
		resp.Model = string(msg.Model)
	}
	if msg != nil {
		resp.CacheReads = int(msg.Usage.CacheReadInputTokens)
		resp.CacheWrites = int(msg.Usage.CacheCreationInputTokens)
	}
	return resp
}

func extractAnthropicText(msg *anthropic.Message) string {
	if msg == nil || len(msg.Content) == 0 {
		return ""
	}
	block := msg.Content[0]
	if block.Type == "text" {
		return block.Text
	}
	return ""
}
