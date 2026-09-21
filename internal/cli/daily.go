package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/daily"
	"github.com/ryanlitalien/aida/internal/dailybriefing"
	"github.com/spf13/cobra"
)

func newDailyCmd() *cobra.Command {
	var (
		profileFlag  string
		promptOnly   bool
		notionBackup bool
	)

	cmd := &cobra.Command{
		Use:   "daily",
		Short: "Run the profile-aware daily briefing",
		Long: "Executes the daily briefing via headless `claude -p` using the active " +
			"profile's daily: config (email recipient, Gmail labels, task tag, etc). " +
			"Refuses cleanly on profiles where daily.enabled is false.\n\n" +
			"Pass --notion-backup to run the Notion meeting-transcript backup job " +
			"instead of the briefing. The backup is decoupled so a wedged Notion " +
			"call cannot delay the morning email.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			profile, profileName, err := resolveDailyProfile(cfg, profileFlag)
			if err != nil {
				return err
			}

			if profile.Daily == nil || !profile.Daily.Enabled {
				return fmt.Errorf("daily briefing not enabled for profile %q -- set daily.enabled: true in ~/.aida/config.yaml", profileName)
			}
			dc := profile.Daily

			// Briefing pipeline selector: "gws" (pure-Go + the gws CLI) or
			// "" / "claude" (legacy claude -p + MCP). Notion backup always
			// runs through claude -p regardless.
			if !notionBackup && dc.Pipeline == "gws" {
				if promptOnly {
					return fmt.Errorf("--prompt-only does not apply when daily.pipeline=gws (no prompt; pure Go)")
				}
				return runViaGWS(cmd.Context(), dc, profileName, dryRun)
			}

			mode := "briefing"
			if notionBackup {
				mode = "notion-backup"
			}

			var rendered string
			if notionBackup {
				rendered, err = daily.RenderBackupPrompt(dc)
			} else {
				rendered, err = daily.RenderPrompt(dc)
			}
			if err != nil {
				return fmt.Errorf("rendering prompt: %w", err)
			}

			if promptOnly {
				fmt.Print(rendered)
				return nil
			}

			projectDir := config.ExpandPath(dc.ProjectDir)
			watchdog, model := resolveDailyRuntime(dc, notionBackup)

			if dryRun {
				fmt.Printf("[dry-run] mode:          %s\n", mode)
				fmt.Printf("[dry-run] profile:       %s\n", profileName)
				fmt.Printf("[dry-run] cwd:           %s\n", projectDir)
				fmt.Printf("[dry-run] watchdog:      %ds\n", watchdog)
				fmt.Printf("[dry-run] model:         %s\n", modelOrDefault(model))
				fmt.Printf("[dry-run] prompt bytes:  %d\n", len(rendered))
				fmt.Printf("[dry-run] command:       %s\n", strings.Join(buildClaudeArgs(model), " ")+" <rendered-prompt>")
				return nil
			}

			startLabel := "Daily briefing"
			if notionBackup {
				startLabel = "Notion backup"
			}
			fmt.Printf("=== %s started at %s ===\n", startLabel, time.Now().Format(time.RFC3339))
			fmt.Printf("=== profile: %s | mode: %s | model: %s ===\n", profileName, mode, modelOrDefault(model))

			exit := runClaudeDaily(rendered, projectDir, model, time.Duration(watchdog)*time.Second)
			fmt.Printf("=== claude exit code (attempt 1): %d ===\n", exit)

			// Retry once on exit 1 (API/transient error). Do not retry on:
			//   0 - clean success
			//   124, 137 - watchdog killed claude; retrying will just hang again
			// Exit 1 is how claude -p surfaces "API Error: Stream idle timeout" and similar
			// streaming failures. Retry is safe because the backup is idempotent (existing
			// files are skipped) and the briefing has no destructive steps before send.
			if exit == 1 {
				fmt.Printf("=== retrying after exit 1 at %s ===\n", time.Now().Format(time.RFC3339))
				exit = runClaudeDaily(rendered, projectDir, model, time.Duration(watchdog)*time.Second)
				fmt.Printf("=== claude exit code (attempt 2): %d ===\n", exit)
			}

			fmt.Printf("=== %s finished at %s ===\n", startLabel, time.Now().Format(time.RFC3339))

			if exit != 0 {
				return fmt.Errorf("claude -p exited with code %d", exit)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&profileFlag, "profile", "", "override active profile (e.g., work)")
	cmd.Flags().BoolVar(&promptOnly, "prompt-only", false, "print the rendered prompt and exit without running claude")
	cmd.Flags().BoolVar(&notionBackup, "notion-backup", false, "run the Notion meeting-transcript backup instead of the briefing")
	return cmd
}

// runViaGWS executes the briefing using the gws CLI directly, bypassing
// claude -p entirely. This is the compliant path under managed-settings.json's
// allowManagedPermissionRulesOnly: true (post-2026-05-06 wipe).
//
// dry == true skips the gmail send and prints the composed body to stdout.
func runViaGWS(ctx context.Context, dc *config.DailyConfig, profileName string, dry bool) error {
	watchdog, _ := resolveDailyRuntime(dc, false)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(watchdog)*time.Second)
	defer cancel()

	mode := "briefing-via-gws"
	if dry {
		mode = "briefing-via-gws (dry-run, no send)"
	}
	fmt.Printf("=== Daily briefing started at %s ===\n", time.Now().Format(time.RFC3339))
	fmt.Printf("=== profile: %s | mode: %s | watchdog: %ds ===\n", profileName, mode, watchdog)

	// Wait for network before kicking off gws calls. Launchd fires this job
	// via `pmset wakepoweron` at 6:15am, but the laptop's network stack often
	// isn't fully up yet - gws then fails with `dns error: failed to lookup
	// address information` and the whole briefing aborts at Step 3. Block on
	// a cheap DNS resolution before the first remote call.
	if err := waitForNetwork(ctx, "www.googleapis.com", 5*time.Minute); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: network wait failed (%v) - proceeding anyway; gws will surface the real error\n", err)
	}

	err := dailybriefing.Run(ctx, dc, dailybriefing.Options{DryRun: dry})
	fmt.Printf("=== Daily briefing finished at %s ===\n", time.Now().Format(time.RFC3339))
	return err
}

