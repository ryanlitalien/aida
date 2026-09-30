package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/models"
	"github.com/ryanlitalien/aida/internal/taskstate"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/ryanlitalien/aida/internal/worktree"
)

// loop_arbiter.go is the --arbiter half of `aida loop` (ADR-0002): every
// attempt asks the capacity view (internal/arbiter, internal/burndown) for
// the cheapest eligible lane instead of always spawning the metered
// `aida --agent` path, claims the task with a cross-machine git-ref lease
// (internal/taskstate, ADR-0001), and hands off to a fresh model on a lane
// switch instead of restarting (ADR-0004). It reuses loop.go's task-neutral
// helpers (setupTaskWorktree, parkHold, loopCommit, finishPassedTask,
// runCodeGate, codeChecksFrom, collectIssues, readRunCostUSD, loopRecall,
// buildLoopPrompt, buildFixPrompt, pickBatch) rather than duplicating them -
// only the parts that differ under enforcement live here.
//
// This file is only ever reached from runLoopCtx's `if o.arbiter` branch, so
// none of it runs, and none of its bugs can regress, a plain `aida loop`.

// capacitySource abstracts the live burn-down capacity view so tests can
// inject canned rows instead of probing real providers over the network.
type capacitySource interface {
	Snapshot(ctx context.Context) ([]burndown.Capacity, error)
}

// liveCapacity is the real capacitySource: it loads the models roster and
// burndown floors config once (newLiveCapacity), matching the pattern
// internal/cli/burndown.go's `aida burndown capacity` follows, then re-probes
// live usage on every Snapshot call - the probe's own TTL cache
// (internal/models/probe_cache.go) keeps repeated calls within one loop run
// cheap rather than hammering every provider on every attempt.
type liveCapacity struct {
	roster *models.Roster
	bcfg   *burndown.Config
}

func newLiveCapacity(cfg *config.Config) (*liveCapacity, error) {
	r, err := models.Load(cfg.ModelsPath())
	if err != nil {
		return nil, fmt.Errorf("loading models roster: %w", err)
	}
	bcfg, err := burndown.Load(cfg.BurndownPath())
	if err != nil {
		return nil, fmt.Errorf("loading burndown config: %w", err)
	}
	return &liveCapacity{roster: r, bcfg: bcfg}, nil
}

func (l *liveCapacity) Snapshot(ctx context.Context) ([]burndown.Capacity, error) {
	pctx, cancel := context.WithTimeout(ctx, modelsProbeTimeout)
	defer cancel()
	usages := models.ProbeWithOptions(pctx, l.roster, models.ProbeOptions{})
	return burndown.Report(l.roster.Providers, usages, l.bcfg, time.Now()), nil
}

// arbiterRuntime bundles everything a --arbiter round needs beyond what
// runLoopCtx already opened (brn, store, aidaBin). Every field is set by
// newArbiterRuntime from config, but is also individually injectable so
// loop_arbiter_test.go can substitute fakes for capacity/runner and a
// local bare-repo remote for the claimer, without touching real providers,
// a real brain, or a real git host.
type arbiterRuntime struct {
	lanes      *arbiter.Config
	capacity   capacitySource
	signals    *arbiter.Store
	ledgerPath string
	host       string
	brainPath  string
	claimer    *taskstate.Claimer
	runner     arbiter.Runner
	now        func() time.Time
	autoSync   bool
}

// newArbiterRuntime builds the real arbiterRuntime for `aida loop --arbiter`.
// A missing lane config is fatal (2026-09-25: a lane roster names real
// accounts and machines, so enforcement must never silently spawn on
// arbiter.DefaultConfig()'s placeholder roster) - see errNoLaneRoster's
// doc comment. A lane config that fails to PARSE is fatal too - see
// arbiter.Load's own doc comment for why silently falling back there
// would be worse than a clear error.
func newArbiterRuntime(cfg *config.Config, o loopOpts, profileName, brainPath string) (*arbiterRuntime, error) {
	lanes, err := loadLanesConfig(cfg.LanesPath())
	if err != nil {
		return nil, err
	}
	if err := lanes.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lane config: %w", err)
	}

	cap, err := newLiveCapacity(cfg)
	if err != nil {
		return nil, err
	}

	signalDir := filepath.Join(config.Dir(), "arbiter")
	store, err := arbiter.OpenStore(signalDir)
	if err != nil {
		return nil, fmt.Errorf("open signal store: %w", err)
	}

	host, _ := os.Hostname()

	return &arbiterRuntime{
		lanes:      lanes,
		capacity:   cap,
		signals:    store,
		ledgerPath: filepath.Join(signalDir, "ledger.ndjson"),
		host:       host,
		brainPath:  brainPath,
		claimer: &taskstate.Claimer{
			RepoDir:  brainPath,
			Remote:   o.leaseRemote,
			Claimant: "loop@" + host,
			Host:     host,
			TTL:      o.leaseTTL,
		},
		runner:   arbiter.ExecRunner{},
		now:      time.Now,
		autoSync: cfg.Brain.AutoSync,
	}, nil
}

