package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
)

// A background job's run-id is a long timestamp-slug ("20260622-022838-…").
// The original design told the model to "read back the run id," but the
// job_start tool deliberately withheld it (the LLM mangles long numerics in
// speech) - so the model invented fake ids the user couldn't resolve later.
// Instead, every run-id deterministically maps to a short, pronounceable
// handle ("amber-otter") via jobs.DeriveHandle, which Jarvis speaks and the
// user (and the web runs UI) can reference; resolveJobRef re-derives it on
// demand, so nothing extra is stored.

// The voice-side job_* tools let Jarvis (the top-level agent) start
// long-running background work, check its state, and deliver replies
// to a paused agent. They mirror the engine-side `aida jobs` CLI but
// surface natural-language friendly inputs and outputs.
//
// Importantly: these tools are registered ONLY on the Jarvis voice
// side. The engine-side agent (spawned by `aida --agent`) never sees
// them - that would create a recursive sub-agent chain (Jarvis →
// agent → "starts another agent" → infinite loop). The discipline is
// captured in user feedback memory feedback_no_mcp_query_tool and
// enforced by where in the codebase these are wired.

// ─── job_start ──────────────────────────────────────────────────────

type jobStartInput struct {
	Kind     string `json:"kind"`
	Question string `json:"question"`
}

func jobStartTool(store *jobs.Store) Tool {
	return Tool{
		Name: "job_start",
		Description: "Start a long-running background agent job. USE THIS for any " +
			"request that needs sustained multi-step work - \"continue working on PR " +
			"583\", \"draft a May budget\", \"investigate why X happened.\" The tool " +
			"reply contains a short spoken handle (e.g. \"amber-otter\") - read THAT " +
			"handle back so the user can reference the job later; never invent or " +
			"speak a long numeric id. The agent proceeds in the " +
			"background; Jarvis will speak completion / awaiting-input notifications " +
			"on the next wake. Kinds: 'pr_work' for PR continuation (extracts the PR " +
			"number from the question and checks the branch out), 'investigate' for " +
			"open-ended deep work, or 'agent' for the generic catch-all. Prefer this " +
			"over aida_query for anything that might run more than thirty seconds, " +
			"and for any request that CHAINS MULTIPLE STEPS together, such as " +
			"\"search my email for the outage, find the matching GitHub issue, and " +
			"draft a follow-up issue for it\" -- even if no single step sounds slow " +
			"on its own, the full chain can take minutes. If satisfying the request " +
			"would need more than one aida_query call, use job_start instead of " +
			"chaining aida_query calls together, since aida_query has a hard time " +
			"limit per call and job_start does not.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"kind": map[string]interface{}{
					"type":        "string",
					"description": "job kind: 'pr_work' | 'investigate' | 'agent'",
				},
				"question": map[string]interface{}{
					"type":        "string",
					"description": "the user's request, in natural language - passed verbatim to the agent",
				},
			},
			"required": []string{"question"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in jobStartInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			kind := strings.TrimSpace(in.Kind)
			if kind == "" {
				kind = "agent"
			}
			q := strings.TrimSpace(in.Question)
			if q == "" {
				return "", fmt.Errorf("job_start: empty question")
			}
			if kind == "pr_work" {
				return startPRWorkJob(store, q)
			}
			return startGenericAgentJob(store, kind, q)
		},
	}
}

// prNumberRegex extracts the first PR number from the user's voice
// question. Voice transcription produces several variants:
//
//	"continue PR 583"           → 583
//	"work on PR #583"           → 583
//	"PR five-eighty-three"      → not matched (handled separately)
//
// Only the digit form is supported for v1 - words-to-number parsing
// for voice ("five eighty three") is a follow-up improvement.
var prNumberRegex = regexp.MustCompile(`(?i)\b(?:pr|pull\s*request)\s*#?\s*(\d{1,6})\b`)

