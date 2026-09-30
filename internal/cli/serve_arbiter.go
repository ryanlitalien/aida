package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis/notify"
	"github.com/ryanlitalien/aida/internal/taskstate"
)

// serve_arbiter.go is `aida serve --arbiter`'s scheduler half
// (docs/arbiter-plan.md section 4): unlike `aida loop --arbiter`'s
// always-on-when-there's-work dispatcher, the scheduler is opt-in, sleeps
// outside the overnight window and away from a weekly reset, and drives
// exactly one wave at a time through the existing arbiter-enforced loop
// (runLoopCtx with loopOpts.arbiter, loop_arbiter.go).
//
// The policy decisions (when to run, when to wake, which tasks belong in
// a wave, how to summarize a finished one) all live in internal/arbiter's
// schedule.go and preflight.go, kept IO-free and independently tested.
// This file is the IO shell around that policy: it probes capacity,
// reads/writes the scheduler's two state files, opens the brain to list
// candidates, and hands the decided wave to runLoopCtx.

// arbiterServeOpts configures `aida serve --arbiter`. Mirrors
// harvestSweepOpts and loopOpts's own opts-struct-plus-validate
// convention so newServeCmd's RunE can fail eagerly on a bad
// configuration, exactly like it already does for --loop and
// --harvest-sweep.
type arbiterServeOpts struct {
	enabled bool
	tags    []string
	checks  []string

	nightlyAt        string
	weeklyDrain      time.Duration
	requireVerifier  bool
	worktree         bool
	pr               bool
	reviewer         string
	concurrency      int
	preflightTimeout time.Duration
}

// defaultArbiterServeOpts returns the scheduler's defaults. --arbiter
// itself defaults to false (enabled starts unset) - the scheduler is
// opt-in, unlike --loop-arbiter's enforcement-inside-the-plain-loop,
// which defaults on. Everything else defaults to a safe, unattended-wave
// shape: a verifier is required, tasks run in isolated worktrees, and no
// PR/reviewer/wider concurrency unless asked for.
func defaultArbiterServeOpts() arbiterServeOpts {
	return arbiterServeOpts{
		nightlyAt:        "23:00",
		weeklyDrain:      24 * time.Hour,
		requireVerifier:  true,
		worktree:         true,
		pr:               false,
		concurrency:      1,
		preflightTimeout: 60 * time.Second,
	}
}

// errArbiterLoopConflict is returned when both --arbiter and --loop are
// requested on the same `aida serve` invocation - the plan is explicit
// that the scheduler and the plain daemon-hosted loop must never dispatch
// over the same task set at once (docs/arbiter-plan.md section 8 step
// 4's build brief). Run one or the other; running the arbiter scheduler
// alone still enforces lane choice per-attempt via loopOpts.arbiter, so
// nothing is lost by picking it over --loop.
var errArbiterLoopConflict = errors.New("aida serve --loop and the arbiter must never dispatch over the same task at once; run one or the other")

// parseHHMM parses a "HH:MM" 24-hour clock string - a local twin of
// internal/arbiter's own unexported parseHHMM (schedule.go), which in
// turn twins internal/burndown's unexported one; none of the three
// packages export it, so each small validation-only use gets its own
// copy rather than a cross-package dependency for three lines of code.
func parseHHMM(s string) (hour, min int, ok bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// validate checks arbiterServeOpts in isolation - the nightly time
// parses, the weekly-drain window is positive, and concurrency is at
// least 1. Note that --pr implying --worktree is a normalization (like
// loopOpts's own --pr/--worktree relationship), not a validation error -
// runArbiterScheduler's loopOpts builder sets worktree=true whenever pr
// is true, mirroring runLoopCtx's own "if o.pr { o.worktree = true }"
// after validate() runs.
func (o arbiterServeOpts) validate() error {
	if _, _, ok := parseHHMM(o.nightlyAt); !ok {
		return fmt.Errorf("--arbiter-nightly-at %q must be an HH:MM 24-hour time", o.nightlyAt)
	}
	if o.weeklyDrain <= 0 {
		return fmt.Errorf("--arbiter-weekly-drain must be > 0 (got %s)", o.weeklyDrain)
	}
	if o.concurrency < 1 {
		return fmt.Errorf("--arbiter-concurrency must be >= 1 (got %d)", o.concurrency)
	}
	return nil
}

// validateArbiterServeConfig is newServeCmd's RunE-time gate: arbiterO's
// own validate() plus the cross-flag --arbiter/--loop exclusivity check,
// factored out so it's testable without building a *cobra.Command.
func validateArbiterServeConfig(o arbiterServeOpts, loopEnabled bool) error {
	if err := o.validate(); err != nil {
		return err
	}
	if o.enabled && loopEnabled {
		return errArbiterLoopConflict
	}
	return nil
}

// filterTasksBySlugs keeps only the candidates whose Slug appears in
// slugs, preserving candidates' order. Used by runArbiterLoop
// (loop_arbiter.go) to restrict a round to exactly the scheduler's
// already-decided wave (loopOpts.slugs) instead of re-deriving a superset
// from tags alone.
func filterTasksBySlugs(candidates []brain.TaskRecord, slugs []string) []brain.TaskRecord {
	if len(slugs) == 0 {
		return candidates
	}
	allow := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		allow[s] = true
	}
	out := make([]brain.TaskRecord, 0, len(candidates))
	for _, c := range candidates {
		if allow[c.Slug] {
			out = append(out, c)
		}
	}
	return out
}