// taskOutcome is what one --arbiter task attempt loop produced, so the round
// loop can tell "every task waited on a lane" (outcomeNoLane, never a task
// failure, never trips the circuit breaker) apart from a real done/hold.
type taskOutcome int

const (
	outcomeDone taskOutcome = iota
	outcomeHold
	outcomeNoLane
)

// runArbiterLoop is runLoopCtx's --arbiter round loop. It differs from the
// plain loop in three ways the plain one never needs: it fetches and fast-
// forwards the brain before picking (ADR-0001, task #498, so it never lists
// or leases a task off a stale view), it claims a lease per task instead of
// just flipping status to in-progress, and a round where every task waited on
// a lane is "wait", not "drained" or "failed".
func runArbiterLoop(ctx context.Context, cfg *config.Config, brn *brain.Brain, aidaBin string, store *jobs.Store, profileName string, o loopOpts, state *loopState, conc int) error {
	rt, err := newArbiterRuntime(cfg, o, profileName, cfg.BrainPath())
	if err != nil {
		return fmt.Errorf("arbiter: %w", err)
	}

	fmt.Printf("aida loop --arbiter - profile=%s tags=%v lanes=%d lease-ttl=%s worktree=%v concurrency=%d daemon=%v\n",
		profileName, o.tags, len(rt.lanes.Sorted()), o.leaseTTL, o.worktree, conc, o.daemon)

	round := 0
	for {
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

		if cfg.Brain.AutoSync {
			status, ffErr := brain.FetchAndFastForward(rt.brainPath, 10*time.Second)
			switch status {
			case brain.FFRemoteAhead:
				msg := fmt.Sprintf("brain repo can't fast-forward (%v) - resolve with `git -C %s status`", ffErr, rt.brainPath)
				if o.daemon {
					fmt.Println(msg + "; waiting")
					if waitOrDone(ctx, o.poll) {
						return ctx.Err()
					}
					continue
				}
				return fmt.Errorf("%s", msg)
			case brain.FFOffline:
				fmt.Printf("brain repo fetch failed (offline?): %v - continuing with the local view\n", ffErr)
			}
		}

		candidates, err := pickBatch(brn, o.tags, 3*conc)
		if err != nil {
			return fmt.Errorf("pick tasks: %w", err)
		}
		// Overnight scheduler wave allowlist (serve_arbiter.go): when set,
		// only the wave's own slugs are eligible this round, so a scheduled
		// wave never picks up a task BuildWave already skipped for lacking
		// a machine verifier.
		if len(o.slugs) > 0 {
			candidates = filterTasksBySlugs(candidates, o.slugs)
		}
		if len(candidates) == 0 {
			if o.daemon {
				if waitOrDone(ctx, o.poll) {
					return ctx.Err()
				}
				continue
			}
			fmt.Println("\nAll tasks in the set are terminal. <promise>COMPLETE</promise>")
			return nil
		}

		// Checked before claimTasks (which pushes real lease commits) so a
		// dry run never leaves a dangling lease behind - unlike pickBatch,
		// claiming is not read-only.
		if dryRunGuard("work tasks via the arbiter", fmt.Sprintf("%d candidate task(s)", len(candidates))) {
			printDryRunPicks(ctx, rt, candidates, conc)
			fmt.Println("  [dry-run] would claim + spawn on the cheapest eligible lane + run the code gate")
			return nil
		}

		batch, leases := claimTasks(ctx, brn, rt, candidates, conc)
		if len(batch) == 0 {
			fmt.Println("  every candidate task is leased elsewhere or contended")
			if o.daemon {
				if waitOrDone(ctx, o.poll) {
					return ctx.Err()
				}
				continue
			}
			fmt.Println("\nNo claimable task in the set right now.")
			return nil
		}
		round++

		fmt.Printf("\n=== round %d (%d task(s), spent $%.2f) ===\n", round, len(batch), state.spent())
		for i := range batch {
			fmt.Printf("  • #%d [%s] %s\n", batch[i].TaskID, batch[i].PriorityTag(), batch[i].Title)
		}

		outcomes := make([]taskOutcome, len(batch))
		var wg sync.WaitGroup
		for i := range batch {
			t := &batch[i]
			lease := leases[t.Slug]
			wg.Add(1)
			go func(idx int, task *brain.TaskRecord, lease *taskstate.Lease) {
				defer wg.Done()
				outcomes[idx] = o.processArbiterTask(ctx, rt, brn, store, aidaBin, profileName, task, lease, state)
			}(i, t, lease)
		}
		wg.Wait()

		allNoLane := true
		for _, oc := range outcomes {
			if oc != outcomeNoLane {
				allNoLane = false
				break
			}
		}
		if allNoLane {
			if o.daemon {
				fmt.Printf("  no lane has headroom above its floor; waiting %s\n", o.noLaneWait)
				if waitOrDone(ctx, o.noLaneWait) {
					return ctx.Err()
				}
				continue
			}
			fmt.Println("\nno lane has headroom above its floor; suggest a lever, never picks new spend")
			return nil
		}
	}
}

