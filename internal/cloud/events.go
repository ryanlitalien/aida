package cloud

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// SendMessage sends a user message event to an active session.
func SendMessage(ctx context.Context, client *Client, sessionID, text string) error {
	_, err := client.SDK.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{
		Events: []anthropic.BetaManagedAgentsEventParamsUnion{
			{
				OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
					Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
						OfText: &anthropic.BetaManagedAgentsTextBlockParam{
							Text: text,
							Type: anthropic.BetaManagedAgentsTextBlockTypeText,
						},
					}},
					Type: anthropic.BetaManagedAgentsUserMessageEventParamsTypeUserMessage,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("sending message: %w", err)
	}
	return nil
}

// EventHandler is called for each streamed event.
type EventHandler func(event anthropic.BetaManagedAgentsStreamSessionEventsUnion) bool

// StreamEvents opens an SSE stream for a session and calls handler for each
// event. Returns when the handler returns false or the stream ends.
func StreamEvents(ctx context.Context, client *Client, sessionID string, handler EventHandler) error {
	stream := client.SDK.Beta.Sessions.Events.StreamEvents(ctx, sessionID, anthropic.BetaSessionEventStreamParams{})
	return processStream(stream, handler)
}

func processStream(stream *ssestream.Stream[anthropic.BetaManagedAgentsStreamSessionEventsUnion], handler EventHandler) error {
	defer stream.Close()
	for stream.Next() {
		event := stream.Current()
		if !handler(event) {
			return nil
		}
	}
	return stream.Err()
}
