package cli

// `aida tasks drafts` - review surface for auto-solve drafts.
//
// Walks the exec-plans on disk, extracts auto-solve decision-log
// entries, and prints task title + draft. Read-only by design: this
// is the human review gate that sits BETWEEN auto-solve writing
// drafts to plans and any future write-back to Notion / Slack /
// email. Without this surface, drafts are buried in plan markdown
// files and the only way to review is grep + cat.
//
// Filters:
//   --source-hash <hash>  one ingest cohort
//   --tag <substr>        any tag substring (e.g. partner name)
//   --since <YYYY-MM-DD>  plans updated on/after the date
//   --include-failed      include plans whose auto-solve errored
//   --status <s>          plan status filter (active|completed|abandoned)
//   --include-abandoned   shorthand for --status active,completed,abandoned
//
// Output is plain markdown so the user can pipe to less, save to a
// file, or paste into a draft document.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
)

// autoSolveDecisionLine matches one decision-log line authored by
// the auto-solve pass. Captures: timestamp, message-tail.
//
//   - 2026-05-05T18:44:14Z [auto-solve] draft answer:
var autoSolveLogLine = regexp.MustCompile(`^- (\S+) \[auto-solve\] (.*)$`)

// draftRecord is one materialized draft pulled out of an exec-plan.
type draftRecord struct {
	PlanID    string
	PlanTitle string
	PlanTags  []string
	Status    brain.ExecPlanStatus
	UpdatedAt time.Time
	Source    string // origin Ref, when set
	// Body is the draft text (everything after "draft answer:" on
	// the log line). Multi-line drafts have the rest of the body
	// concatenated until the next decision-log entry.
	Body string
	// Failed is true when the auto-solve pass logged an error
	// instead of a draft answer. Surfaced to differentiate fallout
	// from successful drafts in the review surface.
	Failed bool
}

func newTasksDraftsCmd() *cobra.Command {
	var sourceHash string
	var tags []string
	var since string
	var includeFailed bool
	var statuses []string
	var includeAbandoned bool

	cmd := &cobra.Command{
		Use:   "drafts",
		Short: "Review auto-solve drafts pulled from exec-plans",
		Long: "Reads the brain's exec-plans and prints each task's auto-solve\n" +
			"draft. Read-only - the human review gate before any future\n" +
			"write-back to Notion / Slack / email.\n\n" +
			"Filters compose. Default scope is active + completed plans;\n" +
			"--include-abandoned widens to all three lifecycle buckets.\n\n" +
			"Examples:\n" +
			"  aida tasks drafts --source-hash 9eb5b1212d27\n" +
			"  aida tasks drafts --tag butterstack\n" +
			"  aida tasks drafts --since 2026-05-01 --include-failed",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := tasksDraftsOpts{
				SourceHash:       strings.TrimSpace(sourceHash),
				Tags:             tags,
				Since:            strings.TrimSpace(since),
				IncludeFailed:    includeFailed,
				Statuses:         statuses,
				IncludeAbandoned: includeAbandoned,
			}
			return runTasksDrafts(opts)
		},
	}
	cmd.Flags().StringVar(&sourceHash, "source-hash", "", "filter to one ingest cohort by source-hash (the short hex id from `aida tasks ingest`)")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "filter by tag (substring match, repeatable)")
	cmd.Flags().StringVar(&since, "since", "", "only drafts whose plan was updated on/after YYYY-MM-DD")
	cmd.Flags().BoolVar(&includeFailed, "include-failed", false, "include plans whose auto-solve errored")
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "exec-plan status filter (active, completed, abandoned; repeatable). Default: active+completed.")
	cmd.Flags().BoolVar(&includeAbandoned, "include-abandoned", false, "shorthand for --status active,completed,abandoned")
	return cmd
}

// tasksDraftsOpts groups runTasksDrafts arguments.
type tasksDraftsOpts struct {
	SourceHash       string
	Tags             []string
	Since            string
	IncludeFailed    bool
	Statuses         []string
	IncludeAbandoned bool
}

