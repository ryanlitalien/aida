package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/fleet"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/remotex"
	"github.com/ryanlitalien/aida/internal/ui"
)

// fleetTailTimeout bounds each individual `tail` attempt over ssh. Distinct
// from --timeout, which bounds the whole watch loop -- a wedged single
// attempt must not eat the whole budget.
const fleetTailTimeout = 20 * time.Second

// newFleetCmd builds the `aida fleet` command tree: `watch`, the
// completion callback for a lane 4 (Ryan's own herdr) run started by
// hand or by a launcher script (iteration B), and `start`, the recorded
// launch that writes the task, starts the herdr agent, and spawns its
// own watch (iteration C) -- see
// ~/.aida/team/fitz/briefs/2026-09-02-lane4-aida-gaps.md.
func newFleetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "Lane 4 fleet operations (herdr agents on Ryan's own login)",
	}
	cmd.AddCommand(newFleetWatchCmd())
	cmd.AddCommand(newFleetStartCmd())
	return cmd
}

// fleetWatchOpts holds `aida fleet watch`'s flags.
type fleetWatchOpts struct {
	Agent    string
	Log      string
	Host     string
	User     string
	RunID    string
	Question string
	Model    string
	Repo     string
	Interval time.Duration
	Timeout  time.Duration
}

// newFleetWatchCmd builds `aida fleet watch`.
func newFleetWatchCmd() *cobra.Command {
	var o fleetWatchOpts
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Poll a lane 4 herdr agent's log for its EXIT marker and complete/fail the job",
		Long: "Tails a remote log over ssh (sudo -iu <user>, per remotex.RunRemote) every\n" +
			"--interval until its last non-empty line matches the lane 4 launcher's\n" +
			"completion marker (`EXIT=<code> <timestamp>`, from\n" +
			"`echo \"EXIT=$? $(date -Is)\" | tee -a <log>`). On match it writes the\n" +
			"agent's report to the job's output.md, extracts the first GitHub PR URL\n" +
			"in that report into artifact_url, and completes the job (EXIT=0) or\n" +
			"fails it (\"EXIT=<n>\", otherwise). Real reports often name the PR by\n" +
			"number only (\"PR #1617\") without a full URL -- when no full URL is\n" +
			"found and --repo owner/name is set, the first \"PR #N\" / \"pull request\n" +
			"#N\" reference in the report is used to build one instead.\n\n" +
			"Transport errors (ssh failures, a non-zero or timed-out tail) are logged\n" +
			"and retried on the next tick -- only --timeout fails the job, so a slow\n" +
			"or briefly unreachable host doesn't abort the watch.\n\n" +
			"Without --run-id, a new kind=fleet job is enqueued and marked running --\n" +
			"this is what makes the command useful against a run started by hand,\n" +
			"before a launcher exists that enqueues its own row.",
		Example: "  aida fleet watch --agent scarlett-978 --log /home/ryan/agents-lane4/scarlett-978.log \\\n" +
			"    --host minty --user ryan --question \"tracer: issue #978\" --model bedrock-opus-4-6 \\\n" +
			"    --interval 5s --timeout 2m",
		RunE: func(_ *cobra.Command, _ []string) error {
			return validateFleetWatchOpts(&o)
		},
	}
	cmd.Flags().StringVar(&o.Agent, "agent", "", "herdr agent name being watched (required)")
	cmd.Flags().StringVar(&o.Log, "log", "", "absolute path to the agent's log file on the remote host (required)")
	cmd.Flags().StringVar(&o.Host, "host", "minty", "ssh host/alias the agent is running on")
	cmd.Flags().StringVar(&o.User, "user", "heimdall", "remote user to sudo -iu into (a login shell -- needed for herdr's PATH, see remotex.RunRemote)")
	cmd.Flags().StringVar(&o.RunID, "run-id", "", "existing job run id to update instead of enqueueing a new one")
	cmd.Flags().StringVar(&o.Question, "question", "", "job question/task text, used only when enqueueing (no --run-id)")
	cmd.Flags().StringVar(&o.Model, "model", "", "model alias the agent ran under, recorded on the job")
	cmd.Flags().StringVar(&o.Repo, "repo", "", "owner/name to build a PR URL from an issue-style \"PR #N\" reference when no full URL is found")
	cmd.Flags().DurationVar(&o.Interval, "interval", 30*time.Second, "how often to tail the log")
	cmd.Flags().DurationVar(&o.Timeout, "timeout", 8*time.Hour, "overall deadline before the job is failed as timed out")
	return cmd
}

