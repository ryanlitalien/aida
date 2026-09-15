package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newBrainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "brain",
		Short: "Manage the shared brain/memory system",
		Long:  "The brain is Aida's shared memory - a git repo of markdown files indexed by SQLite with vector embeddings for semantic search.",
	}

	cmd.AddCommand(newBrainInitCmd())
	cmd.AddCommand(newBrainSyncCmd())
	cmd.AddCommand(newBrainIndexCmd())
	cmd.AddCommand(newBrainCompileCmd())
	cmd.AddCommand(newBrainConsolidateCmd())
	cmd.AddCommand(newBrainMineRunsCmd())
	cmd.AddCommand(newBrainReembedCmd())
	cmd.AddCommand(newBrainSearchCmd())
	cmd.AddCommand(newBrainRecallCmd())
	cmd.AddCommand(newBrainStatsCmd())
	cmd.AddCommand(newBrainGCCmd())
	cmd.AddCommand(newBrainAnalyzeCmd())
	cmd.AddCommand(newBrainGardenCmd())
	cmd.AddCommand(newBrainLessonCmd())
	cmd.AddCommand(newBrainCaptureHookCmd())
	cmd.AddCommand(newBrainRememberCmd())
	cmd.AddCommand(newBrainProjectSoulCmd())
	cmd.AddCommand(newBrainHarvestCmd())

	return cmd
}

func newBrainInitCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize the brain repo",
		Long:  "Creates the brain directory structure and optionally sets up a git remote for syncing across machines.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}

			brainPath := cfg.BrainPath()
			if remote == "" {
				remote = cfg.Brain.Remote
			}

			_, profileName := cfg.ActiveProfileConfig()

			// Open brain (creates directories)
			b, err := brain.Open(brainPath, profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			// Initialize git repo
			if err := brain.InitRepo(brainPath, remote); err != nil {
				return fmt.Errorf("git init: %w", err)
			}

			fmt.Printf("Brain initialized at %s\n", brainPath)
			if remote != "" {
				fmt.Printf("Remote: %s\n", remote)
			}
			if b.Embeddings.Available() {
				fmt.Println("Embeddings: Voyage AI (available)")
			} else {
				fmt.Printf("Embeddings: not configured (set %s for semantic search)\n", cfg.VoyageKeyEnv())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&remote, "remote", "", "git remote URL (e.g., git@github.com:user/aida-brain.git)")
	return cmd
}

func newBrainSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Sync brain with remote (pull + commit + push)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()
			if err := brain.Sync(cfg.BrainPath(), profileName); err != nil {
				return err
			}
			// Refresh the ~/.claude/CLAUDE.md soul projection on every
			// successful sync. Non-fatal: a projection failure must never
			// turn a successful sync into a failed command.
			if err := runProjectSoul(false); err != nil {
				ui.PrintVerbose("Brain sync", "soul projection failed: "+err.Error())
			}
			return nil
		},
	}
}

func newBrainIndexCmd() *cobra.Command {
	var compileDomains bool
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Rebuild brain.db from lesson files and markdown pages",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()
			ctx := context.Background()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			if dryRunGuard("index brain", cfg.BrainPath()) {
				return nil
			}

			spinner := ui.NewSpinner()
			spinner.Start("Indexing brain...")
			err = b.Index(ctx)
			spinner.Stop(fmt.Sprintf("%s Indexed", ui.SuccessIcon))
			if err != nil {
				return err
			}

			if compileDomains {
				apiKey := cfg.GetAPIKey()
				model := cfg.Model.Primary
				client := llm.NewClient(apiKey, model, false)

				libraryPath := filepath.Join(config.Dir(), "library")
				spinner.Start("Compiling domain profiles...")
				count, err := brain.CompileSources(ctx, client, cfg.BrainPath(), libraryPath)
				spinner.Stop(fmt.Sprintf("%s Compiled %d domain profiles", ui.SuccessIcon, count))
				if err != nil {
					return err
				}
			}

			return nil
		},
	}
	cmd.Flags().BoolVar(&compileDomains, "compile-domains", false, "also generate semantic domain profiles for all sources (LLM-powered)")
	return cmd
}

func newBrainCompileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compile",
		Short: "Synthesize compiled truth from evidence (LLM-powered)",
		Long:  "Reads raw lesson evidence and uses an LLM to write compiled routing wisdom and entity summaries.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()
			ctx := context.Background()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			apiKey := cfg.GetAPIKey()
			isOffline := cfg.Model.OfflineMode
			model := cfg.Model.Primary
			if isOffline {
				model = cfg.Model.Fallback
			}
			client := llm.NewClient(apiKey, model, isOffline)

			if dryRunGuard("compile brain", cfg.BrainPath()) {
				return nil
			}

			spinner := ui.NewSpinner()
			spinner.Start("Compiling routing wisdom...")
			err = b.CompileRoutingWisdom(ctx, client)
			if err != nil {
				spinner.Stop(fmt.Sprintf("%s Compile failed", ui.WarnIcon))
				return err
			}
			spinner.Stop(fmt.Sprintf("%s Compiled", ui.SuccessIcon))
			return nil
		},
	}
}

