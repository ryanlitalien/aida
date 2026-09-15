package jobs

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestClaimReturnsNoJobsAvailableOnEmpty(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	if _, err := store.Claim("host1", os.Getpid()); err != ErrNoJobsAvailable {
		t.Errorf("expected ErrNoJobsAvailable, got %v", err)
	}
}

func TestClaimTransitionsToRunning(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	enq, _ := store.Enqueue("draft", "a", "p-a", "q1", "")
	time.Sleep(1100 * time.Millisecond)
	enq2, _ := store.Enqueue("draft", "b", "p-b", "q2", "")

	claimed, err := store.Claim("host1", 12345)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Oldest first.
	if claimed.RunID != enq.RunID {
		t.Errorf("expected oldest=%s, got %s", enq.RunID, claimed.RunID)
	}
	if claimed.State != StateRunning {
		t.Errorf("expected running, got %q", claimed.State)
	}
	if claimed.ClaimedByHost != "host1" || claimed.ClaimedByPID != 12345 {
		t.Errorf("claim metadata wrong: host=%q pid=%d", claimed.ClaimedByHost, claimed.ClaimedByPID)
	}
	if claimed.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}

	// Manifest mirrors.
	m, _ := ReadManifest("work", claimed.RunID)
	if m.State != StateRunning || m.ClaimedByPID != 12345 {
		t.Errorf("manifest after claim wrong: %+v", m)
	}

	// PID file dropped.
	if pid := readPIDFile("work", claimed.RunID); pid != 12345 {
		t.Errorf("expected pid file=12345, got %d", pid)
	}

	// Second claim picks the next-oldest.
	claimed2, err := store.Claim("host1", 67890)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed2.RunID != enq2.RunID {
		t.Errorf("expected second=%s, got %s", enq2.RunID, claimed2.RunID)
	}

	// Third claim - empty.
	if _, err := store.Claim("host1", 11111); err != ErrNoJobsAvailable {
		t.Errorf("expected empty queue, got %v", err)
	}
}

func TestClaimConcurrentNoDoubleAssignment(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	// Pre-populate: 5 jobs, 10 concurrent claimers. Each job
	// should be claimed exactly once; 5 claimers should get
	// ErrNoJobsAvailable.
	for i := 0; i < 5; i++ {
		store.Enqueue("draft", "t", "", "q", "")
		time.Sleep(1100 * time.Millisecond)
	}

	const claimers = 10
	results := make([]*Job, claimers)
	errs := make([]error, claimers)
	var wg sync.WaitGroup
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.Claim("host1", 1000+i)
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	successes := 0
	for i, j := range results {
		if errs[i] == ErrNoJobsAvailable {
			continue
		}
		if errs[i] != nil {
			t.Errorf("claimer %d: unexpected error %v", i, errs[i])
			continue
		}
		seen[j.RunID]++
		successes++
	}
	if successes != 5 {
		t.Errorf("expected 5 successful claims, got %d", successes)
	}
	for runID, n := range seen {
		if n != 1 {
			t.Errorf("run %s claimed %d times - race!", runID, n)
		}
	}
}

func TestReapMarksDeadJobsFailed(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	// Spawn a short-lived child so we have a real-then-dead pid.
	cmd := exec.Command("sleep", "0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	deadPID := cmd.Process.Pid
	cmd.Wait() // child exits

	// Enqueue + Claim using the (now-dead) pid as a stand-in.
	store.Enqueue("draft", "a", "", "q1", "")
	host, _ := os.Hostname()

	// Bypass Claim's pid arg and synthesize the running-state row
	// directly via Reindex-style upsert so we control the host/pid.
	all, _ := store.List(ListOpts{})
	job := &all[0]
	job.State = StateRunning
	job.ClaimedByHost = host
	job.ClaimedByPID = deadPID
	job.StartedAt = time.Now().UTC()
	if err := WriteManifest("work", jobToManifest(job)); err != nil {
		t.Fatalf("write running manifest: %v", err)
	}
	if err := store.upsertRow(job); err != nil {
		t.Fatalf("upsert running: %v", err)
	}
	writePIDFile("work", job.RunID, deadPID)

	n, err := store.Reap()
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 reaped, got %d", n)
	}

	got, _ := store.Get(job.RunID)
	if got.State != StateFailed {
		t.Errorf("expected failed after reap, got %q", got.State)
	}

	// PID file removed.
	if _, err := os.Stat(PIDPath("work", job.RunID)); !os.IsNotExist(err) {
		t.Error("expected pid file removed after reap")
	}

	// Idempotent - second Reap touches nothing.
	n2, _ := store.Reap()
	if n2 != 0 {
		t.Errorf("expected 0 on second reap, got %d", n2)
	}
}

