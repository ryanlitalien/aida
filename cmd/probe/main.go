// Package probe is a compile-time sanity check for the Anthropic SDK
// streaming event field names used in the orchestrator adapter.
// It is not intended to be run; it exists to fail fast if the SDK
// changes its type layout.
package main

import (
	"context"
	"fmt"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

func main() {
	c := sdk.NewClient(option.WithAPIKey("test"))
	params := sdk.MessageNewParams{
		Model:     sdk.Model("claude-haiku-4-5"),
		MaxTokens: 100,
		Messages:  []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock("hello"))},
	}
	stream := c.Messages.NewStreaming(context.Background(), params)
	for stream.Next() {
		ev := stream.Current()

		// Fields used in runAnthropicStream - compile error here means the
		// SDK type layout has changed and the adapter needs updating.
		_ = ev.Type
		_ = ev.Index
		_ = ev.Message.Usage.InputTokens
		_ = ev.Message.Usage.CacheReadInputTokens
		_ = ev.Message.Usage.CacheCreationInputTokens
		_ = ev.ContentBlock.Type
		_ = ev.ContentBlock.ID
		_ = ev.ContentBlock.Name
		_ = ev.Delta.Type
		_ = ev.Delta.Text
		_ = ev.Delta.PartialJSON
		_ = ev.Delta.StopReason
		_ = ev.Usage.OutputTokens

		fmt.Printf("event type: %s\n", ev.Type)
	}
	_ = stream.Err()
}
