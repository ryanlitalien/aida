package taskstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"time"
)

// ErrHeld is returned by Claim when the lease is currently held by a
// different claimant and has not expired. A live holder is not a
// collision - the caller should move on to another task rather than
// retry.
var ErrHeld = errors.New("taskstate: lease held by another claimant")

// ErrContended is returned by Claim when every attempt was rejected as a
// collision (someone else was racing for the same, expired-or-absent
// lease) and the backoff policy's Attempts were exhausted.
var ErrContended = errors.New("taskstate: lease contended, retries exhausted")

// ErrLost is returned by Renew or Release when the ref no longer matches
// the caller's recorded commit - someone else took over an expired lease
// (Renew) or already released/replaced it (Release).
var ErrLost = errors.New("taskstate: lease lost, ref changed under us")

// Lease is the claim on one task, stored as LEASE.json inside a tiny
// commit pushed to refs/leases/<slug> on the brain repo's remote. Commit
// is the sha of that commit and is never persisted inside LEASE.json
// itself (json:"-") - it is metadata about where the lease lives, not
// part of the lease content.
type Lease struct {
	Slug      string    `json:"slug"`
	ClaimedBy string    `json:"claimed_by"`
	Host      string    `json:"host"`
	Lane      string    `json:"lane"`
	ClaimedAt time.Time `json:"claimed_at"`
	Until     time.Time `json:"lease_until"`
	Commit    string    `json:"-"`
}

// BackoffPolicy controls Claim's retry behavior on collision. Jitter is a
// fraction (0..1) of the computed delay to randomize by, so N claimers
// racing for the same lease don't retry in lockstep.
type BackoffPolicy struct {
	Base     time.Duration
	Max      time.Duration
	Attempts int
	Jitter   float64
}

func (b BackoffPolicy) withDefaults() BackoffPolicy {
	if b.Base <= 0 {
		b.Base = 300 * time.Millisecond
	}
	if b.Max <= 0 {
		b.Max = 5 * time.Second
	}
	if b.Attempts <= 0 {
		b.Attempts = 5
	}
	if b.Jitter <= 0 {
		b.Jitter = 0.5
	}
	return b
}

// delay computes the backoff duration before retry number `attempt`
// (0-indexed: the delay after the first failed attempt is attempt=0).
func (b BackoffPolicy) delay(attempt int) time.Duration {
	d := b.Base << uint(attempt)
	if d <= 0 || d > b.Max {
		d = b.Max
	}
	if b.Jitter > 0 {
		// Uniform in [1-Jitter, 1+Jitter], floored at 0.
		factor := 1 + (rand.Float64()*2-1)*b.Jitter
		if factor < 0 {
			factor = 0
		}
		d = time.Duration(float64(d) * factor)
	}
	return d
}

// Claimer claims, renews, and releases task leases against a single
// brain-repo remote. RepoDir is a local clone of that remote (leases are
// built as commits in its local object store before being pushed - see
// buildLeaseCommit); it never touches RepoDir's working tree or index.
type Claimer struct {
	RepoDir  string
	Remote   string
	Claimant string
	Host     string
	Lane     string
	TTL      time.Duration
	Now      func() time.Time
	Backoff  BackoffPolicy

	// push and sleep are overridable for tests (see lease_test.go's
	// collision-path unit test); the zero value uses the real
	// implementations.
	push  func(ctx context.Context, repoDir, remote, slug, commitSHA, expectedSHA string) error
	sleep func(time.Duration)
}

func (c *Claimer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Claimer) pushFunc() func(ctx context.Context, repoDir, remote, slug, commitSHA, expectedSHA string) error {
	if c.push != nil {
		return c.push
	}
	return realPush
}

func (c *Claimer) sleepFunc() func(time.Duration) {
	if c.sleep != nil {
		return c.sleep
	}
	return time.Sleep
}

func leaseRef(slug string) string {
	return "refs/leases/" + slug
}

