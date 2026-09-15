package cloud

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// SessionOptions configures a new managed agent session.
type SessionOptions struct {
	AgentID   string
	EnvID     string
	Title     string
	VaultIDs  []string
	Resources []anthropic.BetaSessionNewParamsResourceUnion
}

// CreateSession creates a new managed agent session.
func CreateSession(ctx context.Context, client *Client, opts SessionOptions) (*anthropic.BetaManagedAgentsSession, error) {
	params := anthropic.BetaSessionNewParams{
		Agent: anthropic.BetaSessionNewParamsAgentUnion{
			OfString: param.NewOpt(opts.AgentID),
		},
		EnvironmentID: opts.EnvID,
		Title:         param.NewOpt(opts.Title),
	}
	if len(opts.VaultIDs) > 0 {
		params.VaultIDs = opts.VaultIDs
	}
	if len(opts.Resources) > 0 {
		params.Resources = opts.Resources
	}
	session, err := client.SDK.Beta.Sessions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("creating session: %w", err)
	}
	return session, nil
}

// GetSession retrieves a session by ID.
func GetSession(ctx context.Context, client *Client, sessionID string) (*anthropic.BetaManagedAgentsSession, error) {
	session, err := client.SDK.Beta.Sessions.Get(ctx, sessionID, anthropic.BetaSessionGetParams{})
	if err != nil {
		return nil, fmt.Errorf("getting session: %w", err)
	}
	return session, nil
}

// ListSessions lists sessions for a given agent.
func ListSessions(ctx context.Context, client *Client, agentID string) ([]anthropic.BetaManagedAgentsSession, error) {
	page, err := client.SDK.Beta.Sessions.List(ctx, anthropic.BetaSessionListParams{
		AgentID: param.NewOpt(agentID),
	})
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	return page.Data, nil
}
