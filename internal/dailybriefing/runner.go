// Package dailybriefing runs the daily briefing without claude -p, by calling
// a former employer's `gws` CLI directly for Google Workspace I/O. This
// sidesteps the managed-settings.json lockdown
// (allowManagedPermissionRulesOnly: true) that killed the MCP-based path on
// 2026-05-07.
//
// This package is deliberately left in the public tree as a worked example
// of a private adapter: `gws` isn't a real published CLI, so nothing here
// works out of the box, but the shape -- gate the whole pipeline behind an
// opt-in config flag (daily.pipeline: "gws"; see runViaGWS in
// internal/cli/daily.go, only reachable when the profile also sets
// daily.enabled: true), shell out to an internal-only binary via os/exec
// with per-call deadlines, and degrade to an error rather than crashing when
// the binary or its auth is missing -- is exactly how you'd wire up any
// other private tool an employer-specific or personal setup depends on.
//
// Stage 1 implements Steps 0 (no-op - gws self-checks at exec time), 2 (tasks
// via brain pkg), 3 (calendar via gws), and 5 (send via gws). Stage 2 will add
// Step 1 (backup-cron health), Step 2.5 (Slack-Zapier sync), and Step 4 (gmail
// triage with label classification).
package dailybriefing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
)

// Options controls Run behavior.
type Options struct {
	// DryRun composes the email but skips the gmail send, printing the body to stdout.
	DryRun bool
	// NewTasksWindow defines what counts as "new" for the NEW TASKS section.
	// Defaults to 24h when zero.
	NewTasksWindow time.Duration
}

// Run executes the briefing end-to-end. Caller (cli/daily.go) is responsible
// for the watchdog timeout (passed in via ctx).
func Run(ctx context.Context, dc *config.DailyConfig, opts Options) error {
	if dc == nil {
		return fmt.Errorf("daily config is nil")
	}
	if dc.EmailTo == "" {
		return fmt.Errorf("daily.email_to is required")
	}
	if dc.TaskTag == "" {
		return fmt.Errorf("daily.task_tag is required")
	}
	if opts.NewTasksWindow == 0 {
		opts.NewTasksWindow = 24 * time.Hour
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return fmt.Errorf("loading America/New_York: %w", err)
	}
	today := time.Now().In(loc)

	progress("STEP_3_START", "calendar via gws")
	events, err := CalendarToday(ctx, loc)
	if err != nil {
		return fmt.Errorf("step 3 (calendar): %w", err)
	}
	progress("STEP_3_END", fmt.Sprintf("%d events (after filters)", len(events)))

	progress("STEP_2_START", "tasks via brain")
	openTasks, newTasks, err := fetchTasks(dc.TaskTag, time.Now().Add(-opts.NewTasksWindow))
	if err != nil {
		return fmt.Errorf("step 2 (tasks): %w", err)
	}
	progress("STEP_2_END", fmt.Sprintf("%d open, %d new in last %s", len(openTasks), len(newTasks), opts.NewTasksWindow))

	progress("STEP_4_START", "gmail triage via gws")
	triage, terr := RunTriage(ctx, dc)
	if terr != nil {
		// Best-effort: report the partial result; the briefing still ships with what we have.
		fmt.Fprintf(os.Stderr, "WARN: gmail triage partial/failed: %v\n", terr)
		progress("STEP_4_PARTIAL", fmt.Sprintf("%v", terr))
	} else {
		progress("STEP_4_END", fmt.Sprintf("%d flagged buckets, %d stale, %d partner",
			len(triage.Flagged), len(triage.Stale), len(triage.Partners)))
	}

	body := ComposeBody(events, openTasks, newTasks, triage, today, opts.NewTasksWindow)
	subject := fmt.Sprintf("Daily Briefing -- %s", today.Format("2006-01-02"))

	// Always write the briefing to a file so there's a durable artifact even
	// if delivery fails. SECREV-443 grants gws gmail.readonly only - gmail.send
	// is SailPoint-gated and currently 403s. The file is the canonical output;
	// gmail send is best-effort delivery on top.
	filePath, err := writeBriefingFile(dc.ProjectDir, today, subject, body)
	if err != nil {
		return fmt.Errorf("writing briefing file: %w", err)
	}
	progress("STEP_4_FILE", fmt.Sprintf("wrote %s", filePath))

	if opts.DryRun {
		fmt.Fprintf(os.Stdout, "\n=== DRY RUN: would send to %s ===\nSubject: %s\n\n%s", dc.EmailTo, subject, body)
		progress("STEP_5_SKIP", "dry-run; no email sent")
		return nil
	}

	progress("STEP_5_START", fmt.Sprintf("send to %s", dc.EmailTo))
	sendCtx := ctx
	if sendCtx.Err() != nil {
		// Parent watchdog already expired (e.g. a prior step hung past its
		// deadline). File is already on disk - give send a fresh 60s budget
		// so the email isn't forfeited by upstream slowness.
		fmt.Fprintf(os.Stderr, "WARN: parent ctx expired before Step 5 (%v); attempting send with fresh 60s budget\n", ctx.Err())
		var sendCancel context.CancelFunc
		sendCtx, sendCancel = context.WithTimeout(context.Background(), 60*time.Second)
		defer sendCancel()
	}
	msgID, err := GmailSend(sendCtx, dc.EmailTo, subject, body)
	if err != nil {
		// Best-effort: file is the canonical artifact. Log loudly but don't fail
		// the run - that would trigger watchdog retries on a permission error
		// that retry won't fix.
		fmt.Fprintf(os.Stderr, "WARN: gmail send failed (briefing saved to %s): %v\n", filePath, err)
		progress("STEP_5_FAIL", fmt.Sprintf("send 403/etc; file at %s", filePath))
		return nil
	}
	progress("STEP_5_END", fmt.Sprintf("sent (msg %s)", msgID))

	if dc.GmailBriefingLabel != "" {
		if err := GmailAddLabel(sendCtx, msgID, dc.GmailBriefingLabel); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: label %q not applied to %s: %v\n", dc.GmailBriefingLabel, msgID, err)
			progress("STEP_6_FAIL", fmt.Sprintf("label %q skipped: %v", dc.GmailBriefingLabel, err))
		} else {
			progress("STEP_6_END", fmt.Sprintf("label %q applied", dc.GmailBriefingLabel))
		}
	}
	return nil
}