// Claim attempts to acquire the lease for slug. It never blocks
// indefinitely: at most Backoff.Attempts pushes are tried, with jittered
// exponential backoff between collisions (docs/arbiter-plan.md section 6,
// decision 3). A lease currently held by someone else and not expired
// returns ErrHeld immediately (not a retryable condition - the caller
// should pick a different task); a genuine race for an absent or expired
// lease returns ErrContended once retries are exhausted.
func (c *Claimer) Claim(ctx context.Context, slug string) (*Lease, error) {
	backoff := c.Backoff.withDefaults()

	for attempt := 0; attempt < backoff.Attempts; attempt++ {
		current, err := ReadLease(ctx, c.RepoDir, c.Remote, slug)
		if err != nil {
			return nil, fmt.Errorf("reading current lease: %w", err)
		}

		now := c.now()
		expected := ""
		if current != nil {
			if current.ClaimedBy != c.Claimant && now.Before(current.Until) {
				return nil, fmt.Errorf("%w: held by %s@%s until %s", ErrHeld, current.ClaimedBy, current.Host, current.Until)
			}
			expected = current.Commit
		}

		newLease := Lease{
			Slug:      slug,
			ClaimedBy: c.Claimant,
			Host:      c.Host,
			Lane:      c.Lane,
			ClaimedAt: now,
			Until:     now.Add(c.TTL),
		}
		commitSHA, err := buildLeaseCommit(ctx, c.RepoDir, newLease)
		if err != nil {
			return nil, fmt.Errorf("building lease commit: %w", err)
		}

		pushErr := c.pushFunc()(ctx, c.RepoDir, c.Remote, slug, commitSHA, expected)
		if pushErr == nil {
			newLease.Commit = commitSHA
			return &newLease, nil
		}
		if !isCollision(pushErr) {
			return nil, fmt.Errorf("pushing lease: %w", pushErr)
		}
		if attempt < backoff.Attempts-1 {
			c.sleepFunc()(backoff.delay(attempt))
		}
	}
	return nil, fmt.Errorf("%w: slug %s", ErrContended, slug)
}

// Renew extends a lease we currently hold by TTL, via a compare-and-swap
// from l.Commit. Returns ErrLost if the ref no longer matches l.Commit -
// someone else already took over (the lease must have expired for that
// to happen; see Claim).
func (c *Claimer) Renew(ctx context.Context, l *Lease) (*Lease, error) {
	now := c.now()
	renewed := *l
	renewed.Until = now.Add(c.TTL)

	commitSHA, err := buildLeaseCommit(ctx, c.RepoDir, renewed)
	if err != nil {
		return nil, fmt.Errorf("building lease commit: %w", err)
	}

	err = c.pushFunc()(ctx, c.RepoDir, c.Remote, l.Slug, commitSHA, l.Commit)
	if err != nil {
		if isCollision(err) {
			return nil, fmt.Errorf("%w: %v", ErrLost, err)
		}
		return nil, fmt.Errorf("pushing renewed lease: %w", err)
	}
	renewed.Commit = commitSHA
	return &renewed, nil
}

// Release deletes the lease ref via a compare-and-swap from l.Commit.
// Returns ErrLost if the ref had already changed (someone else's Claim
// took it over as expired, or it was already released) - the caller no
// longer controls this lease's fate regardless.
func (c *Claimer) Release(ctx context.Context, l *Lease) error {
	ref := leaseRef(l.Slug)
	refspec := ":" + ref
	leaseFlag := fmt.Sprintf("--force-with-lease=%s:%s", ref, l.Commit)

	_, stderr, err := runGit(ctx, c.RepoDir, "push", c.Remote, refspec, leaseFlag)
	if err != nil {
		wrapped := fmt.Errorf("git push --delete %s: %w (%s)", ref, err, strings.TrimSpace(stderr))
		if isCollision(wrapped) {
			return fmt.Errorf("%w: %v", ErrLost, wrapped)
		}
		return wrapped
	}
	return nil
}

