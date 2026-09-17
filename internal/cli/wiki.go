package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newWikiCmd builds the `aida wiki` command group -- tools for the
// optional personal wiki: a separate git repo, located by `wiki.path`
// in config.yaml (default ~/dev/aida-wiki), holding an OKF v0.1 bundle
// of projects/entities/concepts pages distilled from a user's own
// archives. Distinct from the brain's lessons/memory/tasks, which live
// in ~/.aida/brain. Subcommand-per-file, mirroring newBrainCmd's
// convention (brain.go, brain_garden.go).
func newWikiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wiki",
		Short: "Tools for the personal wiki (wiki.path, default ~/dev/aida-wiki)",
		Long: "The wiki is a separate git repo of markdown pages (projects/\n" +
			"entities/concepts) distilled from your own archives -- files\n" +
			"are the source of truth, same design constraint as the brain.\n" +
			"Its location comes from `wiki.path` in ~/.aida/config.yaml,\n" +
			"defaulting to ~/dev/aida-wiki. `aida wiki index` derives a\n" +
			"searchable copy into brain.db (the wiki: recall channel in\n" +
			"SearchMulti); `aida wiki lint` audits the repo for drift.",
	}

	cmd.AddCommand(newWikiLintCmd())
	cmd.AddCommand(newWikiIndexCmd())

	return cmd
}

// newWikiIndexCmd builds `aida wiki index`. Thin CLI wiring over
// (*brain.Brain).IndexWiki -- see internal/brain/wiki_index.go for the
// walk/embed/write pipeline.
func newWikiIndexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "index",
		Short: "Index the wiki repo into brain.db for recall",
		Long: "Walks wiki/{projects,entities,concepts} plus wiki_status:\n" +
			"triaged_keep source notes under evernotes/, embeds each page\n" +
			"(Voyage AI, batched at 128), and writes wiki_pages + FTS rows so\n" +
			"the wiki: recall channel (internal/brain/search_multi.go) can\n" +
			"find them. Incremental -- a page is only re-embedded when its\n" +
			"mtime is newer than the last index run. Embeddings are optional:\n" +
			"with no VOYAGE_API_KEY configured, pages still index for keyword\n" +
			"(FTS) recall, matching the rest of the brain's non-fatal embed\n" +
			"degrade.",
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

			wikiRoot := cfg.WikiPath()
			if dryRunGuard("index wiki", wikiRoot) {
				return nil
			}

			spinner := ui.NewSpinner()
			spinner.Start("Indexing wiki...")
			result, err := b.IndexWiki(ctx, wikiRoot)
			if err != nil {
				spinner.Stop(fmt.Sprintf("%s Wiki index failed", ui.WarnIcon))
				return err
			}
			spinner.Stop(fmt.Sprintf("%s Indexed", ui.SuccessIcon))

			fmt.Printf("Scanned %d page(s)/note(s), (re)indexed %d\n", result.Scanned, result.Reindexed)
			if !result.Embedded {
				fmt.Println("(no embeddings -- set VOYAGE_API_KEY for semantic recall; keyword search still works)")
			}
			return nil
		},
	}
}
