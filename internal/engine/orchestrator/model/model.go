// Package model defines a provider-agnostic LLM interface for agent
// tool-use loops. It is intentionally independent of any specific
// vendor SDK so that the orchestrator can route a single turn to
// Anthropic, OpenAI, or any future adapter without branching callers.
//
// The design hews close to the Anthropic messages API shape because
// that is the more expressive of the two wire protocols (assistant
// messages may contain mixed text + tool_use blocks; user messages
// may contain mixed text + tool_result blocks). OpenAI's
// chat-completion shape is a subset that the OpenAI adapter
// translates into.
package model

import (
	"context"
	"encoding/json"
	"errors"
)

// Role identifies who produced a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// StopReason is the normalized reason a completion ended.
type StopReason string

const (
	// StopEndTurn means the model produced a final answer and is done.
	StopEndTurn StopReason = "end_turn"
	// StopToolUse means the model produced one or more tool_use blocks
	// and is waiting for the caller to execute them and send results back.
	StopToolUse StopReason = "tool_use"
	// StopMaxTokens means output was truncated by MaxTokens.
	StopMaxTokens StopReason = "max_tokens"
	// StopSequence means the model hit a configured stop sequence.
	StopSequence StopReason = "stop_sequence"
	// StopOther covers provider-specific stop reasons that don't map cleanly.
	StopOther StopReason = "other"
)

// BlockType identifies the kind of content a Block carries.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

// Block is one piece of message content. A single Message may contain
// multiple blocks - e.g., an assistant Message with both text and
// tool_use blocks.
type Block struct {
	Type BlockType

	// Text is set when Type == BlockText.
	Text string

	// Tool use fields (Type == BlockToolUse, produced by assistant).
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage

	// Tool result fields (Type == BlockToolResult, produced by user).
	ToolResultID      string
	ToolResultContent string
	IsError           bool
}

// Message is a single turn from user or assistant.
type Message struct {
	Role   Role
	Blocks []Block
}

// ToolDef describes a tool the model may call. InputSchema is a JSON
// Schema document; both Anthropic and OpenAI accept JSON Schema input
// schemas for function/tool definitions, so the same map works for both.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
}

// Request is the normalized input to a single model completion.
//
// The system prompt is split into two slots so adapters that support
// prompt caching (Anthropic) can mark the stable content cacheable and
// keep the variable per-question content out of the cached prefix:
//
//	SystemPrefix - stable content (instructions, tool list, L1 index).
//	                The Anthropic adapter sends this as a separate
//	                cached TextBlockParam. OpenAI's prompt cache is
//	                automatic on prefix match, so its adapter just
//	                concatenates the two slots.
//	System - variable content (per-question brain context,
//	                resolved entities, retrieval).
//
// Callers that don't care about caching may set just System; the
// SystemPrefix slot is optional.
type Request struct {
	SystemPrefix  string
	System        string
	Messages      []Message
	Tools         []ToolDef
	MaxTokens     int
	Temperature   *float64
	StopSequences []string
}

// Response is the normalized output of a single model completion.
type Response struct {
	StopReason StopReason
	Blocks     []Block
	Usage      Usage
	ModelID    string
}

// Usage reports token counts for a single completion. Cache fields are
// best-effort - providers that don't report cache token counts leave
// them at zero.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
}

// LLM is the single-shot tool-use completion interface every adapter
// implements. Streaming is a separate concern and intentionally not
// part of the minimal core interface - add a Stream method on
// implementations that need it.
type LLM interface {
	// Provider returns a stable identifier ("anthropic", "openai").
	Provider() string
	// Model returns the model ID this LLM is configured to use.
	Model() string
	// Complete runs one completion and returns the full response.
	Complete(ctx context.Context, req Request) (*Response, error)
}

// StreamKind identifies the type of a streaming event.
type StreamKind string

