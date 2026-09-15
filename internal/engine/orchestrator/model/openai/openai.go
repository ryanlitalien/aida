// Package openai is the model.LLM adapter for OpenAI's chat-completions API.
//
// OpenAI's wire shape differs from the unified model.Message shape in
// two places: tool results are carried as their own role ("tool") rather
// than as blocks inside a user message, and assistant messages carry
// tool calls as a sibling field rather than inline blocks. This adapter
// bridges both directions.
package openai

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

const defaultMaxTokens = 4096

// Client is the OpenAI chat-completions implementation of model.LLM.
type Client struct {
	sdk   sdk.Client
	model string
}

// New constructs a Client using the given API key and model ID.
func New(apiKey, modelID string) *Client {
	c := sdk.NewClient(option.WithAPIKey(apiKey))
	return &Client{sdk: c, model: modelID}
}

// NewWithOptions is useful in tests: pass option.WithBaseURL pointing
// at an httptest server to intercept requests.
func NewWithOptions(modelID string, opts ...option.RequestOption) *Client {
	c := sdk.NewClient(opts...)
	return &Client{sdk: c, model: modelID}
}

// Provider returns "openai".
func (c *Client) Provider() string { return "openai" }

// Model returns the configured model ID.
func (c *Client) Model() string { return c.model }

// Complete runs one chat completion.
func (c *Client) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	params, err := buildParams(c.model, req)
	if err != nil {
		return nil, err
	}

	completion, err := c.sdk.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("openai chat.completions.new: %w", err)
	}

	return buildResponse(completion), nil
}

// Stream implements model.Streamer. It opens an OpenAI streaming chat
// completion and fans events into the returned channel.
func (c *Client) Stream(ctx context.Context, req model.Request) (<-chan model.StreamEvent, error) {
	params, err := buildParams(c.model, req)
	if err != nil {
		return nil, err
	}

	ch := make(chan model.StreamEvent, 64)
	go func() {
		defer close(ch)
		runOpenAIStream(ctx, c.sdk.Chat.Completions.NewStreaming(ctx, params), ch)
	}()
	return ch, nil
}

// openAIToolState accumulates one tool call across streaming chunks.
type openAIToolState struct {
	id      string
	name    string
	partial string
}

// runOpenAIStream reads from the OpenAI SSE stream and sends model.StreamEvents
// to ch. It is run inside a goroutine; ch must not be closed by the caller.
func runOpenAIStream(ctx context.Context, stream interface {
	Next() bool
	Current() sdk.ChatCompletionChunk
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

	tools := make(map[int]*openAIToolState) // chunk index → state
	var textBuf string
	var stopReason model.StopReason

	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		// Text delta.
		if choice.Delta.Content != "" {
			textBuf += choice.Delta.Content
			if !send(model.StreamEvent{Kind: model.StreamTextDelta, Text: choice.Delta.Content}) {
				return
			}
		}

		// Tool call deltas. OpenAI sends multiple chunks per tool call,
		// accumulating arguments. The id and name arrive only in the
		// first chunk for that index.
		for _, tc := range choice.Delta.ToolCalls {
			idx := int(tc.Index)
			ts, exists := tools[idx]
			if !exists {
				ts = &openAIToolState{}
				tools[idx] = ts
			}
			if tc.ID != "" {
				ts.id = tc.ID
			}
			if tc.Function.Name != "" {
				ts.name = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				ts.partial += tc.Function.Arguments
				if !send(model.StreamEvent{
					Kind:        model.StreamToolUseDelta,
					ToolUseID:   ts.id,
					ToolName:    ts.name,
					ToolPartial: tc.Function.Arguments,
				}) {
					return
				}
			}
		}

		if choice.FinishReason != "" {
			stopReason = normalizeFinishReason(string(choice.FinishReason))
		}
	}

	if err := stream.Err(); err != nil {
		send(model.StreamEvent{Kind: model.StreamDone, Err: err})
		return
	}

	// Emit block_done for accumulated text (if any).
	if textBuf != "" {
		blk := model.TextBlock(textBuf)
		if !send(model.StreamEvent{Kind: model.StreamBlockDone, Block: &blk}) {
			return
		}
	}

	// Emit block_done for all accumulated tool calls (ordered by index).
	for i := 0; ; i++ {
		ts, ok := tools[i]
		if !ok {
			break
		}
		partial := ts.partial
		if partial == "" {
			partial = "{}"
		}
		blk := model.ToolUseBlock(ts.id, ts.name, json.RawMessage(partial))
		if !send(model.StreamEvent{
			Kind:      model.StreamBlockDone,
			ToolUseID: ts.id,
			ToolName:  ts.name,
			Block:     &blk,
		}) {
			return
		}
	}

	send(model.StreamEvent{Kind: model.StreamDone, StopReason: stopReason})
}

// ---------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------

func buildParams(modelID string, req model.Request) (sdk.ChatCompletionNewParams, error) {
	// OpenAI's prompt cache is automatic on prefix match (≥1024
	// tokens), so we just concatenate prefix + system and let the
	// adapter put the stable bit first. No explicit cache_control
	// breakpoint needed.
	system := req.SystemPrefix
	if req.System != "" {
		if system != "" {
			system += "\n\n"
		}
		system += req.System
	}
	messages, err := translateMessages(system, req.Messages)
	if err != nil {
		return sdk.ChatCompletionNewParams{}, err
	}

	tools, err := translateTools(req.Tools)
	if err != nil {
		return sdk.ChatCompletionNewParams{}, err
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}

	params := sdk.ChatCompletionNewParams{
		Model:               shared.ChatModel(modelID),
		Messages:            messages,
		MaxCompletionTokens: param.NewOpt(int64(maxTokens)),
	}
	if len(tools) > 0 {
		params.Tools = tools
	}
	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if len(req.StopSequences) > 0 {
		params.Stop = sdk.ChatCompletionNewParamsStopUnion{OfStringArray: req.StopSequences}
	}
	return params, nil
}

