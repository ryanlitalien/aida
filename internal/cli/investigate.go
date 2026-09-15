package cli

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/cloud"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/investigations"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

var (
	streamFlag bool
	openFlag   bool
)

func newInvestigateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:        "investigate [question]",
		Short:      "Deep investigation via cloud managed agents (deprecated)",
		Long:       "DEPRECATED: Use 'aida --agent <question>' instead. The agent mode now includes cloud_investigate as a tool, routing to managed agents automatically when deep investigation is needed.\n\nLegacy behavior: runs the local parse/classify/resolve pipeline, then delegates deep investigation to a Claude Managed Agent session in the cloud.",
		Deprecated: "use 'aida --agent <question>' - the agent orchestrator handles cloud delegation via the cloud_investigate tool",
		Args:       cobra.ArbitraryArgs,
		RunE:       runInvestigate,
	}
	cmd.Flags().BoolVarP(&streamFlag, "stream", "s", false, "Stream events live")
	cmd.Flags().BoolVarP(&openFlag, "open", "o", false, "Open session in browser")

	cmd.AddCommand(newInvestigateStatusCmd())
	cmd.AddCommand(newInvestigateResultsCmd())
	cmd.AddCommand(newInvestigateListCmd())
	return cmd
}

func runInvestigate(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	question := strings.Join(args, " ")
	ctx := context.Background()

	// 1. Load config, detect profile
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	profile, profileName := cfg.ActiveProfileConfig()
	if profile == nil {
		return fmt.Errorf("no active profile found")
	}
	ui.PrintVerbose("Profile", profileName)

	// 2. Validate cloud config
	if profile.Cloud == nil {
		return fmt.Errorf("profile %q has no cloud config -- add a 'cloud:' section with api_key_env to config.yaml", profileName)
	}

	// 3. Resolve cloud API key
	cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
	if cloudAPIKey == "" {
		envName := profile.Cloud.APIKeyEnv
		if envName == "" {
			envName = cfg.API.AnthropicKeyEnv
		}
		return fmt.Errorf("no cloud API key found -- set %s or configure cloud.api_key_env / cloud.api_key_file", envName)
	}

	// 4. Create cloud client
	cloudClient := cloud.NewClient(cloudAPIKey)

	// 5. EnsureAgent + EnsureEnvironment (auto-setup)
	spinner := ui.NewSpinner()

	spinner.Start("Provisioning cloud agent...")
	agentID, err := cloud.EnsureAgent(ctx, cloudClient, cfg, profileName)
	spinner.Stop(fmt.Sprintf("%s Agent ready", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("ensuring agent: %w", err)
	}
	ui.PrintVerbose("Agent ID", agentID)

	spinner.Start("Provisioning environment...")
	envID, err := cloud.EnsureEnvironment(ctx, cloudClient, cfg, profileName)
	spinner.Stop(fmt.Sprintf("%s Environment ready", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("ensuring environment: %w", err)
	}
	ui.PrintVerbose("Environment ID", envID)

	// 6. Load sources for local pipeline
	_, srcOrigin, err := resolveSources()
	if err != nil {
		return err
	}
	ui.PrintVerbose("Sources from", srcOrigin)

	// 7. Set up local LLM client for parsing (uses global API key)
	localAPIKey := cfg.GetAPIKey()
	isOffline := offline || cfg.Model.OfflineMode
	model := cfg.Model.Primary
	if isOffline {
		model = llm.ResolveOfflineModel(cfg.Model.Fallback, cfg.Model.Offline)
	}
	if !isOffline && localAPIKey == "" {
		return fmt.Errorf("no local API key found for parsing -- set %s or run with --offline", cfg.API.AnthropicKeyEnv)
	}
	client := llm.NewClient(localAPIKey, model, isOffline)
	if cfg.Model.Stages != nil {
		client.SetStageModels(cfg.Model.Stages)
	}

	// Load soul context for parsing
	soul, _ := config.LoadSoul()
	soulCtx := soul.ForPrompt()

	// 8. Parse (LLM call #1)
	spinner.Start("Parsing question...")
	intent, err := engine.Parse(ctx, client, question, soulCtx, nil)
	spinner.Stop(fmt.Sprintf("%s Parsed", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("parse step failed: %w", err)
	}
	ui.PrintParsed(intent.Action, strings.Join(intent.RawEntities, ", "), intent.Timeframe)

	// 9. Classify (deterministic)
	classified := engine.Classify(intent)
	ui.PrintVerbose("Strategy", string(classified.Strategy))

	// 10. Resolve (deterministic)
	resolved := engine.Resolve(classified)

	// 11. Package context from profile.Cloud.ContextPaths
	var contextFiles []cloud.ContextFile
	if len(profile.Cloud.ContextPaths) > 0 {
		spinner.Start("Packaging context...")
		contextFiles, err = cloud.PackageContext(profile.Cloud.ContextPaths)
		spinner.Stop(fmt.Sprintf("%s Context packaged (%d files)", ui.SuccessIcon, len(contextFiles)))
		if err != nil {
			return fmt.Errorf("packaging context: %w", err)
		}
	}

	// 12. Build investigation brief
	brief := cloud.BuildBrief(question, intent, classified, resolved, contextFiles)

	// 13. Dry-run: print brief and return
	if dryRun {
		fmt.Println("\n--- Investigation Brief (dry run) ---")
		fmt.Println(brief)
		fmt.Println("--- End Brief ---")
		return nil
	}

	// 14. Build session resources and vault references
	var resources []anthropic.BetaSessionNewParamsResourceUnion
	if ghToken := profile.Cloud.GetGitHubToken(); ghToken != "" {
		if repoURL := extractGitHubRepo(question, intent); repoURL != "" {
			resources = append(resources, anthropic.BetaSessionNewParamsResourceUnion{
				OfGitHubRepository: &anthropic.BetaManagedAgentsGitHubRepositoryResourceParams{
					AuthorizationToken: ghToken,
					Type:               anthropic.BetaManagedAgentsGitHubRepositoryResourceParamsTypeGitHubRepository,
					URL:                repoURL,
				},
			})
			ui.PrintVerbose("GitHub repo", repoURL)
		}
	}

	var vaultIDs []string
	if profile.Cloud.VaultID != "" {
		vaultIDs = []string{profile.Cloud.VaultID}
		ui.PrintVerbose("Vault", profile.Cloud.VaultID)
	}

	// 15. Create session
	spinner.Start("Creating investigation session...")
	title := fmt.Sprintf("Investigation: %s", truncate(question, 80))
	session, err := cloud.CreateSession(ctx, cloudClient, cloud.SessionOptions{
		AgentID:   agentID,
		EnvID:     envID,
		Title:     title,
		VaultIDs:  vaultIDs,
		Resources: resources,
	})
	spinner.Stop(fmt.Sprintf("%s Session created", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// 15. Send brief as initial message
	spinner.Start("Sending investigation brief...")
	err = cloud.SendMessage(ctx, cloudClient, session.ID, brief)
	spinner.Stop(fmt.Sprintf("%s Brief sent", ui.SuccessIcon))
	if err != nil {
		return fmt.Errorf("sending brief: %w", err)
	}

	// 16. Save investigation record
	inv := &investigations.Investigation{
		SessionID:      session.ID,
		Profile:        profileName,
		Question:       question,
		StartedAt:      time.Now(),
		Status:         "running",
		Action:         intent.Action,
		Strategy:       string(classified.Strategy),
		Entities:       intent.RawEntities,
		ResolvedValues: resolved.GetResolvedValues(),
		AgentID:        agentID,
		EnvironmentID:  envID,
	}
	if _, err := investigations.Save(inv); err != nil {
		ui.PrintVerbose("Investigation save", "error: "+err.Error())
	}

	// 17. Print session info
	consoleURL := fmt.Sprintf("https://platform.claude.com/workspaces/default/sessions/%s", session.ID)
	fmt.Printf("\nSession ID: %s\n", session.ID)
	fmt.Printf("Profile:    %s\n", profileName)
	fmt.Printf("Console:    %s\n", consoleURL)
	fmt.Println()
	fmt.Println("Track progress:")
	fmt.Printf("  aida investigate status %s\n", session.ID)
	fmt.Printf("  aida investigate status %s --stream\n", session.ID)
	fmt.Printf("  aida investigate results %s\n", session.ID)
	fmt.Println()

	// 18. If --open: launch browser
	if openFlag {
		url := fmt.Sprintf("https://platform.claude.com/workspaces/default/sessions/%s", session.ID)
		openBrowser(url)
	}

	// 19. If --stream: stream events until idle
	if streamFlag {
		fmt.Println("--- Streaming events ---")
		err := cloud.StreamEvents(ctx, cloudClient, session.ID, func(event anthropic.BetaManagedAgentsStreamSessionEventsUnion) bool {
			return handleStreamEvent(event)
		})
		if err != nil {
			return fmt.Errorf("streaming events: %w", err)
		}
	}

	return nil
}

func newInvestigateStatusCmd() *cobra.Command {
	var statusStream bool
	cmd := &cobra.Command{
		Use:   "status [session-id]",
		Short: "Show investigation status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			inv, err := resolveInvestigation(args)
			if err != nil {
				return err
			}

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, ok := cfg.Profiles[inv.Profile]
			if !ok {
				return fmt.Errorf("profile %q not found", inv.Profile)
			}

			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q", inv.Profile)
			}

			cloudClient := cloud.NewClient(cloudAPIKey)
			session, err := cloud.GetSession(ctx, cloudClient, inv.SessionID)
			if err != nil {
				return err
			}

			fmt.Printf("Session:  %s\n", session.ID)
			fmt.Printf("Status:   %s\n", session.Status)
			fmt.Printf("Question: %s\n", inv.Question)
			fmt.Printf("Profile:  %s\n", inv.Profile)
			fmt.Printf("Started:  %s\n", inv.StartedAt.Format(time.RFC3339))

			// Update local status
			_ = investigations.UpdateStatus(inv.SessionID, string(session.Status))

			if statusStream {
				fmt.Println("\n--- Streaming events ---")
				err := cloud.StreamEvents(ctx, cloudClient, inv.SessionID, func(event anthropic.BetaManagedAgentsStreamSessionEventsUnion) bool {
					return handleStreamEvent(event)
				})
				if err != nil {
					return fmt.Errorf("streaming events: %w", err)
				}
			}

			return nil
		},
	}
	cmd.Flags().BoolVarP(&statusStream, "stream", "s", false, "Stream events live")
	return cmd
}

func newInvestigateResultsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "results [session-id]",
		Short: "Fetch investigation results",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			inv, err := resolveInvestigation(args)
			if err != nil {
				return err
			}

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, ok := cfg.Profiles[inv.Profile]
			if !ok {
				return fmt.Errorf("profile %q not found", inv.Profile)
			}

			cloudAPIKey := profile.Cloud.GetAPIKey(&cfg.API)
			if cloudAPIKey == "" {
				return fmt.Errorf("no cloud API key for profile %q", inv.Profile)
			}

			cloudClient := cloud.NewClient(cloudAPIKey)

			// List events and extract agent messages
			page, err := cloudClient.SDK.Beta.Sessions.Events.List(ctx, inv.SessionID, anthropic.BetaSessionEventListParams{})
			if err != nil {
				return fmt.Errorf("listing events: %w", err)
			}

			var report strings.Builder
			for _, event := range page.Data {
				if event.Type == "agent.message" {
					msg := event.AsAgentMessage()
					for _, block := range msg.Content {
						report.WriteString(block.Text)
						report.WriteString("\n")
					}
				}
			}

			if report.Len() == 0 {
				fmt.Println("No agent messages found yet. The investigation may still be running.")
				fmt.Printf("Check status: aida investigate status %s\n", inv.SessionID)
				return nil
			}

			fmt.Println(report.String())

			// Update local record with report and save artifact
			inv.Report = report.String()
			_, _ = investigations.Save(inv)

			if path, err := investigations.SaveReport(inv); err == nil {
				fmt.Printf("Report saved to %s\n", path)
			}

			// Record as lesson so investigation outcomes feed the learning loop
			snippet := inv.Report
			if len(snippet) > 500 {
				snippet = snippet[:500]
			}
			status := lessons.StatusSuccess
			hasData := true
			if report.Len() == 0 {
				status = lessons.StatusEmpty
				hasData = false
			}
			invLesson := &lessons.Lesson{
				RunID:    inv.SessionID,
				Question: strings.ToLower(inv.Question),
				Action:   inv.Action,
				Strategy: inv.Strategy,
				Sources:  []string{"cloud-investigator"},
				PerSourceStatus: map[string]lessons.Status{
					"cloud-investigator": status,
				},
				ArtifactCount: 1,
				AnswerSnippet: snippet,
				HasData:       hasData,
			}

			// Write to brain
			invCfg, _ := config.LoadConfig()
			if invCfg != nil {
				brn, err := brain.Open(invCfg.BrainPath(), inv.Profile, invCfg.VoyageKeyEnv(), invCfg.Brain.GitHubRepo())
				if err == nil {
					_ = brn.RecordLesson(ctx, invLesson)
					if invCfg.Brain.AutoSync {
						brain.CommitAndPush(invCfg.BrainPath(), inv.Profile)
					}
					brn.Close()
				}
			}

			return nil
		},
	}
}

func newInvestigateListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List recent investigations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := investigations.List()
			if err != nil {
				return fmt.Errorf("listing investigations: %w", err)
			}

			if len(ids) == 0 {
				fmt.Println("No investigations found.")
				return nil
			}

			fmt.Printf("%-24s %-50s %-12s %s\n", "SESSION", "QUESTION", "STATUS", "STARTED")
			fmt.Printf("%-24s %-50s %-12s %s\n", strings.Repeat("-", 24), strings.Repeat("-", 50), strings.Repeat("-", 12), strings.Repeat("-", 20))

			for _, id := range ids {
				inv, err := investigations.Load(id)
				if err != nil {
					continue
				}
				sid := truncate(inv.SessionID, 22)
				q := truncate(inv.Question, 48)
				started := inv.StartedAt.Format("2006-01-02 15:04")
				fmt.Printf("%-24s %-50s %-12s %s\n", sid, q, inv.Status, started)
			}
			return nil
		},
	}
}

// resolveInvestigation finds an investigation by session ID arg or falls back
// to the latest.
func resolveInvestigation(args []string) (*investigations.Investigation, error) {
	if len(args) > 0 {
		inv, err := investigations.Load(args[0])
		if err != nil {
			return nil, fmt.Errorf("loading investigation %s: %w", args[0], err)
		}
		return inv, nil
	}
	inv, err := investigations.Latest()
	if err != nil {
		return nil, fmt.Errorf("loading latest investigation: %w", err)
	}
	if inv == nil {
		return nil, fmt.Errorf("no investigations found -- run 'aida investigate <question>' first")
	}
	return inv, nil
}