func TestReapLeavesLiveJobsAlone(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	store.Enqueue("draft", "a", "", "q1", "")
	host, _ := os.Hostname()
	all, _ := store.List(ListOpts{})
	job := &all[0]
	job.State = StateRunning
	job.ClaimedByHost = host
	job.ClaimedByPID = os.Getpid() // we are alive
	WriteManifest("work", jobToManifest(job))
	store.upsertRow(job)

	n, err := store.Reap()
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 reaped (own pid is alive), got %d", n)
	}
	got, _ := store.Get(job.RunID)
	if got.State != StateRunning {
		t.Errorf("expected still running, got %q", got.State)
	}
}

func TestReapIgnoresForeignHosts(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	store.Enqueue("draft", "a", "", "q1", "")
	all, _ := store.List(ListOpts{})
	job := &all[0]
	job.State = StateRunning
	job.ClaimedByHost = "some-other-machine.local"
	job.ClaimedByPID = 999999 // dead, but on a different host
	WriteManifest("work", jobToManifest(job))
	store.upsertRow(job)

	n, _ := store.Reap()
	if n != 0 {
		t.Errorf("expected 0 - foreign-host job left alone, got %d", n)
	}
}

func TestReindexFromOrphanManifests(t *testing.T) {
	withTempHome(t)

	// Step 1: open + enqueue + close.
	{
		store, _ := Open("work")
		store.Enqueue("draft", "a", "p-a", "q1", "")
		time.Sleep(1100 * time.Millisecond)
		store.Enqueue("draft", "b", "p-b", "q2", "")
		store.Close()
	}

	// Step 2: nuke jobs.db, leave manifests intact.
	if err := os.Remove(DBPath("work")); err != nil {
		t.Fatalf("remove db: %v", err)
	}
	// And the WAL sidecars, if any.
	os.Remove(DBPath("work") + "-wal")
	os.Remove(DBPath("work") + "-shm")

	// Step 3: reopen - schema is fresh, table is empty.
	store, _ := Open("work")
	defer store.Close()
	all, _ := store.List(ListOpts{})
	if len(all) != 0 {
		t.Fatalf("expected empty after wipe, got %d rows", len(all))
	}

	// Step 4: Reindex repopulates from disk.
	count, skipped, err := store.Reindex()
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if count != 2 || skipped != 0 {
		t.Errorf("expected 2 reindexed / 0 skipped, got %d / %d", count, skipped)
	}
	all, _ = store.List(ListOpts{})
	if len(all) != 2 {
		t.Errorf("expected 2 rows after reindex, got %d", len(all))
	}
}

func TestReindexSkipsBrokenManifests(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	// One legit job.
	job, _ := store.Enqueue("draft", "a", "", "q", "")

	// And a junk run-dir with a non-JSON manifest.
	junkDir := filepath.Join(RunsDir("work"), "20991231-235959-junk")
	os.MkdirAll(junkDir, 0755)
	os.WriteFile(filepath.Join(junkDir, "manifest.json"), []byte("not-json"), 0644)

	// And a runs/ entry that's not a directory at all.
	os.WriteFile(filepath.Join(RunsDir("work"), "stray-file.txt"), []byte("nope"), 0644)

	// Wipe DB to force a real reindex.
	store.Close()
	os.Remove(DBPath("work"))

	store2, _ := Open("work")
	defer store2.Close()
	count, skipped, err := store2.Reindex()
	if err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 indexed, got %d", count)
	}
	if skipped != 1 {
		t.Errorf("expected 1 skipped (junk manifest), got %d", skipped)
	}

	// The legit job is recovered.
	if _, err := store2.Get(job.RunID); err != nil {
		t.Errorf("expected job recovered, got %v", err)
	}
}

// MarkRunning flips a specific row to running with host+pid (the
// direct-spawn path that never goes through Claim), drops the pid file,
// and refuses terminal rows.
func TestMarkRunning(t *testing.T) {
	withTempHome(t)
	store, _ := Open("work")
	defer store.Close()

	j, _ := store.Enqueue("agent", "", "", "q", "")
	if err := store.MarkRunning(j.RunID, "myhost", 4242); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	got, _ := store.Get(j.RunID)
	if got.State != StateRunning || got.ClaimedByHost != "myhost" || got.ClaimedByPID != 4242 {
		t.Errorf("row after MarkRunning: state=%q host=%q pid=%d", got.State, got.ClaimedByHost, got.ClaimedByPID)
	}
	if got.StartedAt.IsZero() {
		t.Error("StartedAt not stamped")
	}
	if readPIDFile("work", j.RunID) != 4242 {
		t.Error("pid file not written")
	}

	// Terminal transition clears the pid file.
	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if readPIDFile("work", j.RunID) != 0 {
		t.Error("pid file should be removed on terminal transition")
	}

	// A cancelled row's process racing to start must not resurrect it.
	if err := store.MarkRunning(j.RunID, "myhost", 4243); err == nil {
		t.Error("MarkRunning should refuse a terminal row")
	}
}
