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

// newBrainHarvestCmd builds `aida brain harvest` -- the Codex/Gemini/
// Meetily side of the multi-agent memory bridge. Where the Claude Code
// bridge mirrors memory files Claude already writes (brain_capture.go),
// Codex and Gemini write only raw session transcripts and Meetily writes
// only exported call recordings, so this is a distillation harvester: an
// LLM extraction pass (brain.DistillFunc, same shape as consolidate.go's
// episodic -> semantic/procedural pass) reads each new/changed session or
// call and proposes durable fact/instruction/event records, written under
// the matching "codex", "gemini", or "meetily" memory profile.
func newBrainHarvestCmd() *cobra.Command {
	var (
		tool        string
		dryRunFlag  bool
		since       string
		maxSessions int
	)
	cmd := &cobra.Command{
		Use:   "harvest",
		Short: "Distill Codex/Gemini sessions into typed memory records (LLM-powered)",
		Long: "Reads sessions/transcripts from another coding agent (Codex or\n" +
			"Gemini) or exported call recordings (Meetily) updated since the\n" +
			"last watermark, asks an LLM to extract durable fact/instruction/\n" +
			"event records the same way the Claude Code memory bridge does,\n" +
			"and writes them under the matching \"codex\", \"gemini\", or\n" +
			"\"meetily\" memory profile (see `aida brain recall --profile`).\n\n" +
			"Incremental by default: a per-tool watermark tracks which\n" +
			"sessions have already been harvested, so re-running only picks up\n" +
			"new or updated ones. Sessions updated in the last 10 minutes are\n" +
			"treated as still in progress and skipped until a later run finds\n" +
			"them quiet.\n\n" +
			"--since backfills by lower-bounding which sessions are eligible;\n" +
			"the persisted watermark still applies on top of it, so a session\n" +
			"already harvested at or after its current update time is skipped\n" +
			"regardless.\n\n" +
			"--dry-run still makes the real LLM call(s) (the preview reflects\n" +
			"real cost) but writes nothing and does not advance the watermark.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainHarvest(cmd.Context(), tool, dryRunFlag, since, maxSessions)
		},
	}
	cmd.Flags().StringVar(&tool, "tool", "", "source tool to harvest: codex, gemini, or meetily (required)")
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "run distillation and print proposed memories without writing")
	cmd.Flags().StringVar(&since, "since", "", "only consider sessions updated at/after this RFC3339 timestamp (backfill)")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 20, "max sessions to harvest in one run")
	return cmd
}

func runBrainHarvest(ctx context.Context, tool string, dryRunFlag bool, since string, maxSessions int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch tool {
	case "codex", "gemini", "meetily":
	case "":
		return fmt.Errorf("--tool is required: codex, gemini, or meetily")
	default:
		return fmt.Errorf("unknown --tool %q: must be codex, gemini, or meetily", tool)
	}

	opts := brain.HarvestOptions{Since: since, MaxSessions: maxSessions, DryRun: dryRunFlag}

	spinner := ui.NewSpinner()
	spinner.Start(fmt.Sprintf("Harvesting %s sessions...", tool))

	result, err := runHarvestCore(ctx, tool, opts)
	if err != nil {
		spinner.Stop(fmt.Sprintf("%s Harvest failed", ui.WarnIcon))
		return err
	}
	spinner.Stop(fmt.Sprintf("%s Harvested", ui.SuccessIcon))

	printHarvestResult(result)
	return nil
}

// harvestFunc matches runHarvestCore's signature: one harvest pass for a
// tool, given the caller's HarvestOptions. Both `aida brain harvest` and
// the periodic sweep in `aida serve` (harvest_sweep.go) go through this
// same seam; tests inject a fake of this shape so they never make a real
// LLM/Voyage call or touch ~/.codex or ~/.gemini.
type harvestFunc func(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error)

// runHarvestCore is the harvest core shared by `aida brain harvest` and the
// background sweep: loads config, opens a brain handle pinned to the
// tool's fixed memory profile ("codex", "gemini", or "meetily" -- mirroring
// claudeMemoryProfile's role for the Claude bridge, not the caller's own
// auto-detected work/home profile), builds an LLM client, and runs the
// matching HarvestCodex/HarvestGemini/HarvestMeetily pass.
func runHarvestCore(ctx context.Context, tool string, opts brain.HarvestOptions) (*brain.HarvestResult, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}

	b, err := brain.Open(cfg.BrainPath(), tool, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return nil, err
	}
	defer b.Close()

	apiKey := cfg.GetAPIKey()
	isOffline := cfg.Model.OfflineMode
	model := cfg.Model.Primary
	if isOffline {
		model = cfg.Model.Fallback
	}
	client := llm.NewClient(apiKey, model, isOffline)

	switch tool {
	case "codex":
		return b.HarvestCodex(ctx, client.CompleteJSON, opts)
	case "gemini":
		return b.HarvestGemini(ctx, client.CompleteJSON, opts)
	case "meetily":
		return b.HarvestMeetily(ctx, client.CompleteJSON, opts)
	default:
		return nil, fmt.Errorf("unknown tool %q: must be codex, gemini, or meetily", tool)
	}
}

// printHarvestResult renders the report shared by real and --dry-run
// invocations.
func printHarvestResult(r *brain.HarvestResult) {
	label := ""
	if r.DryRun {
		label = "[dry-run] "
	}
	fmt.Printf("%s%s: %d candidate session(s), %d selected, %d skipped (still running)\n",
		label, r.Tool, r.CandidatesTotal, r.Selected, r.SkippedQuiet)

	fromSessions := 0
	for _, s := range r.Sessions {
		fromSessions += len(s.Written)
	}
	fmt.Printf("%s%d memory record(s) from sessions, %d from direct mirrors, %d dropped\n\n",
		label, fromSessions, len(r.DirectMirrored), r.Dropped)

	if fromSessions == 0 && len(r.DirectMirrored) == 0 {
		fmt.Println("No memories written.")
		return
	}

	for _, s := range r.Sessions {
		for _, m := range s.Written {
			fmt.Printf("  - [%s] %s (%s)\n    %s\n", m.Type, m.Key, s.Scope, truncate(m.Body, 160))
		}
	}
	for _, m := range r.DirectMirrored {
		fmt.Printf("  - [%s] %s (mirrored)\n", m.Type, m.Key)
	}
}