// waitOrDone sleeps for d unless ctx is cancelled first, returning true when
// ctx won the race (the caller should return ctx.Err()).
func waitOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return true
	case <-time.After(d):
		return false
	}
}

// claimTasks walks candidates in priority order and claims up to conc of
// them, pre-filtering a candidate whose frontmatter mirror shows a live,
// not-ours lease without a round trip, then trying taskstate.Claimer.Claim
// for the rest. ErrHeld/ErrContended are the expected "someone else is
// racing or holding this one" outcomes (ADR-0001, decision 3) - the loser
// just moves to the next candidate, never treated as an error. A won lease
// is mirrored onto the task's frontmatter (claimed_by/lease_until) best
// effort, for humans reading `tasks/`.
func claimTasks(ctx context.Context, brn *brain.Brain, rt *arbiterRuntime, candidates []brain.TaskRecord, conc int) ([]brain.TaskRecord, map[string]*taskstate.Lease) {
	claimed := make([]brain.TaskRecord, 0, conc)
	leases := make(map[string]*taskstate.Lease, conc)

	for _, t := range candidates {
		if len(claimed) >= conc {
			break
		}
		if t.ClaimedBy != "" && t.ClaimedBy != rt.claimer.Claimant {
			if until, perr := time.Parse(time.RFC3339, t.LeaseUntil); perr == nil && until.After(time.Now()) {
				fmt.Printf("  skip #%d %s: leased by %s until %s\n", t.TaskID, t.Slug, t.ClaimedBy, t.LeaseUntil)
				continue
			}
		}

		lease, err := rt.claimer.Claim(ctx, t.Slug)
		if err != nil {
			if errors.Is(err, taskstate.ErrHeld) || errors.Is(err, taskstate.ErrContended) {
				fmt.Printf("  skip #%d %s: %s\n", t.TaskID, t.Slug, err)
			} else {
				fmt.Printf("  skip #%d %s: claim error: %s\n", t.TaskID, t.Slug, err)
			}
			continue
		}

		claimedBy := lease.ClaimedBy
		until := lease.Until.UTC().Format(time.RFC3339)
		if _, uerr := brn.UpdateTask(t.Slug, brain.TaskPatch{ClaimedBy: &claimedBy, LeaseUntil: &until}); uerr != nil {
			ui.PrintVerbose("loop-arbiter", "mirror lease onto task failed: "+uerr.Error())
		}
		claimed = append(claimed, t)
		leases[t.Slug] = lease
	}
	return claimed, leases
}

// leaseBox guards a *taskstate.Lease shared between processArbiterTask's
// defer (release on exit) and its background renewal goroutine, both of
// which read or replace it concurrently.
type leaseBox struct {
	mu sync.Mutex
	l  *taskstate.Lease
}

func (b *leaseBox) get() *taskstate.Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.l
}

func (b *leaseBox) set(l *taskstate.Lease) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.l = l
}