// waitForNetwork blocks until host resolves over DNS or budget elapses.
// Returns nil on first successful resolution. On wake-from-sleep the launchd
// job fires before the network is fully up; without this wait the very first
// gws call DNS-fails and the briefing dies before Step 3 completes.
//
// Retry cadence is 5s, lightweight enough that resolution typically lands
// within one or two ticks once the wifi associates.
func waitForNetwork(ctx context.Context, host string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	resolver := &net.Resolver{}
	attempt := 0
	for {
		attempt++
		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := resolver.LookupHost(lookupCtx, host)
		cancel()
		if err == nil {
			if attempt > 1 {
				fmt.Fprintf(os.Stderr, "[network] %s resolved on attempt %d\n", host, attempt)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("network not ready after %s (%d attempts): %w", budget, attempt, err)
		}
		fmt.Fprintf(os.Stderr, "[network] waiting for %s (attempt %d: %v)\n", host, attempt, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// resolveDailyRuntime returns the (watchdog, model) pair for the chosen mode.
// Backup mode prefers daily.notion_backup.* overrides; falls back to the
// briefing values, then to a hard-coded 30-minute watchdog.
func resolveDailyRuntime(dc *config.DailyConfig, backup bool) (int, string) {
	if backup && dc.NotionBackup != nil {
		watchdog := dc.NotionBackup.WatchdogSeconds
		if watchdog <= 0 {
			watchdog = 1800
		}
		model := dc.NotionBackup.Model
		if model == "" {
			model = dc.Model
		}
		return watchdog, model
	}
	if backup {
		return 1800, dc.Model
	}
	watchdog := dc.WatchdogSeconds
	if watchdog <= 0 {
		watchdog = 2700
	}
	return watchdog, dc.Model
}

// resolveDailyProfile picks the profile to use: explicit flag wins, otherwise
// fall back to ActiveProfileConfig (AIDA_PROFILE / auto-detect).
func resolveDailyProfile(cfg *config.Config, flag string) (*config.Profile, string, error) {
	if flag != "" {
		p, ok := cfg.Profiles[flag]
		if !ok {
			return nil, "", fmt.Errorf("profile %q not found in config", flag)
		}
		return &p, flag, nil
	}
	p, name := cfg.ActiveProfileConfig()
	if p == nil {
		return nil, "", errors.New("no active profile resolved -- set AIDA_PROFILE or active_profile")
	}
	return p, name, nil
}

// runClaudeDaily exec's `claude -p <prompt>` with a watchdog timeout.
// stdout/stderr pass through to the caller (launchd plist redirects these to
// the log file; terminal runs see them live). Returns an exit code:
//
//	0   clean success
//	124 watchdog timed out
//	N   whatever claude exited with
func runClaudeDaily(prompt, cwd, model string, watchdog time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), watchdog)
	defer cancel()

	args := append(buildClaudeArgs(model)[1:], prompt) // drop leading "claude" from buildClaudeArgs
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = scrubAnthropicCreds(os.Environ())

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Printf("[WATCHDOG] %s: TIMEOUT after %ds\n", time.Now().Format(time.RFC3339), int(watchdog.Seconds()))
		return 124
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "claude exec error: %v\n", err)
		return 1
	}
	return 0
}

// buildClaudeArgs returns the argv (including "claude" as argv[0]) used to
// invoke claude -p for the daily briefing. Pulled out so --dry-run can show
// the same command we'd actually exec.
func buildClaudeArgs(model string) []string {
	args := []string{"claude", "-p", "--verbose", "--output-format", "stream-json", "--permission-mode", "default"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return args
}

// scrubAnthropicCreds returns env with any Anthropic credential vars removed.
// Thin alias for config.ScrubAnthropicCreds, kept so the existing cli call
// sites read unchanged.
func scrubAnthropicCreds(env []string) []string {
	return config.ScrubAnthropicCreds(env)
}

func modelOrDefault(model string) string {
	if model == "" {
		return "(claude default)"
	}
	return model
}
