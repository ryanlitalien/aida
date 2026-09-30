package brain

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// FFStatus is the outcome of FetchAndFastForward.
type FFStatus string

const (
	// FFUpToDate means HEAD already matches the upstream branch; nothing
	// to do.
	FFUpToDate FFStatus = "up_to_date"
	// FFFastForwarded means the local branch was moved forward to
	// upstream via a clean fast-forward merge.
	FFFastForwarded FFStatus = "fast_forwarded"
	// FFRemoteAhead means the fetch succeeded but a fast-forward was not
	// possible - local has commits upstream doesn't (or the two have
	// diverged) - so nothing was touched.
	FFRemoteAhead FFStatus = "remote_ahead"
	// FFOffline means `git fetch` itself failed (unreachable remote,
	// bad URL, or it didn't finish within the caller's timeout).
	FFOffline FFStatus = "offline"
	// FFNotRepo means brainPath is not a git repository at all.
	FFNotRepo FFStatus = "not_repo"
)

// FetchAndFastForward is the read side's twin of the pull AddTask already
// does before allocating a task id (aida task #498, docs/arbiter-plan.md
// section 6: "the brain's read side must fetch and fast-forward (or
// refuse to pick when the remote is ahead) before listing or claiming").
// A picker or claimer calls this first so it never lists or leases a task
// off a stale view of the brain repo.
//
// It never rebases, never stashes, and never touches the working tree
// beyond a strictly clean fast-forward: if HEAD can't be fast-forwarded
// onto the upstream branch (local has unpushed commits, or the two have
// diverged), it reports FFRemoteAhead and leaves the repo exactly as it
// found it rather than guessing at a merge or rebase strategy.
func FetchAndFastForward(brainPath string, timeout time.Duration) (FFStatus, error) {
	if !IsGitRepo(brainPath) {
		return FFNotRepo, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if _, stderr, err := runFFGit(ctx, brainPath, "fetch", "--quiet"); err != nil {
		return FFOffline, fmt.Errorf("git fetch: %w (%s)", err, strings.TrimSpace(stderr))
	}

	head, _, err := runFFGit(ctx, brainPath, "rev-parse", "HEAD")
	if err != nil {
		return FFOffline, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	upstream, stderr, err := runFFGit(ctx, brainPath, "rev-parse", "@{upstream}")
	if err != nil {
		// No upstream configured for the current branch - nothing to
		// fast-forward against. Treat as up to date rather than an
		// error: a brain repo with no remote tracking branch (e.g. a
		// fresh `aida brain init` with no push yet) is a valid state.
		return FFUpToDate, nil
	}
	_ = stderr

	if strings.TrimSpace(head) == strings.TrimSpace(upstream) {
		return FFUpToDate, nil
	}

	if _, stderr, err := runFFGit(ctx, brainPath, "merge", "--ff-only", "--quiet", "@{upstream}"); err != nil {
		return FFRemoteAhead, fmt.Errorf("git merge --ff-only: %w (%s)", err, strings.TrimSpace(stderr))
	}
	return FFFastForwarded, nil
}

func runFFGit(ctx context.Context, dir string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}