// processArbiterTask runs one claimed task to a terminal outcome under
// --arbiter: STATE.json tracks phase transitions, every attempt re-consults
// the capacity view for a lane (arbiter.Pick), a lane switch resumes from a
// validated HANDOFF.md instead of restarting (ADR-0004), and a lane reporting
// itself empty (decision 2, fail open) moves to the next lane without
// consuming a fix attempt. The lease is renewed every TTL/2 in the
// background and always released on return.
func (o loopOpts) processArbiterTask(ctx context.Context, rt *arbiterRuntime, brn *brain.Brain, store *jobs.Store, aidaBin, profileName string, task *brain.TaskRecord, lease *taskstate.Lease, state *loopState) taskOutcome {
	slug := task.Slug

	box := &leaseBox{l: lease}
	defer func() {
		cur := box.get()
		if cur == nil {
			return
		}
		relCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := rt.claimer.Release(relCtx, cur); err != nil && !errors.Is(err, taskstate.ErrLost) {
			ui.PrintVerbose("loop-arbiter", "release lease failed: "+err.Error())
		}
		empty := ""
		if _, err := brn.UpdateTask(slug, brain.TaskPatch{ClaimedBy: &empty, LeaseUntil: &empty}); err != nil {
			ui.PrintVerbose("loop-arbiter", "clear lease mirror failed: "+err.Error())
		}
	}()

	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	lostCh := make(chan struct{}, 1)
	go func() {
		interval := rt.claimer.TTL / 2
		if interval <= 0 {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				cur := box.get()
				if cur == nil {
					return
				}
				renewed, err := rt.claimer.Renew(renewCtx, cur)
				if err != nil {
					if errors.Is(err, taskstate.ErrLost) {
						box.set(nil)
						select {
						case lostCh <- struct{}{}:
						default:
						}
						return
					}
					ui.PrintVerbose("loop-arbiter", "renew lease failed: "+err.Error())
					continue
				}
				box.set(renewed)
			}
		}
	}()

	// STATE.json: harness-only, never written to by a model (taskstate
	// package doc comment, decision 1). A fresh task gets Acceptance from
	// --check plus any Deliverables heading in the task body.
	st, err := taskstate.Load(rt.brainPath, slug)
	if err != nil {
		if !errors.Is(err, taskstate.ErrNoState) {
			fmt.Printf("  ✗ #%d %s: load state failed: %s - parking on hold\n", task.TaskID, slug, err)
			parkHold(brn, task)
			return outcomeHold
		}
		body, _ := brn.TaskBody(slug)
		st = &taskstate.State{
			Slug:    slug,
			TaskID:  task.TaskID,
			Phase:   taskstate.PhaseClaimed,
			Started: rt.now(),
			Host:    rt.host,
			Acceptance: taskstate.Acceptance{
				Checks:       o.checks,
				Deliverables: taskstate.ParseDeliverables(body),
			},
		}
	} else {
		st.Phase = taskstate.PhaseClaimed
		st.Host = rt.host
	}
	if err := taskstate.Save(rt.brainPath, st); err != nil {
		ui.PrintVerbose("loop-arbiter", "save state failed: "+err.Error())
	}
	_ = taskstate.AppendLog(rt.brainPath, slug, fmt.Sprintf("claimed by %s", rt.claimer.Claimant))
	if rt.autoSync {
		brain.CommitAndPush(rt.brainPath, profileName)
	}

	workDir := ""
	var wt *worktree.Worktree
	if o.worktree {
		w, werr := o.setupTaskWorktree(profileName, task)
		if werr != nil {
			fmt.Printf("  ✗ worktree setup failed: %s - parking on hold\n", werr)
			return o.parkArbiterHold(rt, brn, st, task, profileName, state)
		}
		wt = w
		workDir = wt.Dir
		defer func() {
			if rerr := wt.Remove(); rerr != nil {
				ui.PrintVerbose("loop-arbiter", "worktree remove failed: "+rerr.Error())
			}
		}()
	}

	preamble := loopRecall(ctx, brn, task, o.recallK)
	checks := codeChecksFrom(o.checks)

	maxFix := o.maxFix
	if maxFix < 1 {
		maxFix = 1
	}
	maxLanes := len(rt.lanes.Sorted())
	if maxLanes < 1 {
		maxLanes = 1
	}

	var lastRunID string
	var lastIssues []eval.Issue
	handoffProblem := ""
	startedWork := false
	passed := false
	attempt := 0
	emptyLoops := 0

	for attempt < maxFix {
		select {
		case <-lostCh:
			fmt.Printf("  ✗ #%d %s: lease lost mid-task, another host took over\n", task.TaskID, slug)
			_ = taskstate.AppendLog(rt.brainPath, slug, "lease lost mid-task; another host took over")
			return o.parkArbiterHold(rt, brn, st, task, profileName, state)
		default:
		}
		if reason := state.stopReason(); reason != "" {
			fmt.Printf("  stopping fix attempts: %s\n", reason)
			break
		}

		caps, capErr := rt.capacity.Snapshot(ctx)
		if capErr != nil {
			ui.PrintVerbose("loop-arbiter", "capacity snapshot failed: "+capErr.Error())
			caps = nil
		}
		decision, pickErr := arbiter.Pick(rt.lanes, arbiter.Request{
			TaskSlug: slug, TaskID: task.TaskID, Tags: task.Tags, Role: arbiter.RoleExecutor, Now: rt.now(),
		}, caps, rt.signals)
		if pickErr != nil {
			for _, rej := range decision.Rejected {
				fmt.Printf("  lane %s: %s\n", rej.Lane, rej.Reason)
			}
			_ = taskstate.AppendLog(rt.brainPath, slug, "no eligible lane: "+pickErr.Error())
			return outcomeNoLane
		}

		if !startedWork {
			if _, serr := brn.SetTaskStatus(slug, brain.StatusInProgress); serr != nil {
				ui.PrintVerbose("loop-arbiter", "set in-progress failed: "+serr.Error())
			}
			startedWork = true
		}

		rt.claimer.Lane = decision.Lane.ID
		prevLane := st.Lane
		prevAttempt := st.Attempt
		firstAttempt := st.Attempt == 0
		st.BeginAttempt(decision.Lane.ID, decision.Model, rt.host, rt.now())
		if serr := taskstate.Save(rt.brainPath, st); serr != nil {
			ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
		}
		fmt.Printf("  lane=%s model=%s class=%s (headroom %.0f%%)\n", decision.Lane.ID, decision.Model, decision.Class, minLaneHeadroom(decision.Capacity))

		prompt, armed, resumed := o.buildArbiterPrompt(rt, brn, st, task, preamble, decision, prevLane, prevAttempt, workDir, firstAttempt, lastIssues, handoffProblem)
		if resumed {
			st.Phase = taskstate.PhaseResumed
			if serr := taskstate.Save(rt.brainPath, st); serr != nil {
				ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
			}
		}

		capsBefore := caps
		startedAt := time.Now()

		var runID string
		var lanered bool // true once we've decided this lane reported empty
		if decision.Lane.Runner == arbiter.RunnerAidaAgent {
			rid, rerr := spawnLoopAgent(ctx, store, aidaBin, profileName, task, prompt, o.perTask, workDir, o.sbxTier, o.sandboxMemory, o.linuxAidaBin)
			runID = rid
			if rerr != nil {
				fmt.Printf("  ✗ agent failed: %s\n", rerr)
				capsAfter, _ := rt.capacity.Snapshot(ctx)
				appendLedger(rt, decision, task, runID, attempt+1, time.Since(startedAt).Milliseconds(), "error", readRunCostUSD(profileName, runID), capsBefore, capsAfter, rerr.Error())
				attempt = maxFix // stop the fix-attempt loop, fall through to hold
				break
			}
		} else {
			job, jerr := store.Enqueue("loop", task.Title, "", prompt, "")
			if jerr != nil {
				fmt.Printf("  ✗ enqueue failed: %s\n", jerr)
				break
			}
			runID = job.RunID
			if gerr := store.SetGovernance(runID, jobs.Governance{TimeoutSec: int(o.perTask.Seconds())}); gerr != nil {
				ui.PrintVerbose("loop-arbiter", "set governance failed: "+gerr.Error())
			}
			spec := arbiter.RunSpec{
				Lane: *decision.Lane, Model: decision.Model, Prompt: prompt, Dir: workDir,
				Timeout: o.perTask, OutputPath: jobs.OutputPath(profileName, runID), Env: os.Environ(),
			}
			rr, rerr := rt.runner.Run(ctx, spec)
			if rerr != nil {
				fmt.Printf("  ✗ lane run failed: %s\n", rerr)
				_ = store.Fail(runID, rerr.Error())
				capsAfter, _ := rt.capacity.Snapshot(ctx)
				appendLedger(rt, decision, task, runID, attempt+1, time.Since(startedAt).Milliseconds(), "error", readRunCostUSD(profileName, runID), capsBefore, capsAfter, rerr.Error())
				break
			}
			if rr.TimedOut {
				_ = store.Fail(runID, "timed out")
			} else if verr := store.Complete(runID); verr != nil {
				ui.PrintVerbose("loop-arbiter", "job complete failed: "+verr.Error())
			}
			if werr := os.WriteFile(jobs.OutputPath(profileName, runID), []byte(rr.Stdout), 0o644); werr != nil {
				ui.PrintVerbose("loop-arbiter", "write output failed: "+werr.Error())
			}

			if rr.Verdict == arbiter.VerdictEmpty || rr.Verdict == arbiter.VerdictAmbiguous {
				lanered = true
				sig := arbiter.Signal{
					At: rt.now(), Lane: decision.Lane.ID, Source: "runner",
					Verdict: rr.Verdict, Ambiguous: rr.Verdict == arbiter.VerdictAmbiguous,
					Matched:    rr.Matched,
					EmptyUntil: arbiter.ResolveEmptyUntil(rt.now(), earliestResetsAt(decision.Capacity.Windows), arbiter.DefaultEmptyFallback),
					Raw:        tailString(rr.Stderr+rr.Stdout, 2000),
					Host:       rt.host,
				}
				if serr := rt.signals.Record(sig); serr != nil {
					ui.PrintVerbose("loop-arbiter", "record signal failed: "+serr.Error())
				}
				verdictLabel := "lane-empty"
				if sig.Ambiguous {
					verdictLabel = "lane-empty-ambiguous"
				}
				fmt.Printf("  lane %s signalled empty (ambiguous=%v) until %s\n", decision.Lane.ID, sig.Ambiguous, sig.EmptyUntil.Format(time.RFC3339))
				_ = taskstate.AppendLog(rt.brainPath, slug, fmt.Sprintf("lane %s signalled empty (ambiguous=%v)", decision.Lane.ID, sig.Ambiguous))
				capsAfter, _ := rt.capacity.Snapshot(ctx)
				appendLedger(rt, decision, task, runID, attempt+1, time.Since(startedAt).Milliseconds(), verdictLabel, readRunCostUSD(profileName, runID), capsBefore, capsAfter, "")
			}
		}
		if lanered {
			emptyLoops++
			if emptyLoops >= maxLanes {
				return outcomeNoLane
			}
			continue // do not consume a fix attempt on a lane-empty signal
		}

		lastRunID = runID
		cost := readRunCostUSD(profileName, runID)
		state.addCost(cost)

		// Hand-off check: every attempt while armed, and every resumed
		// attempt (ADR-0004 - "the check runs every attempt while armed,
		// not once").
		handoffProblem = ""
		if armed || resumed {
			h, herr := taskstate.LoadHandoff(rt.brainPath, slug)
			var cerr error
			if herr != nil {
				cerr = herr
			} else {
				cerr = taskstate.Check(h, st, currentGitHead(workDir), rt.now(), o.handoffMaxAge)
			}
			if cerr != nil {
				label := classifyHandoffProblem(cerr)
				fmt.Printf("  ✗ HANDOFF.md %s: %s\n", label, cerr)
				_ = taskstate.AppendLog(rt.brainPath, slug, "HANDOFF.md "+label+": "+cerr.Error())
				handoffProblem = label
			} else {
				st.HandoffWrittenAt = h.Written
				st.HandoffAttempt = h.Attempt
			}
			st.Phase = taskstate.PhaseHandoff
			if serr := taskstate.Save(rt.brainPath, st); serr != nil {
				ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
			}
			if rt.autoSync {
				brain.CommitAndPush(rt.brainPath, profileName)
			}
		}

		st.Phase = taskstate.PhaseGate
		if serr := taskstate.Save(rt.brainPath, st); serr != nil {
			ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
		}

		records := runCodeGate(ctx, workDir, checks)
		persistLoopEvalRun(brn, runID, task, profileName, records)
		problems := taskstate.VerifyDeliverables(workDir, st.Acceptance.Deliverables)
		for _, p := range problems {
			records = append(records, eval.ReviewRecord{
				Reviewer: "deliverables",
				Verdict:  eval.VerdictFail,
				Issues: []eval.Issue{{
					Type:     "missing-deliverable",
					Severity: "error",
					Message:  fmt.Sprintf("%s: %s", p.Deliverable, p.Reason),
				}},
			})
		}
		gateVerdict := eval.AggregateVerdict(records)
		st.LastVerdict = string(gateVerdict)
		if serr := taskstate.Save(rt.brainPath, st); serr != nil {
			ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
		}

		capsAfter, _ := rt.capacity.Snapshot(ctx)
		appendLedger(rt, decision, task, runID, attempt+1, time.Since(startedAt).Milliseconds(), string(gateVerdict), cost, capsBefore, capsAfter, handoffProblem)

		attempt++
		if gateVerdict != eval.VerdictFail {
			passed = true
			break
		}
		lastIssues = collectIssues(records)
		fmt.Printf("  ✗ gate failed (attempt %d/%d): %d issue(s)\n", attempt, maxFix, len(lastIssues))
	}

	if !passed {
		fmt.Println("  parking task on hold (gate not green / agent failed)")
		return o.parkArbiterHold(rt, brn, st, task, profileName, state)
	}

	state.recordOutcome(true)
	st.Phase = taskstate.PhaseDone
	if serr := taskstate.Save(rt.brainPath, st); serr != nil {
		ui.PrintVerbose("loop-arbiter", "save state failed: "+serr.Error())
	}
	if rt.autoSync {
		brain.CommitAndPush(rt.brainPath, profileName)
	}
	o.finishPassedTask(ctx, brn, store, aidaBin, profileName, task, wt, workDir, lastRunID, state)
	return outcomeDone
}

