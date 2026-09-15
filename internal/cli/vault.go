package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ryanlitalien/aida/internal/cloud"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newVaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Manage per-profile credential vaults for cloud investigations",
	}
	cmd.AddCommand(newVaultSetupGitHubCmd())
	cmd.AddCommand(newVaultAddCredentialCmd())
	cmd.AddCommand(newVaultListCmd())
	cmd.AddCommand(newVaultClearCmd())
	return cmd
}

func newVaultSetupGitHubCmd() *cobra.Command {
	var tokenEnv string

	cmd := &cobra.Command{
		Use:   "setup-github",
		Short: "Configure GitHub access for the active profile",
		Long:  "Ensures a vault exists, saves the GitHub token env var to config, and optionally stores a vault credential for MCP-based GitHub access.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, profileName := cfg.ActiveProfileConfig()
			if profile == nil {
				return fmt.Errorf("no active profile found")
			}

			if profile.Cloud == nil {
				profile.Cloud = &config.CloudConfig{}
			}

			// Resolve cloud API key
			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q -- set api_key_env in cloud config", profileName)
			}

			// Default token env var
			if tokenEnv == "" {
				tokenEnv = "GITHUB_TOKEN"
			}

			// Verify the token is set
			if os.Getenv(tokenEnv) == "" {
				return fmt.Errorf("environment variable %s is not set -- add it to ~/.aida/.env or export it", tokenEnv)
			}

			if dryRunGuard("setup vault", "profile: "+profileName) {
				return nil
			}

			cloudClient := cloud.NewClient(cloudAPIKey)

			// Ensure vault for this profile
			spinner := ui.NewSpinner()
			spinner.Start("Ensuring vault...")
			vaultID, err := cloud.EnsureVault(ctx, cloudClient, cfg, profileName)
			spinner.Stop(fmt.Sprintf("%s Vault ready: %s", ui.SuccessIcon, vaultID))
			if err != nil {
				return fmt.Errorf("ensuring vault: %w", err)
			}

			// If profile has a github MCP server, store the token as a vault credential
			for _, mcp := range profile.Cloud.MCPServers {
				if strings.EqualFold(mcp.Name, "github") {
					spinner.Start("Storing GitHub credential...")
					credID, err := cloud.AddCredential(ctx, cloudClient, vaultID, "GitHub PAT", os.Getenv(tokenEnv), mcp.URL)
					spinner.Stop(fmt.Sprintf("%s Credential stored: %s", ui.SuccessIcon, credID))
					if err != nil {
						return fmt.Errorf("adding credential: %w", err)
					}
					break
				}
			}

			// Save github_token_env to profile config
			profile.Cloud.GitHubTokenEnv = tokenEnv
			cfg.Profiles[profileName] = *profile
			if err := config.SaveConfig(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("\nProfile %q configured.\n", profileName)
			fmt.Println("GitHub repos will be auto-mounted when URLs are detected in questions.")
			if len(profile.Cloud.MCPServers) == 0 {
				fmt.Println("\nTip: Add an MCP server for richer GitHub tools (PR comments, issues, etc.):")
				fmt.Println("  cloud:")
				fmt.Println("    mcp_servers:")
				fmt.Println("      - name: github")
				fmt.Println("        url: https://your-github-mcp.example.com/sse")
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&tokenEnv, "token-env", "", "Environment variable name for GitHub token (default: GITHUB_TOKEN)")
	return cmd
}

func newVaultAddCredentialCmd() *cobra.Command {
	var (
		name     string
		tokenEnv string
		mcpURL   string
	)

	cmd := &cobra.Command{
		Use:   "add-credential",
		Short: "Add a credential to the active profile's vault",
		Long:  "Stores a static bearer credential for MCP server authentication. Use this for any service with an MCP server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			if tokenEnv == "" {
				return fmt.Errorf("--token-env is required")
			}
			if mcpURL == "" {
				return fmt.Errorf("--mcp-url is required")
			}

			token := os.Getenv(tokenEnv)
			if token == "" {
				return fmt.Errorf("environment variable %s is not set", tokenEnv)
			}

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, profileName := cfg.ActiveProfileConfig()
			if profile == nil {
				return fmt.Errorf("no active profile found")
			}
			if profile.Cloud == nil {
				profile.Cloud = &config.CloudConfig{}
			}

			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q", profileName)
			}

			if dryRunGuard("add credential", "profile: "+profileName, "token env: "+tokenEnv) {
				return nil
			}

			cloudClient := cloud.NewClient(cloudAPIKey)

			// Ensure vault
			spinner := ui.NewSpinner()
			spinner.Start("Ensuring vault...")
			vaultID, err := cloud.EnsureVault(ctx, cloudClient, cfg, profileName)
			spinner.Stop(fmt.Sprintf("%s Vault ready", ui.SuccessIcon))
			if err != nil {
				return fmt.Errorf("ensuring vault: %w", err)
			}

			// Store credential
			if name == "" {
				name = tokenEnv
			}
			spinner.Start("Storing credential...")
			credID, err := cloud.AddCredential(ctx, cloudClient, vaultID, name, token, mcpURL)
			spinner.Stop(fmt.Sprintf("%s Credential stored: %s", ui.SuccessIcon, credID))
			if err != nil {
				return fmt.Errorf("adding credential: %w", err)
			}

			fmt.Printf("\nCredential %q added to vault for profile %q.\n", name, profileName)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Display name for the credential")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "", "Environment variable containing the token (required)")
	cmd.Flags().StringVar(&mcpURL, "mcp-url", "", "MCP server URL this credential authenticates against (required)")
	return cmd
}

func newVaultListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show vault and credentials for the active profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, profileName := cfg.ActiveProfileConfig()
			if profile == nil {
				return fmt.Errorf("no active profile found")
			}

			fmt.Printf("Profile: %s\n", profileName)

			if profile.Cloud == nil || profile.Cloud.VaultID == "" {
				fmt.Println("No vault configured. Run 'aida vault setup-github' or 'aida vault add-credential' to get started.")
				return nil
			}

			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q", profileName)
			}

			cloudClient := cloud.NewClient(cloudAPIKey)

			fmt.Printf("Vault:   %s\n", profile.Cloud.VaultID)
			if profile.Cloud.GitHubTokenEnv != "" {
				fmt.Printf("GitHub:  %s (repo mounting)\n", profile.Cloud.GitHubTokenEnv)
			}
			fmt.Println()

			creds, err := cloud.ListCredentials(ctx, cloudClient, profile.Cloud.VaultID)
			if err != nil {
				return fmt.Errorf("listing credentials: %w", err)
			}

			if len(creds) == 0 {
				fmt.Println("No credentials stored in vault.")
				return nil
			}

			fmt.Printf("%-24s %-20s %-12s %s\n", "ID", "NAME", "TYPE", "MCP SERVER")
			fmt.Printf("%-24s %-20s %-12s %s\n", strings.Repeat("-", 24), strings.Repeat("-", 20), strings.Repeat("-", 12), strings.Repeat("-", 30))
			for _, c := range creds {
				name := c.DisplayName
				if name == "" {
					name = "(unnamed)"
				}
				authType := c.Auth.Type
				mcpURL := c.Auth.MCPServerURL
				fmt.Printf("%-24s %-20s %-12s %s\n", truncate(c.ID, 22), truncate(name, 18), authType, mcpURL)
			}

			return nil
		},
	}
}

func newVaultClearCmd() *cobra.Command {
	var clearAll bool

	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Reset vault for the active profile",
		Long:  "Deletes the vault and clears vault_id from config. With --all, also clears agent_id and environment_id to force fresh provisioning.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, profileName := cfg.ActiveProfileConfig()
			if profile == nil {
				return fmt.Errorf("no active profile found")
			}
			if profile.Cloud == nil {
				fmt.Println("No cloud config to clear.")
				return nil
			}

			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q", profileName)
			}

			if dryRunGuard("clear vault", "profile: "+profileName) {
				return nil
			}

			cloudClient := cloud.NewClient(cloudAPIKey)

			// Delete vault if it exists
			if profile.Cloud.VaultID != "" {
				spinner := ui.NewSpinner()
				spinner.Start("Deleting vault...")
				err := cloud.DeleteVault(ctx, cloudClient, profile.Cloud.VaultID)
				if err != nil {
					spinner.Stop(fmt.Sprintf("%s Vault delete failed (may already be deleted)", ui.WarnIcon))
				} else {
					spinner.Stop(fmt.Sprintf("%s Vault deleted", ui.SuccessIcon))
				}
				profile.Cloud.VaultID = ""
			}

			if clearAll {
				profile.Cloud.AgentID = ""
				profile.Cloud.EnvironmentID = ""
				fmt.Println("Cleared agent_id and environment_id (will re-provision on next investigate).")
			}

			cfg.Profiles[profileName] = *profile
			if err := config.SaveConfig(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Printf("Profile %q vault cleared.\n", profileName)
			return nil
		},
	}
	cmd.Flags().BoolVar(&clearAll, "all", false, "Also clear agent_id and environment_id")
	return cmd
}
