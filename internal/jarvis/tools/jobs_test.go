package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/jobs"
)

// jobsTestStore opens a throwaway per-profile jobs store rooted at a temp
// HOME so tests never touch the real ~/.aida.
func jobsTestStore(t *testing.T) *jobs.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := jobs.Open("test")
	if err != nil {
		t.Fatalf("jobs.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestExtractPRNumber(t *testing.T) {
	cases := []struct {
		q    string
		want string
	}{
		{"continue working on PR 583", "583"},
		{"work on PR #583", "583"},
		{"continue PR 100 please", "100"},
		{"pull request 42 needs love", "42"},
		{"Pull Request #999", "999"},
		{"nothing here", ""},
		{"just 583 by itself", ""},   // no "PR" prefix → not a match
		{"PR five eighty three", ""}, // word-form unsupported in v1
	}
	for _, c := range cases {
		got := extractPRNumber(c.q)
		if got != c.want {
			t.Errorf("extractPRNumber(%q) = %q, want %q", c.q, got, c.want)
		}
	}
}

// Spoken job lines lead with the pronounceable handle and never contain the
// raw run-id - the LLM mangles long ids aloud, and the system prompt forbids
// speaking them, so the tool result must not hand it one.
func TestFormatJobLine_HandleFirst(t *testing.T) {
	j := &jobs.Job{
		RunID:          "20260622-022838-create-a-file",
		TaskSlug:       "pr-583",
		State:          jobs.StateRunning,
		AwaitingPrompt: "which branch?",
		Error:          "boom",
	}
	handle := jobs.DeriveHandle(j.RunID)
	for _, state := range []string{jobs.StateRunning, jobs.StateAwaitingInput, jobs.StateFailed} {
		j.State = state
		line := formatJobLine(j)
		if !strings.HasPrefix(line, handle) {
			t.Errorf("state %s: line %q does not lead with handle %q", state, line, handle)
		}
		if strings.Contains(line, j.RunID) {
			t.Errorf("state %s: line %q leaks the raw run-id", state, line)
		}
	}
	j.State = jobs.StateAwaitingInput
	if line := formatJobLine(j); !strings.Contains(line, "which branch?") {
		t.Errorf("awaiting line %q missing the prompt", line)
	}
	j.State = jobs.StateFailed
	if line := formatJobLine(j); !strings.Contains(line, "boom") {
		t.Errorf("failed line %q missing the error", line)
	}
}

// A spoken handle (with or without the hyphen) must resolve back to its job
// via resolveJobRef.
func TestResolveJobRef_ByHandle(t *testing.T) {
	store := jobsTestStore(t)
	job, err := store.Enqueue("agent", "", "", "investigate the outage", "test")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	handle := jobs.DeriveHandle(job.RunID)
	spoken := strings.ReplaceAll(handle, "-", " ") // "amber otter"

	got, err := resolveJobRef(store, spoken)
	if err != nil {
		t.Fatalf("resolveJobRef(%q): %v", spoken, err)
	}
	if got.RunID != job.RunID {
		t.Errorf("resolved %s, want %s", got.RunID, job.RunID)
	}
}

// A single surviving handle word (Whisper mishear, or the user saying just
// "amber") resolves an ACTIVE job; once the job is terminal the weak
// word-level tier no longer matches it.
func TestResolveJobRef_HandleWordActiveOnly(t *testing.T) {
	store := jobsTestStore(t)
	job, err := store.Enqueue("agent", "", "", "investigate the outage", "test")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	adj := strings.SplitN(jobs.DeriveHandle(job.RunID), "-", 2)[0]
	mishear := adj + " other" // noun garbled, adjective survives

	got, err := resolveJobRef(store, mishear)
	if err != nil {
		t.Fatalf("resolveJobRef(%q): %v", mishear, err)
	}
	if got.RunID != job.RunID {
		t.Errorf("resolved %s, want %s", got.RunID, job.RunID)
	}

	if err := store.Fail(job.RunID, "boom"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if j, err := resolveJobRef(store, mishear); err == nil {
		t.Errorf("expected no match for %q against a terminal job, got %s", mishear, j.RunID)
	}
}

// TestJobStatusTool_UnverifiedWhenOutputMissing is the end-to-end
// regression test for the reported bug: a job that stops without ever
// producing a usable output file (the "ran out of turns mid-recon,
// recorded done" scenario) must have job_status hand back
// jobs.UnverifiedResultNotice verbatim, plus the real stop reason --
// never a bare "is done"/"is incomplete" that leaves the model to fill
// the gap with an invented success claim.
func TestJobStatusTool_UnverifiedWhenOutputMissing(t *testing.T) {
	store := jobsTestStore(t)
	job, err := store.Enqueue("agent", "", "", "upgrade the server behind the backup gate", "test")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// No output.md is ever written for this run.
	if err := store.Finalize(job.RunID, jobs.FinalizeParams{
		Termination: jobs.TerminationWallTimeLimit,
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	tool := jobStatusTool(store)
	raw, err := json.Marshal(jobStatusInput{Ref: jobs.DeriveHandle(job.RunID)})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	got, err := tool.Run(context.Background(), raw)
	if err != nil {
		t.Fatalf("job_status: %v", err)
	}
	if !strings.Contains(got, jobs.UnverifiedResultNotice) {
		t.Errorf("job_status reply %q missing the verbatim unverified notice", got)
	}
	if !strings.Contains(got, jobs.TerminationWallTimeLimit) {
		t.Errorf("job_status reply %q missing the actual stop reason", got)
	}
	if strings.Contains(got, "is done") {
		t.Errorf("job_status reply %q must never claim success", got)
	}
}

// initGitRepo creates a throwaway git repo at dir, for tests that need
// resolvePRWorkRepoRoot to find a real repo root.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v (%s)", dir, err, out)
	}
	return dir
}

// sameDir compares two paths after resolving symlinks, since t.TempDir() on
// macOS lives under a /var -> /private/var symlink and `git rev-parse
// --show-toplevel` returns the resolved form.
func sameDir(t *testing.T, got, want string) bool {
	t.Helper()
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", got, err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", want, err)
	}
	return gotResolved == wantResolved
}

