package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newBrainReembedCmd builds `aida brain reembed` - the tool a model swap
// (e.g. voyage-3-lite -> voyage-4-lite) needs: unlike `aida brain index`,
// which only fills embeddings that are NULL, this OVERWRITES every stored
// vector across every embedded table with a fresh call to the currently
// configured model. See brain.Brain.ReembedAll.
func newBrainReembedCmd() *cobra.Command {
	var dryRunFlag bool
	var tables []string
	cmd := &cobra.Command{
		Use:   "reembed",
		Short: "Re-embed every stored vector with the currently configured model",
		Long: "Re-embeds and OVERWRITES every row's vector in every embedded brain.db\n" +
			"table (lessons, routing_rules, entities, memory_records, wiki_pages,\n" +
			"knowledge_pages, jarvis_lessons, run_cache), not just rows missing an embedding.\n\n" +
			"Run this after bumping the embedding model or output dimension\n" +
			"(internal/brain/embeddings.go) - old and new model vectors are not\n" +
			"comparable even at the same dimension, so a partial re-embed leaves\n" +
			"the corpus in a mixed, silently-wrong state.\n\n" +
			"Use --table (repeatable) to restrict the pass to specific tables,\n" +
			"e.g. for staging a migration one table at a time. Valid values: " +
			strings.Join(brain.ReembedTableNames(), ", ") + ".\n" +
			"Use --dry-run to preview row counts without calling the embedding\n" +
			"API or writing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainReembed(cmd.Context(), dryRunFlag, tables)
		},
	}
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "preview row counts without embedding or writing")
	cmd.Flags().StringSliceVar(&tables, "table", nil, "restrict to these tables (repeatable); default all")
	return cmd
}

func runBrainReembed(ctx context.Context, dryRunFlag bool, tables []string) error {
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

	if !dryRunFlag && !b.Embeddings.Available() {
		return fmt.Errorf("no embedding client available - set %s", cfg.VoyageKeyEnv())
	}

	spinner := ui.NewSpinner()
	spinner.Start("Re-embedding brain vectors...")
	result, err := b.ReembedAll(ctx, tables, dryRunFlag)
	if err != nil {
		spinner.Stop(fmt.Sprintf("%s Re-embed failed", ui.WarnIcon))
		return err
	}
	spinner.Stop(fmt.Sprintf("%s Re-embedded", ui.SuccessIcon))

	printReembedResult(result)
	return nil
}

// printReembedResult renders the per-table report shared by real and
// --dry-run invocations. Mirrors printMineRunsResult's dry-run/real split.
func printReembedResult(r *brain.ReembedResult) {
	for _, t := range r.Tables {
		if r.DryRun {
			fmt.Printf("  %-16s %d row(s) would be re-embedded\n", t.Table, t.Rows)
		} else {
			fmt.Printf("  %-16s %d/%d row(s) re-embedded\n", t.Table, t.Updated, t.Rows)
		}
	}
	if r.DryRun {
		fmt.Printf("[dry-run] %d row(s) total across %d table(s)\n", r.TotalRows(), len(r.Tables))
		return
	}
	fmt.Printf("%d row(s) re-embedded across %d table(s)\n", r.TotalUpdated(), len(r.Tables))
}
