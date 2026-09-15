package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
)

// newJobsCmd builds the `aida jobs` command tree. Operations on the
// per-profile job queue: list, get, tail, cat, reap, reindex, vacuum.
//
// All subcommands resolve the active profile via cfg.ActiveProfileConfig
// and operate on that profile's jobs.db. --all-profiles on `list` widens
// the scan to every profile dir under ~/.aida/jobs/ for ops use.
func newJobsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect and maintain the agent job queue",
		Long: "Operations on the per-profile, SQLite-backed agent job queue.\n" +
			"Jobs are spawned by the web UI (Prepare draft button) and the\n" +
			"`tasks ingest --auto-solve` path; this command is for inspecting,\n" +
			"reaping crashed jobs, rebuilding the index from disk, and gc'ing\n" +
			"old run directories.",
	}
	cmd.AddCommand(newJobsListCmd())
	cmd.AddCommand(newJobsGetCmd())
	cmd.AddCommand(newJobsTailCmd())
	cmd.AddCommand(newJobsCatCmd())
	cmd.AddCommand(newJobsReapCmd())
	cmd.AddCommand(newJobsReindexCmd())
	cmd.AddCommand(newJobsVacuumCmd())
	cmd.AddCommand(newJobsSetArtifactURLCmd())
	cmd.AddCommand(newJobsApproveCmd())
	cmd.AddCommand(newJobsRejectCmd())
	return cmd
}

// newJobsApproveCmd / newJobsRejectCmd are the keyboard path for the HITL
// approval gate (the voice path is approve_job/reject_job). They write the
// verdict to the run-dir's approval.txt; the paused agent's request_approval
// tool reads it on its next poll and either proceeds or aborts.
func newJobsApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a job awaiting your sign-off (state=awaiting_approval)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runJobsApproval(args[0], "approve")
		},
	}
}

func newJobsRejectCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a job awaiting approval so it stops without acting",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			verdict := "reject"
			if strings.TrimSpace(reason) != "" {
				verdict = "reject: " + strings.TrimSpace(reason)
			}
			return runJobsApproval(args[0], verdict)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "optional reason for the rejection")
	return cmd
}

