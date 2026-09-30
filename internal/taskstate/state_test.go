package taskstate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDirAndPaths(t *testing.T) {
	brain := "/tmp/brain"
	slug := "fix-the-thing"

	if got, want := Dir(brain, slug), filepath.Join(brain, "arbiter", slug); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if got, want := StatePath(brain, slug), filepath.Join(brain, "arbiter", slug, "STATE.json"); got != want {
		t.Errorf("StatePath() = %q, want %q", got, want)
	}
	if got, want := HandoffPath(brain, slug), filepath.Join(brain, "arbiter", slug, "HANDOFF.md"); got != want {
		t.Errorf("HandoffPath() = %q, want %q", got, want)
	}
	if got, want := LogPath(brain, slug), filepath.Join(brain, "arbiter", slug, "LOG.md"); got != want {
		t.Errorf("LogPath() = %q, want %q", got, want)
	}
}

func TestLoad_NoState(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir, "nope")
	if err == nil {
		t.Fatal("expected an error for missing state")
	}
	if !errors.Is(err, ErrNoState) {
		t.Errorf("expected errors.Is(err, ErrNoState), got %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected errors.Is(err, os.ErrNotExist), got %v", err)
	}
}

func TestSaveAndLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	fixedNow := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	restore := stubNow(fixedNow)
	defer restore()

	s := &State{
		Slug:   "fix-the-thing",
		TaskID: 123,
		Phase:  PhaseClaimed,
		Lane:   "claude-max",
	}
	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Updated.Equal(fixedNow) {
		t.Errorf("Save did not stamp Updated: got %v, want %v", s.Updated, fixedNow)
	}

	got, err := Load(dir, "fix-the-thing")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TaskID != 123 || got.Phase != PhaseClaimed || got.Lane != "claude-max" {
		t.Errorf("round-tripped state mismatch: %+v", got)
	}
	if !got.Updated.Equal(fixedNow) {
		t.Errorf("Updated not round-tripped: got %v, want %v", got.Updated, fixedNow)
	}
}

func TestSave_AtomicNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	s := &State{Slug: "atomic-test", Phase: PhaseRunning}
	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(Dir(dir, "atomic-test"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after Save: %s", e.Name())
		}
	}
}

func TestSave_RequiresSlug(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &State{}); err == nil {
		t.Fatal("expected an error saving state with no slug")
	}
}

func TestBeginAttempt(t *testing.T) {
	s := &State{Attempt: 2, Phase: PhaseHandoff}
	now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	s.BeginAttempt("codex-plus", "gpt-5.6-terra", "minty", now)

	if s.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", s.Attempt)
	}
	if !s.AttemptStartedAt.Equal(now) {
		t.Errorf("AttemptStartedAt = %v, want %v", s.AttemptStartedAt, now)
	}
	if s.Lane != "codex-plus" || s.Model != "gpt-5.6-terra" || s.Host != "minty" {
		t.Errorf("lane/model/host not set: %+v", s)
	}
	if s.Phase != PhaseRunning {
		t.Errorf("Phase = %q, want %q", s.Phase, PhaseRunning)
	}
}

func TestAppendLog(t *testing.T) {
	dir := t.TempDir()
	fixedNow := time.Date(2026, 9, 24, 1, 2, 3, 0, time.UTC)
	restore := stubNow(fixedNow)
	defer restore()

	if err := AppendLog(dir, "fix-the-thing", "started attempt 1"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}
	if err := AppendLog(dir, "fix-the-thing", "attempt 1 failed check"); err != nil {
		t.Fatalf("AppendLog: %v", err)
	}

	data, err := os.ReadFile(LogPath(dir, "fix-the-thing"))
	if err != nil {
		t.Fatalf("reading LOG.md: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "started attempt 1") || !strings.Contains(content, "attempt 1 failed check") {
		t.Errorf("LOG.md missing appended lines: %q", content)
	}
	if !strings.Contains(content, fixedNow.Format(time.RFC3339)) {
		t.Errorf("LOG.md missing timestamp: %q", content)
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 log lines, got %d: %q", len(lines), content)
	}
}

// stubNow overrides the package Now func for the duration of a test and
// returns a restore func.
func stubNow(t time.Time) func() {
	orig := Now
	Now = func() time.Time { return t }
	return func() { Now = orig }
}
