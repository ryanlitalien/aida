// Package anthropic is the model.LLM adapter for Anthropic's messages API.
//
// It translates the provider-agnostic model.Request / model.Response
// types to and from anthropic-sdk-go types. The adapter is a thin
// marshaling layer and holds no state beyond the SDK client.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

const defaultMaxTokens = 4096

// Client is the Anthropic messages-api implementation of model.LLM.
type Client struct {
	sdk   sdk.Client
	model string
}

// New constructs a Client using the given API key and model ID.
func New(apiKey, modelID string) *Client {
	c := sdk.NewClient(option.WithAPIKey(apiKey))
	return &Client{sdk: c, model: modelID}
}

// NewFromClient wraps an already-constructed SDK client. Useful in
// tests where the SDK client is configured with a custom HTTP
// transport pointing at a mock server.
func NewFromClient(c sdk.Client, modelID string) *Client {
	return &Client{sdk: c, model: modelID}
}

// Provider returns "anthropic".
func (c *Client) Provider() string { return "anthropic" }

// Model returns the configured model ID.
func (c *Client) Model() string { return c.model }

// Complete runs one Anthropic completion.
func (c *Client) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	params, err := buildParams(c.model, req)
	if err != nil {
		return nil, err
	}

	msg, err := c.sdk.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("anthropic messages.new: %w", err)
	}

	return buildResponse(msg), nil
}

// Stream implements model.Streamer. It opens an Anthropic streaming
// messages request and fans events into the returned channel. The
// goroutine exits when the SSE stream ends or ctx is cancelled.
func (c *Client) Stream(ctx context.Context, req model.Request) (<-chan model.StreamEvent, error) {
	params, err := buildParams(c.model, req)
	if err != nil {
		return nil, err
	}

	ch := make(chan model.StreamEvent, 64)
	go func() {
		defer close(ch)
		runAnthropicStream(ctx, c.sdk.Messages.NewStreaming(ctx, params), ch)
	}()
	return ch, nil
}

// blockMeta tracks state for a single in-flight content block.
type blockMeta struct {
	blockType string // "text" or "tool_use"
	id        string // tool_use only
	name      string // tool_use only
	textBuf   string // accumulated text for text blocks
	partial   string // accumulated partial JSON for tool_use blocks
}

// runAnthropicStream reads from the SSE stream and sends model.StreamEvents
// to ch. It is run inside a goroutine; ch must not be closed by the caller.
func runAnthropicStream(ctx context.Context, stream interface {
	Next() bool
	Current() sdk.MessageStreamEventUnion
	Err() error
	Close() error
}, ch chan<- model.StreamEvent) {
	defer stream.Close()

	send := func(ev model.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	blocks := make(map[int64]*blockMeta)
	var stopReason model.StopReason
	var usage model.Usage

	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {

		case "message_start":
			usage.InputTokens = int(ev.Message.Usage.InputTokens)
			usage.CacheReadTokens = int(ev.Message.Usage.CacheReadInputTokens)
			usage.CacheWriteTokens = int(ev.Message.Usage.CacheCreationInputTokens)

		case "content_block_start":
			bm := &blockMeta{blockType: ev.ContentBlock.Type}
			if ev.ContentBlock.Type == "tool_use" {
				bm.id = ev.ContentBlock.ID
				bm.name = ev.ContentBlock.Name
			}
			blocks[ev.Index] = bm

		case "content_block_delta":
			bm, ok := blocks[ev.Index]
			if !ok {
				break
			}
			switch ev.Delta.Type {
			case "text_delta":
				bm.textBuf += ev.Delta.Text
				if !send(model.StreamEvent{Kind: model.StreamTextDelta, Text: ev.Delta.Text}) {
					return
				}
			case "input_json_delta":
				bm.partial += ev.Delta.PartialJSON
				if !send(model.StreamEvent{
					Kind:        model.StreamToolUseDelta,
					ToolUseID:   bm.id,
					ToolName:    bm.name,
					ToolPartial: ev.Delta.PartialJSON,
				}) {
					return
				}
			}

		case "content_block_stop":
			bm, ok := blocks[ev.Index]
			if !ok {
				break
			}
			var blk model.Block
			switch bm.blockType {
			case "text":
				blk = model.TextBlock(bm.textBuf)
			case "tool_use":
				partial := bm.partial
				if partial == "" {
					partial = "{}"
				}
				blk = model.ToolUseBlock(bm.id, bm.name, json.RawMessage(partial))
			default:
				delete(blocks, ev.Index)
				continue
			}
			evOut := model.StreamEvent{
				Kind:  model.StreamBlockDone,
				Block: &blk,
			}
			if bm.blockType == "tool_use" {
				evOut.ToolUseID = bm.id
				evOut.ToolName = bm.name
			}
			if !send(evOut) {
				return
			}
			delete(blocks, ev.Index)

		case "message_delta":
			stopReason = normalizeStopReason(string(ev.Delta.StopReason))
			usage.OutputTokens = int(ev.Usage.OutputTokens)

		case "message_stop":
			usageCopy := usage
			send(model.StreamEvent{
				Kind:       model.StreamDone,
				StopReason: stopReason,
				Usage:      &usageCopy,
			})
			return
		}
	}

	if err := stream.Err(); err != nil {
		send(model.StreamEvent{Kind: model.StreamDone, Err: err})
	}
}

