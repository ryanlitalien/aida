package cloud

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
)

// ManagedRunnable wraps an Anthropic Managed Agent session as an
// orchestrator.Runnable. Each Run() creates a new session, sends the
// input as a user message, polls until the session reaches idle or
// terminated, and returns the agent's final text output.
//
// This lets managed agents participate in orchestrator graphs alongside
// local agents and A2A remotes.
type ManagedRunnable struct {
	Name_    string
	Client   *Client
	AgentID  string
	EnvID    string
	VaultIDs []string

	// PollInterval controls how often we check session status.
	// Defaults to 3s if zero.
	PollInterval time.Duration
}

// Run implements orchestrator.Runnable.
func (m *ManagedRunnable) Run(ctx context.Context, input string, _ ...orchestrator.RunOption) (*orchestrator.RunResult, error) {
	pollInterval := m.PollInterval
	if pollInterval == 0 {
		pollInterval = 3 * time.Second
	}

	// 1. Create session.
	session, err := CreateSession(ctx, m.Client, SessionOptions{
		AgentID:  m.AgentID,
		EnvID:    m.EnvID,
		Title:    truncateTitle(input, 80),
		VaultIDs: m.VaultIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("managed runnable %q: %w", m.Name_, err)
	}

	// 2. Send the input as a user message.
	if err := SendMessage(ctx, m.Client, session.ID, input); err != nil {
		return nil, fmt.Errorf("managed runnable %q: %w", m.Name_, err)
	}

	// 3. Poll until done.
	var finalStatus string
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}

		s, err := GetSession(ctx, m.Client, session.ID)
		if err != nil {
			return nil, fmt.Errorf("managed runnable %q polling: %w", m.Name_, err)
		}
		finalStatus = string(s.Status)
		if finalStatus == "idle" || finalStatus == "terminated" || finalStatus == "error" {
			break
		}
	}

	// 4. Extract agent messages from the event history.
	output, err := m.extractOutput(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("managed runnable %q reading output: %w", m.Name_, err)
	}

	if finalStatus == "error" && output == "" {
		output = "(managed agent session ended with error)"
	}

	return &orchestrator.RunResult{
		FinalOutput: output,
		HandoffPath: []string{m.Name_},
	}, nil
}

// extractOutput lists session events and concatenates all agent message
// text blocks into the final output string.
func (m *ManagedRunnable) extractOutput(ctx context.Context, sessionID string) (string, error) {
	page, err := m.Client.SDK.Beta.Sessions.Events.List(ctx, sessionID, anthropic.BetaSessionEventListParams{})
	if err != nil {
		return "", err
	}

	var parts []string
	for _, event := range page.Data {
		if event.Type == "agent.message" {
			msg := event.AsAgentMessage()
			for _, block := range msg.Content {
				if block.Text != "" {
					parts = append(parts, block.Text)
				}
			}
		}
	}
	return strings.Join(parts, "\n"), nil
}

func truncateTitle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-2] + ".."
}