// ReadLease reads the current lease for slug from remote, if any. Returns
// (nil, nil) when no refs/leases/<slug> ref exists yet - that is not an
// error, it just means the task has never been claimed (or was released
// and never re-claimed).
func ReadLease(ctx context.Context, repoDir, remote, slug string) (*Lease, error) {
	ref := leaseRef(slug)

	out, stderr, err := runGit(ctx, repoDir, "ls-remote", remote, ref)
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %s %s: %w (%s)", remote, ref, err, strings.TrimSpace(stderr))
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil, fmt.Errorf("unexpected ls-remote output for %s: %q", ref, out)
	}
	sha := fields[0]

	// Fetch the lease commit's objects into the local repo so cat-file
	// can read the blob content below. This does not touch the working
	// tree or any local branch - it only populates the local object
	// store, and does not update any local ref (no refspec destination).
	if _, stderr, err := runGit(ctx, repoDir, "fetch", "--quiet", remote, ref); err != nil {
		return nil, fmt.Errorf("git fetch %s %s: %w (%s)", remote, ref, err, strings.TrimSpace(stderr))
	}

	blob, stderr, err := runGit(ctx, repoDir, "cat-file", "-p", sha+":LEASE.json")
	if err != nil {
		return nil, fmt.Errorf("git cat-file %s:LEASE.json: %w (%s)", sha, err, strings.TrimSpace(stderr))
	}

	var l Lease
	if err := json.Unmarshal([]byte(blob), &l); err != nil {
		return nil, fmt.Errorf("parsing LEASE.json from %s: %w", sha, err)
	}
	l.Commit = sha
	return &l, nil
}

// buildLeaseCommit writes LEASE.json as a blob, wraps it in a single-file
// tree, and commits that tree - all via plumbing commands that never
// touch repoDir's working tree or index (docs/arbiter-plan.md section 6:
// "no broker yet", but also no accidental interference with whatever the
// caller's worktree is doing). The returned sha is not yet pushed
// anywhere.
func buildLeaseCommit(ctx context.Context, repoDir string, l Lease) (string, error) {
	data, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("marshaling LEASE.json: %w", err)
	}

	blobSHA, err := hashObjectBlob(ctx, repoDir, data)
	if err != nil {
		return "", err
	}
	treeSHA, err := mkTreeSingleBlob(ctx, repoDir, blobSHA, "LEASE.json")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("lease: %s claimed_by=%s until=%s\n", l.Slug, l.ClaimedBy, l.Until.UTC().Format(time.RFC3339))
	commitSHA, err := commitTreeFromStdin(ctx, repoDir, treeSHA, msg)
	if err != nil {
		return "", err
	}
	return commitSHA, nil
}

func hashObjectBlob(ctx context.Context, repoDir string, data []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "hash-object", "-w", "--stdin")
	cmd.Dir = repoDir
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git hash-object: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func mkTreeSingleBlob(ctx context.Context, repoDir, blobSHA, filename string) (string, error) {
	entry := fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, filename)
	cmd := exec.CommandContext(ctx, "git", "mktree")
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(entry)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git mktree: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// commitTreeFromStdin passes the commit message via stdin rather than
// -m, matching the repo's own hook-safety convention for git commit (see
// BRIEF.md) even though commit-tree isn't guarded by that hook - it costs
// nothing and avoids ever putting a lease message on a command line.
func commitTreeFromStdin(ctx context.Context, repoDir, treeSHA, message string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "commit-tree", treeSHA)
	cmd.Dir = repoDir
	cmd.Stdin = strings.NewReader(message)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git commit-tree: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// realPush performs the actual compare-and-swap push of a lease commit to
// refs/leases/<slug>. expectedSHA == "" means the ref must not currently
// exist on remote.
func realPush(ctx context.Context, repoDir, remote, slug, commitSHA, expectedSHA string) error {
	ref := leaseRef(slug)
	refspec := commitSHA + ":" + ref
	leaseFlag := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA)

	_, stderr, err := runGit(ctx, repoDir, "push", remote, refspec, leaseFlag)
	if err != nil {
		return fmt.Errorf("git push %s %s: %w (%s)", remote, refspec, err, strings.TrimSpace(stderr))
	}
	return nil
}

// collisionMarkers are the substrings git's push output uses for a
// rejected compare-and-swap: someone else changed the ref between our
// read and our push. This is the expected, retryable outcome of two
// claimers racing (docs/arbiter-plan.md section 6, decision 3) - not an
// error condition on its own.
var collisionMarkers = []string{
	"rejected",
	"stale info",
	"failed to lock",
	"cannot lock ref",
	"non-fast-forward",
}

func isCollision(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range collisionMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func runGit(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}
