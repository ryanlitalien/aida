package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/models"
	"github.com/ryanlitalien/aida/internal/taskstate"
)

// newArbiterCmd builds the `aida arbiter` command tree: inspection,
// dry-run planning, signal ingestion, and hand-off/ledger reads for the
// arbiter (internal/arbiter, docs/arbiter-plan.md). Nothing under this
// command spawns a model or dispatches a task on its own -- `plan` runs
// nothing (build brief), `signal`/`signals` only read and append to the
// on-disk store, and `handoff`/`ledger` are read-only. Enforcement (a
// loop actually asking Pick before it spawns) lives behind `aida loop
// --arbiter` per ADR-0002, off by default.
func newArbiterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "arbiter",
		Short: "Inspect and dry-run the arbiter: lanes, plan, signals, hand-off, ledger",
		Long: "The arbiter grants scarce model capacity to one requestor at a time by\n" +
			"policy (docs/arbiter-plan.md section 1). This command tree is entirely\n" +
			"read-only or dry-run: `aida arbiter plan` shows which lane each open\n" +
			"task would get without running anything, `aida arbiter signal` is the\n" +
			"ingest path for hooks and wrappers (they signal, they never decide),\n" +
			"and `lanes`/`signals`/`handoff`/`ledger` all just read state that\n" +
			"already exists on disk. Actually enforcing lane choice at dispatch\n" +
			"time is `aida loop --arbiter` (ADR-0002), a separate opt-in flag.",
	}
	cmd.AddCommand(newArbiterLanesCmd())
	cmd.AddCommand(newArbiterPlanCmd())
	cmd.AddCommand(newArbiterSignalCmd())
	cmd.AddCommand(newArbiterSignalsCmd())
	cmd.AddCommand(newArbiterHandoffCmd())
	cmd.AddCommand(newArbiterLedgerCmd())
	cmd.AddCommand(newArbiterStatusCmd())
	return cmd
}

// errNoLaneRoster is the exact required-roster error every lane consumer
// (aida arbiter lanes|plan, aida loop --arbiter's newArbiterRuntime)
// returns when path doesn't exist. Ryan's 2026-09-25 decision: a lane
// roster names real accounts and machines, so a missing one must fail
// loudly here instead of silently running every task through
// arbiter.DefaultConfig()'s placeholder lanes -- see that function's own
// doc comment. `aida arbiter lanes --example` is the escape hatch for
// seeing the placeholder roster without a file.
func errNoLaneRoster(path string) error {
	return fmt.Errorf("no lane roster at %s; copy examples/lanes.yaml there and edit it (aida arbiter lanes --lint validates it)", path)
}

// loadLanesConfig loads the lane roster at path. Unlike the CLI's older
// convention of falling back to arbiter.DefaultConfig() when the file was
// missing, this is now a hard requirement: see errNoLaneRoster's doc
// comment for why.
func loadLanesConfig(path string) (*arbiter.Config, error) {
	cfg, err := arbiter.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoLaneRoster(path)
		}
		return nil, err
	}
	return cfg, nil
}

// ---- aida arbiter lanes ----

