package brain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func ffRunOK(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v (dir=%s): %v\n%s", name, args, dir, err, out)
	}
}

func ffConfigUser(t *testing.T, dir string) {
	t.Helper()
	ffRunOK(t, dir, "git", "config", "user.email", "test@example.com")
	ffRunOK(t, dir, "git", "config", "user.name", "Test")
}

// ffNewBareRemote creates a bare repo with one commit on its default
// branch (so HEAD and an upstream both exist after cloning).
func ffNewBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	ffRunOK(t, root, "git", "init", "--bare", "-q", bare)

	seed := filepath.Join(root, "seed")
	ffRunOK(t, root, "git", "clone", "-q", bare, seed)
	ffConfigUser(t, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ffRunOK(t, seed, "git", "add", "README.md")
	ffRunOK(t, seed, "git", "commit", "-q", "-m", "seed")
	ffRunOK(t, seed, "git", "push", "-q", "origin", "HEAD:refs/heads/main")
	return bare
}

// ffNewClone clones bare into a fresh directory, tracking main.
func ffNewClone(t *testing.T, bare string) string {
	t.Helper()
	root := t.TempDir()
	clone := filepath.Join(root, "clone")
	ffRunOK(t, root, "git", "clone", "-q", "-b", "main", bare, clone)
	ffConfigUser(t, clone)
	return clone
}

func TestFetchAndFastForward_NotRepo(t *testing.T) {
	dir := t.TempDir() // plain directory, never git-init'd
	status, err := FetchAndFastForward(dir, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchAndFastForward: %v", err)
	}
	if status != FFNotRepo {
		t.Errorf("status = %q, want %q", status, FFNotRepo)
	}
}

func TestFetchAndFastForward_UpToDate(t *testing.T) {
	bare := ffNewBareRemote(t)
	clone := ffNewClone(t, bare)

	status, err := FetchAndFastForward(clone, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchAndFastForward: %v", err)
	}
	if status != FFUpToDate {
		t.Errorf("status = %q, want %q", status, FFUpToDate)
	}
}

func TestFetchAndFastForward_FastForwards(t *testing.T) {
	bare := ffNewBareRemote(t)
	a := ffNewClone(t, bare)
	b := ffNewClone(t, bare)

	// A pushes a new commit.
	if err := os.WriteFile(filepath.Join(a, "new-file.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ffRunOK(t, a, "git", "add", "new-file.txt")
	ffRunOK(t, a, "git", "commit", "-q", "-m", "add new-file.txt")
	ffRunOK(t, a, "git", "push", "-q", "origin", "main")

	status, err := FetchAndFastForward(b, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchAndFastForward: %v", err)
	}
	if status != FFFastForwarded {
		t.Fatalf("status = %q, want %q", status, FFFastForwarded)
	}
	if _, err := os.Stat(filepath.Join(b, "new-file.txt")); err != nil {
		t.Errorf("new-file.txt did not appear in b's working tree after fast-forward: %v", err)
	}
}

func TestFetchAndFastForward_RemoteAheadWhenLocalHasUnpushedCommit(t *testing.T) {
	bare := ffNewBareRemote(t)
	a := ffNewClone(t, bare)
	b := ffNewClone(t, bare)

	// A pushes a new commit.
	if err := os.WriteFile(filepath.Join(a, "from-a.txt"), []byte("a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ffRunOK(t, a, "git", "add", "from-a.txt")
	ffRunOK(t, a, "git", "commit", "-q", "-m", "from a")
	ffRunOK(t, a, "git", "push", "-q", "origin", "main")

	// B makes its own local, unpushed commit - now B has diverged from
	// what A pushed.
	if err := os.WriteFile(filepath.Join(b, "from-b.txt"), []byte("b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ffRunOK(t, b, "git", "add", "from-b.txt")
	ffRunOK(t, b, "git", "commit", "-q", "-m", "from b, unpushed")

	beforeHead, _, err := runFFGit(context.Background(), b, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}

	status, err := FetchAndFastForward(b, 5*time.Second)
	if err == nil {
		t.Fatal("expected a non-nil error for FFRemoteAhead")
	}
	if status != FFRemoteAhead {
		t.Fatalf("status = %q, want %q (err: %v)", status, FFRemoteAhead, err)
	}

	// B's working tree must be untouched - never rebase, never stash.
	afterHead, _, err := runFFGit(context.Background(), b, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse HEAD after: %v", err)
	}
	if beforeHead != afterHead {
		t.Errorf("HEAD moved during a refused fast-forward: before=%q after=%q", beforeHead, afterHead)
	}
	if _, err := os.Stat(filepath.Join(b, "from-b.txt")); err != nil {
		t.Errorf("from-b.txt should still be present: %v", err)
	}
}

func TestFetchAndFastForward_Offline(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	ffRunOK(t, root, "git", "init", "-q", repo)
	ffConfigUser(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ffRunOK(t, repo, "git", "add", "f.txt")
	ffRunOK(t, repo, "git", "commit", "-q", "-m", "init")
	// Point origin at a path that doesn't exist - fetch fails fast
	// without needing the full timeout window.
	ffRunOK(t, repo, "git", "remote", "add", "origin", filepath.Join(root, "does-not-exist.git"))

	start := time.Now()
	status, err := FetchAndFastForward(repo, 5*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error for an unreachable remote")
	}
	if status != FFOffline {
		t.Fatalf("status = %q, want %q (err: %v)", status, FFOffline, err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("FetchAndFastForward took %s, expected it to fail well within the 5s timeout", elapsed)
	}
}