// writeBriefingFile saves the rendered briefing as markdown under
// <projectDir>/briefings/<YYYY-MM-DD>-briefing.md. Creates parents as needed.
func writeBriefingFile(projectDir string, today time.Time, subject, body string) (string, error) {
	if projectDir == "" {
		projectDir = "."
	}
	dir := filepath.Join(config.ExpandPath(projectDir), "briefings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, today.Format("2006-01-02")+"-briefing.md")
	content := fmt.Sprintf("# %s\n\n%s", subject, body)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// fetchTasks reads open + in-progress tasks from the brain DB filtered to tag,
// sorts by priority then creation date, and splits out tasks created since `since`
// into the "new" bucket.
func fetchTasks(tag string, since time.Time) (open, fresh []brain.TaskRecord, err error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}
	_, profileName := cfg.ActiveProfileConfig()
	if profileName == "" {
		return nil, nil, fmt.Errorf("no active profile resolved")
	}

	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return nil, nil, fmt.Errorf("opening brain: %w", err)
	}

	tasks, err := b.ListTasksByStatus([]string{"open", "in-progress"}, []string{tag}, 0, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("listing tasks: %w", err)
	}

	sort.SliceStable(tasks, func(i, j int) bool {
		pi, pj := tasks[i].PriorityTag(), tasks[j].PriorityTag()
		if pi != pj {
			return pi < pj
		}
		return tasks[i].Created > tasks[j].Created
	})

	for _, t := range tasks {
		ts, parseErr := time.Parse(time.RFC3339, t.Created)
		if parseErr != nil {
			ts, parseErr = time.Parse("2006-01-02", t.Created)
		}
		if parseErr == nil && ts.After(since) {
			fresh = append(fresh, t)
		}
	}
	return tasks, fresh, nil
}

func progress(step, detail string) {
	fmt.Fprintf(os.Stdout, "[PROGRESS] %s %s  %s\n", time.Now().UTC().Format(time.RFC3339), step, detail)
}
