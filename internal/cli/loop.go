package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/sandbox"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/ryanlitalien/aida/internal/worktree"
)

// SKETCH (feat/context-lake): `aida loop` is the Ralph-style autonomous
// driver - a deterministic outer loop wrapped around the stateless
// `aida --agent` inner loop. See docs/autonomous-loop-design.md for the
// full design and the comparison to snarktank/ralph.
//
// The control flow is real; outward / heavy steps are gated:
//   - commit-on-pass: gated behind --commit (default off)
//   - learning write-back to brain: done (distillLearning)
//   - per-task worktree isolation: done (--worktree, internal/worktree)
//   - budget / concurrency / daemon: done (--max-budget-usd / --concurrency / --daemon)
//   - autonomous PR creation: Phase 3 (pending)
//
// The key difference from ralph: where ralph re-reads a flat progress.txt
// each iteration, aida seeds each fresh-context agent with semantic
// brain recall (loopRecall below). That is the whole reason to build this
// on aida rather than copy ralph's bash loop.

type loopOpts struct {
	tags          []string
	maxIterations int
	checks        []string      // quality-gate shell commands (build/test/lint); each must exit 0
	maxFix        int           // max fix attempts per task before parking it on hold
	commit        bool          // commit working tree on a passing iteration
	pr            bool          // open a PR per passing task (implies worktree; xor commit)
	reviewer      string        // GitHub handle to request review from on the PR
	base          string        // PR base branch (default main)
	reviewPanel   int           // adversarial reviewer agents to run before opening a PR
	worktree      bool          // run each task in an isolated worktree off the base branch's upstream
	concurrency   int           // work up to N tasks in parallel (requires worktree)
	maxBudgetUSD  float64       // stop once accumulated agent cost exceeds this (0 = config/unlimited)
	maxConsecFail int           // circuit breaker: stop after N consecutive holds (0 = unlimited)
	daemon        bool          // run continuously, waiting for new tasks instead of exiting
	poll          time.Duration // in --daemon mode, sleep this long when idle
	sandboxTier   string        // confinement tier for spawned agents: none|docker
	sandboxMemory string        // docker --memory limit (e.g. "8g")
	provisionAida string        // prebuilt linux/arm64 aida to copy into the docker sandbox (else auto-build)
	recallK       int           // number of brain lessons to seed per iteration
	perTask       time.Duration

	sbxTier      sandbox.Tier // resolved tier (after availability fallback); set in runLoop
	linuxAidaBin string       // path to a linux/arm64 aida build, provisioned into the docker sandbox; set in runLoop
	baseRemote   string       // remote the base branch tracks (e.g. "public"); set once by resolveBase
	baseRef      string       // remote-tracking ref worktrees are cut from and diffed against (e.g. "public/main"); set once by resolveBase
}

// resolveBase resolves, once per loop run, which remote and remote-tracking
// ref the --base branch lives on (worktree.ResolveUpstream), so worktree
// creation, the PR push, and the review-panel diff all read the same answer
// and cannot drift back to a hardcoded "origin". Only needed when worktrees
// are in play (--worktree, or --pr which implies it).
func (o *loopOpts) resolveBase(repoRoot string) {
	o.baseRemote, o.baseRef = worktree.ResolveUpstream(repoRoot, orDefault(o.base, "main"))
}

// defaultLoopOpts returns the canonical loop defaults. newLoopCmd seeds from
// it and `aida serve --loop` reuses it so a serve-hosted dispatcher gets the
// same sane defaults (non-zero maxFix/perTask/poll) for the knobs it doesn't
// expose. Keep the values here in sync with newLoopCmd's flag defaults.
func defaultLoopOpts() loopOpts {
	return loopOpts{
		maxIterations: 10,
		maxFix:        3,
		base:          "main",
		concurrency:   1,
		poll:          5 * time.Second,
		sandboxTier:   "none",
		recallK:       3,
		perTask:       450 * time.Second,
	}
}

// validate checks interdependent loop flags and returns an error for any
// incompatible combination. Pure (no IO beyond tier parsing) so `aida serve
// --loop` can call it for fail-fast UX before launching the dispatcher
// goroutine; runLoopCtx calls it too, so standalone `aida loop` validates
// identically. Mirrors the normalization that --pr implies --worktree.
func (o loopOpts) validate() error {
	if o.pr && o.commit {
		return fmt.Errorf("use --pr or --commit, not both")
	}
	worktree := o.worktree || o.pr // --pr needs a branch, so implies a worktree
	if o.concurrency > 1 && !worktree {
		return fmt.Errorf("--concurrency %d requires --worktree (parallel agents need isolated worktrees)", o.concurrency)
	}
	tier, err := sandbox.ParseTier(o.sandboxTier)
	if err != nil {
		return err
	}
	if tier != sandbox.TierNone && !worktree {
		return fmt.Errorf("--sandbox %s requires --worktree (the policy confines writes to the task's worktree)", tier)
	}
	return nil
}

