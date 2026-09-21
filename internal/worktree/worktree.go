// Package worktree manages throwaway git worktrees for isolated agent work.
// It is promoted out of the pr_work job logic (internal/jarvis/tools/jobs.go)
// so both the jobs daemon and `aida loop` can create and clean per-task
// worktrees without importing the voice-tools package.
//
// The model: each unit of autonomous work runs in its own worktree branched
// off a base ref (the base branch's upstream, e.g. public/main, for new work),
// so concurrent tasks never collide
// in a shared working tree and a failed attempt can be discarded by deleting a
// directory. The branch (and any commits on it) survive worktree removal, which
// is what lets Phase 3 push a branch and open a PR after the worktree is gone.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree is a checked-out git worktree.
type Worktree struct {
	Dir      string // absolute path to the worktree's working directory
	Branch   string // branch checked out (empty when detached)
	RepoRoot string // the parent repository's top-level directory
}

// RepoRoot returns the top-level directory of the git repository containing
// cwd.
func RepoRoot(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %w", cwd, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Create makes a worktree at dest off baseRef.
//
//   - branch != "": a new branch of that name is created at baseRef and checked
//     out in the worktree.
//   - branch == "": the worktree is detached at baseRef.
//
// baseRef "" defaults to the repo's current HEAD. When baseRef's first path
// segment names a configured remote (public/main, origin/main, upstream/x),
// that remote's branch is fetched first (best-effort) so the branch is cut
// from the latest remote tip rather than a stale local copy - the plan's
// "branch off the base branch's upstream" requirement that keeps autonomous
// PRs free of stray commits. Any other ref (a local branch, tag, or SHA) is
// used as-is.
//
// The parent directory of dest is created if needed.
func Create(repoRoot, dest, baseRef, branch string) (*Worktree, error) {
	if repoRoot == "" {
		return nil, fmt.Errorf("worktree.Create: empty repoRoot")
	}
	if dest == "" {
		return nil, fmt.Errorf("worktree.Create: empty dest")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktree parent: %w", err)
	}
	if remote := remoteOfRef(repoRoot, baseRef); remote != "" {
		remoteBranch := strings.TrimPrefix(baseRef, remote+"/")
		// Best-effort: an offline fetch failure falls back to whatever
		// <remote>/<branch> ref we already have locally.
		_ = exec.Command("git", "-C", repoRoot, "fetch", remote, remoteBranch).Run()
	}
	args := []string{"-C", repoRoot, "worktree", "add"}
	if branch != "" {
		args = append(args, "-b", branch, dest)
	} else {
		args = append(args, "--detach", dest)
	}
	if baseRef != "" {
		args = append(args, baseRef)
	}
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return &Worktree{Dir: dest, Branch: branch, RepoRoot: repoRoot}, nil
}

// Remove deletes the worktree's working directory via `git worktree remove
// --force`, falling back to a plain directory removal. The branch and any
// commits on it survive in the parent repository. No-op when dir is empty or
// already gone.
func Remove(repoRoot, dir string) error {
	if dir == "" {
		return nil
	}
	if _, err := os.Stat(dir); err != nil {
		return nil // already gone
	}
	if repoRoot != "" {
		if out, err := exec.Command("git", "-C", repoRoot, "worktree", "remove", "--force", dir).CombinedOutput(); err == nil {
			return nil
		} else {
			// Fall through to rm; keep the git error for context only.
			_ = out
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove worktree dir %s: %w", dir, err)
	}
	// Best-effort prune of the now-dangling administrative entry.
	if repoRoot != "" {
		_ = exec.Command("git", "-C", repoRoot, "worktree", "prune").Run()
	}
	return nil
}

// Remove tears down the worktree (see package Remove).
func (w *Worktree) Remove() error { return Remove(w.RepoRoot, w.Dir) }

// DeleteBranch force-deletes a local branch in repoRoot, ignoring "not found".
// Used to clear stale state from a prior crashed run before re-creating a
// deterministically-named worktree branch.
func DeleteBranch(repoRoot, branch string) {
	if repoRoot == "" || branch == "" {
		return
	}
	_ = exec.Command("git", "-C", repoRoot, "branch", "-D", branch).Run()
}
