package cloud

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/ryanlitalien/aida/internal/config"
)

const defaultAgentModel = "claude-sonnet-5"

// EnsureAgent is an idempotent get-or-create for a managed agent. If the
// profile already has an agent_id, it tries a GET. If the agent no longer
// exists (deleted externally), it creates a new one and writes the ID back
// to config.
func EnsureAgent(ctx context.Context, client *Client, cfg *config.Config, profileName string) (string, error) {
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return "", fmt.Errorf("profile %q not found", profileName)
	}

	// Try existing agent
	if profile.Cloud != nil && profile.Cloud.AgentID != "" {
		_, err := client.SDK.Beta.Agents.Get(ctx, profile.Cloud.AgentID, anthropic.BetaAgentGetParams{})
		if err == nil {
			return profile.Cloud.AgentID, nil
		}
		// Agent was deleted or inaccessible; re-create below.
	}

	model := defaultAgentModel
	if profile.Cloud != nil && profile.Cloud.Model != "" {
		model = profile.Cloud.Model
	}

	agent, err := client.SDK.Beta.Agents.New(ctx, anthropic.BetaAgentNewParams{
		Name: fmt.Sprintf("aida-investigator-%s", profileName),
		Model: anthropic.BetaManagedAgentsModelConfigParams{
			ID: anthropic.BetaManagedAgentsModel(model),
		},
		System: param.NewOpt(InvestigatorSystemPrompt),
		Tools: []anthropic.BetaAgentNewParamsToolUnion{{
			OfAgentToolset20260401: &anthropic.BetaManagedAgentsAgentToolset20260401Params{
				Type: anthropic.BetaManagedAgentsAgentToolset20260401ParamsTypeAgentToolset20260401,
			},
		}},
	})
	if err != nil {
		return "", fmt.Errorf("creating agent: %w", err)
	}

	// Write agent ID back to config.
	if profile.Cloud == nil {
		profile.Cloud = &config.CloudConfig{}
	}
	profile.Cloud.AgentID = agent.ID
	cfg.Profiles[profileName] = profile
	if err := config.SaveConfig(cfg); err != nil {
		return agent.ID, fmt.Errorf("saving agent_id to config: %w", err)
	}

	return agent.ID, nil
}