func newBrainSearchCmd() *cobra.Command {
	var k int
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Semantic search against the brain",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()
			ctx := context.Background()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			sc, err := b.Search(ctx, query, nil, k)
			if err != nil {
				return err
			}

			if len(sc.SimilarLessons) == 0 {
				fmt.Println("No similar lessons found.")
				return nil
			}

			fmt.Printf("Found %d similar lessons:\n\n", len(sc.SimilarLessons))
			for i, sl := range sc.SimilarLessons {
				l := sl.Lesson
				srcs := strings.Join(l.Sources, ", ")
				quality := ""
				if l.Quality > 0 {
					quality = fmt.Sprintf(" [quality %d/5]", l.Quality)
				}
				feedback := ""
				if l.Feedback != "" {
					feedback = fmt.Sprintf(" (%s)", l.Feedback)
				}
				fmt.Printf("%d. (sim=%.2f) %q\n   → %s%s%s\n   %s\n\n",
					i+1, sl.Similarity, l.Question, srcs, quality, feedback, l.Timestamp)
			}

			if sc.RoutingWisdom != "" {
				fmt.Println("Routing wisdom available (use --verbose to show)")
			}

			return nil
		},
	}
	cmd.Flags().IntVarP(&k, "limit", "k", 5, "number of results")
	return cmd
}

func newBrainStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show brain statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			s := b.GetStats()
			fmt.Printf("Brain path:    %s\n", s.BrainPath)
			fmt.Printf("Lessons:       %d\n", s.LessonCount)
			if s.JarvisLessonCount > 0 {
				fmt.Printf("Jarvis lessons:%d rated  %s\n",
					s.JarvisLessonCount,
					formatJarvisFeedbackBreakdown(s.JarvisLessonByFeedback))
			} else {
				fmt.Printf("Jarvis lessons:0 rated\n")
			}
			fmt.Printf("Entities:      %d\n", s.EntityCount)
			if line := brain.FormatTaskCounts(s.TasksByStatus); line != "" {
				fmt.Printf("Tasks:         %s\n", line)
			} else {
				fmt.Printf("Tasks:         (none)\n")
			}
			memCounts, err := b.DB.MemoryCounts()
			if err != nil {
				return err
			}
			if line := formatMemoryCounts(memCounts); line != "" {
				fmt.Printf("Memory:        %s\n", line)
			} else {
				fmt.Printf("Memory:        (none)\n")
			}
			fmt.Printf("DB size:       %s\n", formatBytes(s.DBSize))
			fmt.Printf("Embeddings:    %v\n", s.HasEmbeddings)
			fmt.Printf("Git repo:      %v\n", brain.IsGitRepo(s.BrainPath))
			if s.LastLesson != "" {
				fmt.Printf("Last lesson:   %s\n", s.LastLesson)
			}
			if s.IsStale {
				fmt.Printf("Status:        stale (run 'aida brain index' to rebuild)\n")
			} else {
				fmt.Printf("Status:        up to date\n")
			}
			return nil
		},
	}
}

// newBrainLessonCmd is the parent for per-lesson operations. Only
// `retract` exists today, but grouping under `lesson` leaves room for
// future verbs (e.g. `lesson show`) without another top-level command.
func newBrainLessonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lesson",
		Short: "Manage individual lesson records",
	}
	cmd.AddCommand(newBrainLessonRetractCmd())
	return cmd
}

func newBrainLessonRetractCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retract <id>",
		Short: "Retract a lesson so it stops competing in recall",
		Long: "Marks the lesson with the given id superseded: quality is zeroed,\n" +
			"its routing hint is cleared, and it is excluded from every recall path\n" +
			"(FindSimilar, SearchMulti, routing-wisdom compilation). The db row and\n" +
			"JSON file are kept, not deleted, so the retraction is auditable.\n\n" +
			"Use this to manually correct a bad auto-recorded lesson that predates\n" +
			"the run_id-based auto-supersession `aida thumbs-down` now does, or one\n" +
			"whose originating run id is unknown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			if dryRunGuard("retract lesson", id) {
				return nil
			}

			if err := b.RetractLesson(context.Background(), id); err != nil {
				return err
			}
			fmt.Printf("%s Retracted lesson %s\n", ui.SuccessIcon, id)
			return nil
		},
	}
}

func newBrainGCCmd() *cobra.Command {
	var maxDays int
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Garbage collect old evidence and flag stale pages",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			result, err := b.GC(maxDays)
			if err != nil {
				return err
			}

			fmt.Printf("Garbage collection complete:\n")
			fmt.Printf("  Archived evidence lines:  %d (>%d days old)\n", result.ArchivedEvidence, maxDays)
			fmt.Printf("  Stale compiled truth:     %d pages\n", len(result.StaleTruth))
			for _, s := range result.StaleTruth {
				fmt.Printf("    - %s (run 'aida brain compile' to update)\n", s)
			}
			fmt.Printf("  Old lessons in DB:        %d\n", result.OldLessons)
			fmt.Printf("  Total cleaned:            %d\n", result.TotalCleaned)
			return nil
		},
	}
	cmd.Flags().IntVar(&maxDays, "days", 90, "archive evidence older than N days")
	return cmd
}

// formatMemoryCounts renders per-type memory record counts as
// "1 fact (1 superseded), 1 event, 1 instruction" with active counts
// always shown and a "(N superseded)" suffix only when superseded > 0.
// Returns empty string when there are no memory records at all.
func formatMemoryCounts(counts []brain.MemoryCount) string {
	if len(counts) == 0 {
		return ""
	}
	var parts []string
	for _, c := range counts {
		label := string(c.Type)
		if c.Active != 1 {
			label += "s"
		}
		part := fmt.Sprintf("%d %s", c.Active, label)
		if c.Superseded > 0 {
			part += fmt.Sprintf(" (%d superseded)", c.Superseded)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func formatBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
}

// formatJarvisFeedbackBreakdown renders the rating counts as
// "(2 up, 5 down, 1 note)" with zero-count entries omitted. Returns
// empty string when nothing matched, leaving the line tidy.
func formatJarvisFeedbackBreakdown(by map[string]int) string {
	if len(by) == 0 {
		return ""
	}
	var parts []string
	for _, k := range []string{"up", "down", "note"} {
		if n := by[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