func newLoopCmd() *cobra.Command {
	o := defaultLoopOpts()
	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Autonomously work a task set to completion (Ralph-style loop)",
		Long: "Repeatedly spawn a fresh-context `aida --agent` instance, each one\n" +
			"completing the single highest-priority open task in the set, until\n" +
			"every task is terminal or --max-iterations is hit.\n\n" +
			"Each iteration: pick task -> seed fresh agent with brain recall ->\n" +
			"implement -> run quality gate (--check) -> on pass mark the task done\n" +
			"and distill a learning back into the brain -> repeat.\n\n" +
			"Memory between iterations lives in the brain (lessons + recall) and\n" +
			"the task list, not in a flat progress file. See\n" +
			"docs/autonomous-loop-design.md.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLoop(o)
		},
	}
	cmd.Flags().StringSliceVar(&o.tags, "tag", nil, "only loop over tasks carrying ALL of these tags (repeatable)")
	cmd.Flags().IntVar(&o.maxIterations, "max-iterations", 10, "stop after N iterations (safety bound, like ralph.sh)")
	cmd.Flags().StringArrayVar(&o.checks, "check", nil, "quality-gate command run after each attempt; must exit 0 (repeatable, e.g. --check \"make build\" --check \"make test\")")
	cmd.Flags().IntVar(&o.maxFix, "max-fix-iterations", 3, "max fix attempts per task before parking it on hold")
	cmd.Flags().BoolVar(&o.commit, "commit", false, "commit the working tree after a passing iteration")
	cmd.Flags().BoolVar(&o.worktree, "worktree", false, "run each task in an isolated git worktree branched off the base branch's upstream (auto/<id>-<slug>)")
	cmd.Flags().BoolVar(&o.pr, "pr", false, "open a PR per passing task and tag a reviewer (implies --worktree; never merges)")
	cmd.Flags().StringVar(&o.reviewer, "reviewer", "", "GitHub handle to request review from on the PR (--pr)")
	cmd.Flags().StringVar(&o.base, "base", "main", "base branch for PRs (--pr)")
	cmd.Flags().IntVar(&o.reviewPanel, "review-panel", 0, "run N adversarial reviewer agents (correctness/security/test-coverage) that must majority-approve before a PR is opened")
	cmd.Flags().IntVar(&o.concurrency, "concurrency", 1, "work up to N tasks in parallel, each in its own worktree (requires --worktree)")
	cmd.Flags().Float64Var(&o.maxBudgetUSD, "max-budget-usd", 0, "stop once accumulated agent cost exceeds this (0 = use config agent.max_budget_usd, else unlimited)")
	cmd.Flags().IntVar(&o.maxConsecFail, "max-consecutive-failures", 0, "stop after N consecutive tasks parked on hold (0 = unlimited; circuit breaker)")
	cmd.Flags().BoolVar(&o.daemon, "daemon", false, "run continuously: when the task set drains, wait for new tasks instead of exiting")
	cmd.Flags().DurationVar(&o.poll, "poll", 5*time.Second, "in --daemon mode, how long to sleep when no tasks are ready")
	cmd.Flags().StringVar(&o.sandboxTier, "sandbox", "none", "confine spawned agents: none | docker (Docker Sandboxes via sbx; requires --worktree)")
	cmd.Flags().StringVar(&o.sandboxMemory, "sandbox-memory", "", "sbx microVM memory limit, e.g. 8g (docker tier only)")
	cmd.Flags().StringVar(&o.provisionAida, "provision-aida", "", "path to a prebuilt linux/arm64 aida to copy into the docker sandbox (default: auto-build from the aida checkout; build via `make build-linux-arm64`)")
	cmd.Flags().IntVar(&o.recallK, "recall-k", 3, "number of similar brain lessons to seed into each fresh agent")
	cmd.Flags().DurationVar(&o.perTask, "per-task-timeout", 450*time.Second, "per-iteration timeout for the spawned agent")
	cmd.AddCommand(newLoopPlanCmd())
	return cmd
}

func runLoop(o loopOpts) error {
	return runLoopCtx(context.Background(), o)
}