// extractPRNumber returns the first PR number found in q, or "" if
// none. Exposed for testability.
func extractPRNumber(q string) string {
	m := prNumberRegex.FindStringSubmatch(q)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// startPRWorkJob is the pr_work specialisation of startGenericAgentJob.
// Creates a git worktree under ~/.aida/worktrees/<profile>/pr-<num>-<runID>/,
// checks the PR branch out via `gh pr checkout`, then spawns
// `aida --agent --run-dir <run-dir>` with cwd set to the worktree. The
// worktree path is stashed on the manifest so the daemon's job-watch
// goroutine can `git worktree remove` it on terminal transition.
//
// Failures unwind in reverse order - a failed gh checkout cleans up
// the freshly-created worktree before returning so a busted run
// doesn't leak directories.
func startPRWorkJob(store *jobs.Store, question string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("jobs store unavailable")
	}
	prNum := extractPRNumber(question)
	if prNum == "" {
		return "", fmt.Errorf("could not extract PR number from %q", question)
	}
	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return "", fmt.Errorf("aida not on PATH: %w", err)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("gh not on PATH: %w", err)
	}

	repoRoot, err := resolvePRWorkRepoRoot()
	if err != nil {
		return "", err
	}

	job, err := store.Enqueue("pr_work", "", "",
		fmt.Sprintf("continue working on PR #%s - see worktree", prNum),
		"jarvis:voice")
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}

	worktreeDir := filepath.Join(
		config.Dir(), "worktrees", store.Profile(),
		fmt.Sprintf("pr-%s-%s", prNum, job.RunID),
	)
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0755); err != nil {
		_ = store.Fail(job.RunID, "mkdir worktrees parent: "+err.Error())
		return "", fmt.Errorf("mkdir parent: %w", err)
	}

	// `git worktree add --detach <path>` creates the worktree at
	// the repo's HEAD without creating a branch. `gh pr checkout`
	// will switch to the PR's branch from there.
	addCmd := exec.Command("git", "-C", repoRoot, "worktree", "add", "--detach", worktreeDir)
	if out, err := addCmd.CombinedOutput(); err != nil {
		_ = store.Fail(job.RunID, "git worktree add: "+err.Error()+": "+string(out))
		return "", fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	checkoutCmd := exec.Command("gh", "pr", "checkout", prNum)
	checkoutCmd.Dir = worktreeDir
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		// Unwind the worktree before failing.
		_ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", worktreeDir).Run()
		_ = store.Fail(job.RunID, "gh pr checkout: "+err.Error()+": "+string(out))
		return "", fmt.Errorf("gh pr checkout: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	if err := store.SetWorktreePath(job.RunID, worktreeDir); err != nil {
		// Soft-fail: worktree exists and the run can still proceed.
		// Surface as a warning so the cleanup hook knows it might
		// have to fall back to scanning the worktrees directory.
		fmt.Fprintf(os.Stderr, "pr_work: SetWorktreePath failed: %v\n", err)
	}

	prPrompt := prContinuationPrompt(prNum)
	runDir := jobs.RunDir(store.Profile(), job.RunID)

	cmd := exec.Command(aidaBin, "--agent", "--run-dir", runDir, prPrompt)
	cmd.Dir = worktreeDir
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
		"AIDA_PROFILE="+store.Profile(),
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", worktreeDir).Run()
		_ = store.Fail(job.RunID, "spawn: "+err.Error())
		return "", fmt.Errorf("spawn agent: %w", err)
	}
	go reapAgentExit(store, cmd, job.RunID)
	// Print the exact run id to the terminal, but speak the short handle:
	// the LLM mangles a long numeric id into words ("twenty twenty-six…"),
	// whereas the handle is pronounceable and resolves cleanly on follow-up.
	fmt.Fprintf(os.Stderr, "   ↳ run id %s  (handle: %s)\n", job.RunID, jobs.DeriveHandle(job.RunID))
	return fmt.Sprintf("Working on PR %s, sir. I'll call this one %s.", prNum, jobs.DeriveHandle(job.RunID)), nil
}

// prContinuationPrompt is the agent prompt body used by the pr_work
// kind. Phrased as instructions for the engine-side agent, not the
// user.
func prContinuationPrompt(prNum string) string {
	return fmt.Sprintf(
		"You are continuing work on PR #%s. The branch is already checked out in the "+
			"current working directory. Review existing changes (git status, git log, "+
			"git diff origin/main...HEAD), the PR description (gh pr view %s), and any "+
			"review comments (gh pr view %s --comments). Make further progress. Use the "+
			"ask_user tool when you need a decision from me. Commit and push when you "+
			"reach a stopping point or finish.",
		prNum, prNum, prNum,
	)
}

// gitRepoRoot returns the absolute path to the git repo containing
// cwd, or an error if not in a repo.
func gitRepoRoot(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolvePRWorkRepoRoot finds the git repository pr_work jobs use as the
// parent for `git worktree add`. A configured jobs.pr_work_repo_root wins;
// this is required under the launchd daemon, whose WorkingDirectory is the
// user's home directory (scripts/launchd/com.ryanlitalien.aida.plist) and
// is not a git repo, so os.Getwd() there can never resolve one. Falls back
// to the process's cwd, which is what an interactive `aida serve` run from
// inside a checkout gets for free.
func resolvePRWorkRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if cfg, cerr := config.LoadConfig(); cerr == nil {
		if configured := cfg.PRWorkRepoRoot(); configured != "" {
			dir = configured
		}
	}
	repoRoot, err := gitRepoRoot(dir)
	if err != nil {
		return "", fmt.Errorf("locate git repo (dir=%q): %w (set jobs.pr_work_repo_root in config.yaml to the aida checkout path when running under a daemon whose working directory isn't inside a git repo)", dir, err)
	}
	return repoRoot, nil
}