// parkArbiterHold saves STATE.json's hold phase, pushes it (best effort),
// and parks the task the same way the plain loop does. Shared by every
// "give up on this task" exit path in processArbiterTask other than
// outcomeNoLane, which leaves the task open instead (see the type's doc
// comment).
func (o loopOpts) parkArbiterHold(rt *arbiterRuntime, brn *brain.Brain, st *taskstate.State, task *brain.TaskRecord, profileName string, state *loopState) taskOutcome {
	state.recordOutcome(false)
	st.Phase = taskstate.PhaseHold
	if err := taskstate.Save(rt.brainPath, st); err != nil {
		ui.PrintVerbose("loop-arbiter", "save state failed: "+err.Error())
	}
	if rt.autoSync {
		brain.CommitAndPush(rt.brainPath, profileName)
	}
	parkHold(brn, task)
	return outcomeHold
}

// buildArbiterPrompt composes the prompt for one attempt under --arbiter,
// applying ADR-0004's hand-off precedence: a lane switch resumes from a
// validated HANDOFF.md (or gets ReconstructPrompt when it is missing or
// stale), otherwise the standard first-attempt or fix prompt applies. When
// the picked lane is near exhaustion (decision.NearExhaustion), an arming
// block is prepended asking the model to write or refresh HANDOFF.md before
// anything else. Returns the prompt, whether this attempt is armed, and
// whether it is a lane-switch resume (both drive the post-attempt hand-off
// check in the caller).
func (o loopOpts) buildArbiterPrompt(rt *arbiterRuntime, brn *brain.Brain, st *taskstate.State, task *brain.TaskRecord, preamble string, decision arbiter.Decision, prevLane string, prevAttempt int, workDir string, firstAttempt bool, lastIssues []eval.Issue, prevHandoffProblem string) (prompt string, armed bool, resumed bool) {
	switch {
	case prevLane != "" && decision.Lane.ID != prevLane:
		resumed = true
		h, herr := taskstate.LoadHandoff(rt.brainPath, task.Slug)
		var cerr error
		if herr != nil {
			cerr = herr
		} else {
			// BeginAttempt has already advanced st.Attempt for the attempt
			// about to run, but the hand-off we are resuming from was
			// written during the PREVIOUS attempt on the old lane, so the
			// staleness rule "an attempt ran after it was written" must be
			// judged against prevAttempt. Comparing against st.Attempt here
			// would flag every correctly written hand-off as stale and turn
			// every resume into a reconstruct, defeating decision 1.
			asOf := *st
			asOf.Attempt = prevAttempt
			cerr = taskstate.Check(h, &asOf, currentGitHead(workDir), rt.now(), o.handoffMaxAge)
		}
		if cerr == nil {
			prompt = taskstate.ResumePrompt(h, st)
		} else {
			fmt.Printf("  ✗ hand-off unusable on lane switch: %s\n", cerr)
			_ = taskstate.AppendLog(rt.brainPath, task.Slug, "hand-off unusable on lane switch: "+cerr.Error())
			prompt = taskstate.ReconstructPrompt(task.TaskID, task.Slug, taskstate.HandoffPath(rt.brainPath, task.Slug), cerr.Error())
		}
	case firstAttempt:
		prompt = buildLoopPrompt(task, preamble, o.checks)
	default:
		prompt = buildFixPrompt(task, lastIssues)
	}

	armed = decision.NearExhaustion
	if armed {
		path := taskstate.HandoffPath(rt.brainPath, task.Slug)
		body, _ := brn.TaskBody(task.Slug)
		tmpl := taskstate.Template(task.TaskID, task.Slug, decision.Lane.ID, st.Attempt, task.Title, body, st.Acceptance)
		arm := fmt.Sprintf("Your lane (%s) is at %.0f%% of its quota. Before anything else, write HANDOFF.md at %s using exactly this template (fill Done, Left, Tried and rejected):\n\n%s\n\nThen continue the task. Refresh it before you finish.\n\n",
			decision.Lane.ID, decision.Capacity.MaxUsedPct, path, tmpl)
		if prevHandoffProblem != "" {
			arm += fmt.Sprintf("The previous hand-off was %s; rewrite it.\n\n", prevHandoffProblem)
		}
		prompt = arm + prompt
	}
	prompt += fmt.Sprintf("\n\n(Hand-off file: %s - you may write or refresh it at any time.)\n", taskstate.HandoffPath(rt.brainPath, task.Slug))
	return prompt, armed, resumed
}