// handleStreamEvent processes a single SSE event and prints relevant info.
// Returns false to stop streaming (on idle/terminated), true to continue.
func handleStreamEvent(event anthropic.BetaManagedAgentsStreamSessionEventsUnion) bool {
	switch event.Type {
	case "agent.message":
		msg := event.AsAgentMessage()
		for _, block := range msg.Content {
			fmt.Print(block.Text)
		}
	case "agent.tool_use":
		fmt.Printf("\n[Using tool: %s]\n", event.Name)
	case "agent.tool_result":
		// Silently consume tool results
	case "session.status_idle":
		fmt.Println("\n--- Session idle ---")
		return false
	case "session.status_terminated":
		fmt.Println("\n--- Session terminated ---")
		return false
	case "session.error":
		fmt.Printf("\n[Error: %s]\n", event.Error.RawJSON())
		return false
	case "session.status_running":
		// Continue
	}
	return true
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-2] + ".."
}

// extractGitHubRepo finds a GitHub repository URL in the question or parsed
// entities. For PR/issue URLs like github.com/org/repo/pull/123, it returns
// the base repo URL github.com/org/repo.
func extractGitHubRepo(question string, intent *engine.Intent) string {
	re := regexp.MustCompile(`https?://github\.com/([^/\s]+/[^/\s]+)`)
	if m := re.FindStringSubmatch(question); len(m) > 1 {
		return "https://github.com/" + m[1]
	}
	for _, e := range intent.RawEntities {
		if m := re.FindStringSubmatch(e); len(m) > 1 {
			return "https://github.com/" + m[1]
		}
	}
	return ""
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", url)
	} else {
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