// removeWorktree is the cleanup hook called by the daemon's job-watch
// goroutine after a pr_work job reaches a terminal state. Best-effort:
// failures are logged but don't propagate (the SQL row is already
// marked terminal).
func removeWorktree(worktreePath string) error {
	if worktreePath == "" {
		return nil
	}
	if _, err := os.Stat(worktreePath); err != nil {
		// Already gone - nothing to do.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Locate the parent repo by walking up - the worktree itself has
	// a .git file pointing to the parent's .git/worktrees/<name> dir.
	// `git -C <worktree> rev-parse --git-common-dir` reveals the parent.
	cmd := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		// Fall back to plain rm-rf if we can't find the parent repo.
		return os.RemoveAll(worktreePath)
	}
	commonDir := strings.TrimSpace(string(out))
	// .git/worktrees → parent repo's .git → parent repo root.
	parentGit := commonDir
	if filepath.Base(commonDir) != ".git" {
		parentGit = filepath.Dir(commonDir) // strip /worktrees
	}
	parentRoot := filepath.Dir(parentGit)
	rmCmd := exec.CommandContext(context.Background(), "git", "-C", parentRoot, "worktree", "remove", "--force", worktreePath)
	rmCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	rmCmd.Stdout = nil
	rmCmd.Stderr = nil
	// Cap at 30s - git worktree remove sometimes hangs on dirty
	// indexes; the SIGKILL is acceptable.
	done := make(chan error, 1)
	if err := rmCmd.Start(); err != nil {
		return err
	}
	go func() { done <- rmCmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			// Final fallback: brute-force rm.
			return os.RemoveAll(worktreePath)
		}
		return nil
	case <-time.After(30 * time.Second):
		_ = rmCmd.Process.Kill()
		return os.RemoveAll(worktreePath)
	}
}

// startGenericAgentJob is the v1 path: enqueue a job, spawn
// `aida --agent --run-dir <dir>` detached. The pr_work kind specialises
// this further in a later commit (worktree + gh pr checkout in the
// run-dir before spawn).
func startGenericAgentJob(store *jobs.Store, kind, question string) (string, error) {
	if store == nil {
		return "", fmt.Errorf("jobs store unavailable")
	}
	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return "", fmt.Errorf("aida not on PATH: %w", err)
	}
	job, err := store.Enqueue(kind, "", "", question, "jarvis:voice")
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}
	runDir := jobs.RunDir(store.Profile(), job.RunID)

	cmd := exec.Command(aidaBin, "--agent", "--run-dir", runDir, question)
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
		"AIDA_PROFILE="+store.Profile(),
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = store.Fail(job.RunID, "spawn: "+err.Error())
		return "", fmt.Errorf("spawn agent: %w", err)
	}
	// Detach: the daemon's notifier surfaces completion/awaiting on
	// the next wake. The agent's own --run-dir logic (MarkRunning +
	// Complete / Fail) drives store transitions; reapAgentExit is the
	// backstop for a process that dies without reaching them.
	go reapAgentExit(store, cmd, job.RunID)
	// See startPRWorkJob: print the exact run id, speak the short handle so
	// the LLM can't mangle the numeric id and the user can reference it later.
	fmt.Fprintf(os.Stderr, "   ↳ run id %s  (handle: %s)\n", job.RunID, jobs.DeriveHandle(job.RunID))
	return fmt.Sprintf("Working on it, sir. I'll call this one %s.", jobs.DeriveHandle(job.RunID)), nil
}

// reapAgentExit waits for a spawned agent process and closes out its
// jobs row if the agent died without reaching its own terminal
// transition (crash before MarkRunning, panic, kill -9). Sticky
// terminal states make both Fails no-ops when the agent (or a user
// cancel) already finished the row properly - this is a backstop, not
// the primary lifecycle driver.
func reapAgentExit(store *jobs.Store, cmd *exec.Cmd, runID string) {
	err := cmd.Wait()
	if err != nil {
		_ = store.Fail(runID, "agent exited: "+err.Error())
		return
	}
	if j, gerr := store.Get(runID); gerr == nil && !jobs.IsTerminalState(j.State) {
		_ = store.Fail(runID, "agent exited 0 without finishing the job row")
	}
}

