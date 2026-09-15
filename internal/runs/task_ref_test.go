package runs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractTaskRef(t *testing.T) {
	cases := []struct {
		answer string
		want   int
	}{
		{"Showed task #118: 2026-04-27-slack-ask-to-create", 118},
		{"Task #42 added: foo", 42},
		{"Completed task #7: foo-bar", 7},
		{"Completed: 2026-04-27-slack-foo", 0},
		{"", 0},
		{"task #abc", 0},
		{"task # 42", 0},
		{"some other text", 0},
	}
	for _, c := range cases {
		if got := extractTaskRef(c.answer); got != c.want {
			t.Errorf("extractTaskRef(%q) = %d, want %d", c.answer, got, c.want)
		}
	}
}

// withTempHome redirects ~/.aida to a tempdir for the duration of the test.
func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".aida", "runs"), 0755); err != nil {
		t.Fatal(err)
	}
}

func TestLatestTaskRefInCwd(t *testing.T) {
	withTempHome(t)
	cwd := "/tmp/aida-test"

	// A task lookup run in the target cwd.
	if _, err := Save(&Run{
		StartedAt: time.Now().Add(-2 * time.Minute),
		Question:  "show task 118",
		Cwd:       cwd,
		Action:    "task",
		Strategy:  "lookup",
		Answer:    "Showed task #118: 2026-04-27-slack-foo",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := LatestTaskRefInCwd(cwd, 10); got != 118 {
		t.Errorf("got %d, want 118", got)
	}

	// Different cwd: no match.
	if got := LatestTaskRefInCwd("/other", 10); got != 0 {
		t.Errorf("different cwd: got %d, want 0", got)
	}

	// Outside the time window.
	if got := LatestTaskRefInCwd(cwd, 1); got != 0 {
		t.Errorf("outside window: got %d, want 0", got)
	}

	// A more recent NON-task run shadows the task run.
	if _, err := Save(&Run{
		StartedAt: time.Now(),
		Question:  "what about tasks",
		Cwd:       cwd,
		Action:    "query",
		Strategy:  "search",
		Answer:    "Mentioned task #99 in passing",
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := LatestTaskRefInCwd(cwd, 10); got != 0 {
		t.Errorf("after query shadow: got %d, want 0 (only task-action runs count)", got)
	}
}
