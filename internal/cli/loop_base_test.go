package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitTestRepo creates a throwaway repo on branch main with one commit and a
// deterministic identity, returning its path. remotes maps remote name to
// URL; tracking, when non-empty, is the remote main is set to track (which
// must be one of remotes and is created as a real bare repo so the tracking
// push succeeds). Non-tracking remotes are registered by URL only, so a fake
// like "git@github.com:acme/x.git" never gets contacted.
func gitTestRepo(t *testing.T, remotes map[string]string, tracking string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(cwd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(dir, "add", "-A")
	run(dir, "commit", "-q", "-m", "init")
	for name, url := range remotes {
		if name == tracking {
			bare := filepath.Join(t.TempDir(), name+".git")
			run(dir, "init", "-q", "--bare", bare)
			run(dir, "remote", "add", name, bare)
			run(dir, "push", "-q", "-u", name, "main")
			continue
		}
		run(dir, "remote", "add", name, url)
	}
	return dir
}

// TestResolveBase pins the one-per-run resolution that every worktree, PR
// push, and review diff reads: a differently-named upstream is honored, an
// origin-only repo resolves to exactly the old literal, and an empty --base
// means main.
func TestResolveBase(t *testing.T) {
	tests := []struct {
		name       string
		remotes    map[string]string
		tracking   string
		base       string
		wantRemote string
		wantRef    string
	}{
		{"origin only", map[string]string{"origin": ""}, "origin", "main", "origin", "origin/main"},
		{"public upstream with origin mirror", map[string]string{"origin": "forge:ryan/x.git", "public": ""}, "public", "main", "public", "public/main"},
		{"empty base defaults to main", map[string]string{"public": ""}, "public", "", "public", "public/main"},
		{"no upstream falls back to origin", map[string]string{"origin": "forge:ryan/x.git"}, "", "main", "origin", "origin/main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitTestRepo(t, tc.remotes, tc.tracking)
			o := loopOpts{base: tc.base}
			o.resolveBase(dir)
			if o.baseRemote != tc.wantRemote || o.baseRef != tc.wantRef {
				t.Errorf("resolveBase: remote=%q ref=%q, want %q/%q", o.baseRemote, o.baseRef, tc.wantRemote, tc.wantRef)
			}
		})
	}
}