// translateMessages converts the unified message list into OpenAI's
// message-union list, prepending a system message if req.System is set
// and splitting any user messages that carry tool_result blocks into
// separate role-tool messages.
func translateMessages(system string, msgs []model.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	out := make([]sdk.ChatCompletionMessageParamUnion, 0, len(msgs)+1)
	if system != "" {
		out = append(out, sdk.SystemMessage(system))
	}
	for _, m := range msgs {
		converted, err := translateOneMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, converted...)
	}
	return out, nil
}

// translateOneMessage converts a single unified message into one or
// more OpenAI messages. A user message carrying tool_result blocks is
// split: any text block becomes a user message, and each tool_result
// block becomes its own role-tool message.
func translateOneMessage(m model.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case model.RoleUser:
		return translateUserMessage(m.Blocks)
	case model.RoleAssistant:
		asst, err := translateAssistantMessage(m.Blocks)
		if err != nil {
			return nil, err
		}
		return []sdk.ChatCompletionMessageParamUnion{asst}, nil
	default:
		return nil, fmt.Errorf("openai: unknown role %q", m.Role)
	}
}

func translateUserMessage(blocks []model.Block) ([]sdk.ChatCompletionMessageParamUnion, error) {
	var out []sdk.ChatCompletionMessageParamUnion
	var textParts []string
	for _, b := range blocks {
		switch b.Type {
		case model.BlockText:
			textParts = append(textParts, b.Text)
		case model.BlockToolResult:
			out = append(out, sdk.ToolMessage(b.ToolResultContent, b.ToolResultID))
		case model.BlockToolUse:
			return nil, fmt.Errorf("openai: tool_use block in user message is not allowed")
		default:
			return nil, fmt.Errorf("openai: unknown block type %q", b.Type)
		}
	}
	if len(textParts) > 0 {
		combined := textParts[0]
		for _, t := range textParts[1:] {
			combined += "\n" + t
		}
		out = append([]sdk.ChatCompletionMessageParamUnion{sdk.UserMessage(combined)}, out...)
	}
	return out, nil
}

func translateAssistantMessage(blocks []model.Block) (sdk.ChatCompletionMessageParamUnion, error) {
	var textParts []string
	var toolCalls []sdk.ChatCompletionMessageToolCallParam
	for _, b := range blocks {
		switch b.Type {
		case model.BlockText:
			textParts = append(textParts, b.Text)
		case model.BlockToolUse:
			args := string(b.ToolInput)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, sdk.ChatCompletionMessageToolCallParam{
				ID: b.ToolUseID,
				Function: sdk.ChatCompletionMessageToolCallFunctionParam{
					Name:      b.ToolName,
					Arguments: args,
				},
			})
		case model.BlockToolResult:
			return sdk.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai: tool_result block in assistant message is not allowed")
		default:
			return sdk.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai: unknown block type %q", b.Type)
		}
	}

	combined := ""
	if len(textParts) > 0 {
		combined = textParts[0]
		for _, t := range textParts[1:] {
			combined += "\n" + t
		}
	}

	asst := sdk.ChatCompletionAssistantMessageParam{}
	if combined != "" {
		asst.Content.OfString = param.NewOpt(combined)
	}
	if len(toolCalls) > 0 {
		asst.ToolCalls = toolCalls
	}
	return sdk.ChatCompletionMessageParamUnion{OfAssistant: &asst}, nil
}

func translateTools(tools []model.ToolDef) ([]sdk.ChatCompletionToolParam, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]sdk.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		params := shared.FunctionParameters{}
		for k, v := range t.InputSchema {
			params[k] = v
		}
		def := shared.FunctionDefinitionParam{
			Name:       t.Name,
			Parameters: params,
		}
		if t.Description != "" {
			def.Description = param.NewOpt(t.Description)
		}
		out = append(out, sdk.ChatCompletionToolParam{Function: def})
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Response translation
// ---------------------------------------------------------------------

func buildResponse(c *sdk.ChatCompletion) *model.Response {
	if c == nil || len(c.Choices) == 0 {
		return &model.Response{StopReason: model.StopOther}
	}
	choice := c.Choices[0]
	resp := &model.Response{
		ModelID:    c.Model,
		StopReason: normalizeFinishReason(choice.FinishReason),
		Usage: model.Usage{
			InputTokens:     int(c.Usage.PromptTokens),
			OutputTokens:    int(c.Usage.CompletionTokens),
			CacheReadTokens: int(c.Usage.PromptTokensDetails.CachedTokens),
		},
	}
	if choice.Message.Content != "" {
		resp.Blocks = append(resp.Blocks, model.TextBlock(choice.Message.Content))
	}
	for _, tc := range choice.Message.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		resp.Blocks = append(resp.Blocks, model.ToolUseBlock(
			tc.ID,
			tc.Function.Name,
			json.RawMessage(args),
		))
	}
	return resp
}

func normalizeFinishReason(s string) model.StopReason {
	switch s {
	case "stop":
		return model.StopEndTurn
	case "tool_calls":
		return model.StopToolUse
	case "length":
		return model.StopMaxTokens
	case "content_filter":
		return model.StopOther
	case "function_call":
		return model.StopToolUse
	default:
		return model.StopOther
	}
}
