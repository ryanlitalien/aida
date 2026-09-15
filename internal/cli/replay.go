package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newReplayCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "replay [run-id]",
		Short: "Re-run a recorded query against current code/prompts",
		Long: "Loads ~/.aida/runs/<run-id>.json (or the most recent run if no id\n" +
			"is given) and re-executes the recorded question. The new run is saved\n" +
			"separately so you can diff outcomes.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := loadRunOrLatest(args)
			if err != nil {
				return err
			}
			fmt.Printf("%s Replaying run %s\n", ui.SuccessIcon, run.ID)
			fmt.Printf("  question: %s\n\n", run.Question)
			// Reuse the existing query path so the replay produces a fresh
			// run record with current code/prompts.
			return runQuery(cmd, strings.Fields(run.Question))
		},
	}
}

func newTuneCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "tune [run-id]",
		Short: "Print a recorded run in a Claude Code-friendly format",
		Long: "Loads ~/.aida/runs/<run-id>.json (or latest if no id) and prints\n" +
			"a markdown summary suitable for opening alongside the aida source in\n" +
			"a Claude Code session: question, intent, route resolution, per-source\n" +
			"commands and outcomes, final answer.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			run, err := loadRunOrLatest(args)
			if err != nil {
				return err
			}
			if jsonOut {
				data, err := json.MarshalIndent(run, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}
			printTuneMarkdown(run)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the raw JSON instead of markdown")
	return cmd
}

func newRunsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runs",
		Short: "List recent recorded runs",
		RunE: func(_ *cobra.Command, _ []string) error {
			ids, err := runs.List()
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				fmt.Printf("%s No runs recorded yet. Run a query first.\n", ui.WarnIcon)
				return nil
			}
			max := 20
			if len(ids) < max {
				max = len(ids)
			}
			fmt.Printf("Showing %d of %d runs (newest first):\n\n", max, len(ids))
			for _, id := range ids[:max] {
				if r, err := runs.Load(id); err == nil {
					fmt.Printf("  %s  %s\n", id, truncateRunSummary(r.Question, 60))
				} else {
					fmt.Printf("  %s\n", id)
				}
			}
			return nil
		},
	}
}

func loadRunOrLatest(args []string) (*runs.Run, error) {
	if len(args) == 0 {
		r, err := runs.Latest()
		if err != nil {
			return nil, err
		}
		if r == nil {
			return nil, fmt.Errorf("no runs recorded yet")
		}
		return r, nil
	}
	return runs.Load(args[0])
}

func printTuneMarkdown(r *runs.Run) {
	fmt.Printf("# Run %s\n\n", r.ID)
	fmt.Printf("**Question:** %s\n\n", r.Question)
	fmt.Printf("- cwd: `%s`\n", r.Cwd)
	fmt.Printf("- profile: `%s`\n", r.Profile)
	fmt.Printf("- action: `%s`  strategy: `%s`\n", r.Action, r.Strategy)
	if len(r.Entities) > 0 {
		fmt.Printf("- entities: %s\n", strings.Join(r.Entities, ", "))
	}
	fmt.Printf("- total: %d ms\n\n", r.TotalMs)

	fmt.Println("## Library resolution")
	fmt.Printf("- routes matched: %d\n", r.RouteMatches)
	if len(r.LibraryLayers) > 0 {
		fmt.Printf("- layers: %s\n", strings.Join(r.LibraryLayers, ", "))
	}
	if len(r.LibrarySources) > 0 {
		fmt.Printf("- routed sources: %s\n", strings.Join(r.LibrarySources, ", "))
	}
	fmt.Println()

	fmt.Println("## Phases")
	for _, p := range r.Phases {
		fmt.Printf("### %s\n", p.Name)
		for _, s := range p.Sources {
			fmt.Printf("- **%s** (score=%d, %dms, status=%s, artifacts=%d)\n",
				s.Name, s.Score, s.DurationMs, s.Status, s.ArtifactCount)
			if s.Command != "" {
				fmt.Printf("  command: `%s`\n", truncateRunSummary(s.Command, 300))
			}
			if s.Summary != "" {
				fmt.Printf("  > %s\n", truncateRunSummary(s.Summary, 300))
			}
		}
		fmt.Println()
	}

	if len(r.Errors) > 0 {
		fmt.Println("## Errors")
		for _, e := range r.Errors {
			fmt.Printf("- %s\n", e)
		}
		fmt.Println()
	}

	fmt.Println("## Answer")
	fmt.Println(r.Answer)
}
