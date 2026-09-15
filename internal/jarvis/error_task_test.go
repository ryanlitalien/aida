package jarvis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
)

func newTestBrain(t *testing.T) *brain.Brain {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0755); err != nil {
		t.Fatalf("mkdir tasks: %v", err)
	}
	db, err := brain.OpenDB(dir)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &brain.Brain{Path: dir, DB: db, Embeddings: brain.NewEmbeddingClient("")}
}

// TestSaveToolErrorTask_DedupSameSignature covers the backlog-inflation bug:
// the same tool failing with the same (normalized) error repeatedly used to
// file a brand new permanent ticket every single time. Two calls that only
// differ by the variable duration in the error message must land on one
// task, not two.
func TestSaveToolErrorTask_DedupSameSignature(t *testing.T) {
	b := &Assistant{brain: newTestBrain(t)}

	slug1 := b.SaveToolErrorTask("what is the seed", "minecraft_ask", "ssh: exit status 1 (took_ms: 45000)")
	if slug1 == "" {
		t.Fatal("first SaveToolErrorTask returned empty slug")
	}

	slug2 := b.SaveToolErrorTask("what is the seed again", "minecraft_ask", "ssh: exit status 1 (took_ms: 47231)")
	if slug2 != slug1 {
		t.Errorf("second call filed a new task (%q) instead of deduping onto %q", slug2, slug1)
	}

	tasks, err := b.brain.ListTasksByStatus(brain.NonTerminalStatuses(), []string{"jarvis-error", "tool:minecraft_ask"}, 0, 0)
	if err != nil {
		t.Fatalf("ListTasksByStatus: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected exactly 1 open jarvis-error task, got %d", len(tasks))
	}

	body, err := b.brain.TaskBody(slug1)
	if err != nil {
		t.Fatalf("TaskBody: %v", err)
	}
	if !strings.Contains(body, "Recurred at") {
		t.Errorf("expected recurrence line appended to body, got:\n%s", body)
	}
}

// TestSaveToolErrorTask_DifferentToolNotDeduped ensures the tool name is
// part of the dedup key - the same error text from a different tool must
// still file its own ticket.
func TestSaveToolErrorTask_DifferentToolNotDeduped(t *testing.T) {
	b := &Assistant{brain: newTestBrain(t)}

	slug1 := b.SaveToolErrorTask("q1", "minecraft_ask", "signal: killed")
	slug2 := b.SaveToolErrorTask("q2", "weather", "signal: killed")

	if slug1 == "" || slug2 == "" {
		t.Fatalf("expected both calls to file a task, got %q and %q", slug1, slug2)
	}
	if slug1 == slug2 {
		t.Error("different tools with the same error text should not dedup onto the same task")
	}
}

// TestSaveToolErrorTask_TerminalTaskNotDedupedAgainst ensures a closed/done
// task doesn't silently swallow a fresh recurrence - once the earlier
// ticket is resolved, a new failure should file its own task again rather
// than reopening or appending to the resolved one.
func TestSaveToolErrorTask_TerminalTaskNotDedupedAgainst(t *testing.T) {
	br := newTestBrain(t)
	a := &Assistant{brain: br}

	slug1 := a.SaveToolErrorTask("q1", "minecraft_ask", "ssh: exit status 1")
	if slug1 == "" {
		t.Fatal("first SaveToolErrorTask returned empty slug")
	}
	if _, err := br.CompleteTask(slug1); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	slug2 := a.SaveToolErrorTask("q2", "minecraft_ask", "ssh: exit status 1")
	if slug2 == "" {
		t.Fatal("second SaveToolErrorTask returned empty slug")
	}
	if slug2 == slug1 {
		t.Error("recurrence after the earlier ticket was closed should file a fresh task, not reopen the closed one")
	}
}

// TestSaveToolErrorTask_NilSafety mirrors the existing "non-fatal" contract:
// a nil-brain Assistant must not panic.
func TestSaveToolErrorTask_NilSafety(t *testing.T) {
	var a *Assistant
	if got := a.SaveToolErrorTask("q", "tool", "err"); got != "" {
		t.Errorf("nil Assistant returned %q, want empty", got)
	}

	a2 := &Assistant{}
	if got := a2.SaveToolErrorTask("q", "tool", "err"); got != "" {
		t.Errorf("Assistant with nil brain returned %q, want empty", got)
	}
}