func runJobsApproval(runID, verdict string) error {
	profile, err := resolveProfile()
	if err != nil {
		return err
	}
	store, err := jobs.Open(profile)
	if err != nil {
		return err
	}
	defer store.Close()

	j, err := store.Get(runID)
	if err != nil {
		return err
	}
	if j.State != jobs.StateAwaitingApproval {
		return fmt.Errorf("job %s is not awaiting approval (state=%s)", runID, j.State)
	}
	runDir := jobs.RunDir(profile, runID)
	tmp := filepath.Join(runDir, ".approval.txt.tmp")
	if err := os.WriteFile(tmp, []byte(verdict), 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(runDir, "approval.txt")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	fmt.Printf("Verdict %q delivered to %s.\n", verdict, runID)
	return nil
}

// newJobsSetArtifactURLCmd builds `aida jobs set-artifact-url`. Used by
// agent runs whose destination produces a typed deliverable (e.g. a
// github-pr URL) - the agent invokes this in its final step so the
// web modal can render the URL as the primary action button
// ("Open PR") instead of falling back to the Copy/Save controls.
//
// Idempotent on identical URLs; errors only on missing manifest.
func newJobsSetArtifactURLCmd() *cobra.Command {
	var runID, url string
	cmd := &cobra.Command{
		Use:   "set-artifact-url",
		Short: "Record an agent's deliverable URL (e.g. PR URL) on a run manifest",
		Long: "Writes `artifact_url` to <run-dir>/manifest.json and the jobs.db row\n" +
			"so the web modal renders a typed primary action (Open PR / Open Doc / …)\n" +
			"instead of the generic Copy/Save buttons.\n\n" +
			"Used by agents executing destination-typed drafts (issue #58).\n" +
			"Idempotent on identical URLs.",
		RunE: func(_ *cobra.Command, _ []string) error {
			if runID == "" {
				return fmt.Errorf("--run-id is required")
			}
			if url == "" {
				return fmt.Errorf("--url is required")
			}
			profile, err := resolveProfile()
			if err != nil {
				return err
			}
			store, err := jobs.Open(profile)
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.SetArtifactURL(runID, url); err != nil {
				return fmt.Errorf("set artifact url: %w", err)
			}
			fmt.Printf("artifact_url set on %s\n", runID)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "run id (matches the directory under ~/.aida/jobs/<profile>/runs/)")
	cmd.Flags().StringVar(&url, "url", "", "deliverable URL (e.g. https://github.com/owner/repo/pull/N)")
	return cmd
}

// resolveProfile returns the active profile name, or an error if it
// can't be resolved. Avoids the silent "default to first profile"
// fallback in cfg - for jobs we want the user to fix their config
// rather than write to a surprising profile dir.
func resolveProfile() (string, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}
	_, name := cfg.ActiveProfileConfig()
	if name == "" {
		return "", fmt.Errorf("no active profile resolved (set AIDA_PROFILE or `aida profile use <name>`)")
	}
	return name, nil
}

func newJobsListCmd() *cobra.Command {
	var state, taskSlug, since string
	var limit int
	var allProfiles bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs in the queue",
		Long: "Prints jobs from the active profile's queue, newest first.\n" +
			"Use --state to filter (queued|running|awaiting_input|awaiting_approval|\n" +
			"done|failed|incomplete), --task to narrow to one task slug, --since to\n" +
			"limit by enqueue time. --all-profiles widens the scan to every profile dir.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runJobsList(state, taskSlug, since, limit, allProfiles)
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter by state (queued|running|awaiting_input|awaiting_approval|done|failed|incomplete)")
	cmd.Flags().StringVar(&taskSlug, "task", "", "filter to one task slug")
	cmd.Flags().StringVar(&since, "since", "", "only jobs enqueued on/after YYYY-MM-DD or relative duration like 24h")
	cmd.Flags().IntVar(&limit, "limit", 50, "max rows to print (0 = unlimited)")
	cmd.Flags().BoolVar(&allProfiles, "all-profiles", false, "scan every profile under ~/.aida/jobs/")
	return cmd
}

func runJobsList(state, taskSlug, sinceStr string, limit int, allProfiles bool) error {
	if state != "" && !jobs.IsValidState(state) {
		return fmt.Errorf("invalid --state %q (want queued|running|awaiting_input|awaiting_approval|done|failed|incomplete)", state)
	}
	since, err := parseSinceFilter(sinceStr)
	if err != nil {
		return err
	}

	var profiles []string
	if allProfiles {
		profiles, err = listProfileDirs()
		if err != nil {
			return err
		}
	} else {
		p, err := resolveProfile()
		if err != nil {
			return err
		}
		profiles = []string{p}
	}

	var rows []listRow
	for _, p := range profiles {
		store, err := jobs.Open(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", p, err)
			continue
		}
		js, err := store.List(jobs.ListOpts{
			State:    state,
			TaskSlug: taskSlug,
			Since:    since,
			Limit:    limit,
		})
		store.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "list %s: %v\n", p, err)
			continue
		}
		for _, j := range js {
			rows = append(rows, listRow{profile: p, job: j})
		}
	}

	if len(rows) == 0 {
		fmt.Println("No jobs.")
		return nil
	}

	// Newest first across all profiles.
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].job.EnqueuedAt.After(rows[j].job.EnqueuedAt)
	})

	for _, r := range rows {
		printListRow(r, allProfiles)
	}
	return nil
}

type listRow struct {
	profile string
	job     jobs.Job
}

func printListRow(r listRow, showProfile bool) {
	// EffectiveState, not the raw column: a legacy pre-result-contract
	// "done" row (see jobs.EffectiveState) must read as incomplete here
	// too, not just in `jobs get`. This list is the surface an operator
	// scans first, and it's exactly where a false "done" used to hide.
	state, _, _ := jobs.EffectiveState(&r.job)
	marker := stateMarker(state)
	prefix := ""
	if showProfile {
		prefix = fmt.Sprintf("[%s] ", r.profile)
	}
	title := r.job.TaskSlug
	if title == "" {
		title = "(no task)"
	}
	fmt.Printf("%s%s %s  %s  task=%s  plan=%s\n",
		prefix, marker, r.job.RunID, state, title, defaulted(r.job.PlanID, "-"))
	if r.job.Error != "" {
		fmt.Printf("    error: %s\n", truncate(r.job.Error, 200))
	}
}

func stateMarker(s string) string {
	switch s {
	case jobs.StateQueued:
		return "·"
	case jobs.StateRunning:
		return "→"
	case jobs.StateAwaitingInput:
		return "?"
	case jobs.StateAwaitingApproval:
		return "⏸"
	case jobs.StateDone:
		return "✓"
	case jobs.StateFailed:
		return "✗"
	case jobs.StateIncomplete:
		return "⚠"
	}
	return "?"
}

func defaulted(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// parseSinceFilter accepts both YYYY-MM-DD and Go duration strings
// like "24h", "7d" (interpreted as 7*24h), so the CLI matches
// `aida tasks --since` ergonomics. Empty input → zero time = no filter.
func parseSinceFilter(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	// Date form first.
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	// Duration form: "24h", "7d" (translate days), "30m" etc.
	if strings.HasSuffix(s, "d") {
		n := strings.TrimSuffix(s, "d")
		if dur, err := time.ParseDuration(n + "h"); err == nil {
			return time.Now().UTC().Add(-dur * 24), nil
		}
	}
	if dur, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-dur), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD or duration like 24h)", s)
}

