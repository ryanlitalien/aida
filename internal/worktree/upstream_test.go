package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRun runs git in dir with a deterministic identity, failing the test on
// error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// initMainRepo creates a repo whose default branch is main with one commit.
func initMainRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// addBareRemote creates a bare repo, registers it as remote name in dir, and
// pushes branch to it with -u so branch tracks name/branch.
func addBareRemote(t *testing.T, dir, name, branch string, track bool) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), name+".git")
	// -b pins the bare repo's HEAD. Without it the branch comes from
	// init.defaultBranch, which Apple's git sets to main in Xcode's system
	// gitconfig while a stock Linux git still uses master. A clone of a bare
	// whose HEAD names a branch that does not exist lands on that empty
	// branch, and a later `push <remote> main` then has no local ref to push.
	gitRun(t, dir, "init", "-q", "--bare", "-b", branch, bare)
	gitRun(t, dir, "remote", "add", name, bare)
	if track {
		gitRun(t, dir, "push", "-q", "-u", name, branch)
	} else {
		gitRun(t, dir, "push", "-q", name, branch)
	}
	return bare
}

func TestResolveUpstream(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		branch     string
		wantRemote string
		wantRef    string
	}{
		{
			// The safety property: a normal single-remote project resolves to
			// exactly what the old "origin/main" literal produced.
			name:       "origin only, main tracks origin/main",
			setup:      func(t *testing.T, dir string) { addBareRemote(t, dir, "origin", "main", true) },
			branch:     "main",
			wantRemote: "origin",
			wantRef:    "origin/main",
		},
		{
			// A repo like aida after its remote rename: origin is a mirror,
			// main tracks the differently-named public remote.
			name: "main tracks public while origin also exists",
			setup: func(t *testing.T, dir string) {
				addBareRemote(t, dir, "origin", "main", false)
				addBareRemote(t, dir, "public", "main", true)
			},
			branch:     "main",
			wantRemote: "public",
			wantRef:    "public/main",
		},
		{
			name: "non-main base branch with its own upstream",
			setup: func(t *testing.T, dir string) {
				gitRun(t, dir, "branch", "release")
				addBareRemote(t, dir, "upstream", "release", true)
			},
			branch:     "release",
			wantRemote: "upstream",
			wantRef:    "upstream/release",
		},
		{
			name:       "remote exists but branch has no upstream",
			setup:      func(t *testing.T, dir string) { addBareRemote(t, dir, "origin", "main", false) },
			branch:     "main",
			wantRemote: "origin",
			wantRef:    "origin/main",
		},
		{
			name:       "no remotes at all",
			setup:      func(t *testing.T, dir string) {},
			branch:     "main",
			wantRemote: "origin",
			wantRef:    "origin/main",
		},
		{
			name: "dot remote tracks a local branch",
			setup: func(t *testing.T, dir string) {
				gitRun(t, dir, "branch", "develop")
				gitRun(t, dir, "branch", "--set-upstream-to=develop", "main")
			},
			branch:     "main",
			wantRemote: "",
			wantRef:    "develop",
		},
		{
			name:       "empty branch defaults to main",
			setup:      func(t *testing.T, dir string) { addBareRemote(t, dir, "public", "main", true) },
			branch:     "",
			wantRemote: "public",
			wantRef:    "public/main",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := initMainRepo(t)
			tc.setup(t, dir)
			remote, ref := ResolveUpstream(dir, tc.branch)
			if remote != tc.wantRemote || ref != tc.wantRef {
				t.Errorf("ResolveUpstream(%q) = (%q, %q), want (%q, %q)", tc.branch, remote, ref, tc.wantRemote, tc.wantRef)
			}
			if ref == "" {
				t.Errorf("ref must never be empty")
			}
		})
	}
}

func TestRemotesAndRemoteURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := initMainRepo(t)
	if got := Remotes(dir); len(got) != 0 {
		t.Errorf("Remotes of a remote-less repo = %v, want none", got)
	}
	bare := addBareRemote(t, dir, "public", "main", true)
	gitRun(t, dir, "remote", "add", "origin", "forge:ryan/x.git")
	got := Remotes(dir)
	if len(got) != 2 || got[0] != "origin" || got[1] != "public" {
		t.Errorf("Remotes = %v, want [origin public]", got)
	}
	if u := RemoteURL(dir, "public"); u != bare {
		t.Errorf("RemoteURL(public) = %q, want %q", u, bare)
	}
	if u := RemoteURL(dir, "origin"); u != "forge:ryan/x.git" {
		t.Errorf("RemoteURL(origin) = %q", u)
	}
	if u := RemoteURL(dir, "nope"); u != "" {
		t.Errorf("RemoteURL of a missing remote = %q, want empty", u)
	}
	if r := remoteOfRef(dir, "public/main"); r != "public" {
		t.Errorf("remoteOfRef(public/main) = %q", r)
	}
	if r := remoteOfRef(dir, "archive/main"); r != "" {
		t.Errorf("remoteOfRef of an unconfigured remote = %q, want empty", r)
	}
	if r := remoteOfRef(dir, "develop"); r != "" {
		t.Errorf("remoteOfRef of a local branch = %q, want empty", r)
	}
}

// TestCreateFetchesBaseRefRemote proves Create fetches whichever remote the
// base ref names (not only origin) so the worktree is cut from the fresh tip:
// a commit pushed to the "public" bare repo by a second clone must show up in
// a worktree created off public/main even though the first clone never
// fetched it.
func TestCreateFetchesBaseRefRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := initMainRepo(t)
	bare := addBareRemote(t, dir, "public", "main", true)

	// Advance the remote from a second clone.
	other := filepath.Join(t.TempDir(), "other")
	gitRun(t, dir, "clone", "-q", bare, other)
	if err := os.WriteFile(filepath.Join(other, "NEW.md"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, other, "add", "-A")
	gitRun(t, other, "commit", "-q", "-m", "remote-side commit")
	// The bare repo is "origin" from the second clone's point of view.
	// Push HEAD explicitly rather than naming a local branch, so the test
	// does not depend on what the clone decided to call it.
	gitRun(t, other, "push", "-q", "origin", "HEAD:refs/heads/main")

	dest := filepath.Join(t.TempDir(), "wt")
	wt, err := Create(dir, dest, "public/main", "auto/x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer wt.Remove()
	if _, err := os.Stat(filepath.Join(dest, "NEW.md")); err != nil {
		t.Errorf("worktree should include the commit fetched from public/main: %v", err)
	}
}
