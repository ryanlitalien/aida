package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/arbiter"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/taskstate"
)

// TestNewArbiterRuntimeRequiresLaneRoster covers the 2026-09-25 decision:
// a missing ~/.aida/lanes.yaml must fail `aida loop --arbiter` outright,
// never silently fall back to arbiter.DefaultConfig()'s placeholder
// roster (which used to happen here with a printed notice).
func TestNewArbiterRuntimeRequiresLaneRoster(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "lanes.yaml")
	cfg := &config.Config{Lanes: config.LanesConfig{Path: missing}}
	_, err := newArbiterRuntime(cfg, defaultLoopOpts(), "home", t.TempDir())
	if err == nil {
		t.Fatal("newArbiterRuntime with no lane roster = nil error, want one naming the missing path")
	}
	if !strings.Contains(err.Error(), "no lane roster at "+missing) {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
}

// ---- fakes ----

// fakeCapacity is a canned capacitySource - no network, no real provider
// probe, so tests are hermetic and fast.
type fakeCapacity struct {
	caps []burndown.Capacity
}

func (f *fakeCapacity) Snapshot(_ context.Context) ([]burndown.Capacity, error) {
	return f.caps, nil
}

// fakeRunner records every prompt it was given and returns canned results in
// order, repeating the last (or a default pass) once the queue is drained.
type fakeRunner struct {
	prompts []string
	queue   []arbiter.RunResult
	// onRun, when set, runs before each result is returned - the test seam
	// for "the model wrote HANDOFF.md during this attempt".
	onRun func(call int, spec arbiter.RunSpec)
}

func (f *fakeRunner) Run(_ context.Context, spec arbiter.RunSpec) (arbiter.RunResult, error) {
	f.prompts = append(f.prompts, spec.Prompt)
	if f.onRun != nil {
		f.onRun(len(f.prompts), spec)
	}
	if len(f.queue) == 0 {
		return arbiter.RunResult{Verdict: arbiter.VerdictNotEmpty, ExitCode: 0, Stdout: "ok"}, nil
	}
	r := f.queue[0]
	f.queue = f.queue[1:]
	return r, nil
}

// ---- test helpers ----

func runGitOK(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (dir=%s): %v\n%s", args, dir, err, out)
	}
}

// setupArbiterGitBrain returns a scratch brain (see newTestBrainAndJobs)
// turned into a git repo with a "origin" remote pointing at a fresh bare
// repo - the minimal setup taskstate.Claimer needs for lease compare-and-
// swap pushes. Mirrors internal/taskstate/lease_test.go's own bare-remote
// helper, adapted to reuse an existing (non-git) brain directory instead of
// creating a fresh clone.
func setupArbiterGitBrain(t *testing.T) (*brain.Brain, *jobs.Store) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	brn, store := newTestBrainAndJobs(t)

	runGitOK(t, brn.Path, "init", "-q")
	runGitOK(t, brn.Path, "config", "user.email", "test@example.com")
	runGitOK(t, brn.Path, "config", "user.name", "Test")

	bareRoot := t.TempDir()
	bare := filepath.Join(bareRoot, "remote.git")
	runGitOK(t, bareRoot, "init", "--bare", "-q", bare)
	runGitOK(t, brn.Path, "remote", "add", "origin", bare)

	return brn, store
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it - used to assert on the loop's printed
// diagnostics (e.g. "HANDOFF.md missing") without plumbing a Writer through
// every print call.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func strPtr(s string) *string { return &s }

func testArbiterOpts() loopOpts {
	o := defaultLoopOpts()
	o.arbiter = true
	o.maxFix = 3
	o.perTask = 5 * time.Second
	return o
}

func newTestArbiterRuntime(t *testing.T, brainPath string, lanes *arbiter.Config, caps []burndown.Capacity, runner arbiter.Runner) *arbiterRuntime {
	t.Helper()
	if err := lanes.Validate(); err != nil {
		t.Fatalf("lane config invalid: %v", err)
	}
	sigStore, err := arbiter.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	host := "test-host"
	return &arbiterRuntime{
		lanes:      lanes,
		capacity:   &fakeCapacity{caps: caps},
		signals:    sigStore,
		ledgerPath: filepath.Join(t.TempDir(), "ledger.ndjson"),
		host:       host,
		brainPath:  brainPath,
		claimer: &taskstate.Claimer{
			RepoDir:  brainPath,
			Remote:   "origin",
			Claimant: "loop@" + host,
			Host:     host,
			TTL:      15 * time.Minute,
		},
		runner: runner,
		now:    time.Now,
	}
}