// listProfileDirs returns the names of every directory directly under
// ~/.aida/jobs/ - each is a profile that has at least one job.
func listProfileDirs() ([]string, error) {
	root := filepath.Join(config.Dir(), "jobs")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------
// `aida jobs get <run-id>`
// ---------------------------------------------------------------

func newJobsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <run-id>",
		Short: "Print one job's manifest + last events",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runJobsGet(args[0])
		},
	}
}

func runJobsGet(runID string) error {
	profile, err := resolveProfile()
	if err != nil {
		return err
	}
	store, err := jobs.Open(profile)
	if err != nil {
		return err
	}
	defer store.Close()

	j, err := store.Get(runID)
	if err != nil {
		return err
	}
	// state is EffectiveState's corrected value, not the raw column, so
	// a legacy pre-result-contract "done" prints as incomplete here.
	// The full evidence (including the originally-recorded value) comes
	// from DescribeOutcome below.
	state, _, _ := jobs.EffectiveState(j)
	fmt.Printf("run_id:        %s\n", j.RunID)
	fmt.Printf("kind:          %s\n", j.Kind)
	fmt.Printf("state:         %s %s\n", stateMarker(state), state)
	fmt.Printf("profile:       %s\n", j.Profile)
	fmt.Printf("task_slug:     %s\n", defaulted(j.TaskSlug, "-"))
	fmt.Printf("plan_id:       %s\n", defaulted(j.PlanID, "-"))
	fmt.Printf("enqueued_at:   %s\n", j.EnqueuedAt.Format(time.RFC3339))
	if !j.StartedAt.IsZero() {
		fmt.Printf("started_at:    %s\n", j.StartedAt.Format(time.RFC3339))
	}
	if !j.FinishedAt.IsZero() {
		fmt.Printf("finished_at:   %s\n", j.FinishedAt.Format(time.RFC3339))
	}
	if j.ClaimedByHost != "" {
		fmt.Printf("claimed_by:    %s pid=%d\n", j.ClaimedByHost, j.ClaimedByPID)
	}
	if j.Question != "" {
		fmt.Printf("question:      %s\n", j.Question)
	}
	if j.SourceRef != "" {
		fmt.Printf("source_ref:    %s\n", j.SourceRef)
	}
	if j.Error != "" {
		fmt.Printf("error:         %s\n", j.Error)
	}
	fmt.Printf("run_dir:       %s\n", jobs.RunDir(profile, runID))
	if jobs.IsTerminalState(j.State) {
		fmt.Printf("outcome:       %s\n", jobs.DescribeOutcome(j))
	}

	// Tail of events for context.
	fmt.Println()
	fmt.Println("--- last events ---")
	if err := streamEventsTail(profile, runID, 25, false); err != nil {
		fmt.Fprintf(os.Stderr, "events: %v\n", err)
	}
	return nil
}

// ---------------------------------------------------------------
// `aida jobs tail <run-id> [-f]`
// ---------------------------------------------------------------

func newJobsTailCmd() *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "tail <run-id>",
		Short: "Print tail of events.ndjson",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			profile, err := resolveProfile()
			if err != nil {
				return err
			}
			return streamEventsTail(profile, args[0], lines, follow)
		},
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of trailing lines to print")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow appends to events.ndjson (like tail -f)")
	return cmd
}

// streamEventsTail prints the last `n` lines of events.ndjson, then
// optionally follows for new appends. Used by both `jobs tail` and
// `jobs get` (with follow=false).
func streamEventsTail(profile, runID string, n int, follow bool) error {
	path := jobs.EventsPath(profile, runID)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Cheap last-N: read all lines, slice the tail. Events files
	// stay small (a few KB to a few MB), so a full read is fine.
	all, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(all), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		fmt.Println(l)
	}

	if !follow {
		return nil
	}

	// Follow mode: re-open at end, poll for appends. modernc/sqlite
	// has no notify hook on the events file (it's plain disk), so a
	// 250ms poll is fine for human-paced reading.
	pos, _ := f.Seek(0, io.SeekEnd)
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			fmt.Print(line)
		}
		if err == io.EOF {
			time.Sleep(250 * time.Millisecond)
			// Re-stat to detect file rotation; not expected here
			// but defensive.
			if fi, err := os.Stat(path); err == nil && fi.Size() < pos {
				pos = 0
				f.Seek(0, io.SeekStart)
				reader.Reset(f)
			}
			continue
		}
		if err != nil {
			return err
		}
	}
}