// schedulerState is the shape of <stateDir>/scheduler.json - the
// dashboard card and `aida arbiter status` both read it, so it's plain
// exported-field JSON rather than anything internal to the scheduler
// loop.
type schedulerState struct {
	State    string    `json:"state"` // "sleeping" | "running" | "idle"
	Started  time.Time `json:"started,omitempty"`
	NextWake time.Time `json:"next_wake,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Updated  time.Time `json:"updated"`
}

func schedulerStatePath(dir string) string  { return filepath.Join(dir, "scheduler.json") }
func waveSummaryPath(dir string) string     { return filepath.Join(dir, "last-wave.json") }
func schedulerLedgerPath(dir string) string { return filepath.Join(dir, "ledger.ndjson") }

// writeJSONAtomic marshals v and writes it to path via a temp-file-plus-
// rename, the same pattern arbiter.Store.saveState uses, so a reader
// (the dashboard, `aida arbiter status`) never observes a half-written
// file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeSchedulerState(dir string, s schedulerState) error {
	if s.Updated.IsZero() {
		s.Updated = time.Now()
	}
	return writeJSONAtomic(schedulerStatePath(dir), s)
}

// readSchedulerState reads scheduler.json, returning the zero value (not
// an error) when it doesn't exist yet - the scheduler hasn't completed
// its first loop iteration, which is a normal state for `aida arbiter
// status`/the dashboard to render as "no data yet" rather than fail on.
func readSchedulerState(dir string) (schedulerState, error) {
	data, err := os.ReadFile(schedulerStatePath(dir))
	if os.IsNotExist(err) {
		return schedulerState{}, nil
	}
	if err != nil {
		return schedulerState{}, err
	}
	var s schedulerState
	if err := json.Unmarshal(data, &s); err != nil {
		return schedulerState{}, err
	}
	return s, nil
}

func writeWaveSummary(dir string, s arbiter.WaveSummary) error {
	return writeJSONAtomic(waveSummaryPath(dir), s)
}

// readWaveSummary reads last-wave.json. ok is false (not an error) when
// no wave has ever run on this box - the same "no data yet" convention
// readSchedulerState follows.
func readWaveSummary(dir string) (s arbiter.WaveSummary, ok bool, err error) {
	data, rerr := os.ReadFile(waveSummaryPath(dir))
	if os.IsNotExist(rerr) {
		return arbiter.WaveSummary{}, false, nil
	}
	if rerr != nil {
		return arbiter.WaveSummary{}, false, rerr
	}
	if uerr := json.Unmarshal(data, &s); uerr != nil {
		return arbiter.WaveSummary{}, false, uerr
	}
	return s, true, nil
}

// laneMarkView is one lane's current empty mark, read directly from
// arbiter's lane-state.json (its Store type keeps the load/save
// unexported - Store.EmptyUntil answers "is this ONE lane marked" but
// exposes no "list every mark" method, and this package's build brief
// scopes edits to signals.go out of this unit). The JSON shape mirrors
// the unexported laneStateEntry in signals.go field for field, since it's
// the same file.
type laneMarkView struct {
	Lane       string    `json:"lane"`
	EmptyUntil time.Time `json:"empty_until"`
	Since      time.Time `json:"since"`
	Source     string    `json:"source"`
	Ambiguous  bool      `json:"ambiguous"`
}

// readLaneMarks reads every current lane mark from
// <dir>/lane-state.json, sorted by lane name for deterministic output.
// A missing file (no lane has ever been marked) returns an empty slice,
// not an error.
func readLaneMarks(dir string) ([]laneMarkView, error) {
	data, err := os.ReadFile(filepath.Join(dir, "lane-state.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var raw map[string]struct {
		EmptyUntil time.Time `json:"empty_until"`
		Since      time.Time `json:"since"`
		Source     string    `json:"source"`
		Ambiguous  bool      `json:"ambiguous"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]laneMarkView, 0, len(raw))
	for lane, v := range raw {
		out = append(out, laneMarkView{Lane: lane, EmptyUntil: v.EmptyUntil, Since: v.Since, Source: v.Source, Ambiguous: v.Ambiguous})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lane < out[j].Lane })
	return out, nil
}

// schedulerDeps is everything runArbiterScheduler needs beyond cfg/o -
// every field individually injectable so serve_arbiter_test.go can run
// the whole loop against fakes: no real provider probe, no real sleep, no
// real `aida loop --arbiter` spawn, no real preflight process. Production
// fills every unset field via withDefaults.
type schedulerDeps struct {
	capacity  capacitySource
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) bool
	runWave   func(ctx context.Context, o loopOpts) error
	preflight func(ctx context.Context, lane arbiter.Lane) arbiter.PreflightResult
	speak     func(string)
	stateDir  string
}

// withDefaults fills every unset field of d with the production
// implementation: live capacity probing, a real clock, waitOrDone,
// runLoopCtx, a real PreflightLane over ExecRunner, and speaking through
// notifier plus a stderr line. Only capacity construction can fail (a bad
// models/burndown config) - everything else is a plain function value.
func (d schedulerDeps) withDefaults(cfg *config.Config, o arbiterServeOpts, notifier *notify.Notifier) (schedulerDeps, error) {
	if d.now == nil {
		d.now = time.Now
	}
	if d.sleep == nil {
		d.sleep = waitOrDone
	}
	if d.runWave == nil {
		d.runWave = runLoopCtx
	}
	if d.stateDir == "" {
		d.stateDir = filepath.Join(config.Dir(), "arbiter")
	}
	if d.preflight == nil {
		timeout := o.preflightTimeout
		d.preflight = func(ctx context.Context, lane arbiter.Lane) arbiter.PreflightResult {
			return arbiter.PreflightLane(ctx, lane, arbiter.ExecRunner{}, timeout)
		}
	}
	if d.speak == nil {
		d.speak = func(msg string) {
			fmt.Fprintf(os.Stderr, "🌙 %s\n", msg)
			notifier.Enqueue(msg)
		}
	}
	if d.capacity == nil {
		c, err := newLiveCapacity(cfg)
		if err != nil {
			return schedulerDeps{}, err
		}
		d.capacity = c
	}
	return d, nil
}

// summarizeSkips renders BuildWave's skip reasons as one short state
// string for scheduler.json's "idle, nothing to run" case, e.g.
// "3 candidate(s) skipped: no machine verifier (...)". Distinct reasons
// are deduplicated so ten tasks failing the same gate don't repeat the
// same sentence ten times.
func summarizeSkips(skipped []arbiter.Skip) string {
	if len(skipped) == 0 {
		return "no candidate tasks"
	}
	seen := make(map[string]bool, len(skipped))
	var reasons []string
	for _, s := range skipped {
		if seen[s.Reason] {
			continue
		}
		seen[s.Reason] = true
		reasons = append(reasons, s.Reason)
	}
	return fmt.Sprintf("%d candidate(s) skipped: %s", len(skipped), strings.Join(reasons, "; "))
}

// buildWaveTasks lists open+in-progress tasks (tags-filtered, same as
// pickBatch with no cap) and attaches each one's Verifier string: a
// configured --arbiter-check wins outright ("check: a && b"), otherwise
// a task body's ## Deliverables section (taskstate.ParseDeliverables)
// contributes "deliverables: N" when it found any, else the task carries
// no verifier at all.
func buildWaveTasks(brn *brain.Brain, tags []string, checks []string) ([]arbiter.WaveTask, error) {
	candidates, err := pickBatch(brn, tags, 0)
	if err != nil {
		return nil, err
	}
	out := make([]arbiter.WaveTask, 0, len(candidates))
	for _, t := range candidates {
		wt := arbiter.WaveTask{Slug: t.Slug, TaskID: t.TaskID, Title: t.Title, Tags: t.Tags}
		if len(checks) > 0 {
			wt.Verifier = "check: " + strings.Join(checks, " && ")
		} else {
			body, berr := brn.TaskBody(t.Slug)
			if berr == nil {
				if dels := taskstate.ParseDeliverables(body); len(dels) > 0 {
					wt.Verifier = fmt.Sprintf("deliverables: %d", len(dels))
				}
			}
		}
		out = append(out, wt)
	}
	return out, nil
}

// buildWaveLoopOpts turns the scheduler's own opts plus a decided wave's
// slugs into the loopOpts runWave (runLoopCtx) actually runs: arbiter
// enforcement and daemon mode are always on (a scheduled wave is
// inherently unattended and must keep re-picking within the wave rather
// than exit after one round), tags/checks/worktree/pr/reviewer/
// concurrency come straight from o, and slugs restricts the round to
// exactly this wave (loop_arbiter.go's filterTasksBySlugs).
func buildWaveLoopOpts(o arbiterServeOpts, slugs []string) loopOpts {
	lo := defaultLoopOpts()
	lo.arbiter = true
	lo.daemon = true
	lo.tags = o.tags
	lo.checks = o.checks
	lo.worktree = o.worktree || o.pr // --pr implies --worktree, same normalization runLoopCtx applies
	lo.pr = o.pr
	lo.reviewer = o.reviewer
	lo.concurrency = o.concurrency
	lo.slugs = slugs
	return lo
}

// firstPersonalLane returns the first (cheapest) lane in cfg that allows
// the personal data class - lane 1 in docs/arbiter-plan.md section 3's
// numbering, the one plan section 8 step 4 calls out for preflight.
// false when no lane allows personal data at all (a butterstack-only
// roster, say) - the scheduler simply skips preflight in that case rather
// than picking an arbitrary substitute lane to check.
func firstPersonalLane(cfg *arbiter.Config) (arbiter.Lane, bool) {
	if cfg == nil {
		return arbiter.Lane{}, false
	}
	for _, l := range cfg.Sorted() {
		if l.AllowsClass(arbiter.DataClassPersonal) {
			return l, true
		}
	}
	return arbiter.Lane{}, false
}

// runArbiterScheduler is `aida serve --arbiter`'s background loop:
// snapshot capacity, decide whether to run (arbiter.ShouldRun), and
// either sleep until arbiter.NextWake or run one wave through
// deps.runWave. It returns only on ctx cancellation (ctx.Err()) or a
// fatal setup error (a missing/invalid lane roster, a bad burndown
// config) - a per-wave failure (deps.runWave returning an error, a
// capacity snapshot failing) is logged and the loop continues, mirroring
// runHarvestSweepLoop and the loop dispatcher's own "one bad round never
// takes down the goroutine" convention.
func runArbiterScheduler(ctx context.Context, cfg *config.Config, profileName string, o arbiterServeOpts, notifier *notify.Notifier, deps schedulerDeps) error {
	deps, err := deps.withDefaults(cfg, o, notifier)
	if err != nil {
		return fmt.Errorf("arbiter scheduler: %w", err)
	}

	lanes, err := loadLanesConfig(cfg.LanesPath())
	if err != nil {
		return fmt.Errorf("arbiter scheduler: %w", err)
	}
	if err := lanes.Validate(); err != nil {
		return fmt.Errorf("arbiter scheduler: invalid lane config: %w", err)
	}

	bcfg, err := burndown.Load(cfg.BurndownPath())
	if err != nil {
		return fmt.Errorf("arbiter scheduler: loading burndown config: %w", err)
	}
	schedCfg := arbiter.DefaultScheduleConfig(bcfg.Overnight)
	schedCfg.NightlyAt = o.nightlyAt
	schedCfg.WeeklyDrain = o.weeklyDrain

	signalStore, err := arbiter.OpenStore(deps.stateDir)
	if err != nil {
		return fmt.Errorf("arbiter scheduler: %w", err)
	}
	host, _ := os.Hostname()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		caps, cerr := deps.capacity.Snapshot(ctx)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "arbiter scheduler: capacity snapshot failed: %v\n", cerr)
			caps = nil
		}
		now := deps.now()

		should, reason := arbiter.ShouldRun(schedCfg, caps, now)
		if !should {
			next, _ := arbiter.NextWake(schedCfg, caps, now)
			_ = writeSchedulerState(deps.stateDir, schedulerState{State: "sleeping", NextWake: next, Reason: reason, Updated: now})
			d := next.Sub(now)
			if d <= 0 {
				d = time.Minute
			}
			if deps.sleep(ctx, d) {
				return ctx.Err()
			}
			continue
		}

		_ = writeSchedulerState(deps.stateDir, schedulerState{State: "running", Started: now, Reason: reason, Updated: now})

		if lane, ok := firstPersonalLane(lanes); ok {
			pf := deps.preflight(ctx, lane)
			if !pf.OK {
				deps.speak(fmt.Sprintf("Arbiter preflight failed on lane %s: %s", pf.Lane, pf.Reason))
				nextWake, _ := arbiter.NextWake(schedCfg, caps, now)
				_ = signalStore.Record(arbiter.Signal{
					At:         now,
					Lane:       pf.Lane,
					Source:     "preflight",
					Verdict:    arbiter.VerdictEmpty, // Store.Record derives Ambiguous from Verdict; Empty keeps ambiguous=false.
					Matched:    pf.Reason,
					EmptyUntil: nextWake,
					Host:       host,
				})
			}
		}

		brn, berr := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
		if berr != nil {
			return fmt.Errorf("arbiter scheduler: open brain: %w", berr)
		}
		waveTasks, werr := buildWaveTasks(brn, o.tags, o.checks)
		brn.Close()
		if werr != nil {
			return fmt.Errorf("arbiter scheduler: list candidate tasks: %w", werr)
		}

		wave, skipped := arbiter.BuildWave(waveTasks, o.requireVerifier)
		if len(wave) == 0 {
			_ = writeSchedulerState(deps.stateDir, schedulerState{State: "idle", Reason: summarizeSkips(skipped), Updated: deps.now()})
			next, _ := arbiter.NextWake(schedCfg, caps, deps.now())
			d := next.Sub(deps.now())
			if d <= 0 {
				d = time.Minute
			}
			if deps.sleep(ctx, d) {
				return ctx.Err()
			}
			continue
		}

		slugs := make([]string, len(wave))
		for i, w := range wave {
			slugs[i] = w.Slug
		}
		lo := buildWaveLoopOpts(o, slugs)

		waveStarted := deps.now()
		waveCtx, waveCancel := context.WithCancel(ctx)
		tickerDone := make(chan struct{})
		go func() {
			defer close(tickerDone)
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-waveCtx.Done():
					return
				case <-ticker.C:
					c, err := deps.capacity.Snapshot(waveCtx)
					if err != nil {
						continue
					}
					if ok, _ := arbiter.ShouldRun(schedCfg, c, deps.now()); !ok {
						waveCancel()
						return
					}
				}
			}
		}()

		runErr := deps.runWave(waveCtx, lo)
		waveCancel()
		<-tickerDone
		waveEnded := deps.now()
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			fmt.Fprintf(os.Stderr, "arbiter scheduler: wave run failed: %v\n", runErr)
		}

		entries, _ := arbiter.ReadLedger(schedulerLedgerPath(deps.stateDir))
		summary := arbiter.SummarizeWave(entries, waveStarted, waveEnded)
		deps.speak(arbiter.SpeakSummary(summary))
		_ = writeWaveSummary(deps.stateDir, summary)
		_ = writeSchedulerState(deps.stateDir, schedulerState{State: "idle", Reason: "wave complete", Updated: deps.now()})

		if deps.sleep(ctx, time.Minute) {
			return ctx.Err()
		}
	}
}