// runLoopCtx runs the dispatcher until the task set drains (non-daemon), a
// runtime bound trips, or ctx is cancelled. `aida loop` calls it with a
// background context; `aida serve --loop` passes the daemon's signal context so
// SIGINT/SIGTERM stops the loop alongside the HTTP server.
func runLoopCtx(ctx context.Context, o loopOpts) error {
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

	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return fmt.Errorf("locate aida binary on PATH: %w", err)
	}

	store, err := jobs.Open(profileName)
	if err != nil {
		return fmt.Errorf("open jobs: %w", err)
	}
	defer store.Close()

	// Validate interdependent flags up front (same checks `aida serve --loop`
	// runs before launching its goroutine), then normalize: --pr needs a
	// branch, so it implies --worktree.
	if err := o.validate(); err != nil {
		return err
	}
	if o.pr {
		o.worktree = true
	}

	// Resolve the base branch's upstream once, up front, from the repo the
	// loop runs in. Every worktree this run cuts, every PR push, and every
	// review-panel diff uses this one answer.
	if o.worktree {
		repoRoot, rerr := worktree.RepoRoot(".")
		if rerr != nil {
			return fmt.Errorf("--worktree requires running inside a git repository: %w", rerr)
		}
		o.resolveBase(repoRoot)
	}

	conc := o.concurrency
	if conc < 1 {
		conc = 1
	}

	// Resolve the sandbox tier (falling back when stronger tooling is absent).
	// validate() already confirmed the tier string + the worktree requirement.
	wantTier, _ := sandbox.ParseTier(o.sandboxTier)
	resolvedTier, note := sandbox.Resolve(wantTier)
	if note != "" {
		fmt.Printf("sandbox: %s\n", note)
	}
	o.sbxTier = resolvedTier

	// The docker tier is a Linux microVM - the host (macOS) aida can't run in
	// it. Build a linux/arm64 aida once up front and provision it into each
	// sandbox so a real `aida --agent` runs confined. Built fresh per loop run
	// so the confined agent matches the current code.
	if o.sbxTier == sandbox.TierDocker {
		linuxBin, berr := resolveLinuxAida(ctx, o.provisionAida)
		if berr != nil {
			return fmt.Errorf("provision linux aida for sandbox: %w", berr)
		}
		o.linuxAidaBin = linuxBin
		fmt.Printf("sandbox: provisioning linux/arm64 aida into each microVM (%s)\n", linuxBin)
	}

	// Budget: explicit flag wins, else fall back to config agent.max_budget_usd
	// (which was declared but never read until now).
	budget := o.maxBudgetUSD
	if budget == 0 {
		budget = cfg.Agent.MaxBudgetUSD
	}
	state := &loopState{budgetUSD: budget, maxConsecFail: o.maxConsecFail}

	fmt.Printf("aida loop - profile=%s tags=%v max=%d checks=%v max-fix=%d worktree=%v concurrency=%d budget=$%.2f daemon=%v\n",
		profileName, o.tags, o.maxIterations, o.checks, o.maxFix, o.worktree, conc, budget, o.daemon)
	if o.worktree {
		fmt.Printf("worktrees: cut from %s (base branch %s)\n", o.baseRef, orDefault(o.base, "main"))
	}

	// Round model: each round picks up to `conc` tasks and works them (in
	// parallel when conc>1), with a barrier between rounds so task selection
	// is race-free - every batch task reaches a terminal-for-this-round state
	// (done/hold) before the next pick. maxIterations counts rounds.
	round := 0
	for {
		// Stop promptly on cancellation (serve's SIGINT/SIGTERM signal context)
		// regardless of pending work - otherwise a cancel mid-run would spawn a
		// fresh round of born-cancelled agents and churn the queue to `hold`
		// during shutdown. The empty-batch idle wait below also observes ctx,
		// but only when the set happens to be drained.
		if err := ctx.Err(); err != nil {
			return err
		}
		if !o.daemon && o.maxIterations > 0 && round >= o.maxIterations {
			fmt.Printf("\nReached max-iterations (%d) without draining the set.\n", o.maxIterations)
			return nil
		}
		if reason := state.stopReason(); reason != "" {
			fmt.Printf("\nStopping: %s.\n", reason)
			return nil
		}

		batch, err := pickBatch(brn, o.tags, conc)
		if err != nil {
			return fmt.Errorf("pick tasks: %w", err)
		}
		if len(batch) == 0 {
			if o.daemon {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(o.poll):
				}
				continue
			}
			fmt.Println("\nAll tasks in the set are terminal. <promise>COMPLETE</promise>")
			return nil
		}
		round++

		fmt.Printf("\n=== round %d (%d task(s), spent $%.2f) ===\n", round, len(batch), state.spent())
		for i := range batch {
			fmt.Printf("  • #%d [%s] %s\n", batch[i].TaskID, batch[i].PriorityTag(), batch[i].Title)
		}

		if dryRunGuard("work tasks", fmt.Sprintf("%d task(s)", len(batch))) {
			fmt.Println("  [dry-run] would spawn fresh agent(s) + run the code gate")
			return nil
		}

		var wg sync.WaitGroup
		for i := range batch {
			t := &batch[i] // stable pointer into the slice for the goroutine
			wg.Add(1)
			go func(task *brain.TaskRecord) {
				defer wg.Done()
				o.processTask(ctx, brn, store, aidaBin, profileName, task, state)
			}(t)
		}
		wg.Wait()
	}
}

// loopState is the concurrency-safe runtime accounting shared across a loop
// run: total agent spend (for the budget stop) and the consecutive-failure
// circuit breaker.
type loopState struct {
	mu            sync.Mutex
	spentUSD      float64
	consecFail    int
	budgetUSD     float64 // 0 = unlimited
	maxConsecFail int     // 0 = unlimited
}

func (s *loopState) addCost(c float64) {
	s.mu.Lock()
	s.spentUSD += c
	s.mu.Unlock()
}

func (s *loopState) recordOutcome(passed bool) {
	s.mu.Lock()
	if passed {
		s.consecFail = 0
	} else {
		s.consecFail++
	}
	s.mu.Unlock()
}

func (s *loopState) spent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spentUSD
}

// stopReason returns a non-empty halt reason when a runtime bound is hit.
func (s *loopState) stopReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.budgetUSD > 0 && s.spentUSD >= s.budgetUSD {
		return fmt.Sprintf("budget reached ($%.2f >= $%.2f)", s.spentUSD, s.budgetUSD)
	}
	if s.maxConsecFail > 0 && s.consecFail >= s.maxConsecFail {
		return fmt.Sprintf("%d consecutive failures (circuit breaker)", s.consecFail)
	}
	return ""
}