// ---------------------------------------------------------------
// `aida jobs cat <run-id>`
// ---------------------------------------------------------------

func newJobsCatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat <run-id>",
		Short: "Print output.md for a completed job",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			profile, err := resolveProfile()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(jobs.OutputPath(profile, args[0]))
			if err != nil {
				return err
			}
			os.Stdout.Write(data)
			return nil
		},
	}
}

// ---------------------------------------------------------------
// `aida jobs reap`
// ---------------------------------------------------------------

func newJobsReapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reap",
		Short: "Mark running jobs whose worker pid is dead as failed",
		Long: "Sweeps the local-host running jobs and transitions any whose\n" +
			"recorded pid is no longer alive to state=failed. Idempotent;\n" +
			"foreign-host running rows are left alone.",
		RunE: func(_ *cobra.Command, _ []string) error {
			profile, err := resolveProfile()
			if err != nil {
				return err
			}
			store, err := jobs.Open(profile)
			if err != nil {
				return err
			}
			defer store.Close()
			n, err := store.Reap()
			if err != nil {
				return err
			}
			fmt.Printf("Reap: %d job(s) marked failed.\n", n)
			return nil
		},
	}
}

// ---------------------------------------------------------------
// `aida jobs reindex`
// ---------------------------------------------------------------

func newJobsReindexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild jobs.db from manifest.json files on disk",
		Long: "Walks <profile>/runs/, parses each manifest, and INSERT OR\n" +
			"REPLACE into jobs.db. Idempotent. Use after a DB-loss event\n" +
			"(jobs.db deleted, corrupt) - disk manifests are source of truth.",
		RunE: func(_ *cobra.Command, _ []string) error {
			profile, err := resolveProfile()
			if err != nil {
				return err
			}
			store, err := jobs.Open(profile)
			if err != nil {
				return err
			}
			defer store.Close()
			count, skipped, err := store.Reindex()
			if err != nil {
				return err
			}
			fmt.Printf("Reindex: %d indexed, %d skipped.\n", count, skipped)
			return nil
		},
	}
}

// ---------------------------------------------------------------
// `aida jobs vacuum --older-than 30d`
// ---------------------------------------------------------------

func newJobsVacuumCmd() *cobra.Command {
	var olderThan string
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Garbage-collect old terminal-state run directories",
		Long: "Deletes run-dirs (and their jobs.db rows) for jobs in state\n" +
			"done|failed|incomplete whose finished_at is older than --older-than.\n" +
			"Default cutoff: 30 days. Skips queued/running/awaiting_* rows entirely.",
		RunE: func(_ *cobra.Command, _ []string) error {
			cutoff, err := parseDurationCutoff(olderThan)
			if err != nil {
				return err
			}
			return runJobsVacuum(cutoff)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "30d", "duration like 30d, 168h")
	return cmd
}

func parseDurationCutoff(s string) (time.Time, error) {
	since, err := parseSinceFilter(s)
	if err != nil || since.IsZero() {
		return time.Time{}, fmt.Errorf("invalid --older-than %q", s)
	}
	return since, nil
}

func runJobsVacuum(cutoff time.Time) error {
	profile, err := resolveProfile()
	if err != nil {
		return err
	}
	store, err := jobs.Open(profile)
	if err != nil {
		return err
	}
	defer store.Close()

	// Every terminal state is a vacuum candidate, incomplete included,
	// or a run that stopped mid-task (wall-clock/cost bound, no
	// verified result) would sit in its run-dir forever since it never
	// lands on done or failed.
	doneList, _ := store.List(jobs.ListOpts{State: jobs.StateDone})
	failedList, _ := store.List(jobs.ListOpts{State: jobs.StateFailed})
	incompleteList, _ := store.List(jobs.ListOpts{State: jobs.StateIncomplete})
	candidates := append([]jobs.Job(nil), doneList...)
	candidates = append(candidates, failedList...)
	candidates = append(candidates, incompleteList...)

	var deleted int
	for _, j := range candidates {
		if !j.FinishedAt.IsZero() && j.FinishedAt.After(cutoff) {
			continue
		}
		// Drop the SQL row first; if the rmdir fails the next
		// vacuum will catch it via Reindex-style rebuild.
		if err := store.DeleteJob(j.RunID); err != nil {
			fmt.Fprintf(os.Stderr, "delete row %s: %v\n", j.RunID, err)
			continue
		}
		if err := os.RemoveAll(jobs.RunDir(profile, j.RunID)); err != nil {
			fmt.Fprintf(os.Stderr, "rm -rf %s: %v\n", j.RunID, err)
			continue
		}
		deleted++
	}
	fmt.Printf("Vacuum: deleted %d job(s) older than %s.\n", deleted, cutoff.Format(time.RFC3339))
	return nil
}
