package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
)

// ---- test fixtures ----

func writeTestLanesYAML(t *testing.T, path string) {
	t.Helper()
	data := []byte(`lanes:
  - id: test-lane
    order: 1
    data_classes: [personal, public]
    runner: aida-agent
    auth: api-key
    models:
      executor: test-model
`)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write lanes.yaml: %v", err)
	}
}

func writeTestBurndownYAML(t *testing.T, path string) {
	t.Helper()
	data := []byte(`overnight:
  start: "23:00"
  end: "08:00"
  timezone: "UTC"
floors: {}
`)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write burndown.yaml: %v", err)
	}
}

// newSchedulerTestConfig builds a *config.Config pointed at a scratch
// lanes.yaml/burndown.yaml/brain dir under a fresh HOME - no real
// ~/.aida/* is ever touched by these tests.
func newSchedulerTestConfig(t *testing.T) *config.Config {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	lanesPath := filepath.Join(dir, "lanes.yaml")
	burndownPath := filepath.Join(dir, "burndown.yaml")
	brainPath := filepath.Join(dir, "brain")
	writeTestLanesYAML(t, lanesPath)
	writeTestBurndownYAML(t, burndownPath)
	return &config.Config{
		Lanes:    config.LanesConfig{Path: lanesPath},
		Burndown: config.BurndownConfig{Path: burndownPath},
		Brain:    config.BrainConfig{Path: brainPath},
	}
}

// fakeClock hands out a fixed sequence of instants, repeating the last one
// once exhausted - deterministic without pinning exactly how many times
// runArbiterScheduler happens to call deps.now() per iteration.
type fakeClock struct {
	times []time.Time
	i     int
}

func (c *fakeClock) now() time.Time {
	t := c.times[c.i]
	if c.i < len(c.times)-1 {
		c.i++
	}
	return t
}

// alwaysStop is a deps.sleep fake that always reports "ctx already won,"
// ending runArbiterScheduler's loop after exactly one iteration -
// every scheduler test in this file exercises a single pass.
func alwaysStop(_ context.Context, _ time.Duration) bool { return true }

// ---- arbiterServeOpts / validate ----

func TestArbiterServeOptsValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*arbiterServeOpts)
		wantErr string
	}{
		{"defaults are valid", func(o *arbiterServeOpts) {}, ""},
		{"bad nightly time", func(o *arbiterServeOpts) { o.nightlyAt = "not-a-time" }, "arbiter-nightly-at"},
		{"zero weekly drain", func(o *arbiterServeOpts) { o.weeklyDrain = 0 }, "arbiter-weekly-drain"},
		{"negative concurrency", func(o *arbiterServeOpts) { o.concurrency = 0 }, "arbiter-concurrency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := defaultArbiterServeOpts()
			tc.mutate(&o)
			err := o.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateArbiterServeConfigRejectsLoopConflict(t *testing.T) {
	o := defaultArbiterServeOpts()
	o.enabled = true
	if err := validateArbiterServeConfig(o, true); err == nil {
		t.Fatal("expected an error when --arbiter and --loop are both set")
	} else if !strings.Contains(err.Error(), "must never dispatch over the same task") {
		t.Errorf("error = %v, want the arbiter/loop exclusivity message", err)
	}

	if err := validateArbiterServeConfig(o, false); err != nil {
		t.Errorf("validateArbiterServeConfig with --arbiter alone = %v, want nil", err)
	}

	bad := defaultArbiterServeOpts()
	bad.nightlyAt = "nope"
	if err := validateArbiterServeConfig(bad, false); err == nil {
		t.Fatal("expected the bad nightly time to fail validation even with --loop off")
	}
}

// ---- filterTasksBySlugs ----

func TestFilterTasksBySlugs(t *testing.T) {
	candidates := []brain.TaskRecord{{Slug: "a"}, {Slug: "b"}, {Slug: "c"}}

	if got := filterTasksBySlugs(candidates, nil); len(got) != 3 {
		t.Errorf("empty allowlist should keep everything, got %d", len(got))
	}

	got := filterTasksBySlugs(candidates, []string{"c", "a"})
	if len(got) != 2 || got[0].Slug != "a" || got[1].Slug != "c" {
		t.Fatalf("got %+v, want [a, c] preserving candidate order", got)
	}
}

// ---- state file round trips ----

func TestSchedulerStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := readSchedulerState(dir); err != nil {
		t.Fatalf("readSchedulerState on a missing file: %v", err)
	}
	want := schedulerState{State: "running", Reason: "overnight window", Updated: time.Now().UTC().Truncate(time.Second)}
	if err := writeSchedulerState(dir, want); err != nil {
		t.Fatalf("writeSchedulerState: %v", err)
	}
	got, err := readSchedulerState(dir)
	if err != nil {
		t.Fatalf("readSchedulerState: %v", err)
	}
	if got.State != want.State || got.Reason != want.Reason {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWaveSummaryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := readWaveSummary(dir); err != nil || ok {
		t.Fatalf("readWaveSummary on a missing file: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	want := arbiter.WaveSummary{Tasks: 3, Passed: 2, ByLane: map[string]int{"test-lane": 3}, ByVerdict: map[string]int{"pass": 2, "fail": 1}}
	if err := writeWaveSummary(dir, want); err != nil {
		t.Fatalf("writeWaveSummary: %v", err)
	}
	got, ok, err := readWaveSummary(dir)
	if err != nil || !ok {
		t.Fatalf("readWaveSummary: ok=%v err=%v", ok, err)
	}
	if got.Tasks != 3 || got.Passed != 2 {
		t.Errorf("got %+v, want Tasks=3 Passed=2", got)
	}
}

func TestReadLaneMarks(t *testing.T) {
	dir := t.TempDir()
	marks, err := readLaneMarks(dir)
	if err != nil || marks != nil {
		t.Fatalf("readLaneMarks on a missing file: marks=%v err=%v, want nil,nil", marks, err)
	}

	store, err := arbiter.OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := store.Record(arbiter.Signal{Lane: "test-lane", Source: "preflight", Verdict: arbiter.VerdictEmpty, EmptyUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	marks, err = readLaneMarks(dir)
	if err != nil {
		t.Fatalf("readLaneMarks: %v", err)
	}
	if len(marks) != 1 || marks[0].Lane != "test-lane" || marks[0].Source != "preflight" {
		t.Fatalf("marks = %+v, want one test-lane/preflight entry", marks)
	}
}

// ---- runArbiterScheduler ----

// TestRunArbiterSchedulerOutsideWindowNeverRunsWave covers (a): outside
// the overnight window with no weekly reset due, the scheduler writes a
// "sleeping" state naming the right next wake and never calls runWave.
func TestRunArbiterSchedulerOutsideWindowNeverRunsWave(t *testing.T) {
	cfg := newSchedulerTestConfig(t)
	stateDir := t.TempDir()
	noon := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	runWaveCalled := false
	deps := schedulerDeps{
		capacity: &fakeCapacity{},
		now:      func() time.Time { return noon },
		sleep:    alwaysStop,
		runWave: func(_ context.Context, _ loopOpts) error {
			runWaveCalled = true
			return nil
		},
		stateDir: stateDir,
	}

	if err := runArbiterScheduler(context.Background(), cfg, "work", defaultArbiterServeOpts(), nil, deps); err != nil {
		t.Fatalf("runArbiterScheduler: %v", err)
	}
	if runWaveCalled {
		t.Error("runWave must not be called outside the overnight window")
	}

	st, err := readSchedulerState(stateDir)
	if err != nil {
		t.Fatalf("readSchedulerState: %v", err)
	}
	if st.State != "sleeping" {
		t.Errorf("state = %q, want sleeping", st.State)
	}
	if st.NextWake.IsZero() {
		t.Error("expected a non-zero NextWake")
	}
	if !strings.Contains(st.Reason, "outside the overnight window") {
		t.Errorf("reason = %q", st.Reason)
	}
}

// TestRunArbiterSchedulerRunsWaveInsideWindow covers (b): inside the
// window, with a candidate task carrying a configured --arbiter-check
// (so it clears the verifier gate), the scheduler preflights the lane,
// runs the wave with arbiter+daemon on and the tag filter applied, then
// summarizes the canned ledger and speaks it.
func TestRunArbiterSchedulerRunsWaveInsideWindow(t *testing.T) {
	cfg := newSchedulerTestConfig(t)
	stateDir := t.TempDir()

	brn, err := brain.Open(cfg.BrainPath(), "work", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	if _, err := brn.AddTask("Overnight task", []string{"prio"}, "Do the thing."); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	brn.Close()

	// Pre-seed a ledger entry landing inside [waveStarted, waveEnded] so
	// SummarizeWave has something to reduce.
	night := time.Date(2026, 9, 25, 23, 30, 0, 0, time.UTC)
	ledgerEntryAt := night.Add(2 * time.Minute)
	if err := arbiter.AppendLedger(schedulerLedgerPath(stateDir), arbiter.Entry{
		At: ledgerEntryAt, TaskSlug: "overnight-task", Lane: "test-lane", Verdict: "pass",
	}); err != nil {
		t.Fatalf("AppendLedger: %v", err)
	}

	clock := &fakeClock{times: []time.Time{night, night, night.Add(5 * time.Minute), night.Add(5 * time.Minute)}}

	var preflightCalled bool
	var runWaveCalled bool
	var capturedOpts loopOpts
	var spoken []string

	deps := schedulerDeps{
		capacity: &fakeCapacity{},
		now:      clock.now,
		sleep:    alwaysStop,
		preflight: func(_ context.Context, lane arbiter.Lane) arbiter.PreflightResult {
			preflightCalled = true
			return arbiter.PreflightResult{Lane: lane.ID, OK: true}
		},
		runWave: func(_ context.Context, o loopOpts) error {
			runWaveCalled = true
			capturedOpts = o
			return nil
		},
		speak:    func(msg string) { spoken = append(spoken, msg) },
		stateDir: stateDir,
	}

	o := defaultArbiterServeOpts()
	o.tags = []string{"prio"}
	o.checks = []string{"make test"}

	if err := runArbiterScheduler(context.Background(), cfg, "work", o, nil, deps); err != nil {
		t.Fatalf("runArbiterScheduler: %v", err)
	}
	if !preflightCalled {
		t.Error("expected preflight to be called inside the overnight window")
	}
	if !runWaveCalled {
		t.Fatal("expected runWave to be called with a non-empty wave")
	}
	if !capturedOpts.arbiter || !capturedOpts.daemon {
		t.Errorf("loopOpts = %+v, want arbiter=true daemon=true", capturedOpts)
	}
	if len(capturedOpts.tags) != 1 || capturedOpts.tags[0] != "prio" {
		t.Errorf("loopOpts.tags = %v, want [prio]", capturedOpts.tags)
	}
	if len(capturedOpts.slugs) != 1 {
		t.Errorf("loopOpts.slugs = %v, want exactly one slug", capturedOpts.slugs)
	}

	if len(spoken) != 1 || !strings.Contains(spoken[0], "1 tasks") || !strings.Contains(spoken[0], "1 passed") {
		t.Errorf("spoken = %v, want a one-task-passed summary", spoken)
	}

	wave, ok, err := readWaveSummary(stateDir)
	if err != nil || !ok {
		t.Fatalf("readWaveSummary: ok=%v err=%v", ok, err)
	}
	if wave.Tasks != 1 || wave.Passed != 1 {
		t.Errorf("wave summary = %+v, want Tasks=1 Passed=1", wave)
	}
}

// TestRunArbiterSchedulerFailedPreflightSpeaksAndMarksLane covers (c): a
// failing preflight speaks an alert and marks the lane empty in the
// signal store, ambiguous=false, source=preflight.
func TestRunArbiterSchedulerFailedPreflightSpeaksAndMarksLane(t *testing.T) {
	cfg := newSchedulerTestConfig(t)
	stateDir := t.TempDir()

	brn, err := brain.Open(cfg.BrainPath(), "work", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	brn.Close() // no tasks -> the wave is trivially empty; only the preflight path matters here

	night := time.Date(2026, 9, 25, 23, 30, 0, 0, time.UTC)
	clock := &fakeClock{times: []time.Time{night}}

	var spoken []string
	deps := schedulerDeps{
		capacity: &fakeCapacity{},
		now:      clock.now,
		sleep:    alwaysStop,
		preflight: func(_ context.Context, lane arbiter.Lane) arbiter.PreflightResult {
			return arbiter.PreflightResult{Lane: lane.ID, OK: false, Reason: "matched auth-failure phrase \"oauth\""}
		},
		speak:    func(msg string) { spoken = append(spoken, msg) },
		stateDir: stateDir,
	}

	if err := runArbiterScheduler(context.Background(), cfg, "work", defaultArbiterServeOpts(), nil, deps); err != nil {
		t.Fatalf("runArbiterScheduler: %v", err)
	}

	if len(spoken) == 0 || !strings.Contains(spoken[0], "Arbiter preflight failed on lane test-lane") {
		t.Fatalf("spoken = %v, want a preflight-failure alert", spoken)
	}

	store, err := arbiter.OpenStore(stateDir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	until, marked := store.EmptyUntil("test-lane", night)
	if !marked {
		t.Fatal("expected test-lane to be marked empty after a failed preflight")
	}
	if !until.After(night) {
		t.Errorf("empty_until = %v, want it after %v", until, night)
	}

	sigs, err := store.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 1 || sigs[0].Source != "preflight" || sigs[0].Ambiguous {
		t.Fatalf("signals = %+v, want one preflight signal with ambiguous=false", sigs)
	}
}

// TestRunArbiterSchedulerEmptyWaveNeverRunsWave covers (d): with
// --arbiter-require-verifier on (the default) and no --arbiter-check or
// ## Deliverables, the one candidate task is skipped, the scheduler
// writes an idle state, and runWave is never called.
func TestRunArbiterSchedulerEmptyWaveNeverRunsWave(t *testing.T) {
	cfg := newSchedulerTestConfig(t)
	stateDir := t.TempDir()

	brn, err := brain.Open(cfg.BrainPath(), "work", "VOYAGE_TEST_KEY_UNSET", "")
	if err != nil {
		t.Fatalf("brain.Open: %v", err)
	}
	if _, err := brn.AddTask("No verifier here", nil, "Just prose, no deliverables section."); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	brn.Close()

	night := time.Date(2026, 9, 25, 23, 30, 0, 0, time.UTC)

	runWaveCalled := false
	deps := schedulerDeps{
		capacity: &fakeCapacity{},
		now:      func() time.Time { return night },
		sleep:    alwaysStop,
		preflight: func(_ context.Context, lane arbiter.Lane) arbiter.PreflightResult {
			return arbiter.PreflightResult{Lane: lane.ID, OK: true}
		},
		runWave: func(_ context.Context, _ loopOpts) error {
			runWaveCalled = true
			return nil
		},
		stateDir: stateDir,
	}

	if err := runArbiterScheduler(context.Background(), cfg, "work", defaultArbiterServeOpts(), nil, deps); err != nil {
		t.Fatalf("runArbiterScheduler: %v", err)
	}
	if runWaveCalled {
		t.Error("runWave must not be called when the wave is empty")
	}

	st, err := readSchedulerState(stateDir)
	if err != nil {
		t.Fatalf("readSchedulerState: %v", err)
	}
	if st.State != "idle" {
		t.Errorf("state = %q, want idle", st.State)
	}
	if !strings.Contains(st.Reason, "no machine verifier") {
		t.Errorf("reason = %q, want it to explain the missing verifier", st.Reason)
	}
}
