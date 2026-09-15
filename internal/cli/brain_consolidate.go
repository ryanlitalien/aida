package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newBrainConsolidateCmd builds `aida brain consolidate` - promotes
// accumulated typed-memory `event` records into durable `fact`/
// `instruction` records with provenance. Cloned from `aida brain
// compile`'s shape (brain.go's newBrainCompileCmd) but targets typed
// memory instead of routing wisdom; see brain.Brain.Consolidate.
//
// --dry-run still makes the LLM call (the preview is the real
// proposal, not a stub) but skips every write and skips updating the
// auto-trigger state file, so a dry-run never counts as "consolidated"
// for ShouldAutoConsolidate's purposes.
func newBrainConsolidateCmd() *cobra.Command {
	var dryRunFlag bool
	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Promote event records into durable facts/instructions (LLM-powered)",
		Long: "Reads recent typed-memory events plus the currently-active facts\n" +
			"and instructions, and asks an LLM what should be promoted into\n" +
			"durable memory - the Atlas episodic-to-semantic/procedural loop,\n" +
			"cloned from `aida brain compile`'s pattern.\n\n" +
			"Every proposed fact/instruction/supersede must cite the event\n" +
			"id(s) it's based on; proposals without provenance are dropped.\n" +
			"Contradictions are written as supersedes: a `natural` update\n" +
			"keeps full confidence, a `harsh` (flat-reversal) contradiction\n" +
			"is penalized to 0.7.\n\n" +
			"Use --dry-run to see the proposed output without writing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainConsolidate(cmd.Context(), dryRunFlag)
		},
	}
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "print proposed facts/instructions without writing")
	return cmd
}

func runBrainConsolidate(ctx context.Context, dryRunFlag bool) error {
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

	apiKey := cfg.GetAPIKey()
	isOffline := cfg.Model.OfflineMode
	model := cfg.Model.Primary
	if isOffline {
		model = cfg.Model.Fallback
	}
	client := llm.NewClient(apiKey, model, isOffline)

	spinner := ui.NewSpinner()
	spinner.Start("Consolidating memory...")
	result, err := b.Consolidate(ctx, client, dryRunFlag)
	if err != nil {
		spinner.Stop(fmt.Sprintf("%s Consolidate failed", ui.WarnIcon))
		return err
	}
	spinner.Stop(fmt.Sprintf("%s Consolidated", ui.SuccessIcon))

	printConsolidateResult(result, dryRunFlag)

	if !dryRunFlag {
		if err := b.MarkConsolidated(); err != nil {
			ui.PrintVerbose("Brain consolidate", "MarkConsolidated failed: "+err.Error())
		}
	}
	return nil
}

// printConsolidateResult renders the report shared by real and
// --dry-run invocations. Real writes carry an ID (assigned by
// WriteMemory); dry-run proposals don't, so the "(id)" suffix is only
// printed when non-empty.
func printConsolidateResult(r *brain.ConsolidateResult, dryRun bool) {
	label := ""
	if dryRun {
		label = "[dry-run] "
	}
	fmt.Printf("%s%d event(s) considered, %d dropped for missing provenance\n",
		label, r.EventsConsidered, r.Dropped)

	if len(r.FactsWritten) == 0 && len(r.InstructionsWritten) == 0 && len(r.Superseded) == 0 {
		fmt.Println("No facts, instructions, or supersedes proposed.")
		return
	}

	if len(r.FactsWritten) > 0 {
		fmt.Printf("\n%sFacts (%d):\n", label, len(r.FactsWritten))
		for _, f := range r.FactsWritten {
			fmt.Printf("  - %s%s (from %v)\n", keyPrefix(f.Key), f.Body, f.Provenance)
		}
	}
	if len(r.InstructionsWritten) > 0 {
		fmt.Printf("\n%sInstructions (%d):\n", label, len(r.InstructionsWritten))
		for _, ins := range r.InstructionsWritten {
			fmt.Printf("  - %s%s (from %v)\n", keyPrefix(ins.Key), ins.Body, ins.Provenance)
		}
	}
	if len(r.Superseded) > 0 {
		fmt.Printf("\n%sSupersedes (%d):\n", label, len(r.Superseded))
		for _, s := range r.Superseded {
			newID := s.NewID
			if newID == "" {
				newID = "(not written)"
			}
			fmt.Printf("  - %s -> %s [%s]: %s\n", s.OldID, newID, s.Contradiction, s.NewBody)
		}
	}
}

// keyPrefix renders "[key] " for display, or "" when there's no key -
// avoids a bare "[]" on records that legitimately have no dedup key.
func keyPrefix(key string) string {
	if key == "" {
		return ""
	}
	return "[" + key + "] "
}
