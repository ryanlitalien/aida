package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isGitRepo returns true if dir is inside a git work tree. Used by
// the delegate tool to decide whether worktree-per-delegation
// isolation is even possible - we can't spawn a worktree on a
// non-git directory.
func isGitRepo(dir string) bool {
	if dir == "" {
		return false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// spawnDelegateWorktree creates a fresh detached-HEAD git worktree
// off the current HEAD of repoDir and returns its absolute path
// plus a cleanup function. The cleanup must be called regardless
// of whether the caller succeeds - the worktree directory will
// otherwise persist under /tmp and pollute future runs.
//
// Per OpenAI's harness writeup: "we made the app bootable per git
// worktree, so Codex could launch and drive one instance per
// change." This is the aida equivalent - `delegate_to_claude_code`
// runs claude in an isolated worktree so changes don't bleed into
// the user's working tree until they explicitly apply the diff.
//
// Returns an error when repoDir isn't a git work tree, when the
// `git worktree add` command fails, or when the temp directory
// can't be created. Caller treats any error as "isolation not
// available - fall back."
func spawnDelegateWorktree(repoDir string) (path string, cleanup func(), err error) {
	if !isGitRepo(repoDir) {
		return "", nil, fmt.Errorf("not a git repository: %s", repoDir)
	}

	tmp, err := os.MkdirTemp("", "aida-delegate-*")
	if err != nil {
		return "", nil, fmt.Errorf("mkdir temp: %w", err)
	}

	// `git worktree add --detach <path> HEAD` creates a worktree
	// at HEAD without creating a new branch. Detached so we don't
	// pollute the user's branch namespace with one-shot delegates.
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", "--detach", tmp, "HEAD")
	if out, addErr := cmd.CombinedOutput(); addErr != nil {
		_ = os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("git worktree add: %w\n%s", addErr, string(out))
	}

	cleanup = func() {
		// `git worktree remove --force <path>` un-registers it
		// from the repo's worktree list. Force is needed because
		// the worktree often has uncommitted changes from claude's
		// edits. RemoveAll handles the case where the repo
		// itself was deleted out from under us.
		_ = exec.Command("git", "-C", repoDir, "worktree", "remove", "--force", tmp).Run()
		_ = os.RemoveAll(tmp)
	}
	abs, err := filepath.Abs(tmp)
	if err != nil {
		// Path resolution shouldn't really fail here, but if it
		// does, fall back to the original tmp path.
		abs = tmp
	}
	return abs, cleanup, nil
}

// captureWorktreeDiff returns the unified diff of all changes in
// worktreeDir relative to its HEAD. Includes both staged and
// unstaged changes plus untracked files (via the ls-files +
// diff-files combination).
//
// Errors are folded into the returned string rather than returned
// as Go errors - the diff is informational; a diff-capture failure
// shouldn't abort the agent's tool call. Empty string means
// "claude touched nothing."
func captureWorktreeDiff(worktreeDir string) string {
	if worktreeDir == "" {
		return ""
	}

	// Tracked changes: git diff HEAD captures both staged and
	// unstaged modifications to files git already knows about.
	tracked, _ := exec.Command("git", "-C", worktreeDir, "diff", "HEAD").Output()

	// Untracked files: list them, then synthesize a fake "new
	// file" diff for each. git diff HEAD doesn't include
	// untracked content otherwise.
	untrackedList, _ := exec.Command("git", "-C", worktreeDir, "ls-files",
		"--others", "--exclude-standard").Output()

	var b strings.Builder
	b.Write(tracked)
	for _, name := range strings.Split(strings.TrimSpace(string(untrackedList)), "\n") {
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "\n--- /dev/null\n+++ %s (new file)\n", name)
		// Read up to ~64KB of the new file - large untracked
		// blobs (build artifacts, binaries) shouldn't bloat the
		// agent's response context.
		data, err := os.ReadFile(filepath.Join(worktreeDir, name))
		if err != nil {
			fmt.Fprintf(&b, "(read error: %s)\n", err.Error())
			continue
		}
		const max = 64 * 1024
		if len(data) > max {
			data = append(data[:max], []byte("\n... (truncated)")...)
		}
		b.WriteString(string(data))
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}