func newArbiterLanesCmd() *cobra.Command {
	var jsonOut, lint, example bool

	cmd := &cobra.Command{
		Use:   "lanes",
		Short: "List the configured arbiter lanes",
		Long: "Loads ~/.aida/lanes.yaml (config.Config.LanesPath) and prints one row\n" +
			"per lane in cheapest-first order; a missing file is an error naming\n" +
			"the path to create (see errNoLaneRoster). --example prints the\n" +
			"built-in placeholder roster (arbiter.DefaultConfig()) instead, never\n" +
			"touching disk -- the starting point to copy into ~/.aida/lanes.yaml.\n" +
			"--lint runs Config.Validate and exits non-zero (printing the error)\n" +
			"when the roster is misconfigured; without --lint a validation\n" +
			"problem is shown as a warning but the table still prints. --json\n" +
			"dumps the parsed config instead.",
		RunE: func(_ *cobra.Command, _ []string) error {
			var acfg *arbiter.Config
			if example {
				acfg = arbiter.DefaultConfig()
				fmt.Println("showing the built-in placeholder roster (--example) -- not read from any file; copy examples/lanes.yaml to ~/.aida/lanes.yaml and edit it for real use")
			} else {
				cfg, err := config.LoadConfig()
				if err != nil {
					return fmt.Errorf("load config: %w", err)
				}
				acfg, err = loadLanesConfig(cfg.LanesPath())
				if err != nil {
					return err
				}
			}

			verr := acfg.Validate()

			if jsonOut {
				data, err := json.MarshalIndent(acfg, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling lane config: %w", err)
				}
				fmt.Println(string(data))
			} else {
				fmt.Print(renderLanesTable(acfg.Sorted()))
			}

			if verr != nil {
				if lint {
					return fmt.Errorf("lane config invalid: %w", verr)
				}
				fmt.Printf("\nwarning: lane config invalid: %v\n", verr)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "dump the parsed lane config as JSON")
	cmd.Flags().BoolVar(&lint, "lint", false, "exit non-zero (and print the error) when the lane config fails validation")
	cmd.Flags().BoolVar(&example, "example", false, "print the built-in placeholder roster (arbiter.DefaultConfig()) instead of reading ~/.aida/lanes.yaml")
	return cmd
}

// renderLanesTable renders one row per lane, cheapest-first (the order
// lanes is already expected to be in -- callers pass Config.Sorted()).
// Factored out from newArbiterLanesCmd's RunE so it's unit-testable
// without a config file on disk.
func renderLanesTable(lanes []arbiter.Lane) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ORDER\tID\tRUNNER\tAUTH\tDATA CLASSES\tROLES\tEXECUTOR\tPROVIDER\tWINDOWS\tUNPROBED")
	for _, l := range lanes {
		roles := "all"
		if len(l.Roles) > 0 {
			roles = strings.Join(l.Roles, ",")
		}
		classes := make([]string, len(l.DataClasses))
		for i, c := range l.DataClasses {
			classes[i] = string(c)
		}
		windows := strings.Join(l.Windows, ",")
		if l.Spend {
			windows = "spend"
		}
		if windows == "" {
			windows = "-"
		}
		provider := l.Provider
		if provider == "" {
			provider = "-"
		}
		unprobed := "no"
		if l.AllowUnprobed {
			unprobed = "yes"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			l.Order, l.ID, l.Runner, l.Auth, strings.Join(classes, ","), roles,
			l.ModelFor(arbiter.RoleExecutor), provider, windows, unprobed)
	}
	w.Flush()
	return buf.String()
}

// ---- aida arbiter plan ----

// planRow is one task's dry-run decision: the class its tags resolved
// to, the lane and model Pick chose (or "none"), whether that lane is
// near exhaustion, and the reason -- the winning lane's Decision.Reason,
// or every rejected lane's reason joined with "; " when nothing was
// eligible. Exported field names so it round-trips cleanly through
// `--json`.
type planRow struct {
	TaskID         int    `json:"task_id"`
	Slug           string `json:"slug"`
	Class          string `json:"class"`
	Lane           string `json:"lane"`
	Model          string `json:"model,omitempty"`
	NearExhaustion bool   `json:"near_exhaustion"`
	Reason         string `json:"reason"`
}

// formatRejections joins every lane Pick skipped into one string, e.g.
// "claude-max: signalled empty until ...; claude-pro-bs: data class ...
// not allowed". Rejected already carries exactly one entry per skipped
// lane (arbiter.Pick's own doc comment), so this is literally "the first
// rejection per lane" the build brief calls for.
func formatRejections(rejected []arbiter.Rejection) string {
	if len(rejected) == 0 {
		return "no lanes configured"
	}
	parts := make([]string, len(rejected))
	for i, r := range rejected {
		parts[i] = fmt.Sprintf("%s: %s", r.Lane, r.Reason)
	}
	return strings.Join(parts, "; ")
}

// buildPlanRow classifies one task's tags and asks Pick for a lane,
// without running anything -- the pure step behind `aida arbiter plan`,
// factored out so it's testable against a canned []burndown.Capacity and
// arbiter.DefaultConfig without a real probe or signal store. A
// ClassifyTags error (the personal+butterstack conflict, ADR-0003) is
// reported as class "CONFLICT" rather than aborting the whole plan --
// one bad task's tags must never hide every other task's decision.
func buildPlanRow(cfg *arbiter.Config, task brain.TaskRecord, role string, caps []burndown.Capacity, signals arbiter.SignalReader, now time.Time) planRow {
	row := planRow{TaskID: task.TaskID, Slug: task.Slug, Lane: "none"}

	if _, err := arbiter.ClassifyTags(task.Tags); err != nil {
		row.Class = "CONFLICT"
		row.Reason = err.Error()
		return row
	}

	req := arbiter.Request{TaskSlug: task.Slug, TaskID: task.TaskID, Tags: task.Tags, Role: role, Now: now}
	dec, err := arbiter.Pick(cfg, req, caps, signals)
	row.Class = string(dec.Class)
	if err != nil {
		row.Reason = formatRejections(dec.Rejected)
		return row
	}
	row.Lane = dec.Lane.ID
	row.Model = dec.Model
	row.NearExhaustion = dec.NearExhaustion
	row.Reason = dec.Reason
	return row
}

// pickPlanTasks lists every open + in-progress task carrying all of tags,
// sorted the same way loop.go's pickBatch sorts a round's batch
// (priority tag then created) -- the plan should show tasks in the same
// order the loop would actually work them, with no cap (n=0: this is a
// dry run over the whole set, not one round's batch).
func pickPlanTasks(brn *brain.Brain, tags []string) ([]brain.TaskRecord, error) {
	tasks, err := brn.ListTasksByStatus([]string{brain.StatusOpen, brain.StatusInProgress}, tags, 0, 0)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(tasks, func(a, b int) bool {
		pa, pb := tasks[a].PriorityTag(), tasks[b].PriorityTag()
		if pa != pb {
			return pa < pb // "p1" < "p2" < "p3" lexically
		}
		return tasks[a].Created < tasks[b].Created
	})
	return tasks, nil
}

// loadCapacityJSON reads a JSON array of burndown.Capacity from path --
// the offline/test seam --capacity-json opens: `aida arbiter plan` can
// be exercised without probing a single live provider.
func loadCapacityJSON(path string) ([]burndown.Capacity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var caps []burndown.Capacity
	if err := json.Unmarshal(data, &caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// renderPlanTable renders one row per planRow.
func renderPlanTable(rows []planRow) string {
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "TASK\tCLASS\tLANE\tMODEL\tNEAR-EXHAUSTION\tREASON")
	for _, r := range rows {
		model := r.Model
		if model == "" {
			model = "-"
		}
		near := "no"
		if r.NearExhaustion {
			near = "yes"
		}
		if r.Lane == "" || r.Lane == "none" {
			near = "-"
		}
		fmt.Fprintf(w, "#%d\t%s\t%s\t%s\t%s\t%s\n", r.TaskID, r.Class, r.Lane, model, near, r.Reason)
	}
	w.Flush()
	return buf.String()
}

// planFooter reports how many tasks each lane picked up, plus how many
// landed on no lane at all -- the two numbers the build brief asks for.
func planFooter(rows []planRow) string {
	counts := map[string]int{}
	var order []string
	noLane := 0
	for _, r := range rows {
		if r.Lane == "" || r.Lane == "none" {
			noLane++
			continue
		}
		if _, ok := counts[r.Lane]; !ok {
			order = append(order, r.Lane)
		}
		counts[r.Lane]++
	}
	sort.Strings(order)
	parts := make([]string, 0, len(order)+1)
	for _, l := range order {
		parts = append(parts, fmt.Sprintf("%s: %d", l, counts[l]))
	}
	parts = append(parts, fmt.Sprintf("no lane: %d", noLane))
	return fmt.Sprintf("%d task(s) -- %s", len(rows), strings.Join(parts, ", "))
}

func newArbiterPlanCmd() *cobra.Command {
	var tags []string
	var role string
	var jsonOut bool
	var fresh bool
	var capacityJSONPath string

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Dry run: show which lane each open task would get -- runs nothing",
		Long: "Lists every open + in-progress task (optionally filtered by --tag,\n" +
			"ANDed), probes live capacity the same way `aida burndown capacity`\n" +
			"does, and for each task shows the data class its tags resolved to,\n" +
			"the lane and model arbiter.Pick chose, whether that lane is near\n" +
			"exhaustion, and why. This is plan section 8 step 2's dry run: it\n" +
			"never spawns anything and never mutates a task. --capacity-json\n" +
			"reads a JSON array of burndown.Capacity from a file instead of\n" +
			"probing (the offline/test path).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			_, profileName := cfg.ActiveProfileConfig()
			if profileName == "" {
				return fmt.Errorf("no active profile resolved (set AIDA_PROFILE or `aida profile use <name>`)")
			}
			brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return fmt.Errorf("open brain: %w", err)
			}

			tasks, err := pickPlanTasks(brn, tags)
			if err != nil {
				return fmt.Errorf("list tasks: %w", err)
			}

			acfg, err := loadLanesConfig(cfg.LanesPath())
			if err != nil {
				return err
			}

			var caps []burndown.Capacity
			if capacityJSONPath != "" {
				caps, err = loadCapacityJSON(capacityJSONPath)
				if err != nil {
					return fmt.Errorf("load --capacity-json %q: %w", capacityJSONPath, err)
				}
			} else {
				r, err := models.Load(cfg.ModelsPath())
				if err != nil {
					return fmt.Errorf("loading models roster: %w", err)
				}
				bcfg, err := burndown.Load(cfg.BurndownPath())
				if err != nil {
					return fmt.Errorf("loading burndown config: %w", err)
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), modelsProbeTimeout)
				defer cancel()
				usages := models.ProbeWithOptions(ctx, r, models.ProbeOptions{Force: fresh})
				caps = burndown.Report(r.Providers, usages, bcfg, time.Now())
			}

			store, err := arbiter.OpenStore(filepath.Join(config.Dir(), "arbiter"))
			if err != nil {
				return fmt.Errorf("open signal store: %w", err)
			}

			effRole := role
			if effRole == "" {
				effRole = arbiter.RoleExecutor
			}
			now := time.Now()
			rows := make([]planRow, 0, len(tasks))
			for _, t := range tasks {
				rows = append(rows, buildPlanRow(acfg, t, effRole, caps, store, now))
			}

			if jsonOut {
				data, err := json.MarshalIndent(rows, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling decisions: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			fmt.Print(renderPlanTable(rows))
			fmt.Println()
			fmt.Println(planFooter(rows))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "only plan tasks carrying ALL of these tags (repeatable)")
	cmd.Flags().StringVar(&role, "role", "", "Pick role to plan for: executor, thinker, or trivial (default executor)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the decisions as JSON")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "force a re-probe of every provider, ignoring the TTL cache")
	cmd.Flags().StringVar(&capacityJSONPath, "capacity-json", "", "read []burndown.Capacity from this JSON file instead of probing live providers")
	return cmd
}