// validateFleetWatchOpts checks the flags cobra can't validate itself (no
// MarkFlagRequired precedent in this codebase -- every other command does
// its own checks in RunE, see e.g. newJobsSetArtifactURLCmd) and, once
// clean, hands off to runFleetWatch.
func validateFleetWatchOpts(o *fleetWatchOpts) error {
	if o.Agent == "" {
		return fmt.Errorf("--agent is required")
	}
	if err := remotex.ValidateTarget(o.Agent); err != nil {
		return err
	}
	if o.Log == "" {
		return fmt.Errorf("--log is required")
	}
	if !filepath.IsAbs(o.Log) {
		// remotex exec's argv without a shell, so no tilde expansion --
		// every remote path in argv must be absolute (brief fact 4).
		return fmt.Errorf("--log must be an absolute path (no tilde expansion on the remote argv): %q", o.Log)
	}
	if o.Repo != "" && !repoRe.MatchString(o.Repo) {
		return fmt.Errorf("--repo must be owner/name: %q", o.Repo)
	}
	if o.Interval <= 0 {
		return fmt.Errorf("--interval must be positive")
	}
	if o.Timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	return runFleetWatch(*o)
}

// repoRe validates --repo's "owner/name" shape: two GitHub-safe segments
// (letters, digits, '.', '_', '-') separated by exactly one '/'.
var repoRe = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// runFleetWatch is the command body: resolve/enqueue the job row, poll
// the remote log to completion, then record the outcome.
func runFleetWatch(o fleetWatchOpts) error {
	profile, err := resolveProfile()
	if err != nil {
		return err
	}
	store, err := jobs.Open(profile)
	if err != nil {
		return err
	}
	defer store.Close()

	runID := o.RunID
	if runID == "" {
		// This is what makes `aida fleet watch` useful against a run
		// started by hand today, ahead of a launcher that enqueues its
		// own row (iteration C).
		sourceRef := fmt.Sprintf("fleet:%s:%s:%s", o.Host, o.User, o.Agent)
		j, err := store.Enqueue("fleet", "", "", o.Question, sourceRef)
		if err != nil {
			return fmt.Errorf("enqueue: %w", err)
		}
		runID = j.RunID
		host, _ := os.Hostname()
		if err := store.MarkRunning(runID, host, os.Getpid()); err != nil {
			return fmt.Errorf("mark running: %w", err)
		}
		fmt.Printf("Enqueued job %s (kind=fleet, %s)\n", runID, sourceRef)
	} else if _, err := store.Get(runID); err != nil {
		return fmt.Errorf("get job %s: %w", runID, err)
	}

	// Best-effort: a fleet job is useful even if this write fails (the
	// state transition + artifact URL are the parts that matter).
	if err := store.SetAgentModel(runID, o.Agent, o.Model); err != nil {
		ui.PrintVerbose("fleet watch", "set agent/model failed: "+err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()

	result, pollErr := pollUntilExit(ctx, remotex.ExecRunner{}, o.Host, o.User, o.Log, o.Interval)
	if pollErr != nil {
		msg := "timeout waiting for EXIT marker"
		if !errors.Is(pollErr, context.DeadlineExceeded) {
			msg = pollErr.Error()
		}
		if failErr := store.Fail(runID, msg); failErr != nil {
			ui.PrintVerbose("fleet watch", "fail on timeout failed: "+failErr.Error())
		}
		return fmt.Errorf("fleet watch %s: %s", runID, msg)
	}

	// --repo fallback: a report often names the PR by number only
	// ("PR #1617") without a full URL -- ExtractPRURL alone can't
	// recover a URL from that, so when --repo is set and no full URL
	// was found, build one from the first issue-style PR reference.
	if result.PRURL == "" && o.Repo != "" {
		if n, ok := fleet.ExtractPRRef(result.Output); ok {
			result.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", o.Repo, n)
		}
	}

	runDir := jobs.RunDir(profile, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return fmt.Errorf("mkdir run dir: %w", err)
	}
	if err := os.WriteFile(jobs.OutputPath(profile, runID), []byte(result.Output), 0644); err != nil {
		return fmt.Errorf("write output.md: %w", err)
	}
	appendFleetEvent(profile, runID, result)

	if result.ExitCode == 0 {
		if err := store.Complete(runID); err != nil {
			return fmt.Errorf("complete: %w", err)
		}
	} else if err := store.Fail(runID, fmt.Sprintf("EXIT=%d", result.ExitCode)); err != nil {
		return fmt.Errorf("fail: %w", err)
	}

	// Called after Complete/Fail. ArtifactURL is now a Job/SQL field
	// (manifest v6), so Complete/Fail's own manifest rewrite carries it
	// forward correctly either way -- this ordering is kept because it
	// reads better (state settles, then the deliverable), not because
	// it's required anymore.
	if result.PRURL != "" {
		if err := store.SetArtifactURL(runID, result.PRURL); err != nil {
			ui.PrintVerbose("fleet watch", "set artifact url failed: "+err.Error())
		}
	}

	fmt.Printf("%s: EXIT=%d (%s)\n", runID, result.ExitCode, result.Timestamp)
	if result.PRURL != "" {
		fmt.Printf("  artifact_url: %s\n", result.PRURL)
	}
	return nil
}

// pollResult is what a completed poll loop learned from the log's EXIT
// marker: the exit code and timestamp the launcher recorded, the agent's
// report with that marker line stripped, and (if present) the first
// GitHub PR URL in that report.
type pollResult struct {
	ExitCode  int
	Timestamp string
	Output    string
	PRURL     string
}

// pollUntilExit polls host's log via `tail -c 65536 <log>` (sudo -iu user,
// through remotex.RunRemote) every interval until the tail's last
// non-empty line matches fleet.ParseExitLine, or ctx is done.
//
// A transport failure -- RunRemote returning an error, the remote command
// itself timing out, or `tail` exiting non-zero (e.g. ENOENT before the
// launcher has created the log yet) -- is logged to stderr and retried on
// the next tick. Per the brief, only the caller's ctx deadline (--timeout)
// fails the watch; a flaky link to minty must not fail a job that's still
// running fine on the other end.
func pollUntilExit(ctx context.Context, r remotex.Runner, host, user, log string, interval time.Duration) (pollResult, error) {
	for {
		res, err := remotex.RunRemote(ctx, r, host, user, []string{"tail", "-c", "65536", log}, fleetTailTimeout)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "fleet watch: tail %s@%s:%s: %v (retrying)\n", user, host, log, err)
		case res.TimedOut:
			fmt.Fprintf(os.Stderr, "fleet watch: tail %s@%s:%s: remote command timed out (retrying)\n", user, host, log)
		case res.ExitCode != 0:
			fmt.Fprintf(os.Stderr, "fleet watch: tail %s@%s:%s: remote exit %d: %s (retrying)\n",
				user, host, log, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		default:
			body, last := splitTail(string(res.Stdout))
			if code, ts, ok := fleet.ParseExitLine(last); ok {
				return pollResult{
					ExitCode:  code,
					Timestamp: ts,
					Output:    body,
					PRURL:     fleet.ExtractPRURL(body),
				}, nil
			}
		}

		select {
		case <-ctx.Done():
			return pollResult{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// splitTail splits a tailed log's raw text into (body, lastLine): the
// report before the final line, and that final line with surrounding
// whitespace trimmed so it's ready for fleet.ParseExitLine.
func splitTail(text string) (body, lastLine string) {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return "", ""
	}
	idx := strings.LastIndexByte(trimmed, '\n')
	if idx < 0 {
		return "", strings.TrimSpace(trimmed)
	}
	return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(trimmed[idx+1:])
}

// appendFleetEvent appends one events.ndjson line recording the poll
// loop's outcome, in the same {"ts","type","data"} envelope
// agent_events.go uses for every other event kind so `aida jobs tail`
// and the /runs SSE stream render it without special-casing kind=fleet.
// Best-effort: a failure here is logged, not fatal -- the job's
// state transition is what matters.
func appendFleetEvent(profile, runID string, result pollResult) {
	runDir := jobs.RunDir(profile, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "fleet watch: mkdir run dir for event: %v\n", err)
		return
	}
	f, err := os.OpenFile(jobs.EventsPath(profile, runID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet watch: open events.ndjson: %v\n", err)
		return
	}
	defer f.Close()

	eventType := EventKindComplete
	if result.ExitCode != 0 {
		eventType = EventKindError
	}
	envelope := map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"type": eventType,
		"data": map[string]any{
			"exit_code": result.ExitCode,
			"exit_at":   result.Timestamp,
			"pr_url":    result.PRURL,
		},
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet watch: marshal event: %v\n", err)
		return
	}
	f.Write(line)
	f.Write([]byte("\n"))
}
