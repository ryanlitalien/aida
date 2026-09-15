package cloud

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/ryanlitalien/aida/internal/config"
)

// EnsureEnvironment is an idempotent get-or-create for a cloud environment.
// If the profile already has an environment_id, it tries a GET. If the
// environment no longer exists, it creates a new one and writes the ID back
// to config.
func EnsureEnvironment(ctx context.Context, client *Client, cfg *config.Config, profileName string) (string, error) {
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return "", fmt.Errorf("profile %q not found", profileName)
	}

	// Try existing environment
	if profile.Cloud != nil && profile.Cloud.EnvironmentID != "" {
		_, err := client.SDK.Beta.Environments.Get(ctx, profile.Cloud.EnvironmentID, anthropic.BetaEnvironmentGetParams{})
		if err == nil {
			return profile.Cloud.EnvironmentID, nil
		}
		// Environment was deleted or inaccessible; re-create below.
	}

	env, err := client.SDK.Beta.Environments.New(ctx, anthropic.BetaEnvironmentNewParams{
		Name: fmt.Sprintf("aida-env-%s", profileName),
		Config: anthropic.BetaCloudConfigParams{
			Networking: anthropic.BetaCloudConfigParamsNetworkingUnion{
				OfUnrestricted: &anthropic.BetaUnrestrictedNetworkParam{},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating environment: %w", err)
	}

	// Write environment ID back to config.
	if profile.Cloud == nil {
		profile.Cloud = &config.CloudConfig{}
	}
	profile.Cloud.EnvironmentID = env.ID
	cfg.Profiles[profileName] = profile
	if err := config.SaveConfig(cfg); err != nil {
		return env.ID, fmt.Errorf("saving environment_id to config: %w", err)
	}

	return env.ID, nil
}