// ─── job_status ──────────────────────────────────────────────────────

type jobStatusInput struct {
	Ref string `json:"ref"`
}

func jobStatusTool(store *jobs.Store) Tool {
	return Tool{
		Name: "job_status",
		Description: "Check the state of a background agent job. Ref is the spoken " +
			"handle Jarvis announced when starting the job (e.g. \"amber-otter\") or " +
			"a partial-substring match against the question text. Returns the current " +
			"state and a one-line summary.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "spoken handle like \"amber-otter\" (or a phrase from the job's question)",
				},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in jobStatusInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			ref := strings.TrimSpace(in.Ref)
			if ref == "" {
				return "", fmt.Errorf("job_status: empty ref")
			}
			j, err := resolveJobRef(store, ref)
			if err != nil {
				return "", err
			}
			return formatJobLine(j), nil
		},
	}
}

// ─── job_list ───────────────────────────────────────────────────────

type jobListInput struct {
	State string `json:"state"`
}

func jobListTool(store *jobs.Store) Tool {
	return Tool{
		Name: "job_list",
		Description: "List background agent jobs. Default returns active jobs " +
			"(running + awaiting_input + queued); pass state='all' for terminal jobs " +
			"too, or a specific state name to filter.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"state": map[string]interface{}{
					"type":        "string",
					"description": "optional: 'running' | 'awaiting_input' | 'queued' | 'done' | 'failed' | 'all'",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in jobListInput
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &in)
			}
			if store == nil {
				return "", fmt.Errorf("jobs store unavailable")
			}
			state := strings.TrimSpace(in.State)
			var rows []jobs.Job
			var err error
			if state == "" {
				// "Active" default: queued + running + awaiting_input.
				// SQL doesn't OR these natively through ListOpts; do
				// three small queries and concatenate.
				for _, s := range []string{jobs.StateQueued, jobs.StateRunning, jobs.StateAwaitingInput} {
					rs, lerr := store.List(jobs.ListOpts{State: s, Limit: 50})
					if lerr != nil {
						err = lerr
						break
					}
					rows = append(rows, rs...)
				}
			} else if state == "all" {
				rows, err = store.List(jobs.ListOpts{Limit: 50})
			} else {
				rows, err = store.List(jobs.ListOpts{State: state, Limit: 50})
			}
			if err != nil {
				return "", err
			}
			if len(rows) == 0 {
				return "no jobs match", nil
			}
			var sb strings.Builder
			for i, j := range rows {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, formatJobLine(&j))
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// resolveJobRef accepts a full run-id (preferred), the spoken handle,
// or a substring match against the question/task_slug/run_id. Returns
// the newest matching row (List is newest-first). Matching runs in
// three passes over one row scan, strongest evidence first:
//
//  1. full handle - normalized ref contains the whole "amber-otter";
//  2. substring of the question / task-slug / run-id;
//  3. single handle word ("amber", or the mishear "amber other"),
//     ACTIVE jobs only - weakest evidence last, so a wordlist word
//     that happens to appear in some job's question wins pass 2
//     before a lone word can grab the wrong job, and a stale
//     terminal job can never shadow an active one.
func resolveJobRef(store *jobs.Store, ref string) (*jobs.Job, error) {
	if store == nil {
		return nil, fmt.Errorf("jobs store unavailable")
	}
	if j, err := store.Get(ref); err == nil {
		return j, nil
	}
	rows, err := store.List(jobs.ListOpts{Limit: 200})
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	lref := strings.ToLower(ref)
	nref := jobs.NormalizeRef(ref)
	if nref != "" {
		for _, j := range rows {
			if strings.Contains(nref, jobs.NormalizeRef(jobs.DeriveHandle(j.RunID))) {
				jj := j
				return &jj, nil
			}
		}
	}
	for _, j := range rows {
		if strings.Contains(strings.ToLower(j.Question), lref) ||
			strings.Contains(strings.ToLower(j.TaskSlug), lref) ||
			strings.Contains(strings.ToLower(j.RunID), lref) {
			jj := j
			return &jj, nil
		}
	}
	for _, j := range rows {
		if jobs.IsTerminalState(j.State) {
			continue
		}
		if jobs.RefMatchesHandleWord(j.RunID, ref) {
			jj := j
			return &jj, nil
		}
	}
	return nil, fmt.Errorf("no job matches %q", ref)
}

// ─── job_send_input ─────────────────────────────────────────────────

type jobSendInputInput struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