// ---- aida arbiter signal ----

// formatSignalLine renders the one-line summary `aida arbiter signal`
// prints (to stdout normally, to stderr with --verbose under
// --claude-stop-hook). Factored out from the command's RunE so the
// empty/ambiguous/not-empty shapes are unit-testable without stdin, a
// clock, or a signal store.
func formatSignalLine(lane string, verdict arbiter.Verdict, matched string, emptyUntil time.Time) string {
	if verdict == arbiter.VerdictNotEmpty {
		return fmt.Sprintf("lane %s: not-empty, no mark", lane)
	}
	return fmt.Sprintf("lane %s: %s (matched %q) empty until %s", lane, verdict, matched, emptyUntil.Format(time.RFC3339))
}

func newArbiterSignalCmd() *cobra.Command {
	var lane, source, text, resetsAt string
	var exitCode int
	var useStdin, claudeStopHook, verbose bool

	cmd := &cobra.Command{
		Use:   "signal",
		Short: "Record a lane-empty observation (hooks and wrappers only -- the arbiter decides)",
		Long: "The ingest path for docs/arbiter-plan.md section 5: a Claude Code Stop\n" +
			"hook or a launcher wrapper only signals what it observed -- classified\n" +
			"via arbiter.ClassifySignal and recorded to ~/.aida/arbiter/signals.ndjson\n" +
			"-- it never decides what to do about it. Per plan decision 2, an\n" +
			"ambiguous signal FAILS OPEN: it is treated exactly like an empty one\n" +
			"(the lane is marked empty), just flagged ambiguous=true so the\n" +
			"false-positive rate stays measurable (`aida arbiter signals`).\n\n" +
			"This command ALWAYS exits 0 -- a hook must never break the session\n" +
			"it's attached to -- and with --claude-stop-hook it prints nothing on\n" +
			"stdout unless --verbose is also given (Claude Code shows hook stdout\n" +
			"to the user; the summary line goes to stderr instead in that mode).\n\n" +
			"Claude Code Stop hook, in ~/.claude/settings.json:\n\n" +
			"  {\"hooks\":{\"Stop\":[{\"hooks\":[{\"type\":\"command\",\"command\":\"aida arbiter signal --lane claude-max --source claude-stop --claude-stop-hook\"}]}]}}\n\n" +
			"Wrapper form, for codex exec / agy:\n\n" +
			"  ... ; aida arbiter signal --lane codex-plus --source wrapper --exit-code $? --stdin < out.txt\n",
		RunE: func(_ *cobra.Command, _ []string) error {
			runArbiterSignal(arbiterSignalOpts{
				lane: lane, source: source, text: text, resetsAt: resetsAt,
				exitCode: exitCode, useStdin: useStdin, claudeStopHook: claudeStopHook, verbose: verbose,
			})
			// Always exit 0 -- see the Long help: a hook must never break
			// the session it's attached to, whatever went wrong classifying
			// or recording this observation.
			return nil
		},
	}
	cmd.Flags().StringVar(&lane, "lane", "", "lane id this signal is about")
	cmd.Flags().StringVar(&source, "source", "", "who is signalling (e.g. claude-stop, wrapper)")
	cmd.Flags().IntVar(&exitCode, "exit-code", 0, "exit code of the observed command")
	cmd.Flags().StringVar(&text, "text", "", "inline text to classify")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read raw text to classify from stdin")
	cmd.Flags().BoolVar(&claudeStopHook, "claude-stop-hook", false, "read a Claude Code Stop hook JSON payload from stdin and classify the last assistant message")
	cmd.Flags().StringVar(&resetsAt, "resets-at", "", "RFC3339 time this lane's window resets, if known")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print the summary line even under --claude-stop-hook (to stderr)")
	return cmd
}