// readRunCostUSD parses the "complete" event from a finished run's
// events.ndjson and returns its cost_usd. Returns 0 when absent/unreadable -
// cost is informational for the budget gate, never fatal.
func readRunCostUSD(profile, runID string) float64 {
	data, err := os.ReadFile(jobs.EventsPath(profile, runID))
	if err != nil {
		return 0
	}
	var cost float64
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line == "" || !strings.Contains(line, `"complete"`) {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				CostUSD float64 `json:"cost_usd"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err == nil && ev.Type == EventKindComplete {
			cost = ev.Data.CostUSD
		}
	}
	return cost
}

// processTask runs one task to a terminal outcome: spawn a fresh agent in an
// (optionally isolated) worktree, run the code gate, iterate-until-green, then
// either complete the task + distill a learning, or park it on hold. All
// worktree teardown happens via defer so every return path cleans up - which
// also makes this safe to run concurrently (Phase 4b's --concurrency).
func (o loopOpts) processTask(ctx context.Context, brn *brain.Brain, store *jobs.Store, aidaBin, profileName string, task *brain.TaskRecord, state *loopState) {
	if _, err := brn.SetTaskStatus(task.Slug, brain.StatusInProgress); err != nil {
		ui.PrintVerbose("loop", "set in-progress failed: "+err.Error())
	}

	// Optional per-task worktree isolation off the resolved base ref. The branch
	// (auto/<id>-<slug>) and any commit-on-pass survive teardown so Phase 3
	// can push it; the working directory is always removed. We do NOT stash
	// the path on the job manifest - the loop owns cleanup, and letting the
	// serve daemon also remove it would yank the tree mid-task between fix
	// attempts (each attempt completes its own job).
	workDir := ""
	var wt *worktree.Worktree
	if o.worktree {
		w, err := o.setupTaskWorktree(profileName, task)
		if err != nil {
			fmt.Printf("  ✗ worktree setup failed: %s - parking on hold\n", err)
			parkHold(brn, task)
			return
		}
		wt = w
		workDir = wt.Dir
		defer func() {
			if err := wt.Remove(); err != nil {
				ui.PrintVerbose("loop", "worktree remove failed: "+err.Error())
			}
		}()
	}

	// Seed fresh context with brain recall - the aida analog of ralph's
	// "read the Codebase Patterns section first".
	preamble := loopRecall(ctx, brn, task, o.recallK)
	checks := codeChecksFrom(o.checks)
	if len(checks) == 0 {
		ui.PrintVerbose("loop", "no --check configured; gate is agent self-report only (broken code can compound)")
	}

	// Iterate-until-green: spawn a fresh agent, run the code gate, and on
	// failure re-spawn with the failure messages until green or maxFix is
	// exhausted (the deferred "Ralph Wiggum" variant - internal/eval/reviewer.go).
	maxFix := o.maxFix
	if maxFix < 1 {
		maxFix = 1
	}
	var lastRunID string
	var lastIssues []eval.Issue
	passed := false
	for attempt := 1; attempt <= maxFix; attempt++ {
		if reason := state.stopReason(); reason != "" {
			fmt.Printf("  stopping fix attempts: %s\n", reason)
			break
		}
		var prompt string
		if attempt == 1 {
			prompt = buildLoopPrompt(task, preamble, o.checks)
		} else {
			fmt.Printf("  fix attempt %d/%d (%d issue(s) from last gate)\n", attempt, maxFix, len(lastIssues))
			prompt = buildFixPrompt(task, lastIssues)
		}

		runID, runErr := spawnLoopAgent(ctx, store, aidaBin, profileName, task, prompt, o.perTask, workDir, o.sbxTier, o.sandboxMemory, o.linuxAidaBin)
		if runErr != nil {
			fmt.Printf("  ✗ agent failed: %s\n", runErr)
			break // park on hold below
		}
		lastRunID = runID
		state.addCost(readRunCostUSD(profileName, runID))

		records := runCodeGate(ctx, workDir, checks)
		persistLoopEvalRun(brn, runID, task, profileName, records)
		if eval.AggregateVerdict(records) != eval.VerdictFail {
			passed = true
			break
		}
		lastIssues = collectIssues(records)
		fmt.Printf("  ✗ gate failed (attempt %d/%d): %d issue(s)\n", attempt, maxFix, len(lastIssues))
	}

	state.recordOutcome(passed)

	if !passed {
		fmt.Println("  parking task on hold (gate not green / agent failed)")
		parkHold(brn, task)
		return
	}

	switch {
	case o.pr:
		// Commit on the worktree branch, run the adversarial review panel,
		// then push + open a PR and tag the reviewer. The loop NEVER merges:
		// the task is parked on hold (awaiting external = the human's review +
		// merge), tagged pr-open so it's findable. Phase 5 replaces the hold
		// with a dedicated awaiting_approval state.
		if wt == nil {
			fmt.Println("  ✗ --pr requires a worktree - parking on hold")
			parkHold(brn, task)
			return
		}
		if err := loopCommit(workDir, task); err != nil {
			fmt.Printf("  ✗ commit failed: %s - parking on hold\n", err)
			parkHold(brn, task)
			return
		}
		if !o.runReviewPanel(ctx, store, aidaBin, profileName, workDir, task, state) {
			fmt.Println("  ✗ adversarial review panel rejected - parking on hold")
			parkHold(brn, task)
			return
		}
		prURL, err := openTaskPR(workDir, wt.Branch, task, o.reviewer, o.base, o.baseRemote)
		if err != nil {
			fmt.Printf("  ✗ open PR failed: %s - parking on hold\n", err)
			parkHold(brn, task)
			return
		}
		if lastRunID != "" {
			if err := store.SetArtifactURL(lastRunID, prURL); err != nil {
				ui.PrintVerbose("loop", "set artifact url failed: "+err.Error())
			}
		}
		distillLearning(ctx, brn, task, profileName, lastRunID)
		if _, err := brn.UpdateTask(task.Slug, brain.TaskPatch{AddTags: []string{"pr-open"}}); err != nil {
			ui.PrintVerbose("loop", "tag pr-open failed: "+err.Error())
		}
		parkHold(brn, task)
		fmt.Printf("  ✓ PR opened (awaiting your review + merge): %s\n", prURL)
		return

	case o.commit:
		if err := loopCommit(workDir, task); err != nil {
			fmt.Printf("  ✗ commit failed: %s - parking on hold\n", err)
			parkHold(brn, task)
			return
		}
	}

	if _, err := brn.CompleteTask(task.Slug); err != nil {
		ui.PrintVerbose("loop", "complete failed: "+err.Error())
	}
	distillLearning(ctx, brn, task, profileName, lastRunID)
	fmt.Printf("  ✓ #%d done\n", task.TaskID)
}

// parkHold flips a task to hold (the loop's "needs a human / next run" state).
func parkHold(brn *brain.Brain, task *brain.TaskRecord) {
	if _, err := brn.SetTaskStatus(task.Slug, brain.StatusHold); err != nil {
		ui.PrintVerbose("loop", "set hold failed: "+err.Error())
	}
}

// setupTaskWorktree creates a fresh worktree off the resolved base ref
// (o.baseRef, e.g. public/main) for a task, clearing any stale branch/dir from
// a prior crashed run first. The branch is auto/<id>-<slug> so Phase 3 can push
// it as the task's PR branch.
func (o loopOpts) setupTaskWorktree(profileName string, task *brain.TaskRecord) (*worktree.Worktree, error) {
	repoRoot, err := worktree.RepoRoot(".")
	if err != nil {
		return nil, err
	}
	if o.baseRef == "" {
		return nil, fmt.Errorf("base ref not resolved (resolveBase must run before worktrees are cut)")
	}
	label := fmt.Sprintf("loop-%d-%s", task.TaskID, sanitizeLabel(task.Slug))
	branch := "auto/" + label
	dest := filepath.Join(config.Dir(), "worktrees", profileName, label)
	// Clear stale state from a previous crashed run of the same task.
	_ = worktree.Remove(repoRoot, dest)
	worktree.DeleteBranch(repoRoot, branch)
	return worktree.Create(repoRoot, dest, o.baseRef, branch)
}

// sanitizeLabel keeps a slug filesystem- and branch-safe (lowercase
// alphanumerics + dashes, capped).
func sanitizeLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	if out == "" {
		out = "task"
	}
	return out
}

// pickBatch returns up to n highest-priority non-terminal tasks in the set
// (priority p1 > p2 > p3, then oldest first). Open and in-progress are both
// eligible (an in-progress task is one a prior round started but did not
// finish). The round barrier in runLoop guarantees the batch's tasks are
// distinct from the next round's, so concurrent workers never collide.
func pickBatch(brn *brain.Brain, tags []string, n int) ([]brain.TaskRecord, error) {
	candidates, err := brn.ListTasksByStatus(
		[]string{brain.StatusOpen, brain.StatusInProgress}, tags, 0, 0)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		pa, pb := candidates[a].PriorityTag(), candidates[b].PriorityTag()
		if pa != pb {
			return pa < pb // "p1" < "p2" < "p3" lexically
		}
		return candidates[a].Created < candidates[b].Created
	})
	if n > 0 && len(candidates) > n {
		candidates = candidates[:n]
	}
	return candidates, nil
}

// loopRecall pulls the top-k semantically-similar brain lessons for this
// task and formats them as a preamble. This is the synergy that makes the
// aida loop better than ralph's progress.txt: the fresh agent starts
// already knowing the relevant gotchas and patterns.
func loopRecall(ctx context.Context, brn *brain.Brain, task *brain.TaskRecord, k int) string {
	if k <= 0 {
		return ""
	}
	query := task.Title
	if task.Description != "" {
		query += "\n" + task.Description
	}
	sc, err := brn.Search(ctx, query, task.Tags, k)
	if err != nil || sc == nil || len(sc.SimilarLessons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Relevant past learnings (from the brain)\n\n")
	for _, sl := range sc.SimilarLessons {
		l := sl.Lesson
		line := l.Question
		switch {
		case l.FeedbackReason != "":
			line += " - " + l.FeedbackReason
		case l.AnswerSnippet != "":
			line += " - " + l.AnswerSnippet
		}
		b.WriteString(fmt.Sprintf("- %s\n", strings.TrimSpace(line)))
	}
	return b.String()
}

// buildLoopPrompt composes the single-task instruction handed to the
// fresh agent. Mirrors ralph's per-iteration prompt: do ONE task, keep it
// focused, run the checks.
func buildLoopPrompt(task *brain.TaskRecord, preamble string, checks []string) string {
	var b strings.Builder
	if preamble != "" {
		b.WriteString(preamble)
		b.WriteString("\n")
	}
	b.WriteString("## Your single task this iteration\n\n")
	b.WriteString(fmt.Sprintf("#%d: %s\n", task.TaskID, task.Title))
	if task.Description != "" {
		b.WriteString("\n" + task.Description + "\n")
	}
	b.WriteString("\nImplement exactly this task and nothing else. Keep the change focused and minimal. ")
	b.WriteString("Follow existing code patterns. If you add or change behavior, add or update a test that covers it.")
	if len(checks) > 0 {
		b.WriteString(fmt.Sprintf(" Before finishing, these commands must ALL pass: `%s`.", strings.Join(checks, "`, `")))
	}
	b.WriteString("\n\nEnd your response with a `## Learnings` section: 2-4 bullets of reusable " +
		"patterns or gotchas a future iteration should know. Keep them general, not story-specific.")
	return b.String()
}

