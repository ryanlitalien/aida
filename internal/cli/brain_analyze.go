package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/eval"
)

// analyzeRow is one source's aggregated failure summary (package-level so the
// proposal generator can consume it).
type analyzeRow struct {
	source    string
	failures  int
	topType   string
	topCount  int
	breakdown []string // e.g. "missing-citation: 5, missing-source: 2"
}

// newBrainAnalyzeCmd builds the `aida brain analyze` subcommand.
//
// The eval-run files in ~/.aida/brain/eval-runs/ are append-only
// per-invocation records of how each synthesis was graded by the
// reviewer loop. Reading them by hand is tedious. This command
// walks them, groups failures by source × type, and prints a
// summary table - the read-side counterpart to the write-side
// machinery added in earlier commits of Action #2.
//
// Surfaces patterns like:
//   - "sqlite has 5 failures over the last 50 runs, all
//     missing-source - synth keeps citing it but planner isn't
//     routing to it"
//   - "github has 2 missing-citation failures - when it does get
//     routed, results don't match what synth claims"
//
// These are exactly the signals that should drive prompt tuning,
// source-config edits, and per-category routing rules.
func newBrainAnalyzeCmd() *cobra.Command {
	var limit int
	var sourceFilter string
	var ndjsonOut bool
	var propose bool
	var outPath string
	var minFailures int

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Summarize eval-run failure patterns by source and type",
		Long: "Walks ~/.aida/brain/eval-runs/, groups failures by source\n" +
			"and failure type, and prints a per-source summary table.\n\n" +
			"Use --source <name> to drill into one source. Use --ndjson to\n" +
			"dump raw eval-run records for piping to jq / bq / pandas.\n\n" +
			"Use --propose to turn the failure patterns into concrete, review-ready\n" +
			"proposals - routing-boost suggestions + golden-test stubs - the\n" +
			"self-improvement meta-loop. --out <file> writes them for a gated PR.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainAnalyze(limit, sourceFilter, ndjsonOut, propose, outPath, minFailures)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "scan only the most recent N eval-runs (0 = all)")
	cmd.Flags().StringVar(&sourceFilter, "source", "", "filter to a single source name")
	cmd.Flags().BoolVar(&ndjsonOut, "ndjson", false, "print raw eval-runs as NDJSON instead of summary table")
	cmd.Flags().BoolVar(&propose, "propose", false, "emit routing-boost + golden-test proposals from the failure patterns")
	cmd.Flags().StringVar(&outPath, "out", "", "also write the --propose report to this file (for a review-gated PR)")
	cmd.Flags().IntVar(&minFailures, "min-failures", 2, "minimum failed runs for a source to earn a proposal")
	return cmd
}

func runBrainAnalyze(limit int, sourceFilter string, ndjsonOut, propose bool, outPath string, minFailures int) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	brainPath := cfg.BrainPath()

	evalRuns, err := brain.ListRecentEvalRuns(brainPath, limit)
	if err != nil {
		return fmt.Errorf("list eval-runs: %w", err)
	}

	if ndjsonOut {
		// One JSON object per line, suitable for piping to jq / bq /
		// pandas. Marshal errors skip that record rather than abort
		// the whole dump - better one missing line than no output.
		for _, r := range evalRuns {
			data, err := json.Marshal(r)
			if err != nil {
				continue
			}
			fmt.Println(string(data))
		}
		return nil
	}

	// Per-source aggregation.
	type bucketKey struct {
		source    string
		issueType string
	}
	bucketCounts := map[bucketKey]int{}
	sourceTotals := map[string]int{}

	totalRuns := len(evalRuns)
	failedRuns := 0
	for _, r := range evalRuns {
		if r.AggregateVerdict != "fail" {
			continue
		}
		failedRuns++
		failures := eval.ParseFailures(r.Reviewers)
		// One source contributes at most once per eval-run to the
		// per-source total - counting every Issue would over-weight
		// runs that produced multiple failures from the same root
		// cause (e.g. 13 missing citations in a single answer).
		seenInRun := map[string]bool{}
		for _, f := range failures {
			if sourceFilter != "" && f.Source != sourceFilter {
				continue
			}
			bucketCounts[bucketKey{f.Source, f.IssueType}]++
			if !seenInRun[f.Source] {
				sourceTotals[f.Source]++
				seenInRun[f.Source] = true
			}
		}
	}

	fmt.Printf("Scanned %d eval-runs (%d failed)\n\n", totalRuns, failedRuns)

	if len(sourceTotals) == 0 {
		fmt.Println("No source-attributable failures.")
		return nil
	}

	// Build per-source summary rows.
	var rows []analyzeRow
	for src, total := range sourceTotals {
		var topType string
		var topCount int
		var breakdown []string
		for k, v := range bucketCounts {
			if k.source != src {
				continue
			}
			breakdown = append(breakdown, fmt.Sprintf("%s: %d", k.issueType, v))
			if v > topCount {
				topType = k.issueType
				topCount = v
			}
		}
		sort.Strings(breakdown)
		rows = append(rows, analyzeRow{
			source:    src,
			failures:  total,
			topType:   topType,
			topCount:  topCount,
			breakdown: breakdown,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].failures != rows[j].failures {
			return rows[i].failures > rows[j].failures
		}
		return rows[i].source < rows[j].source
	})

	// Render. Two columns: header + per-source breakdown line.
	const (
		colSource = 24
		colFails  = 10
	)
	fmt.Printf("%-*s %-*s  %s\n", colSource, "Source", colFails, "Fail runs", "Top failure (count)")
	fmt.Println(strings.Repeat("-", colSource+colFails+30))
	for _, r := range rows {
		topDisplay := " - "
		if r.topType != "" {
			topDisplay = fmt.Sprintf("%s (%d)", r.topType, r.topCount)
		}
		fmt.Printf("%-*s %-*d  %s\n", colSource, r.source, colFails, r.failures, topDisplay)
		// If --source filter is set, also dump the per-type breakdown.
		if sourceFilter != "" && len(r.breakdown) > 1 {
			for _, b := range r.breakdown {
				fmt.Printf("  %s\n", b)
			}
		}
	}

	if propose {
		report := proposeFromFailures(rows, evalRuns, minFailures)
		fmt.Println()
		fmt.Print(report)
		if outPath != "" {
			if err := os.WriteFile(outPath, []byte(report), 0644); err != nil {
				return fmt.Errorf("write proposals: %w", err)
			}
			fmt.Printf("\nWrote proposals to %s - review and open a gated PR.\n", outPath)
		}
	}
	return nil
}