func oneLane(id string, order int, provider string, classes []arbiter.DataClass) arbiter.Lane {
	return arbiter.Lane{
		ID: id, Order: order, Provider: provider, Windows: []string{"5h"},
		DataClasses: classes,
		Runner:      arbiter.RunnerExec,
		Command:     []string{"true"},
		Auth:        arbiter.AuthSubscription,
		Models:      map[string]string{arbiter.RoleExecutor: "model-" + id},
	}
}

// ---- tests ----

// TestArbiterHandoffArmedWhenLaneNearExhaustion covers requirement (1): a
// lane at 95% used arms the HANDOFF.md instruction in the first prompt, and
// when the runner never writes it, the loop detects and logs "HANDOFF.md
// missing" to both stdout and LOG.md.
func TestArbiterHandoffArmedWhenLaneNearExhaustion(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	caps := []burndown.Capacity{{Provider: "P1", Label: "5-hour", UsedPct: 95, Headroom: 50}}
	fr := &fakeRunner{}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)

	task, err := brn.AddTask("Test task one", nil, "Do the thing.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	o.maxFix = 1
	state := &loopState{}

	out := captureStdout(t, func() {
		o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, state)
	})

	if len(fr.prompts) == 0 {
		t.Fatal("runner was never invoked")
	}
	path := taskstate.HandoffPath(brn.Path, task.Slug)
	if !strings.Contains(fr.prompts[0], "HANDOFF.md") || !strings.Contains(fr.prompts[0], path) {
		t.Errorf("first prompt missing arming block / handoff path:\n%s", fr.prompts[0])
	}
	if !strings.Contains(out, "HANDOFF.md missing") {
		t.Errorf("stdout missing %q, got:\n%s", "HANDOFF.md missing", out)
	}
	logData, _ := os.ReadFile(taskstate.LogPath(brn.Path, task.Slug))
	if !strings.Contains(string(logData), "HANDOFF.md missing") {
		t.Errorf("LOG.md missing %q, got:\n%s", "HANDOFF.md missing", logData)
	}
}

// TestArbiterLaneEmptySignalMovesToNextLane covers requirement (2): a
// usage-limit verdict on the cheaper lane records a non-ambiguous empty
// signal, the next Pick lands on the other lane, the resulting lane-switch
// prompt is a ReconstructPrompt (no hand-off exists to resume from), and
// both attempts are recorded in the ledger.
func TestArbiterLaneEmptySignalMovesToNextLane(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
		oneLane("backup", 2, "P2", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	caps := []burndown.Capacity{
		{Provider: "P1", Label: "5-hour", UsedPct: 10, Headroom: 50},
		{Provider: "P2", Label: "5-hour", UsedPct: 10, Headroom: 50},
	}
	fr := &fakeRunner{queue: []arbiter.RunResult{
		{Verdict: arbiter.VerdictEmpty, ExitCode: 1, Stderr: "usage limit reached", Matched: "usage limit reached"},
	}}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)

	task, err := brn.AddTask("Test task two", nil, "Do the other thing.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	state := &loopState{}
	outcome := o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, state)
	if outcome != outcomeDone {
		t.Fatalf("outcome = %v, want outcomeDone", outcome)
	}

	if len(fr.prompts) < 2 {
		t.Fatalf("expected at least 2 runner invocations, got %d", len(fr.prompts))
	}
	if !strings.Contains(strings.ToLower(fr.prompts[1]), "rebuild") {
		t.Errorf("second prompt is not a reconstruct prompt (missing 'rebuild'):\n%s", fr.prompts[1])
	}

	until, ok := rt.signals.EmptyUntil("max", time.Now())
	if !ok || until.Before(time.Now()) {
		t.Errorf("lane max was not marked empty: ok=%v until=%v", ok, until)
	}
	sigs, err := rt.signals.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 1 || sigs[0].Ambiguous {
		t.Errorf("expected exactly one non-ambiguous signal, got %+v", sigs)
	}

	entries, err := arbiter.ReadLedger(rt.ledgerPath)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger has %d entries, want 2", len(entries))
	}
	if entries[0].Lane != "max" || entries[0].Verdict != "lane-empty" {
		t.Errorf("first ledger entry = %+v, want lane=max verdict=lane-empty", entries[0])
	}
}