// A configured jobs.pr_work_repo_root must win even when the process cwd is
// not itself a git repo -- the exact launchd daemon failure mode from bug
// #211: WorkingDirectory is @HOME@, which git rev-parse rejects.
func TestResolvePRWorkRepoRoot_ConfigWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repo := initGitRepo(t, filepath.Join(home, "checkout"))
	cfgYAML := "jobs:\n  pr_work_repo_root: " + repo + "\n"
	if err := os.MkdirAll(filepath.Join(home, ".aida"), 0755); err != nil {
		t.Fatalf("mkdir .aida: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".aida", "config.yaml"), []byte(cfgYAML), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	// cwd is HOME itself, which is NOT a git repo -- mirrors the daemon.
	t.Chdir(home)

	got, err := resolvePRWorkRepoRoot()
	if err != nil {
		t.Fatalf("resolvePRWorkRepoRoot: %v", err)
	}
	if !sameDir(t, got, repo) {
		t.Errorf("resolvePRWorkRepoRoot = %q, want %q", got, repo)
	}
}

// With no config set, resolvePRWorkRepoRoot must still resolve the repo
// from cwd -- the pre-existing interactive `aida serve` behavior must not
// regress.
func TestResolvePRWorkRepoRoot_FallsBackToCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.aida/config.yaml written

	repo := initGitRepo(t, filepath.Join(home, "checkout"))
	t.Chdir(repo)

	got, err := resolvePRWorkRepoRoot()
	if err != nil {
		t.Fatalf("resolvePRWorkRepoRoot: %v", err)
	}
	if !sameDir(t, got, repo) {
		t.Errorf("resolvePRWorkRepoRoot = %q, want %q", got, repo)
	}
}

// When neither config nor cwd resolve a repo, the error must name the
// config key to set -- a bare "exit status 128" (the original bug #211
// symptom) gives no signal that jobs.pr_work_repo_root exists.
func TestResolvePRWorkRepoRoot_ErrorNamesConfigKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config, and home itself isn't a git repo
	t.Chdir(home)

	_, err := resolvePRWorkRepoRoot()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "jobs.pr_work_repo_root") {
		t.Errorf("error %q does not name the config key", err.Error())
	}
}