// proposeFromFailures turns aggregated failure patterns into concrete,
// review-ready proposals: routing-boost suggestions (per source over the
// threshold) and golden-test stubs (for the failing question shapes). It only
// SURFACES proposals - nothing is auto-applied; a human (or a gated PR) is the
// gate, which is the whole point of the self-improvement meta-loop.
func proposeFromFailures(rows []analyzeRow, evalRuns []brain.EvalRun, minFailures int) string {
	var b strings.Builder
	b.WriteString("# Aida self-improvement proposals\n\n")
	b.WriteString("Generated by `aida brain analyze --propose` from recent eval-run failures. ")
	b.WriteString("Nothing here is auto-applied - review and apply (or open a gated PR).\n\n")

	b.WriteString("## Routing-boost proposals\n\n")
	any := false
	for _, r := range rows {
		if r.failures < minFailures {
			continue
		}
		any = true
		fmt.Fprintf(&b, "- **%s** - %d failed runs (top: %s ×%d). ", r.source, r.failures, r.topType, r.topCount)
		switch {
		case strings.Contains(r.topType, "missing-source"):
			fmt.Fprintf(&b, "Synthesis keeps citing `%s` but the planner isn't routing to it - add a capability/entity mapping or a +boost so it's actually queried.\n", r.source)
		case strings.Contains(r.topType, "missing-citation"), strings.Contains(r.topType, "scope-mismatch"):
			fmt.Fprintf(&b, "Results from `%s` don't match synthesis's claims - consider a -demote for this question shape or tightening its context doc.\n", r.source)
		case strings.Contains(r.topType, "test-failure"), strings.Contains(r.topType, "build-failure"), strings.Contains(r.topType, "lint-failure"):
			fmt.Fprintf(&b, "Recurring code-gate failures while working `%s` - capture the fix as a STANDING INSTRUCTION (typed-memory instruction) so future agents avoid it.\n", r.source)
		default:
			fmt.Fprintf(&b, "Recurring `%s` failures - review `%s`'s config/context doc.\n", r.topType, r.source)
		}
	}
	if !any {
		fmt.Fprintf(&b, "_No source reached the proposal threshold (--min-failures %d)._\n", minFailures)
	}

	b.WriteString("\n## Golden-test proposals\n\n")
	b.WriteString("Add these failing queries to the golden routing tests so each becomes a regression guard:\n\n")
	seen := map[string]bool{}
	count := 0
	for _, r := range evalRuns {
		if r.AggregateVerdict != "fail" {
			continue
		}
		q := strings.TrimSpace(r.Question)
		if q == "" || seen[q] {
			continue
		}
		seen[q] = true
		fmt.Fprintf(&b, "- Query: %q (run %s)\n", q, r.RunID)
		if count++; count >= 10 {
			break
		}
	}
	if count == 0 {
		b.WriteString("_No failing questions to propose tests for._\n")
	}
	return b.String()
}