func runTasksDrafts(opts tasksDraftsOpts) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	brainPath := cfg.BrainPath()
	if _, err := os.Stat(brainPath); err != nil {
		return fmt.Errorf("brain not found at %s: %w", brainPath, err)
	}

	// Pick statuses to scan. Explicit --status wins; otherwise
	// active+completed unless --include-abandoned widens it.
	var scanStatuses []brain.ExecPlanStatus
	switch {
	case len(opts.Statuses) > 0:
		for _, s := range opts.Statuses {
			st := brain.ExecPlanStatus(strings.TrimSpace(s))
			if !st.IsValid() {
				return fmt.Errorf("invalid status %q (want one of: active, completed, abandoned)", s)
			}
			scanStatuses = append(scanStatuses, st)
		}
	case opts.IncludeAbandoned:
		scanStatuses = []brain.ExecPlanStatus{brain.ExecPlanStatusActive, brain.ExecPlanStatusCompleted, brain.ExecPlanStatusAbandoned}
	default:
		scanStatuses = []brain.ExecPlanStatus{brain.ExecPlanStatusActive, brain.ExecPlanStatusCompleted}
	}

	// Parse --since once.
	var sinceTime time.Time
	if opts.Since != "" {
		t, err := time.Parse("2006-01-02", opts.Since)
		if err != nil {
			return fmt.Errorf("invalid --since %q (want YYYY-MM-DD): %w", opts.Since, err)
		}
		sinceTime = t
	}

	// Compose the source-hash filter into the tag filter so the
	// match logic is uniform downstream. Empty source-hash leaves
	// the tag set untouched.
	tagFilters := append([]string(nil), opts.Tags...)
	if opts.SourceHash != "" {
		tagFilters = append(tagFilters, sourceTagPrefix+opts.SourceHash)
	}

	var allDrafts []draftRecord
	for _, st := range scanStatuses {
		plans, err := brain.ListExecPlans(brainPath, st)
		if err != nil {
			return fmt.Errorf("list %s plans: %w", st, err)
		}
		for _, p := range plans {
			if !sinceTime.IsZero() && p.UpdatedAt.Before(sinceTime) {
				continue
			}
			if !planMatchesTags(p.Tags, tagFilters) {
				continue
			}
			drafts := extractAutoSolveDrafts(&p)
			for _, d := range drafts {
				if d.Failed && !opts.IncludeFailed {
					continue
				}
				allDrafts = append(allDrafts, d)
			}
		}
	}

	// Newest first by plan UpdatedAt - matches the order users
	// scan their inbox. Stable so multiple drafts from the same
	// plan keep their internal log order.
	sort.SliceStable(allDrafts, func(i, j int) bool {
		return allDrafts[i].UpdatedAt.After(allDrafts[j].UpdatedAt)
	})

	if len(allDrafts) == 0 {
		fmt.Fprintln(os.Stdout, "No drafts found matching the filters.")
		return nil
	}
	renderDrafts(os.Stdout, allDrafts)
	return nil
}

// planMatchesTags reports whether a plan's tags pass the filter.
// Each filter must match at least one tag (AND across filters,
// substring within each filter - same convention as `aida tasks --tag`).
func planMatchesTags(planTags, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		fl := strings.ToLower(f)
		hit := false
		for _, t := range planTags {
			if strings.Contains(strings.ToLower(t), fl) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// extractAutoSolveDrafts walks the plan body looking for
// decision-log lines authored by [auto-solve] and returns one
// draftRecord per "draft answer:" or per failure entry. Multi-line
// draft bodies (the auto-solve pass writes "draft answer:\n<long
// text>") are reassembled until the next "- <ts> [author]" line.
func extractAutoSolveDrafts(plan *brain.ExecPlan) []draftRecord {
	scanner := bufio.NewScanner(strings.NewReader(plan.Body))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var out []draftRecord
	var current *draftRecord
	flush := func() {
		if current != nil {
			current.Body = strings.TrimSpace(current.Body)
			out = append(out, *current)
			current = nil
		}
	}
	for scanner.Scan() {
		line := scanner.Text()
		// New decision-log line - closes any pending draft.
		if strings.HasPrefix(line, "- ") && bracketAfter(line) {
			flush()
			m := autoSolveLogLine.FindStringSubmatch(line)
			if m == nil {
				continue // not an auto-solve line
			}
			tail := m[2]
			current = &draftRecord{
				PlanID:    plan.ID,
				PlanTitle: plan.Title,
				PlanTags:  plan.Tags,
				Status:    plan.Status,
				UpdatedAt: plan.UpdatedAt,
				Source:    plan.Origin.Ref,
			}
			switch {
			case strings.HasPrefix(tail, "draft answer:"):
				body := strings.TrimPrefix(tail, "draft answer:")
				current.Body = strings.TrimSpace(body)
			case strings.HasPrefix(tail, "aida --agent failed"):
				current.Failed = true
				current.Body = tail
			default:
				current.Body = tail
			}
			continue
		}
		// Continuation of the current draft (no leading "- ").
		if current != nil {
			if current.Body == "" {
				current.Body = line
			} else {
				current.Body += "\n" + line
			}
		}
	}
	flush()
	return out
}

// bracketAfter is a cheap pre-filter for "- <ts> [<author>] ..." -
// avoids running the regex on every plan body line.
func bracketAfter(line string) bool {
	i := strings.Index(line, "[")
	j := strings.Index(line, "]")
	return i > 0 && j > i
}

// renderDrafts prints the drafts in a markdown-friendly format
// suitable for terminal review or piping to a file.
func renderDrafts(w *os.File, drafts []draftRecord) {
	fmt.Fprintf(w, "# Drafts (%d)\n\n", len(drafts))
	for i, d := range drafts {
		marker := "✓"
		if d.Failed {
			marker = "✗"
		}
		fmt.Fprintf(w, "## %d. %s %s\n\n", i+1, marker, d.PlanTitle)
		fmt.Fprintf(w, "- plan id: `%s`\n", d.PlanID)
		fmt.Fprintf(w, "- status:  `%s`\n", d.Status)
		fmt.Fprintf(w, "- updated: %s\n", d.UpdatedAt.Format(time.RFC3339))
		if d.Source != "" {
			fmt.Fprintf(w, "- source:  `%s`\n", d.Source)
		}
		if len(d.PlanTags) > 0 {
			fmt.Fprintf(w, "- tags:    %s\n", strings.Join(d.PlanTags, ", "))
		}
		fmt.Fprintln(w)
		if d.Failed {
			fmt.Fprintln(w, "**FAILED**")
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, d.Body)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "---")
		fmt.Fprintln(w)
	}
}

// _ avoids an unused-import flag if the file is later trimmed; the
// filepath import is reserved for follow-up commits that may pull
// extra metadata from the plan path.
var _ = filepath.Join
