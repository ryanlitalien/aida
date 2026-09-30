package taskstate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func runOK(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v (dir=%s): %v\n%s", name, args, dir, err, out)
	}
}

func configUser(t *testing.T, dir string) {
	t.Helper()
	runOK(t, dir, "git", "config", "user.email", "test@example.com")
	runOK(t, dir, "git", "config", "user.name", "Test")
}

// newBareRemote creates a bare repo with one commit on main (so HEAD
// exists), returning its path.
func newBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	runOK(t, root, "git", "init", "--bare", "-q", bare)

	seed := filepath.Join(root, "seed")
	runOK(t, root, "git", "clone", "-q", bare, seed)
	configUser(t, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runOK(t, seed, "git", "add", "README.md")
	runOK(t, seed, "git", "commit", "-q", "-m", "seed")
	runOK(t, seed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	return bare
}

// newClone clones bare into a fresh directory with a user identity set.
func newClone(t *testing.T, bare string) string {
	t.Helper()
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	runOK(t, root, "git", "clone", "-q", bare, clone)
	configUser(t, clone)
	return clone
}

func TestClaim_FreshClaimSucceeds(t *testing.T) {
	bare := newBareRemote(t)
	clone := newClone(t, bare)
	ctx := context.Background()

	c := &Claimer{
		RepoDir:  clone,
		Remote:   "origin",
		Claimant: "claude-max@edith",
		Host:     "edith",
		Lane:     "claude-max",
		TTL:      5 * time.Minute,
	}

	l, err := c.Claim(ctx, "fix-the-thing")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if l.Slug != "fix-the-thing" || l.ClaimedBy != "claude-max@edith" || l.Commit == "" {
		t.Errorf("unexpected lease: %+v", l)
	}

	got, err := ReadLease(ctx, clone, "origin", "fix-the-thing")
	if err != nil {
		t.Fatalf("ReadLease: %v", err)
	}
	if got == nil {
		t.Fatal("ReadLease returned nil after a successful claim")
	}
	if got.ClaimedBy != "claude-max@edith" || got.Commit != l.Commit {
		t.Errorf("ReadLease mismatch: %+v vs claimed %+v", got, l)
	}
}

func TestReadLease_AbsentReturnsNilNil(t *testing.T) {
	bare := newBareRemote(t)
	clone := newClone(t, bare)
	l, err := ReadLease(context.Background(), clone, "origin", "never-claimed")
	if err != nil {
		t.Fatalf("ReadLease: %v", err)
	}
	if l != nil {
		t.Errorf("expected nil lease, got %+v", l)
	}
}

func TestClaim_SecondClaimerGetsErrHeldWhileLive(t *testing.T) {
	bare := newBareRemote(t)
	cloneA := newClone(t, bare)
	cloneB := newClone(t, bare)
	ctx := context.Background()

	a := &Claimer{RepoDir: cloneA, Remote: "origin", Claimant: "a", Host: "edith", Lane: "claude-max", TTL: 5 * time.Minute}
	b := &Claimer{RepoDir: cloneB, Remote: "origin", Claimant: "b", Host: "minty", Lane: "claude-max", TTL: 5 * time.Minute}

	if _, err := a.Claim(ctx, "shared-task"); err != nil {
		t.Fatalf("a.Claim: %v", err)
	}

	_, err := b.Claim(ctx, "shared-task")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("b.Claim() = %v, want ErrHeld", err)
	}
}

func TestClaim_ExpiredLeaseTakenOver_OldHolderRenewFails(t *testing.T) {
	bare := newBareRemote(t)
	cloneA := newClone(t, bare)
	cloneB := newClone(t, bare)
	ctx := context.Background()

	t0 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	a := &Claimer{
		RepoDir: cloneA, Remote: "origin", Claimant: "a", Host: "edith", Lane: "claude-max",
		TTL: 1 * time.Minute,
		Now: func() time.Time { return t0 },
	}
	aLease, err := a.Claim(ctx, "shared-task")
	if err != nil {
		t.Fatalf("a.Claim: %v", err)
	}

	// b claims after a's lease has expired.
	tLater := t0.Add(5 * time.Minute)
	b := &Claimer{
		RepoDir: cloneB, Remote: "origin", Claimant: "b", Host: "minty", Lane: "claude-max",
		TTL: 1 * time.Minute,
		Now: func() time.Time { return tLater },
	}
	bLease, err := b.Claim(ctx, "shared-task")
	if err != nil {
		t.Fatalf("b.Claim (expired takeover): %v", err)
	}
	if bLease.ClaimedBy != "b" {
		t.Errorf("expected b to hold the lease, got %+v", bLease)
	}

	// a no longer holds it - renew must fail with ErrLost.
	_, err = a.Renew(ctx, aLease)
	if !errors.Is(err, ErrLost) {
		t.Fatalf("a.Renew() after takeover = %v, want ErrLost", err)
	}
}

func TestReleaseThenReclaim(t *testing.T) {
	bare := newBareRemote(t)
	cloneA := newClone(t, bare)
	cloneB := newClone(t, bare)
	ctx := context.Background()

	a := &Claimer{RepoDir: cloneA, Remote: "origin", Claimant: "a", Host: "edith", Lane: "claude-max", TTL: 5 * time.Minute}
	aLease, err := a.Claim(ctx, "shared-task")
	if err != nil {
		t.Fatalf("a.Claim: %v", err)
	}

	if err := a.Release(ctx, aLease); err != nil {
		t.Fatalf("a.Release: %v", err)
	}

	got, err := ReadLease(ctx, cloneB, "origin", "shared-task")
	if err != nil {
		t.Fatalf("ReadLease after release: %v", err)
	}
	if got != nil {
		t.Errorf("expected no lease after release, got %+v", got)
	}

	b := &Claimer{RepoDir: cloneB, Remote: "origin", Claimant: "b", Host: "minty", Lane: "claude-max", TTL: 5 * time.Minute}
	bLease, err := b.Claim(ctx, "shared-task")
	if err != nil {
		t.Fatalf("b.Claim after release: %v", err)
	}
	if bLease.ClaimedBy != "b" {
		t.Errorf("expected b to hold the lease, got %+v", bLease)
	}
}