// buildFixPrompt is the prompt for a retry attempt: it tells the fresh agent
// the previous attempt failed the gate and feeds it the exact failure messages
// so it can fix them. The working tree already carries the previous attempt's
// changes (same cwd / worktree), so this is a continuation, not a restart.
func buildFixPrompt(task *brain.TaskRecord, issues []eval.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Continue task #%d: %s\n\n", task.TaskID, task.Title)
	b.WriteString("Your previous attempt did NOT pass the quality gate. The working tree still " +
		"has that attempt's changes. Fix these failures and nothing else:\n\n")
	for _, is := range issues {
		fmt.Fprintf(&b, "### %s\n%s\n\n", is.Type, strings.TrimSpace(is.Message))
	}
	b.WriteString("Make the minimal change needed for every check to pass. " +
		"End with a `## Learnings` section (2-4 reusable bullets).")
	return b.String()
}

// aidaModulePath is this project's Go module path - used to confirm an
// auto-build is happening from the aida checkout (and not some other repo
// the loop happens to be working tasks in).
const aidaModulePath = "github.com/ryanlitalien/aida"

// resolveLinuxAida returns the path to a linux/arm64 aida to provision into the
// docker sandbox. An explicit --provision-aida path wins (just validated to
// exist); otherwise it auto-builds from the aida checkout.
func resolveLinuxAida(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--provision-aida %s: %w", explicit, err)
		}
		return explicit, nil
	}
	return buildLinuxAida(ctx)
}