// TestArbiterAmbiguousSignalMarksLaneEmpty covers requirement (3): an
// ambiguous verdict (a non-zero exit with no recognized phrase) is recorded
// with Ambiguous=true, and the lane is excluded from the very next Pick.
func TestArbiterAmbiguousSignalMarksLaneEmpty(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	caps := []burndown.Capacity{{Provider: "P1", Label: "5-hour", UsedPct: 10, Headroom: 50}}
	fr := &fakeRunner{queue: []arbiter.RunResult{
		{Verdict: arbiter.VerdictAmbiguous, ExitCode: 1},
	}}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)

	task, err := brn.AddTask("Test task three", nil, "Do a third thing.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	o.maxFix = 2
	state := &loopState{}
	outcome := o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, state)
	if outcome != outcomeNoLane {
		t.Fatalf("outcome = %v, want outcomeNoLane (the only lane just went empty)", outcome)
	}

	sigs, err := rt.signals.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(sigs) != 1 || !sigs[0].Ambiguous {
		t.Errorf("expected exactly one ambiguous signal, got %+v", sigs)
	}

	_, pickErr := arbiter.Pick(lanes, arbiter.Request{TaskSlug: task.Slug, TaskID: task.TaskID, Role: arbiter.RoleExecutor, Now: time.Now()}, caps, rt.signals)
	if pickErr == nil {
		t.Error("expected the next Pick to find no eligible lane (the only lane is signalled empty)")
	}
}

// TestArbiterMissingDeliverableFailsGate covers requirement (4): a
// task naming a deliverable the runner never creates fails the gate even
// with zero --check commands, and the task is parked on hold.
func TestArbiterMissingDeliverableFailsGate(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	caps := []burndown.Capacity{{Provider: "P1", Label: "5-hour", UsedPct: 10, Headroom: 50}}
	fr := &fakeRunner{}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)

	body := "Do the thing.\n\n## Deliverables\n- totally-made-up-deliverable-8f3a.txt\n"
	task, err := brn.AddTask("Test task four", nil, body)
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	o.maxFix = 1
	o.checks = nil
	state := &loopState{}
	outcome := o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, state)
	if outcome != outcomeHold {
		t.Fatalf("outcome = %v, want outcomeHold", outcome)
	}

	got, err := brn.GetTask(task.Slug)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != brain.StatusHold {
		t.Errorf("task status = %q, want %q", got.Status, brain.StatusHold)
	}
}

// TestArbiterNoLaneLeavesTaskOpen covers requirement (5): a task whose
// class no configured lane serves leaves the task open (never touched), the
// lease is released, and the outcome is outcomeNoLane - never a failure,
// never a hold.
func TestArbiterNoLaneLeavesTaskOpen(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	// Only serves butterstack; an untagged task classifies personal
	// (ADR-0003), so no lane is ever eligible for it.
	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("bs-only", 1, "P1", []arbiter.DataClass{arbiter.DataClassButterstack}),
	}}
	caps := []burndown.Capacity{{Provider: "P1", Label: "5-hour", UsedPct: 10, Headroom: 50}}
	fr := &fakeRunner{}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)

	task, err := brn.AddTask("Test task five", nil, "Do the fifth thing.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	state := &loopState{}
	outcome := o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, state)
	if outcome != outcomeNoLane {
		t.Fatalf("outcome = %v, want outcomeNoLane", outcome)
	}
	if len(fr.prompts) != 0 {
		t.Error("runner should never have been invoked - no lane was ever eligible")
	}

	got, err := brn.GetTask(task.Slug)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != brain.StatusOpen {
		t.Errorf("task status = %q, want %q (untouched)", got.Status, brain.StatusOpen)
	}
	if got.ClaimedBy != "" || got.LeaseUntil != "" {
		t.Errorf("lease mirror not cleared: claimed_by=%q lease_until=%q", got.ClaimedBy, got.LeaseUntil)
	}

	remaining, err := taskstate.ReadLease(context.Background(), brn.Path, "origin", task.Slug)
	if err != nil {
		t.Fatalf("ReadLease: %v", err)
	}
	if remaining != nil {
		t.Errorf("expected the lease to be released, got %+v", remaining)
	}
}

// TestClaimTasksSkipsLiveForeignLease covers requirement (6): a candidate
// whose frontmatter mirror shows a live lease held by another claimant is
// skipped without a round trip to the remote.
func TestClaimTasksSkipsLiveForeignLease(t *testing.T) {
	brn, _ := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, nil, &fakeRunner{})

	task, err := brn.AddTask("Test task six", nil, "Do the sixth thing.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	updated, err := brn.UpdateTask(task.Slug, brain.TaskPatch{ClaimedBy: strPtr("other@another-host"), LeaseUntil: strPtr(future)})
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	batch, leases := claimTasks(context.Background(), brn, rt, []brain.TaskRecord{*updated}, 1)
	if len(batch) != 0 || len(leases) != 0 {
		t.Errorf("expected the leased candidate to be skipped, got batch=%v leases=%v", batch, leases)
	}
}

