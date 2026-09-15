// Package cloud wraps the Anthropic SDK's managed agents API for per-profile
// cloud investigations.
package cloud

import (
	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Client is a thin wrapper around the Anthropic SDK client, scoped to a single
// API key. Each profile creates its own Client for credential isolation.
type Client struct {
	SDK *anthropic.Client
}

// NewClient creates a Client using the given API key.
func NewClient(apiKey string) *Client {
	sdk := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Client{SDK: &sdk}
}
