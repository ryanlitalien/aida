package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newBrainMineRunsCmd builds `aida brain mine-runs` - mines ~/.aida/runs/
// history into the Tier-1 run_cache table. Cloned from `aida brain
// consolidate`'s shape (brain_consolidate.go), but needs no LLM client:
// MineRuns only requires the brain's DB + embeddings client (for question
// similarity clustering), not a Claude call. See brain.Brain.MineRuns.
func newBrainMineRunsCmd() *cobra.Command {
	var dryRunFlag bool
	cmd := &cobra.Command{
		Use:   "mine-runs",
		Short: "Mine ~/.aida/runs/ history into the Tier-1 run_cache table",
		Long: "Reads every past aida invocation recorded under ~/.aida/runs/,\n" +
			"drops answers that shouldn't be cached verbatim (empty, a refusal,\n" +
			"a task-intercept, or a thumbs-down'd answer), clusters near-duplicate\n" +
			"questions by embedding similarity, and rebuilds run_cache with the\n" +
			"single best-scoring answer per cluster.\n\n" +
			"The query pipeline's Tier-1 lookup (a later phase) reads this table\n" +
			"to short-circuit repeat questions without re-running the full\n" +
			"parse/classify/resolve/plan/execute/synthesize pipeline.\n\n" +
			"Use --dry-run to see what would be mined without writing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainMineRuns(cmd.Context(), dryRunFlag)
		},
	}
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "preview what would be mined without writing to run_cache")
	return cmd
}

func runBrainMineRuns(ctx context.Context, dryRunFlag bool) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	_, profileName := cfg.ActiveProfileConfig()
	if ctx == nil {
		ctx = context.Background()
	}

	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return err
	}
	defer b.Close()

	spinner := ui.NewSpinner()
	spinner.Start("Mining run history...")
	result, err := b.MineRuns(ctx, dryRunFlag)
	if err != nil {
		spinner.Stop(fmt.Sprintf("%s Mine failed", ui.WarnIcon))
		return err
	}
	spinner.Stop(fmt.Sprintf("%s Mined", ui.SuccessIcon))

	printMineRunsResult(result, dryRunFlag)
	return nil
}

// printMineRunsResult renders the report shared by real and --dry-run
// invocations. EntriesWritten == 0 is a valid, expected outcome for a real
// run (e.g. a fresh brain with no run history yet, or every run filtered
// out) and is printed plainly rather than treated as an error. MineRuns
// leaves EntriesWritten at its zero value on --dry-run (it skips the
// write entirely), so the preview line reports ClustersFormed instead --
// otherwise a dry-run with real clusters would misleadingly print "0
// entries written".
func printMineRunsResult(r *brain.MineResult, dryRun bool) {
	if dryRun {
		fmt.Printf("[dry-run] %d run(s) considered, %d eligible, %d cluster(s) formed (would write %d run_cache entries)\n",
			r.RunsConsidered, r.RunsEligible, r.ClustersFormed, r.ClustersFormed)
		return
	}
	fmt.Printf("%d run(s) considered, %d eligible, %d cluster(s) formed, %d run_cache entries written\n",
		r.RunsConsidered, r.RunsEligible, r.ClustersFormed, r.EntriesWritten)
}