// TestValidateArbiterRejectsDockerSandbox covers requirement (7): --arbiter
// with --sandbox docker is refused (the lane runners are host CLIs, not
// provisioned into the docker sandbox).
func TestValidateArbiterRejectsDockerSandbox(t *testing.T) {
	o := defaultLoopOpts()
	o.arbiter = true
	o.worktree = true
	o.sandboxTier = "docker"
	if err := o.validate(); err == nil {
		t.Fatal("expected an error for --arbiter with --sandbox docker")
	}
}

// TestValidateArbiterAllowsNoSandbox is the control case for (7): --arbiter
// alone, with no sandbox tier, validates cleanly.
func TestValidateArbiterAllowsNoSandbox(t *testing.T) {
	o := defaultLoopOpts()
	o.arbiter = true
	if err := o.validate(); err != nil {
		t.Errorf("validate() = %v, want nil", err)
	}
}

// TestArbiterValidHandoffResumesOnNextLane is the positive twin of
// TestArbiterLaneEmptySignalMovesToNextLane: when the model DID write a
// correct HANDOFF.md during the attempt that then hit the lane's limit,
// the next lane must get ResumePrompt (continue from Left, do not
// restart), not ReconstructPrompt. This pins the attempt-numbering rule:
// the hand-off carries the attempt it was written in, and the resume
// check judges it against that attempt, not the one about to run.
func TestArbiterValidHandoffResumesOnNextLane(t *testing.T) {
	brn, store := setupArbiterGitBrain(t)

	lanes := &arbiter.Config{Lanes: []arbiter.Lane{
		oneLane("max", 1, "P1", []arbiter.DataClass{arbiter.DataClassPersonal}),
		oneLane("backup", 2, "P2", []arbiter.DataClass{arbiter.DataClassPersonal}),
	}}
	caps := []burndown.Capacity{
		{Provider: "P1", Label: "5-hour", UsedPct: 10, Headroom: 50},
		{Provider: "P2", Label: "5-hour", UsedPct: 10, Headroom: 50},
	}
	task, err := brn.AddTask("Test task resume", nil, "Do the thing in two halves.")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	fr := &fakeRunner{queue: []arbiter.RunResult{
		{Verdict: arbiter.VerdictEmpty, ExitCode: 1, Stderr: "usage limit reached", Matched: "usage limit reached"},
	}}
	fr.onRun = func(call int, _ arbiter.RunSpec) {
		if call != 1 {
			return
		}
		// The first attempt (attempt 1 on lane max) writes a real hand-off
		// with the attempt number the harness put in its template.
		st, lerr := taskstate.Load(brn.Path, task.Slug)
		if lerr != nil {
			t.Fatalf("Load state during attempt: %v", lerr)
		}
		body := taskstate.Template(task.TaskID, task.Slug, "max", st.Attempt, task.Title, "Do the thing in two halves.", st.Acceptance)
		body = strings.Replace(body, "## Done\n", "## Done\n- first half implemented\n", 1)
		body = strings.Replace(body, "## Left\n", "## Left\n- second half\n", 1)
		if werr := os.WriteFile(taskstate.HandoffPath(brn.Path, task.Slug), []byte(body), 0o644); werr != nil {
			t.Fatalf("write handoff: %v", werr)
		}
	}
	rt := newTestArbiterRuntime(t, brn.Path, lanes, caps, fr)
	lease, err := rt.claimer.Claim(context.Background(), task.Slug)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	o := testArbiterOpts()
	outcome := o.processArbiterTask(context.Background(), rt, brn, store, "aida", "work", task, lease, &loopState{})
	if outcome != outcomeDone {
		t.Fatalf("outcome = %v, want outcomeDone", outcome)
	}
	if len(fr.prompts) < 2 {
		t.Fatalf("expected at least 2 runner invocations, got %d", len(fr.prompts))
	}
	second := fr.prompts[1]
	if !strings.Contains(second, "do not restart") {
		t.Errorf("second prompt is not a resume prompt (missing 'do not restart'):\n%s", second)
	}
	if strings.Contains(strings.ToLower(second), "rebuild") {
		t.Errorf("second prompt is a reconstruct prompt although the hand-off was valid:\n%s", second)
	}
	if !strings.Contains(second, "second half") {
		t.Errorf("second prompt does not inline the hand-off's Left section:\n%s", second)
	}
	logBytes, _ := os.ReadFile(taskstate.LogPath(brn.Path, task.Slug))
	if strings.Contains(string(logBytes), "hand-off unusable") {
		t.Errorf("LOG.md reports the valid hand-off as unusable:\n%s", logBytes)
	}
}