// arbiterSignalOpts bundles `aida arbiter signal`'s flags for
// runArbiterSignal.
type arbiterSignalOpts struct {
	lane, source, text, resetsAt      string
	exitCode                          int
	useStdin, claudeStopHook, verbose bool
}

// runArbiterSignal is the impure body of `aida arbiter signal`: reads
// the observation (stdin, --text, or the Stop hook payload), classifies
// it, resolves how long the lane should be marked empty, and records it.
// Every failure here is swallowed (optionally noted on stderr with
// --verbose) rather than propagated -- see the command's Long help on
// why this must never break the caller's session.
func runArbiterSignal(o arbiterSignalOpts) {
	if o.lane == "" {
		if o.verbose {
			fmt.Fprintln(os.Stderr, "arbiter signal: --lane is required, nothing recorded")
		}
		return
	}

	var text string
	switch {
	case o.claudeStopHook:
		_, lastText, err := arbiter.ParseClaudeStopHook(os.Stdin)
		if err != nil {
			if o.verbose {
				fmt.Fprintf(os.Stderr, "arbiter signal: parsing stop hook payload: %v\n", err)
			}
			return
		}
		text = lastText
	case o.useStdin:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			if o.verbose {
				fmt.Fprintf(os.Stderr, "arbiter signal: reading stdin: %v\n", err)
			}
			return
		}
		text = string(data)
	default:
		text = o.text
	}

	verdict, matched := arbiter.ClassifySignal(o.exitCode, text)

	var emptyUntil time.Time
	if verdict == arbiter.VerdictEmpty || verdict == arbiter.VerdictAmbiguous {
		var resetsAt time.Time
		if o.resetsAt != "" {
			if t, err := time.Parse(time.RFC3339, o.resetsAt); err == nil {
				resetsAt = t
			} else if o.verbose {
				fmt.Fprintf(os.Stderr, "arbiter signal: parsing --resets-at %q: %v\n", o.resetsAt, err)
			}
		}
		emptyUntil = arbiter.ResolveEmptyUntil(time.Now(), resetsAt, arbiter.DefaultEmptyFallback)
	}

	host, _ := os.Hostname()
	sig := arbiter.Signal{
		At:         time.Now(),
		Lane:       o.lane,
		Source:     o.source,
		Verdict:    verdict,
		Ambiguous:  verdict == arbiter.VerdictAmbiguous,
		Matched:    matched,
		EmptyUntil: emptyUntil,
		Raw:        text,
		Host:       host,
	}

	store, err := arbiter.OpenStore(filepath.Join(config.Dir(), "arbiter"))
	if err != nil {
		if o.verbose {
			fmt.Fprintf(os.Stderr, "arbiter signal: opening store: %v\n", err)
		}
	} else if err := store.Record(sig); err != nil && o.verbose {
		fmt.Fprintf(os.Stderr, "arbiter signal: recording: %v\n", err)
	}

	line := formatSignalLine(o.lane, verdict, matched, emptyUntil)
	if o.claudeStopHook {
		if o.verbose {
			fmt.Fprintln(os.Stderr, line)
		}
		return
	}
	fmt.Println(line)
}

