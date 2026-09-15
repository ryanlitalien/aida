package cli

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// buildVersion reads module/VCS info stamped into the binary by `go install`
// or `go build` (Go 1.18+). Tagged releases installed via `go install
// github.com/ryanlitalien/aida/cmd/aida@vX.Y.Z` get Main.Version; local `make
// install` builds fall back to "(devel)" + the commit hash and dirty bit.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	version := strings.TrimSuffix(info.Main.Version, "+dirty")
	if version == "" || version == "(devel)" {
		version = "dev"
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	var parts []string
	parts = append(parts, version)
	if rev != "" {
		parts = append(parts, rev+modified)
	}
	return strings.Join(parts, " ")
}

var (
	verbose         bool
	dryRun          bool
	offline         bool
	agentMode       bool
	continueSession bool
	agentStream     bool
	agentGraph      bool
	explain         bool
	// pinnedSource, when non-empty (--source NAME), restricts the query to
	// exactly one library source and bypasses the LLM router, so the plan
	// cannot drift off it. Used by `aida ask` source-backed roster entries.
	pinnedSource string
	// agentRunDir, when non-empty, redirects --agent output away
	// from the TTY and into the directory: events.ndjson (append-
	// only NDJSON event log) plus output.md (final answer). Used by
	// the jobs queue and any other caller that wants a structured,
	// pipe-safe agent run. See internal/cli/agent_events.go.
	agentRunDir string
)

// dryRunGuard returns true (and prints a preview) when --dry-run is active,
// signalling the caller to skip the real write. When --dry-run is not set
// it returns false and the caller should proceed normally.
func dryRunGuard(action string, details ...string) bool {
	if !dryRun {
		return false
	}
	fmt.Printf("[dry-run] would %s\n", action)
	for _, d := range details {
		fmt.Printf("  %s\n", d)
	}
	return true
}

// NewRootCmd creates the root aida command with all subcommands.
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aida",
		Short: "A.I.D.A. -- agent of agents CLI",
		Long:  "A.I.D.A. (Artificial Intelligent Digital Assistant) parses natural language questions and routes them to the right tools/sources automatically.",
		// If args are provided without a subcommand, treat them as a query
		Args:    cobra.ArbitraryArgs,
		RunE:    runQuery, // default command is query
		Version: buildVersion(),
		// Silence default usage/error printing so we control output.
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.SetVersionTemplate(fmt.Sprintf("aida %s\n", buildVersion()))

	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "show full execution trace")
	cmd.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "show plan without executing")
	cmd.PersistentFlags().BoolVar(&offline, "offline", false, "use local Ollama model")
	cmd.PersistentFlags().BoolVar(&agentMode, "agent", false, "use agent mode for multi-step reasoning")
	cmd.PersistentFlags().BoolVarP(&continueSession, "continue", "c", false, "continue from the last query's context")
	cmd.PersistentFlags().BoolVar(&agentStream, "stream", false, "stream response tokens live (agent mode only)")
	cmd.PersistentFlags().BoolVar(&agentGraph, "graph", false, "agent mode: run the multi-agent investigate graph (triage → parallel specialists → evaluator-optimizer) instead of a single agent")
	cmd.PersistentFlags().BoolVar(&explain, "explain", false, "print planner scoring table for each candidate source")
	cmd.PersistentFlags().StringVar(&agentRunDir, "run-dir", "", "agent mode: write events.ndjson + output.md to this dir, no TTY output")
	cmd.PersistentFlags().StringVar(&pinnedSource, "source", "", "pin execution to a single named library source (bypasses the LLM router)")

	// Load .env and propagate verbose flag before any command runs.
	cmd.PersistentPreRun = func(_ *cobra.Command, _ []string) {
		config.LoadDotEnv()
		ui.Verbose = verbose
		ui.Explain = explain
		// --run-dir mode: silence every spinner globally so the
		// pre-agent parse/classify path doesn't leak ANSI into
		// subprocess capture. Non-spinner stdout (results, prompts)
		// is already gated separately.
		if agentRunDir != "" {
			ui.Quiet = true
		}
	}

	// Add subcommands
	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newIndexCmd())
	cmd.AddCommand(newProfileCmd())
	cmd.AddCommand(newSourcesCmd())
	cmd.AddCommand(newLibraryCmd())
	cmd.AddCommand(newReplayCmd())
	cmd.AddCommand(newTuneCmd())
	cmd.AddCommand(newRunsCmd())
	cmd.AddCommand(newGoldenCmd())
	cmd.AddCommand(newThumbsUpCmd())
	cmd.AddCommand(newThumbsDownCmd())
	cmd.AddCommand(newNoteCmd())
	cmd.AddCommand(newLessonsCmd())
	cmd.AddCommand(newInvestigateCmd())
	cmd.AddCommand(newVaultCmd())
	cmd.AddCommand(newBrainCmd())
	cmd.AddCommand(newWikiCmd())
	cmd.AddCommand(newTasksCmd())
	cmd.AddCommand(newServeCmd())
	cmd.AddCommand(newJarvisCmd())
	cmd.AddCommand(newSessionCmd())
	cmd.AddCommand(newSkillCmd())
	cmd.AddCommand(newSeedCmd())
	cmd.AddCommand(newSetupCmd())
	cmd.AddCommand(newLintCmd())
	cmd.AddCommand(newDailyCmd())
	cmd.AddCommand(newJobsCmd())
	cmd.AddCommand(newLoopCmd())
	cmd.AddCommand(newAskCmd())
	cmd.AddCommand(newRosterCmd())
	cmd.AddCommand(newModelsCmd())
	cmd.AddCommand(newBurndownCmd())
	cmd.AddCommand(newLMDCmd())
	cmd.AddCommand(newFleetCmd())

	return cmd
}
