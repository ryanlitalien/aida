package cloud

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/ryanlitalien/aida/internal/config"
)

// EnsureVault is an idempotent get-or-create for a vault associated with a
// profile. If the profile already has a vault_id, it tries a GET. If the vault
// no longer exists, it creates a new one and writes the ID back to config.
func EnsureVault(ctx context.Context, client *Client, cfg *config.Config, profileName string) (string, error) {
	profile, ok := cfg.Profiles[profileName]
	if !ok {
		return "", fmt.Errorf("profile %q not found", profileName)
	}

	// Try existing vault
	if profile.Cloud != nil && profile.Cloud.VaultID != "" {
		_, err := client.SDK.Beta.Vaults.Get(ctx, profile.Cloud.VaultID, anthropic.BetaVaultGetParams{})
		if err == nil {
			return profile.Cloud.VaultID, nil
		}
		// Vault was deleted or inaccessible; re-create below.
	}

	vault, err := client.SDK.Beta.Vaults.New(ctx, anthropic.BetaVaultNewParams{
		DisplayName: fmt.Sprintf("aida-%s", profileName),
		Metadata: map[string]string{
			"aida_profile": profileName,
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating vault: %w", err)
	}

	// Write vault ID back to config.
	if profile.Cloud == nil {
		profile.Cloud = &config.CloudConfig{}
	}
	profile.Cloud.VaultID = vault.ID
	cfg.Profiles[profileName] = profile
	if err := config.SaveConfig(cfg); err != nil {
		return vault.ID, fmt.Errorf("saving vault_id to config: %w", err)
	}

	return vault.ID, nil
}

// AddCredential stores a static bearer credential in a vault for MCP server auth.
func AddCredential(ctx context.Context, client *Client, vaultID, displayName, token, mcpServerURL string) (string, error) {
	cred, err := client.SDK.Beta.Vaults.Credentials.New(ctx, vaultID, anthropic.BetaVaultCredentialNewParams{
		DisplayName: param.NewOpt(displayName),
		Auth: anthropic.BetaVaultCredentialNewParamsAuthUnion{
			OfStaticBearer: &anthropic.BetaManagedAgentsStaticBearerCreateParams{
				Token:        token,
				MCPServerURL: mcpServerURL,
				Type:         anthropic.BetaManagedAgentsStaticBearerCreateParamsTypeStaticBearer,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("creating credential: %w", err)
	}
	return cred.ID, nil
}

// ListCredentials lists credentials in a vault (secrets are not exposed).
func ListCredentials(ctx context.Context, client *Client, vaultID string) ([]anthropic.BetaManagedAgentsCredential, error) {
	page, err := client.SDK.Beta.Vaults.Credentials.List(ctx, vaultID, anthropic.BetaVaultCredentialListParams{})
	if err != nil {
		return nil, fmt.Errorf("listing credentials: %w", err)
	}
	return page.Data, nil
}

// DeleteVault deletes a vault and all its credentials.
func DeleteVault(ctx context.Context, client *Client, vaultID string) error {
	_, err := client.SDK.Beta.Vaults.Delete(ctx, vaultID, anthropic.BetaVaultDeleteParams{})
	if err != nil {
		return fmt.Errorf("deleting vault: %w", err)
	}
	return nil
}
