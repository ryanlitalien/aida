package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initTestRepo creates a fresh git repo in a temp dir with one
// initial commit so HEAD is real. Returns the abs path. Skips
// the calling test when git isn't on PATH (we don't want CI
// without git to fail loud - the worktree path is a layered
// optional feature).
func initTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; worktree tests skipped")
	}
	dir := t.TempDir()
	mustRun(t, dir, "git", "init", "-q")
	mustRun(t, dir, "git", "config", "user.email", "test@example.com")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	mustRun(t, dir, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "git", "add", "README.md")
	mustRun(t, dir, "git", "commit", "-q", "-m", "init")
	return dir
}

func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, string(out))
	}
}

func TestIsGitRepo(t *testing.T) {
	if isGitRepo("") {
		t.Errorf("empty dir should be false")
	}
	tmp := t.TempDir()
	if isGitRepo(tmp) {
		t.Errorf("plain temp dir should be false")
	}
	repo := initTestRepo(t)
	if !isGitRepo(repo) {
		t.Errorf("initialized repo should be true")
	}
}

func TestSpawnDelegateWorktree_RejectsNonRepo(t *testing.T) {
	_, _, err := spawnDelegateWorktree(t.TempDir())
	if err == nil {
		t.Errorf("expected error for non-repo dir")
	}
}

func TestSpawnDelegateWorktree_CreatesAndCleansUp(t *testing.T) {
	repo := initTestRepo(t)

	worktree, cleanup, err := spawnDelegateWorktree(repo)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()

	// Worktree dir exists and contains the initial commit's file.
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Errorf("worktree missing README.md: %v", err)
	}
	if !strings.HasPrefix(worktree, os.TempDir()) {
		t.Errorf("worktree should be under tmp; got %q", worktree)
	}
	// Worktree is a separate dir from the repo.
	if worktree == repo {
		t.Errorf("worktree path equals repo path")
	}
}

func TestSpawnDelegateWorktree_CleanupRemovesIt(t *testing.T) {
	repo := initTestRepo(t)
	worktree, cleanup, err := spawnDelegateWorktree(repo)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	cleanup()
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone after cleanup, got %v", err)
	}
}

func TestCaptureWorktreeDiff_DetectsModification(t *testing.T) {
	repo := initTestRepo(t)
	worktree, cleanup, err := spawnDelegateWorktree(repo)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()

	// Modify the existing file in the worktree.
	if err := os.WriteFile(filepath.Join(worktree, "README.md"),
		[]byte("hello, modified\n"), 0644); err != nil {
		t.Fatal(err)
	}

	diff := captureWorktreeDiff(worktree)
	if diff == "" {
		t.Fatalf("expected diff, got empty")
	}
	if !strings.Contains(diff, "README.md") {
		t.Errorf("diff should mention README.md: %q", diff)
	}
	if !strings.Contains(diff, "modified") {
		t.Errorf("diff should contain new content: %q", diff)
	}
}

func TestCaptureWorktreeDiff_DetectsUntrackedFile(t *testing.T) {
	repo := initTestRepo(t)
	worktree, cleanup, err := spawnDelegateWorktree(repo)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()

	// Add an untracked file that claude would create.
	if err := os.WriteFile(filepath.Join(worktree, "new.go"),
		[]byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	diff := captureWorktreeDiff(worktree)
	if !strings.Contains(diff, "new.go") {
		t.Errorf("diff should include untracked file: %q", diff)
	}
	if !strings.Contains(diff, "new file") {
		t.Errorf("diff should mark as new file: %q", diff)
	}
}

func TestCaptureWorktreeDiff_EmptyOnNoChanges(t *testing.T) {
	repo := initTestRepo(t)
	worktree, cleanup, err := spawnDelegateWorktree(repo)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()
	if diff := captureWorktreeDiff(worktree); diff != "" {
		t.Errorf("fresh worktree should have empty diff, got: %q", diff)
	}
}

func TestCaptureWorktreeDiff_EmptyDirReturnsEmpty(t *testing.T) {
	if got := captureWorktreeDiff(""); got != "" {
		t.Errorf("empty dir should return empty diff")
	}
}
