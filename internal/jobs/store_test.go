package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempHome redirects ~/.aida to a per-test tmp dir. Returns the
// tmp path so tests can inspect on-disk state directly.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestOpenCreatesDirectories(t *testing.T) {
	withTempHome(t)
	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(Root("work")); err != nil {
		t.Errorf("profile root not created: %v", err)
	}
	if _, err := os.Stat(RunsDir("work")); err != nil {
		t.Errorf("runs subdir not created: %v", err)
	}
	if _, err := os.Stat(DBPath("work")); err != nil {
		t.Errorf("jobs.db not created: %v", err)
	}
}

func TestOpenRejectsEmptyProfile(t *testing.T) {
	withTempHome(t)
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") should fail")
	}
}

func TestEnqueueWritesManifestAndRow(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	job, err := store.Enqueue("draft", "buy-milk", "20260506-buy-milk",
		"draft a response about milk", "file:/tmp/foo.md")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if job.State != StateQueued {
		t.Errorf("expected state=queued, got %q", job.State)
	}
	if job.Profile != "work" {
		t.Errorf("expected profile=work, got %q", job.Profile)
	}
	if job.RunID == "" {
		t.Error("expected non-empty run id")
	}

	// Manifest on disk.
	m, err := ReadManifest("work", job.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.State != StateQueued || m.TaskSlug != "buy-milk" || m.PlanID != "20260506-buy-milk" {
		t.Errorf("manifest fields wrong: %+v", m)
	}
	if m.Version != manifestVersion {
		t.Errorf("expected version=%d, got %d", manifestVersion, m.Version)
	}

	// SQL row.
	got, err := store.Get(job.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.RunID != job.RunID || got.Profile != "work" {
		t.Errorf("Get returned wrong row: %+v", got)
	}
}

func TestGetMissingReturnsErrNotFound(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	_, err := store.Get("does-not-exist")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListFilters(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	// Three jobs across two task slugs and two states.
	a, _ := store.Enqueue("draft", "alpha", "p-alpha", "q1", "")
	time.Sleep(1100 * time.Millisecond) // run-id has 1-second resolution
	b, _ := store.Enqueue("draft", "beta", "p-beta", "q2", "")
	time.Sleep(1100 * time.Millisecond)
	c, _ := store.Enqueue("draft", "alpha", "p-alpha-2", "q3", "")

	if err := store.Complete(b.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	all, err := store.List(ListOpts{})
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 rows, got %d", len(all))
	}
	// Newest first.
	if all[0].RunID != c.RunID {
		t.Errorf("expected newest=c, got %s", all[0].RunID)
	}

	queued, _ := store.List(ListOpts{State: StateQueued})
	if len(queued) != 2 {
		t.Errorf("expected 2 queued, got %d", len(queued))
	}

	alpha, _ := store.List(ListOpts{TaskSlug: "alpha"})
	if len(alpha) != 2 {
		t.Errorf("expected 2 alpha rows, got %d", len(alpha))
	}

	limited, _ := store.List(ListOpts{Limit: 1})
	if len(limited) != 1 {
		t.Errorf("expected 1 row with Limit=1, got %d", len(limited))
	}

	_ = a // keep referenced - silences lint and documents intent
}

func TestCompleteAndFailTransitions(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	j1, _ := store.Enqueue("draft", "a", "", "q1", "")
	if err := store.Complete(j1.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _ := store.Get(j1.RunID)
	if got.State != StateDone {
		t.Errorf("expected done, got %q", got.State)
	}
	if got.FinishedAt.IsZero() {
		t.Error("expected non-zero FinishedAt")
	}

	j2, _ := store.Enqueue("draft", "b", "", "q2", "")
	if err := store.Fail(j2.RunID, "boom"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	got2, _ := store.Get(j2.RunID)
	if got2.State != StateFailed {
		t.Errorf("expected failed, got %q", got2.State)
	}
	if got2.Error != "boom" {
		t.Errorf("expected error='boom', got %q", got2.Error)
	}

	// Manifests on disk reflect the new state.
	m1, _ := ReadManifest("work", j1.RunID)
	if m1.State != StateDone {
		t.Errorf("manifest 1: expected done, got %q", m1.State)
	}
	m2, _ := ReadManifest("work", j2.RunID)
	if m2.State != StateFailed || m2.Error != "boom" {
		t.Errorf("manifest 2 wrong: %+v", m2)
	}
}

func TestProfileIsolation(t *testing.T) {
	withTempHome(t)
	work, _ := Open("work")
	defer work.Close()
	home, _ := Open("home")
	defer home.Close()

	wj, _ := work.Enqueue("draft", "a", "", "work-q", "")
	hj, _ := home.Enqueue("draft", "a", "", "home-q", "")

	// Each store sees only its own row.
	if _, err := work.Get(hj.RunID); err != ErrNotFound {
		t.Errorf("work store leaked home row: %v", err)
	}
	if _, err := home.Get(wj.RunID); err != ErrNotFound {
		t.Errorf("home store leaked work row: %v", err)
	}

	// And the on-disk paths are separate.
	if !strings.Contains(ManifestPath("work", wj.RunID), filepath.Join("jobs", "work")) {
		t.Errorf("work manifest path missing /jobs/work/: %s", ManifestPath("work", wj.RunID))
	}
	if !strings.Contains(ManifestPath("home", hj.RunID), filepath.Join("jobs", "home")) {
		t.Errorf("home manifest path missing /jobs/home/: %s", ManifestPath("home", hj.RunID))
	}
}

func TestManifestAtomicWriteOverwrite(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	job, _ := store.Enqueue("draft", "a", "", "q", "")
	// Verify no leftover tmp files in the run dir.
	dir := RunDir("work", job.RunID)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "manifest.json.tmp") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
	// Update via Fail and re-check.
	store.Fail(job.RunID, "x")
	entries2, _ := os.ReadDir(dir)
	for _, e := range entries2 {
		if strings.HasPrefix(e.Name(), "manifest.json.tmp") {
			t.Errorf("leftover tmp file after Fail: %s", e.Name())
		}
	}
}

// First terminal state wins: the agent's exit path, the spawner's
// cmd.Wait goroutine, and a user cancel can all race to finish a row -
// a late Fail must not overwrite Complete, and a killed process's exit
// error must not clobber "cancelled by user".
func TestTerminalStateIsSticky(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	j, _ := store.Enqueue("agent", "", "", "q", "")
	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := store.Fail(j.RunID, "signal: terminated"); err != nil {
		t.Fatalf("late Fail should be a silent no-op, got %v", err)
	}
	got, _ := store.Get(j.RunID)
	if got.State != StateDone {
		t.Errorf("state = %q after late Fail, want done", got.State)
	}
	if got.Error != "" {
		t.Errorf("error = %q after late Fail, want empty", got.Error)
	}

	// Cancelled-then-reaped: the cancel's message survives.
	j2, _ := store.Enqueue("agent", "", "", "q2", "")
	store.Fail(j2.RunID, "cancelled by user")
	store.Fail(j2.RunID, "signal: killed")
	got2, _ := store.Get(j2.RunID)
	if got2.Error != "cancelled by user" {
		t.Errorf("error = %q, want the first failure message", got2.Error)
	}
}