// buildLinuxAida cross-compiles a linux/arm64 aida from the aida checkout
// containing the cwd and returns the binary path. CGO_ENABLED=0 + the pure-Go
// sqlite driver make this dependency-free. Built into a stable temp path,
// overwritten each loop run so the confined agent tracks the current code.
// Errors (with guidance to pass --provision-aida) when cwd isn't the aida
// module - the loop can work tasks in any repo, but only aida can build aida.
func buildLinuxAida(ctx context.Context) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := worktree.RepoRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("locate repo root: %w (pass --provision-aida <linux aida>)", err)
	}
	if mod := modulePath(root); mod != aidaModulePath {
		return "", fmt.Errorf("cwd repo module is %q, not %s - pass --provision-aida <path to a linux/arm64 aida> (build it in the aida checkout via `make build-linux-arm64`)", mod, aidaModulePath)
	}
	// Per-process path: the binary is read lazily by `sbx cp` on every spawn
	// across a multi-minute run, so a fixed path would let a second concurrent
	// loop process (built from different code) silently overwrite the bytes
	// this run provisions. Keyed by PID so concurrent loops can't stomp.
	out := filepath.Join(os.TempDir(), fmt.Sprintf("aida-linux-arm64-%d", os.Getpid()))
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/aida")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if b, berr := cmd.CombinedOutput(); berr != nil {
		return "", fmt.Errorf("go build linux/arm64 aida: %w (%s)", berr, strings.TrimSpace(string(b)))
	}
	return out, nil
}

// modulePath reads the `module` line from <root>/go.mod, or "" if unreadable.
func modulePath(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

// spawnLoopAgent runs `aida --agent --run-dir <dir> <prompt>` as a fresh
// subprocess - clean context every iteration. Reuses the exact spawn
// pattern from tasks_ingest --auto-solve (enqueue -> run -> complete/fail).
func spawnLoopAgent(ctx context.Context, store *jobs.Store, aidaBin, profileName string, task *brain.TaskRecord, prompt string, perTask time.Duration, workDir string, tier sandbox.Tier, memLimit, linuxAidaBin string) (string, error) {
	job, err := store.Enqueue("loop", task.Title, "", prompt, "")
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}
	// Record the per-job governance this loop enforces (tier + timeout) so the
	// confinement + bound is observable per job in the manifest / `aida jobs`.
	// Best-effort: governance is for audit, never gates the run.
	if gerr := store.SetGovernance(job.RunID, jobs.Governance{
		TimeoutSec:  int(perTask.Seconds()),
		SandboxTier: string(tier),
	}); gerr != nil {
		ui.PrintVerbose("loop", "set governance failed: "+gerr.Error())
	}
	runDir := jobs.RunDir(profileName, job.RunID)

	runCtx, cancel := context.WithTimeout(ctx, perTask)
	defer cancel()

	// Confine the agent to its worktree (+ ~/.aida for the run-dir/brain)
	// under the resolved tier. workDir is the task's isolated worktree.
	policy := sandbox.Policy{
		WorkDir: workDir,
		// Mount the run-dir too, so a (linux) agent can write its
		// events.ndjson/output.md back; the worktree is the primary workspace.
		ExtraMounts: []string{runDir},
		MemoryLimit: memLimit,
		Env: map[string]string{
			"NO_COLOR": "1", "CLICOLOR": "0", "TERM": "dumb",
			"AIDA_PROFILE": profileName,
		},
		// docker tier only: the linux/arm64 aida copied into the microVM and run
		// in place of the macOS aidaBin. Empty for TierNone (runs aidaBin directly).
		ProvisionBin: linuxAidaBin,
	}
	cmd, cleanup, werr := sandbox.Wrap(runCtx, tier, policy, aidaBin, "--agent", "--run-dir", runDir, prompt)
	if werr != nil {
		_ = store.Fail(job.RunID, werr.Error())
		return job.RunID, werr
	}
	defer cleanup()
	cmd.Stdout = nil
	cmd.Stderr = nil

	if runErr := cmd.Run(); runErr != nil {
		_ = store.Fail(job.RunID, runErr.Error())
		return job.RunID, runErr
	}
	if err := store.Complete(job.RunID); err != nil {
		ui.PrintVerbose("loop", "job complete failed: "+err.Error())
	}
	return job.RunID, nil
}