func TestRelease_ErrLostOnStaleCommit(t *testing.T) {
	bare := newBareRemote(t)
	cloneA := newClone(t, bare)
	ctx := context.Background()

	a := &Claimer{RepoDir: cloneA, Remote: "origin", Claimant: "a", Host: "edith", Lane: "claude-max", TTL: 5 * time.Minute}
	aLease, err := a.Claim(ctx, "shared-task")
	if err != nil {
		t.Fatalf("a.Claim: %v", err)
	}
	// Corrupt the recorded commit so the CAS on release fails as if
	// someone else changed the ref underneath us.
	stale := *aLease
	stale.Commit = "0000000000000000000000000000000000000000"
	if err := a.Release(ctx, &stale); !errors.Is(err, ErrLost) {
		t.Fatalf("Release with stale commit = %v, want ErrLost", err)
	}
}

// TestClaim_Contention is the N-machine collision test: six goroutines,
// each its own clone and Claimer, all Claim the same slug concurrently.
// Exactly one must win; every other must report ErrHeld or ErrContended;
// the remote must end up with exactly one refs/leases/<slug> ref. Run
// with -count=3 locally to check for flakiness.
func TestClaim_Contention(t *testing.T) {
	const n = 6
	bare := newBareRemote(t)
	ctx := context.Background()

	claimers := make([]*Claimer, n)
	for i := 0; i < n; i++ {
		claimers[i] = &Claimer{
			RepoDir:  newClone(t, bare),
			Remote:   "origin",
			Claimant: filepath.Base(t.TempDir()), // unique per goroutine
			Host:     "host",
			Lane:     "claude-max",
			TTL:      5 * time.Minute,
			Backoff:  BackoffPolicy{Base: 20 * time.Millisecond, Max: 200 * time.Millisecond, Attempts: 10, Jitter: 0.5},
		}
	}

	var wg sync.WaitGroup
	results := make([]*Lease, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := claimers[i].Claim(ctx, "contended-task")
			results[i] = l
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			winners++
			if results[i] == nil {
				t.Errorf("goroutine %d: nil error but nil lease", i)
			}
			continue
		}
		if !errors.Is(errs[i], ErrHeld) && !errors.Is(errs[i], ErrContended) {
			t.Errorf("goroutine %d: unexpected error %v, want ErrHeld or ErrContended", i, errs[i])
		}
	}
	if winners != 1 {
		t.Errorf("winners = %d, want exactly 1 (errs: %v)", winners, errs)
	}

	// Exactly one ref on the remote.
	out, stderr, err := runGit(ctx, claimers[0].RepoDir, "ls-remote", "origin", "refs/leases/contended-task")
	if err != nil {
		t.Fatalf("ls-remote: %v (%s)", err, stderr)
	}
	lines := 0
	for _, line := range splitNonEmptyLines(out) {
		_ = line
		lines++
	}
	if lines != 1 {
		t.Errorf("expected exactly 1 lease ref on remote, ls-remote output: %q", out)
	}
}

func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}

// TestClaim_CollisionPathBacksOffAndRetries fakes the push CAS to reject
// twice with a collision marker before succeeding on the third attempt,
// and asserts Claim retried exactly three times with backoff sleeps
// requested between attempts (not after the final success).
func TestClaim_CollisionPathBacksOffAndRetries(t *testing.T) {
	bare := newBareRemote(t)
	clone := newClone(t, bare)
	ctx := context.Background()

	var pushCalls int
	var sleeps []time.Duration

	c := &Claimer{
		RepoDir:  clone,
		Remote:   "origin",
		Claimant: "a",
		Host:     "edith",
		Lane:     "claude-max",
		TTL:      5 * time.Minute,
		Backoff:  BackoffPolicy{Base: time.Millisecond, Max: 10 * time.Millisecond, Attempts: 5, Jitter: 0},
		push: func(ctx context.Context, repoDir, remote, slug, commitSHA, expectedSHA string) error {
			pushCalls++
			if pushCalls < 3 {
				return errors.New("! [rejected] refs/leases/x (stale info)")
			}
			return nil
		},
		sleep: func(d time.Duration) {
			sleeps = append(sleeps, d)
		},
	}

	l, err := c.Claim(ctx, "fake-collision-task")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if l == nil {
		t.Fatal("expected a lease on eventual success")
	}
	if pushCalls != 3 {
		t.Errorf("pushCalls = %d, want 3", pushCalls)
	}
	if len(sleeps) != 2 {
		t.Errorf("len(sleeps) = %d, want 2 (backoff only between failed attempts)", len(sleeps))
	}
}

func TestIsCollision(t *testing.T) {
	tests := []struct {
		errMsg string
		want   bool
	}{
		{"! [rejected] refs/leases/x -> refs/leases/x (stale info)", true},
		{"error: failed to push some refs", false},
		{"cannot lock ref 'refs/leases/x': is at abc123 but expected def456", true},
		{"non-fast-forward", true},
		{"fatal: unable to access remote", false},
	}
	for _, tc := range tests {
		got := isCollision(errors.New(tc.errMsg))
		if got != tc.want {
			t.Errorf("isCollision(%q) = %v, want %v", tc.errMsg, got, tc.want)
		}
	}
}