// classifyHandoffProblem turns a taskstate.Check/LoadHandoff error into the
// short label used in log lines and the ledger Note.
func classifyHandoffProblem(err error) string {
	switch {
	case errors.Is(err, taskstate.ErrMissing):
		return "missing"
	case errors.Is(err, taskstate.ErrMalformed):
		return "malformed"
	case errors.Is(err, taskstate.ErrWrongTask):
		return "wrong-task"
	case taskstate.IsStale(err):
		return "stale"
	default:
		return "invalid"
	}
}

// currentGitHead returns dir's current HEAD sha, or "" when it can't be
// determined (no worktree, not a git repo, git not on PATH) - taskstate.Check
// treats an empty currentHead as "unknown", never as a match.
func currentGitHead(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// minLaneHeadroom returns the smallest Headroom across a LaneCapacity's
// matched windows (0 for an unprobed lane with no windows at all), for the
// "(headroom N%)" line printed when a lane is picked.
func minLaneHeadroom(lc arbiter.LaneCapacity) float64 {
	if len(lc.Windows) == 0 {
		return 0
	}
	min := lc.Windows[0].Headroom
	for _, w := range lc.Windows[1:] {
		if w.Headroom < min {
			min = w.Headroom
		}
	}
	return min
}

// earliestResetsAt returns the earliest non-zero ResetsAt among windows, or
// the zero time when none is known - ResolveEmptyUntil falls back to
// DefaultEmptyFallback from now in that case.
func earliestResetsAt(windows []burndown.Capacity) time.Time {
	var earliest time.Time
	for _, w := range windows {
		if w.ResetsAt.IsZero() {
			continue
		}
		if earliest.IsZero() || w.ResetsAt.Before(earliest) {
			earliest = w.ResetsAt
		}
	}
	return earliest
}

// tailString returns the last n bytes of s (all of s when shorter), for
// carrying a bounded snippet of a run's output into a Signal.Raw field
// without unbounded log growth.
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// appendLedger records one arbiter.Entry for an attempt. A write failure is
// printed, never fatal (plan section 7: the ledger is diagnostic history).
func appendLedger(rt *arbiterRuntime, decision arbiter.Decision, task *brain.TaskRecord, runID string, attempt int, wallMS int64, verdict string, cost float64, before, after []burndown.Capacity, note string) {
	if decision.Lane == nil {
		return
	}
	e := arbiter.Entry{
		At:       rt.now(),
		TaskSlug: task.Slug,
		TaskID:   task.TaskID,
		Lane:     decision.Lane.ID,
		Model:    decision.Model,
		Role:     arbiter.RoleExecutor,
		Host:     rt.host,
		RunID:    runID,
		Attempt:  attempt,
		WallMS:   wallMS,
		Verdict:  verdict,
		CostUSD:  cost,
		Before:   arbiter.SnapshotFor(*decision.Lane, before),
		After:    arbiter.SnapshotFor(*decision.Lane, after),
		Note:     note,
	}
	if err := arbiter.AppendLedger(rt.ledgerPath, e); err != nil {
		ui.PrintVerbose("loop-arbiter", "append ledger failed: "+err.Error())
	}
}

// printDryRunPicks shows what a real round would do with the candidates:
// the first conc tasks a claim would take and, for each, the lane and
// model the picker resolves against the live capacity snapshot. Read-only
// by construction - it calls Pick, never Claim, never a runner. Without
// this a dry run only said "3 candidate task(s)", which answers "how
// many" but not "which, and where", the two things a dry run exists to
// show.
func printDryRunPicks(ctx context.Context, rt *arbiterRuntime, candidates []brain.TaskRecord, conc int) {
	caps, err := rt.capacity.Snapshot(ctx)
	if err != nil {
		fmt.Printf("  [dry-run] capacity snapshot failed: %v (only allow_unprobed lanes would be eligible)\n", err)
		caps = nil
	}
	shown := 0
	for i := range candidates {
		t := &candidates[i]
		if shown >= conc {
			break
		}
		if t.ClaimedBy != "" {
			if until, perr := time.Parse(time.RFC3339, t.LeaseUntil); perr == nil && until.After(rt.now()) {
				fmt.Printf("  [dry-run] skip #%d %s: leased by %s until %s\n", t.TaskID, t.Slug, t.ClaimedBy, t.LeaseUntil)
				continue
			}
		}
		shown++
		d, perr := arbiter.Pick(rt.lanes, arbiter.Request{TaskSlug: t.Slug, TaskID: t.TaskID, Tags: t.Tags, Role: arbiter.RoleExecutor, Now: rt.now()}, caps, rt.signals)
		switch {
		case perr != nil && errors.Is(perr, arbiter.ErrNoLane):
			fmt.Printf("  [dry-run] #%d [%s] %s -> no lane (class %s)\n", t.TaskID, t.PriorityTag(), t.Title, d.Class)
			for _, rej := range d.Rejected {
				fmt.Printf("             %s: %s\n", rej.Lane, rej.Reason)
			}
		case perr != nil:
			fmt.Printf("  [dry-run] #%d [%s] %s -> %v\n", t.TaskID, t.PriorityTag(), t.Title, perr)
		default:
			near := ""
			if d.NearExhaustion {
				near = " (near exhaustion: HANDOFF.md would be armed)"
			}
			fmt.Printf("  [dry-run] #%d [%s] %s -> lane=%s model=%s class=%s%s\n", t.TaskID, t.PriorityTag(), t.Title, d.Lane.ID, d.Model, d.Class, near)
		}
	}
}