// ---------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------

func buildParams(modelID string, req model.Request) (sdk.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	messages, err := translateMessages(req.Messages)
	if err != nil {
		return sdk.MessageNewParams{}, err
	}

	tools, err := translateTools(req.Tools)
	if err != nil {
		return sdk.MessageNewParams{}, err
	}

	params := sdk.MessageNewParams{
		Model:     sdk.Model(modelID),
		MaxTokens: int64(maxTokens),
		Messages:  messages,
	}
	// System prompt: emit the optional stable prefix as its own
	// TextBlockParam with cache_control: ephemeral so the Anthropic
	// prompt cache will hit it on repeat calls within the 5-minute
	// TTL. The variable per-question portion follows as an uncached
	// block. Empty slots are skipped - single-block path is preserved
	// for callers that only set System.
	if req.SystemPrefix != "" {
		params.System = append(params.System, sdk.TextBlockParam{
			Text:         req.SystemPrefix,
			CacheControl: sdk.NewCacheControlEphemeralParam(),
		})
	}
	if req.System != "" {
		params.System = append(params.System, sdk.TextBlockParam{Text: req.System})
	}
	if req.Temperature != nil {
		params.Temperature = sdk.Float(*req.Temperature)
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if len(req.StopSequences) > 0 {
		params.StopSequences = req.StopSequences
	}
	return params, nil
}

func translateMessages(msgs []model.Message) ([]sdk.MessageParam, error) {
	out := make([]sdk.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		blocks, err := translateBlocksToAnthropic(m.Blocks)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case model.RoleUser:
			out = append(out, sdk.NewUserMessage(blocks...))
		case model.RoleAssistant:
			out = append(out, sdk.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("anthropic: unknown role %q", m.Role)
		}
	}
	return out, nil
}

func translateBlocksToAnthropic(blocks []model.Block) ([]sdk.ContentBlockParamUnion, error) {
	out := make([]sdk.ContentBlockParamUnion, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case model.BlockText:
			out = append(out, sdk.NewTextBlock(b.Text))
		case model.BlockToolUse:
			var input interface{}
			if len(b.ToolInput) > 0 {
				if err := json.Unmarshal(b.ToolInput, &input); err != nil {
					return nil, fmt.Errorf("anthropic: tool_use input not valid JSON: %w", err)
				}
			}
			out = append(out, sdk.ContentBlockParamUnion{
				OfToolUse: &sdk.ToolUseBlockParam{
					ID:    b.ToolUseID,
					Name:  b.ToolName,
					Input: input,
				},
			})
		case model.BlockToolResult:
			tr := &sdk.ToolResultBlockParam{
				ToolUseID: b.ToolResultID,
				Content: []sdk.ToolResultBlockParamContentUnion{
					{OfText: &sdk.TextBlockParam{Text: b.ToolResultContent}},
				},
			}
			if b.IsError {
				tr.IsError = sdk.Bool(true)
			}
			out = append(out, sdk.ContentBlockParamUnion{OfToolResult: tr})
		default:
			return nil, fmt.Errorf("anthropic: unknown block type %q", b.Type)
		}
	}
	return out, nil
}

func translateTools(tools []model.ToolDef) ([]sdk.ToolUnionParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := sdk.ToolInputSchemaParam{}
		if props, ok := t.InputSchema["properties"]; ok {
			schema.Properties = props
		}
		schema.Required = extractRequired(t.InputSchema)

		tool := sdk.ToolParam{
			Name:        t.Name,
			Description: sdk.String(t.Description),
			InputSchema: schema,
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &tool})
	}
	return out, nil
}

func extractRequired(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, r := range v {
			if s, ok := r.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ---------------------------------------------------------------------
// Response translation
// ---------------------------------------------------------------------

func buildResponse(msg *sdk.Message) *model.Response {
	if msg == nil {
		return &model.Response{StopReason: model.StopOther}
	}

	resp := &model.Response{
		StopReason: normalizeStopReason(string(msg.StopReason)),
		ModelID:    string(msg.Model),
		Usage: model.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	}

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			resp.Blocks = append(resp.Blocks, model.TextBlock(block.Text))
		case "tool_use":
			input, _ := json.Marshal(block.Input)
			resp.Blocks = append(resp.Blocks, model.ToolUseBlock(block.ID, block.Name, input))
		}
	}
	return resp
}

func normalizeStopReason(s string) model.StopReason {
	switch s {
	case "end_turn":
		return model.StopEndTurn
	case "tool_use":
		return model.StopToolUse
	case "max_tokens":
		return model.StopMaxTokens
	case "stop_sequence":
		return model.StopSequence
	case "":
		// Anthropic sometimes omits stop_reason on error paths.
		return model.StopOther
	default:
		return model.StopOther
	}
}