func jobSendInputTool(store *jobs.Store) Tool {
	return Tool{
		Name: "job_send_input",
		Description: "Deliver the user's reply to a job that's paused on ask_user " +
			"(state=awaiting_input). The text is written atomically to the run-dir's " +
			"input.txt; the paused agent picks it up on its next two-second poll tick " +
			"and resumes. Use this when the user just answered a prompt Jarvis spoke " +
			"on a previous wake - \"yes, use option two\", \"tell it to retry\", " +
			"\"rebase onto main\". Ref is the run-id Jarvis announced when the prompt " +
			"played, or a substring of the job's question.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "run-id (preferred) or substring match of the job's question",
				},
				"text": map[string]interface{}{
					"type":        "string",
					"description": "the user's reply, in natural language",
				},
			},
			"required": []string{"ref", "text"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in jobSendInputInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			ref := strings.TrimSpace(in.Ref)
			text := strings.TrimSpace(in.Text)
			if ref == "" || text == "" {
				return "", fmt.Errorf("job_send_input: ref and text are required")
			}
			j, err := resolveJobRef(store, ref)
			if err != nil {
				return "", err
			}
			if j.State != jobs.StateAwaitingInput {
				return "", fmt.Errorf("job %s is not awaiting input (state=%s)", j.RunID, j.State)
			}
			runDir := jobs.RunDir(store.Profile(), j.RunID)
			tmp := fmt.Sprintf("%s/.input.txt.tmp", runDir)
			if err := os.WriteFile(tmp, []byte(text), 0644); err != nil {
				return "", fmt.Errorf("write tmp: %w", err)
			}
			if err := os.Rename(tmp, runDir+"/input.txt"); err != nil {
				os.Remove(tmp)
				return "", fmt.Errorf("rename: %w", err)
			}
			return "Sent.", nil
		},
	}
}

// ─── job_cancel ─────────────────────────────────────────────────────

type jobCancelInput struct {
	Ref string `json:"ref"`
}

func jobCancelTool(store *jobs.Store) Tool {
	return Tool{
		Name: "job_cancel",
		Description: "Stop a running background job. Sends SIGTERM to the worker " +
			"subprocess and transitions the row to failed. Use sparingly - most jobs " +
			"will finish on their own or pause on ask_user.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "run-id (preferred) or substring match of the job's question",
				},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in jobCancelInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			ref := strings.TrimSpace(in.Ref)
			if ref == "" {
				return "", fmt.Errorf("job_cancel: empty ref")
			}
			j, err := resolveJobRef(store, ref)
			if err != nil {
				return "", err
			}
			if j.ClaimedByPID > 0 {
				proc, perr := os.FindProcess(j.ClaimedByPID)
				if perr == nil {
					_ = proc.Signal(syscall.SIGTERM)
				}
			}
			if err := store.Fail(j.RunID, "cancelled by user"); err != nil {
				return "", fmt.Errorf("mark failed: %w", err)
			}
			return fmt.Sprintf("Cancelled %s.", j.RunID), nil
		},
	}
}

// formatJobLine is the one-line description voice tools return.
// Compact enough to be read aloud naturally.
// formatJobLine renders one job for a spoken reply, handle-first: the
// long run-id stays OUT of the text (the LLM mangles it into "twenty
// twenty-six…" when reading aloud, and the system prompt forbids
// speaking it) - the pronounceable handle IS the job's spoken identity,
// matching what job_start announced and what /runs displays.
//
// A terminal job (done/failed/incomplete) is described via
// jobs.DescribeOutcome rather than a bare "is done", see the doc
// comment there for why. This used to hand-roll a "failed: <error>"
// case; DescribeOutcome folds that in (and more) for every terminal
// state, so it's the only place either kind of job's evidence is
// assembled.
func formatJobLine(j *jobs.Job) string {
	label := j.TaskSlug
	if label == "" {
		label = j.Question
		if len(label) > 60 {
			// Back off to a rune boundary - a mid-rune slice of a
			// question containing °/ - /… yields invalid UTF-8.
			cut := 60
			for cut > 0 && !utf8.RuneStart(label[cut]) {
				cut--
			}
			label = label[:cut] + "…"
		}
	}
	handle := jobs.DeriveHandle(j.RunID)
	if j.State == jobs.StateAwaitingInput {
		return fmt.Sprintf("%s (%s) is awaiting input: %s", handle, label, j.AwaitingPrompt)
	}
	if jobs.IsTerminalState(j.State) {
		return fmt.Sprintf("%s (%s): %s", handle, label, jobs.DescribeOutcome(j))
	}
	return fmt.Sprintf("%s (%s) is %s", handle, label, j.State)
}
