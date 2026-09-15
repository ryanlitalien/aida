package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitInitRepo creates a throwaway git repo with one commit and returns its path.
func gitInitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestCreateAndRemove(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root, err := RepoRoot(gitInitRepo(t))
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "wt")
	wt, err := Create(root, dest, "", "feature-x") // baseRef "" => HEAD, no origin fetch
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.Branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", wt.Branch)
	}
	if fi, err := os.Stat(dest); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("base files should be checked out in the worktree: %v", err)
	}

	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err = %v", err)
	}
	// The branch (and its commits) must survive worktree teardown - that's
	// what lets Phase 3 push it after the dir is removed.
	out, _ := exec.Command("git", "-C", root, "branch", "--list", "feature-x").CombinedOutput()
	if len(out) == 0 {
		t.Errorf("branch feature-x should survive worktree removal")
	}
}

func TestRemoveMissingIsNoop(t *testing.T) {
	if err := Remove("", filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("Remove of missing dir should be a no-op, got %v", err)
	}
}
