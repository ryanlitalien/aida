package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/spf13/cobra"
)

// newBrainRecallCmd builds `aida brain recall` -- scope-aware recall over
// the typed-memory records captured from Claude Code sessions (Phase 3 of
// the memory bridge). It is deliberately separate from `brain search`
// (lesson/entity-shaped): recall reads memory_records.body_embedding, which
// nothing else surfaces, and adds a recency mode for "last/latest" queries.
func newBrainRecallCmd() *cobra.Command {
	var (
		k       int
		scope   string
		tag     string
		profile string
		recent  bool
	)
	cmd := &cobra.Command{
		Use:   "recall [query]",
		Short: "Recall captured coding-agent memories (semantic or recency)",
		Long: "Recall the durable memories captured from coding-agent sessions.\n\n" +
			"Two modes: semantic (embed the query, rank by cosine similarity) and\n" +
			"recency (--recent, or a query containing last/latest/recent -- lists the\n" +
			"newest records). Filter to a project with --scope project:<slug>; a\n" +
			"scoped query sees global memories plus that project's, never another\n" +
			"project's. Filter to a tag with --tag <tag> (e.g. a meetily classifier\n" +
			"tag like \"cta\" to recall only that project's calls).\n\n" +
			"Defaults to the pinned \"claude\" memory profile (captured live from\n" +
			"Claude Code memory files). Pass --profile codex, --profile gemini, or\n" +
			"--profile meetily for memories the multi-agent memory bridge\n" +
			"harvested from those sources (`aida brain harvest --tool\n" +
			"codex|gemini|meetily`), or --profile all to search every profile at\n" +
			"once.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			if strings.TrimSpace(query) == "" && !recent {
				return fmt.Errorf("provide a query, or pass --recent to list the newest memories")
			}
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

			res, err := b.RecallMemories(context.Background(), query, k, profile, scope, tag, recent)
			if err != nil {
				return err
			}
			if len(res.Memories) == 0 {
				fmt.Println("No memories found.")
				return nil
			}

			mode := "most recent"
			if res.BySimilarity {
				mode = "semantic"
			}
			fmt.Printf("Recalled %d memories (%s):\n\n", len(res.Memories), mode)
			for i, m := range res.Memories {
				r := m.Record
				created := r.Created
				if len(created) >= 10 {
					created = created[:10]
				}
				meta := created
				if res.BySimilarity {
					meta = fmt.Sprintf("sim %.2f, %s", m.Similarity, created)
				}
				fmt.Printf("%d. [%s] %s  (%s)\n", i+1, r.Type, r.Key, meta)
				body := strings.ReplaceAll(strings.TrimSpace(r.Body), "\n", " ")
				fmt.Printf("   %s\n\n", truncate(body, 200))
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&k, "limit", "k", 5, "number of results")
	cmd.Flags().StringVar(&scope, "scope", "", "restrict to a scope: global or project:<slug>")
	cmd.Flags().StringVar(&tag, "tag", "", "restrict to records carrying this tag (case-insensitive), e.g. a meetily classifier tag like \"cta\"")
	cmd.Flags().StringVar(&profile, "profile", brain.ClaudeMemoryProfile, "memory profile to search: claude (default), codex, gemini, meetily, or \"all\" for every profile")
	cmd.Flags().BoolVar(&recent, "recent", false, "list the newest memories instead of semantic search")
	return cmd
}