// codeChecksFrom turns the repeatable --check command strings into typed
// eval.CodeChecks, deriving a stable name (build/test/lint/check) for issue
// grouping. Blank entries are dropped.
func codeChecksFrom(checks []string) []eval.CodeCheck {
	out := make([]eval.CodeCheck, 0, len(checks))
	for _, c := range checks {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		out = append(out, eval.CodeCheck{Name: deriveCheckName(c), Cmd: c})
	}
	return out
}

// deriveCheckName heuristically classifies a check command so its failures get
// a meaningful Issue.Type (e.g. "make test" -> "test" -> "test-failure").
func deriveCheckName(cmd string) string {
	l := strings.ToLower(cmd)
	switch {
	case strings.Contains(l, "test"):
		return "test"
	case strings.Contains(l, "vet"), strings.Contains(l, "lint"):
		return "lint"
	case strings.Contains(l, "build"):
		return "build"
	default:
		return "check"
	}
}

// runCodeGate runs the CodeReviewer over the working tree and returns its
// records. workDir "" means the current directory. No checks => no records
// (treated as pass by the caller's AggregateVerdict).
func runCodeGate(ctx context.Context, workDir string, checks []eval.CodeCheck) []eval.ReviewRecord {
	if len(checks) == 0 {
		return nil
	}
	fmt.Printf("  running code gate: %d check(s)\n", len(checks))
	records, err := eval.Loop(ctx, []eval.Reviewer{eval.NewCodeReviewer()}, eval.ReviewInput{
		WorkDir:  workDir,
		Commands: checks,
	})
	if err != nil {
		ui.PrintVerbose("loop", "code gate error: "+err.Error())
	}
	return records
}

// collectIssues flattens all Issues across the gate's records, for feeding into
// the next fix attempt's prompt.
func collectIssues(records []eval.ReviewRecord) []eval.Issue {
	var issues []eval.Issue
	for _, r := range records {
		issues = append(issues, r.Issues...)
	}
	return issues
}

// persistLoopEvalRun writes the gate's records to the brain's eval-runs/ so the
// router boost and `aida brain analyze` (Phase 7) can mine code-gate failures the
// same way they mine answer-quality failures. Best-effort; non-fatal.
func persistLoopEvalRun(brn *brain.Brain, runID string, task *brain.TaskRecord, profile string, records []eval.ReviewRecord) {
	if runID == "" || len(records) == 0 {
		return
	}
	er := &brain.EvalRun{
		RunID:            runID,
		Question:         task.Title,
		Profile:          profile,
		AggregateVerdict: string(eval.AggregateVerdict(records)),
		Reviewers:        records,
	}
	if err := brain.WriteEvalRun(brn.Path, er); err != nil {
		ui.PrintVerbose("loop", "write eval-run failed: "+err.Error())
	}
}

// loopCommit stages and commits the working tree for a passing iteration.
//
// TODO(feat/context-lake): wire this to honour dryRunGuard, derive a
// conventional-commit message from the task, and optionally isolate the
// work in a per-iteration worktree (reuse the pr_work worktree infra in
// internal/jobs). Kept minimal + behind --commit for now.
func loopCommit(workDir string, task *brain.TaskRecord) error {
	msg := fmt.Sprintf("feat: #%d %s", task.TaskID, task.Title)
	git := func(rest ...string) []string {
		if workDir != "" {
			return append([]string{"-C", workDir}, rest...)
		}
		return rest
	}
	if err := exec.Command("git", git("add", "-A")...).Run(); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if err := exec.Command("git", git("commit", "-m", msg)...).Run(); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	fmt.Printf("  committed: %s\n", msg)
	return nil
}

// distillLearning reads the just-finished run's output.md, extracts the
// agent's "## Learnings" section, and records it as a first-class brain
// lesson with an embedding. That is how loopRecall surfaces it on the next
// similar task - ralph's progress.txt, but compounding through semantic
// recall instead of grep.
func distillLearning(ctx context.Context, brn *brain.Brain, task *brain.TaskRecord, profileName, runID string) {
	if runID == "" {
		return
	}
	out, err := os.ReadFile(jobs.OutputPath(profileName, runID))
	if err != nil {
		ui.PrintVerbose("loop", "distill: read output.md failed: "+err.Error())
		return
	}
	learning := extractLearnings(string(out))
	if learning == "" {
		ui.PrintVerbose("loop", fmt.Sprintf("distill: no ## Learnings section in #%d output", task.TaskID))
		return
	}
	lesson := &lessons.Lesson{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		RunID:         runID,
		Question:      task.Title,
		Strategy:      "loop",
		AnswerSnippet: learning,
	}
	if err := brn.RecordLesson(ctx, lesson); err != nil {
		ui.PrintVerbose("loop", "distill: record lesson failed: "+err.Error())
		return
	}
	ui.PrintVerbose("loop", fmt.Sprintf("distilled learning from #%d into brain", task.TaskID))
}