// ---- aida arbiter signals ----

// filterSignalsSince keeps only signals at or after cutoff. A zero
// cutoff (no --since given) keeps everything.
func filterSignalsSince(sigs []arbiter.Signal, cutoff time.Time) []arbiter.Signal {
	if cutoff.IsZero() {
		return sigs
	}
	out := make([]arbiter.Signal, 0, len(sigs))
	for _, s := range sigs {
		if !s.At.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// renderSignalLine renders one recorded signal.
func renderSignalLine(s arbiter.Signal) string {
	parts := []string{
		s.At.Format(time.RFC3339),
		s.Lane,
		s.Source,
		string(s.Verdict),
		fmt.Sprintf("ambiguous=%t", s.Ambiguous),
	}
	if s.Matched != "" {
		parts = append(parts, fmt.Sprintf("matched=%q", s.Matched))
	}
	if !s.EmptyUntil.IsZero() {
		parts = append(parts, "empty_until="+s.EmptyUntil.Format(time.RFC3339))
	}
	return strings.Join(parts, "  ")
}

// signalsFooter renders the false-positive-rate line the build brief
// calls for -- "so the false-positive rate is one command away".
func signalsFooter(total, ambiguous int) string {
	if total == 0 {
		return "0 signals, 0 ambiguous (fail-open marks), ambiguous rate n/a"
	}
	rate := float64(ambiguous) / float64(total) * 100
	return fmt.Sprintf("%d signals, %d ambiguous (fail-open marks), ambiguous rate %.1f%%", total, ambiguous, rate)
}

// renderLaneMarks reports which of lanes' ids are currently marked
// empty in signals, and until when.
func renderLaneMarks(lanes []arbiter.Lane, signals arbiter.SignalReader, now time.Time) string {
	var lines []string
	for _, l := range lanes {
		if until, ok := signals.EmptyUntil(l.ID, now); ok {
			lines = append(lines, fmt.Sprintf("  %s: empty until %s", l.ID, until.Format(time.RFC3339)))
		}
	}
	if len(lines) == 0 {
		return "No lanes currently marked empty."
	}
	return "Lanes marked empty:\n" + strings.Join(lines, "\n")
}

func newArbiterSignalsCmd() *cobra.Command {
	var jsonOut bool
	var since string

	cmd := &cobra.Command{
		Use:   "signals",
		Short: "Show recorded lane signals and the false-positive rate",
		Long: "Reads ~/.aida/arbiter/signals.ndjson, optionally filtered to the last\n" +
			"--since duration (e.g. 24h), and prints one line per signal plus a\n" +
			"footer with the ambiguous (fail-open) rate -- the measurable proxy\n" +
			"docs/arbiter-plan.md decision 2 calls for. Also shows which lanes are\n" +
			"currently marked empty in lane-state.json, and until when.",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			store, err := arbiter.OpenStore(filepath.Join(config.Dir(), "arbiter"))
			if err != nil {
				return fmt.Errorf("open signal store: %w", err)
			}
			sigs, err := store.ReadAll()
			if err != nil {
				return fmt.Errorf("read signals: %w", err)
			}

			var cutoff time.Time
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("parsing --since %q: %w", since, err)
				}
				cutoff = time.Now().Add(-d)
			}
			filtered := filterSignalsSince(sigs, cutoff)

			if jsonOut {
				data, err := json.MarshalIndent(filtered, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling signals: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			for _, s := range filtered {
				fmt.Println(renderSignalLine(s))
			}
			fmt.Println()
			total, ambiguous := arbiter.FalsePositiveSummary(filtered)
			fmt.Println(signalsFooter(total, ambiguous))

			acfg, err := loadLanesConfig(cfg.LanesPath())
			if err != nil {
				return err
			}
			fmt.Println()
			fmt.Println(renderLaneMarks(acfg.Sorted(), store, time.Now()))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the filtered signals as JSON")
	cmd.Flags().StringVar(&since, "since", "", "only show signals within this duration of now (e.g. 24h)")
	return cmd
}

// ---- aida arbiter handoff ----

func newArbiterHandoffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "Inspect a task's STATE.json / HANDOFF.md",
	}
	cmd.AddCommand(newArbiterHandoffCheckCmd())
	cmd.AddCommand(newArbiterHandoffShowCmd())
	return cmd
}

// handoffCheckResult maps an error from taskstate.Load/taskstate.Check
// into the message `aida arbiter handoff check` prints and the exit
// code it returns with -- the pure step the fresh/stale/missing/no-state
// matrix is tested against, without a real brain repo or worktree.
func handoffCheckResult(err error) (message string, exitCode int) {
	if err == nil {
		return "fresh", 0
	}
	if errors.Is(err, taskstate.ErrNoState) {
		return "no STATE.json for this task (the loop has not claimed it)", 1
	}
	return err.Error(), 1
}

// gitRevParseHead returns dir's current HEAD sha, for --worktree.
func gitRevParseHead(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func newArbiterHandoffCheckCmd() *cobra.Command {
	var maxAge time.Duration
	var worktree string

	cmd := &cobra.Command{
		Use:   "check <task-ref>",
		Short: "Check whether a task's hand-off is fresh enough to resume from",
		Long: "Resolves <task-ref> (slug, partial slug, or '#N'), loads STATE.json and\n" +
			"HANDOFF.md from the brain repo, and runs taskstate.Check against the\n" +
			"current worktree HEAD (--worktree) and --max-age. Prints \"fresh\" and\n" +
			"exits 0 when the hand-off is usable; otherwise prints the exact\n" +
			"staleness/missing error and exits 1 -- a stale or missing hand-off\n" +
			"must be detected, never silently ignored (docs/arbiter-plan.md\n" +
			"section 6, decision 1).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			_, profileName := cfg.ActiveProfileConfig()
			if profileName == "" {
				return fmt.Errorf("no active profile resolved (set AIDA_PROFILE or `aida profile use <name>`)")
			}
			brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return fmt.Errorf("open brain: %w", err)
			}
			task, err := brn.ResolveTaskRef(args[0])
			if err != nil {
				return fmt.Errorf("resolve task %q: %w", args[0], err)
			}

			state, err := taskstate.Load(cfg.BrainPath(), task.Slug)
			if err != nil {
				msg, code := handoffCheckResult(err)
				fmt.Println(msg)
				if code != 0 {
					return errors.New(msg)
				}
				return nil
			}

			h, err := taskstate.LoadHandoff(cfg.BrainPath(), task.Slug)
			if err != nil && !errors.Is(err, taskstate.ErrMissing) {
				return fmt.Errorf("load handoff: %w", err)
			}

			var head string
			if worktree != "" {
				head, err = gitRevParseHead(cmd.Context(), worktree)
				if err != nil {
					return fmt.Errorf("git rev-parse HEAD in %s: %w", worktree, err)
				}
			}

			checkErr := taskstate.Check(h, state, head, time.Now(), maxAge)
			msg, code := handoffCheckResult(checkErr)
			fmt.Println(msg)
			if code != 0 {
				return errors.New(msg)
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&maxAge, "max-age", 2*time.Hour, "max age for a hand-off to still be fresh (0 disables the age check)")
	cmd.Flags().StringVar(&worktree, "worktree", "", "git worktree dir to read the current HEAD from (compared against the hand-off's recorded worktree head)")
	return cmd
}

func newArbiterHandoffShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <task-ref>",
		Short: "Print a task's HANDOFF.md raw",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			_, profileName := cfg.ActiveProfileConfig()
			if profileName == "" {
				return fmt.Errorf("no active profile resolved (set AIDA_PROFILE or `aida profile use <name>`)")
			}
			brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return fmt.Errorf("open brain: %w", err)
			}
			task, err := brn.ResolveTaskRef(args[0])
			if err != nil {
				return fmt.Errorf("resolve task %q: %w", args[0], err)
			}
			h, err := taskstate.LoadHandoff(cfg.BrainPath(), task.Slug)
			if err != nil {
				return fmt.Errorf("load handoff for %q: %w", task.Slug, err)
			}
			fmt.Println(h.Raw)
			return nil
		},
	}
	return cmd
}