const (
	// StreamTextDelta carries a partial text token from the model.
	StreamTextDelta StreamKind = "text_delta"
	// StreamToolUseDelta carries a partial JSON argument string for a
	// tool call. Callers should accumulate these; the full input is
	// available in the subsequent StreamBlockDone event.
	StreamToolUseDelta StreamKind = "tool_use_delta"
	// StreamBlockDone signals that a complete content block is ready.
	// Block is set to the fully-formed Block (text or tool_use).
	StreamBlockDone StreamKind = "block_done"
	// StreamDone signals end-of-stream. Usage and StopReason are set.
	// If Err is non-nil the stream terminated abnormally.
	StreamDone StreamKind = "done"
)

// StreamEvent is one event emitted by Streamer.Stream.
type StreamEvent struct {
	Kind StreamKind

	// Text is the partial token text when Kind == StreamTextDelta.
	Text string

	// ToolUseID and ToolName identify the tool call for
	// StreamToolUseDelta and StreamBlockDone events.
	ToolUseID string
	ToolName  string

	// ToolPartial is the incremental argument JSON fragment for
	// StreamToolUseDelta events. Callers should concatenate but not
	// parse these until the StreamBlockDone event arrives.
	ToolPartial string

	// Block is the fully-formed content block when Kind == StreamBlockDone.
	// For text blocks Block.Type == BlockText; for tool calls Block.Type == BlockToolUse.
	Block *Block

	// Usage is set on the final StreamDone event.
	Usage *Usage

	// StopReason is set on the final StreamDone event.
	StopReason StopReason

	// Err is set when Kind == StreamDone and the stream terminated with
	// an error. Receiving this terminates the channel.
	Err error
}

// Streamer extends LLM with per-token streaming. Adapters that support
// streaming implement this interface in addition to LLM. Check with a
// type assertion before use:
//
//	if s, ok := llm.(model.Streamer); ok { ... }
type Streamer interface {
	LLM
	// Stream opens a streaming request and returns a channel of events.
	// The channel is closed when the stream ends. A StreamDone event
	// with Err set is sent before close on failure. The caller must
	// drain the channel promptly; the goroutine writing to it respects
	// ctx cancellation to avoid leaking on early exit.
	Stream(ctx context.Context, req Request) (<-chan StreamEvent, error)
}

// ErrUnsupported is returned by adapters for requests they cannot honor
// (e.g., tool schemas that use features the provider doesn't support).
var ErrUnsupported = errors.New("model: unsupported request shape")

// ---------------------------------------------------------------------
// Convenience constructors for common message shapes.
// ---------------------------------------------------------------------

// UserText builds a user message containing a single text block.
func UserText(s string) Message {
	return Message{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: s}}}
}

// AssistantText builds an assistant message containing a single text block.
// Useful for seeding prior-assistant-turn context in test fixtures.
func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockText, Text: s}}}
}

// UserToolResults builds a user message carrying one or more tool_result
// blocks. Both Anthropic and OpenAI represent tool results as user-role
// messages in the rolling history.
func UserToolResults(results ...Block) Message {
	return Message{Role: RoleUser, Blocks: results}
}

// TextBlock builds a text Block.
func TextBlock(s string) Block {
	return Block{Type: BlockText, Text: s}
}

// ToolUseBlock builds an assistant-side tool_use Block.
func ToolUseBlock(id, name string, input json.RawMessage) Block {
	return Block{Type: BlockToolUse, ToolUseID: id, ToolName: name, ToolInput: input}
}

// ToolResultBlock builds a user-side tool_result Block.
func ToolResultBlock(id, content string, isError bool) Block {
	return Block{Type: BlockToolResult, ToolResultID: id, ToolResultContent: content, IsError: isError}
}

// ---------------------------------------------------------------------
// Helpers for navigating Response.Blocks.
// ---------------------------------------------------------------------

// TextOnly concatenates all text blocks in a response in order. Tool_use
// blocks are skipped.
func (r *Response) TextOnly() string {
	if r == nil {
		return ""
	}
	out := ""
	for _, b := range r.Blocks {
		if b.Type == BlockText {
			out += b.Text
		}
	}
	return out
}

// ToolCalls returns only the tool_use blocks from a response, in order.
func (r *Response) ToolCalls() []Block {
	if r == nil {
		return nil
	}
	var calls []Block
	for _, b := range r.Blocks {
		if b.Type == BlockToolUse {
			calls = append(calls, b)
		}
	}
	return calls
}