// extractLearnings pulls the text under a "## Learnings" heading from the
// agent's output. Returns "" when there is no such section - better to
// record nothing than to pollute the lessons table with a full answer.
func extractLearnings(out string) string {
	const maxLen = 800
	lower := strings.ToLower(out)
	idx := strings.Index(lower, "## learnings")
	if idx < 0 {
		return ""
	}
	section := strings.TrimSpace(out[idx:])
	// Trim at the next top-level heading, if any.
	if end := strings.Index(section[len("## learnings"):], "\n## "); end >= 0 {
		section = strings.TrimSpace(section[:len("## learnings")+end])
	}
	if len(section) > maxLen {
		section = section[:maxLen] + "…"
	}
	return section
}

// newLoopPlanCmd sketches `aida loop plan "<goal>"` - the decomposition
// front-end. Turns a plain-English goal into a set of right-sized aida
// tasks (tagged for the loop) by reusing the same LLM extraction path as
// `aida tasks ingest`. `aida loop --tag <tag>` then drains that set.
//
// SKETCH: the extraction itself is real; what is still TODO is a
// loop-specific prompt enforcing ralph's discipline (dependency ordering,
// one-iteration sizing, verifiable acceptance criteria) instead of the
// generic notes-to-tasks extraction.
func newLoopPlanCmd() *cobra.Command {
	var tags []string
	var maxTasks int
	cmd := &cobra.Command{
		Use:   "plan <goal>",
		Short: "Decompose a plain-English goal into loop-ready tasks",
		Long: "Runs an LLM decomposition pass over a goal and creates one aida\n" +
			"task per resulting step, tagged so `aida loop --tag <tag>` can drain\n" +
			"them. Reuses the `aida tasks ingest` extraction path.\n\n" +
			"SKETCH: a loop-specific prompt (dependency ordering, one-iteration\n" +
			"sizing, verifiable acceptance criteria) is still TODO.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLoopPlan(cmd.Context(), strings.Join(args, " "), tags, maxTasks)
		},
	}
	cmd.Flags().StringSliceVar(&tags, "tag", []string{"loop"}, "tag(s) to apply to every created task")
	cmd.Flags().IntVar(&maxTasks, "max-tasks", 0, "cap the number of tasks created (0 = no cap)")
	return cmd
}

func runLoopPlan(ctx context.Context, goal string, tags []string, maxTasks int) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	_, profileName := cfg.ActiveProfileConfig()
	if profileName == "" {
		return fmt.Errorf("no active profile resolved")
	}
	brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return fmt.Errorf("open brain: %w", err)
	}

	apiKey := cfg.GetAPIKey()
	if apiKey == "" {
		return fmt.Errorf("no API key configured (set %s)", cfg.API.AnthropicKeyEnv)
	}
	client := llm.NewClient(apiKey, cfg.Model.Primary, false)

	// TODO(feat/context-lake): swap TaskExtraction* for a loop-specific
	// prompt that enforces dependency order, one-iteration sizing, and
	// verifiable acceptance criteria written into each task body.
	raw, err := client.CompleteJSONWithStage(ctx, "loop-plan",
		llm.TaskExtractionSystemPrompt,
		llm.TaskExtractionUserPrompt(goal, "loop-plan goal"),
		llm.TaskExtractionSchema(),
	)
	if err != nil {
		return fmt.Errorf("decompose goal: %w", err)
	}
	var resp struct {
		Tasks []struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return fmt.Errorf("parse decomposition: %w", err)
	}
	if len(resp.Tasks) == 0 {
		fmt.Println("Decomposition produced 0 tasks.")
		return nil
	}
	if maxTasks > 0 && len(resp.Tasks) > maxTasks {
		resp.Tasks = resp.Tasks[:maxTasks]
	}

	if dryRun {
		fmt.Printf("[dry-run] would create %d task(s) tagged %v:\n", len(resp.Tasks), tags)
		for i, t := range resp.Tasks {
			fmt.Printf("  %d. %s\n", i+1, t.Title)
		}
		return nil
	}

	// This has its own AddTask loop rather than delegating to
	// runTasksIngest (it reuses only the LLM extraction path, per the
	// command's Long help above), so it needs its own pull-before-batch
	// call - one pull for the whole plan, not per task.
	if cfg.Brain.AutoSync {
		brain.PullAndWait(cfg.BrainPath())
	}

	for i, t := range resp.Tasks {
		// Priority by document order so pickNextTask drains the plan
		// roughly top-down. The loop-specific prompt (TODO) will set
		// this explicitly from real dependency analysis.
		taskTags := append([]string{}, tags...)
		taskTags = append(taskTags, loopPriorityTag(i))
		rec, err := brn.AddTask(t.Title, taskTags, t.Body)
		if err != nil {
			fmt.Printf("  ✗ create %q failed: %s\n", t.Title, err)
			continue
		}
		fmt.Printf("  ✓ #%d %s\n", rec.TaskID, rec.Title)
	}
	return nil
}

// loopPriorityTag maps document order to a priority tag: first two → p1,
// next three → p2, rest p3. A coarse stand-in for dependency ordering.
func loopPriorityTag(i int) string {
	switch {
	case i < 2:
		return "p1"
	case i < 5:
		return "p2"
	default:
		return "p3"
	}
}