// ---- aida arbiter ledger ----

// lastLedgerEntries returns the last n entries of entries (all of them
// when n <= 0 or there aren't that many).
func lastLedgerEntries(entries []arbiter.Entry, n int) []arbiter.Entry {
	if n <= 0 || len(entries) <= n {
		return entries
	}
	return entries[len(entries)-n:]
}

// filterLedgerByTask keeps only entries for the given task slug.
func filterLedgerByTask(entries []arbiter.Entry, slug string) []arbiter.Entry {
	if slug == "" {
		return entries
	}
	out := make([]arbiter.Entry, 0, len(entries))
	for _, e := range entries {
		if e.TaskSlug == slug {
			out = append(out, e)
		}
	}
	return out
}

// ledgerWindowDelta renders the first Before/After window pair (matched
// by Provider+Label) as a "before -> after" delta -- the build brief
// asks for "first Before/After window used% delta per window", which
// this reads as: of the windows this entry snapshot, show the first
// one's before/after movement, not a dump of every window. Returns ""
// when there's nothing to pair (no Before rows, or no matching After
// row for the first Before row -- e.g. the run failed before a snapshot
// was taken).
func ledgerWindowDelta(e arbiter.Entry) string {
	if len(e.Before) == 0 {
		return ""
	}
	b := e.Before[0]
	for _, a := range e.After {
		if a.Provider == b.Provider && a.Label == b.Label {
			return fmt.Sprintf("%s %s %.1f%%->%.1f%%", b.Provider, b.Label, b.UsedPct, a.UsedPct)
		}
	}
	return ""
}

// renderLedgerLine renders one ledger entry.
func renderLedgerLine(e arbiter.Entry) string {
	parts := []string{
		e.At.Format(time.RFC3339),
		e.TaskSlug,
		e.Lane,
		e.Model,
		fmt.Sprintf("attempt=%d", e.Attempt),
		e.Verdict,
		fmt.Sprintf("wall=%s", (time.Duration(e.WallMS) * time.Millisecond).Round(time.Second)),
	}
	if e.CostUSD > 0 {
		parts = append(parts, fmt.Sprintf("cost=$%.4f", e.CostUSD))
	}
	if delta := ledgerWindowDelta(e); delta != "" {
		parts = append(parts, delta)
	}
	return strings.Join(parts, "  ")
}

// ledgerFooter reports entry counts per lane and per verdict.
func ledgerFooter(entries []arbiter.Entry) string {
	laneCounts := map[string]int{}
	verdictCounts := map[string]int{}
	var laneOrder, verdictOrder []string
	for _, e := range entries {
		if _, ok := laneCounts[e.Lane]; !ok {
			laneOrder = append(laneOrder, e.Lane)
		}
		laneCounts[e.Lane]++
		if _, ok := verdictCounts[e.Verdict]; !ok {
			verdictOrder = append(verdictOrder, e.Verdict)
		}
		verdictCounts[e.Verdict]++
	}
	sort.Strings(laneOrder)
	sort.Strings(verdictOrder)
	laneParts := make([]string, 0, len(laneOrder))
	for _, l := range laneOrder {
		laneParts = append(laneParts, fmt.Sprintf("%s: %d", l, laneCounts[l]))
	}
	verdictParts := make([]string, 0, len(verdictOrder))
	for _, v := range verdictOrder {
		verdictParts = append(verdictParts, fmt.Sprintf("%s: %d", v, verdictCounts[v]))
	}
	return fmt.Sprintf("%d entries -- by lane: %s -- by verdict: %s",
		len(entries), strings.Join(laneParts, ", "), strings.Join(verdictParts, ", "))
}

func newArbiterLedgerCmd() *cobra.Command {
	var jsonOut bool
	var n int
	var taskRef string

	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "Show recent arbiter run-ledger entries",
		Long: "Reads ~/.aida/arbiter/ledger.ndjson (docs/arbiter-plan.md section 7:\n" +
			"one JSON line per run) and prints the last -n entries with a\n" +
			"per-lane/per-verdict footer. --task filters to one task first.",
		RunE: func(_ *cobra.Command, _ []string) error {
			entries, err := arbiter.ReadLedger(filepath.Join(config.Dir(), "arbiter", "ledger.ndjson"))
			if err != nil {
				return fmt.Errorf("read ledger: %w", err)
			}

			if taskRef != "" {
				cfg, err := config.LoadConfig()
				if err != nil {
					return fmt.Errorf("load config: %w", err)
				}
				_, profileName := cfg.ActiveProfileConfig()
				if profileName == "" {
					return fmt.Errorf("no active profile resolved (set AIDA_PROFILE or `aida profile use <name>`)")
				}
				brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
				if err != nil {
					return fmt.Errorf("open brain: %w", err)
				}
				task, err := brn.ResolveTaskRef(taskRef)
				if err != nil {
					return fmt.Errorf("resolve task %q: %w", taskRef, err)
				}
				entries = filterLedgerByTask(entries, task.Slug)
			}

			entries = lastLedgerEntries(entries, n)

			if jsonOut {
				data, err := json.MarshalIndent(entries, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling ledger: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			for _, e := range entries {
				fmt.Println(renderLedgerLine(e))
			}
			fmt.Println()
			fmt.Println(ledgerFooter(entries))
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the ledger entries as JSON")
	cmd.Flags().IntVarP(&n, "n", "n", 20, "show the last N entries")
	cmd.Flags().StringVar(&taskRef, "task", "", "only show entries for this task (slug, partial slug, or '#N')")
	return cmd
}
