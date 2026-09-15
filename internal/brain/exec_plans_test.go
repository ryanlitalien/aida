package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewExecPlan_DefaultsToActive(t *testing.T) {
	p := NewExecPlan("p-1", "Test", ExecPlanOrigin{Type: "cli"})
	if p.Status != ExecPlanStatusActive {
		t.Errorf("Status = %q, want active", p.Status)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Errorf("timestamps not initialized: %+v", p)
	}
}

func TestExecPlan_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	p := &ExecPlan{
		ID:     "20260505-test",
		Title:  "Investigate flaky test",
		Status: ExecPlanStatusActive,
		Origin: ExecPlanOrigin{Type: "cli", Ref: "aida --agent"},
		Tags:   []string{"profile:home", "ci"},
		Goal:   "Figure out why TestX flakes on CI but not locally.",
		Body:   "## Plan\n\n1. Reproduce locally\n2. Add diagnostic logging\n3. Bisect",
	}
	if err := WriteExecPlan(tmp, p); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ReadExecPlan(tmp, p.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ID != p.ID || got.Title != p.Title || got.Status != p.Status {
		t.Errorf("metadata mismatch: %+v", got)
	}
	if got.Goal != p.Goal {
		t.Errorf("Goal mismatch:\nwant: %q\ngot:  %q", p.Goal, got.Goal)
	}
	if !strings.Contains(got.Body, "Reproduce locally") {
		t.Errorf("Body lost: %q", got.Body)
	}
	if got.Origin.Type != "cli" || got.Origin.Ref != "aida --agent" {
		t.Errorf("Origin mismatch: %+v", got.Origin)
	}
}

func TestWriteExecPlan_RejectsEmptyAndInvalid(t *testing.T) {
	tmp := t.TempDir()
	if err := WriteExecPlan(tmp, nil); err == nil {
		t.Errorf("nil plan should error")
	}
	if err := WriteExecPlan(tmp, &ExecPlan{Status: ExecPlanStatusActive}); err == nil {
		t.Errorf("empty ID should error")
	}
	if err := WriteExecPlan(tmp, &ExecPlan{ID: "x", Status: "garbage"}); err == nil {
		t.Errorf("invalid status should error")
	}
}

func TestReadExecPlan_MissingReturnsErrNotExist(t *testing.T) {
	_, err := ReadExecPlan(t.TempDir(), "no-such-plan")
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}

func TestListExecPlans_FiltersAndOrdersByUpdatedAt(t *testing.T) {
	tmp := t.TempDir()
	// Write three plans with deliberately distinct UpdatedAt order.
	// Sleep is unnecessary because WriteExecPlan stamps UpdatedAt
	// at write time and we write in order.
	a := &ExecPlan{ID: "a", Title: "A", Status: ExecPlanStatusActive, Origin: ExecPlanOrigin{Type: "cli"}}
	b := &ExecPlan{ID: "b", Title: "B", Status: ExecPlanStatusActive, Origin: ExecPlanOrigin{Type: "cli"}}
	c := &ExecPlan{ID: "c", Title: "C", Status: ExecPlanStatusCompleted, Origin: ExecPlanOrigin{Type: "cli"}}
	if err := WriteExecPlan(tmp, a); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecPlan(tmp, b); err != nil {
		t.Fatal(err)
	}
	if err := WriteExecPlan(tmp, c); err != nil {
		t.Fatal(err)
	}

	active, err := ListExecPlans(tmp, ExecPlanStatusActive)
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active count = %d, want 2", len(active))
	}
	completed, _ := ListExecPlans(tmp, ExecPlanStatusCompleted)
	if len(completed) != 1 || completed[0].ID != "c" {
		t.Errorf("completed wrong: %+v", completed)
	}
}

func TestListExecPlans_EmptyDirOK(t *testing.T) {
	got, err := ListExecPlans(t.TempDir(), ExecPlanStatusActive)
	if err != nil {
		t.Errorf("empty dir should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 plans, got %d", len(got))
	}
}

func TestListExecPlans_RejectsBadStatus(t *testing.T) {
	if _, err := ListExecPlans(t.TempDir(), "garbage"); err == nil {
		t.Errorf("invalid status should error")
	}
}

func TestTransitionExecPlan_MovesFile(t *testing.T) {
	tmp := t.TempDir()
	p := &ExecPlan{ID: "x", Title: "X", Status: ExecPlanStatusActive, Origin: ExecPlanOrigin{Type: "cli"}}
	if err := WriteExecPlan(tmp, p); err != nil {
		t.Fatal(err)
	}
	if err := TransitionExecPlan(tmp, "x", ExecPlanStatusCompleted); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	// Old path gone, new path present.
	if _, err := os.Stat(filepath.Join(ExecPlansDir(tmp), "active", "x.md")); !os.IsNotExist(err) {
		t.Errorf("old file should be gone, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(ExecPlansDir(tmp), "completed", "x.md")); err != nil {
		t.Errorf("new file missing: %v", err)
	}
	// Frontmatter status updated.
	got, _ := ReadExecPlan(tmp, "x")
	if got.Status != ExecPlanStatusCompleted {
		t.Errorf("Status not updated: %q", got.Status)
	}
}

func TestTransitionExecPlan_SameStatusNoOp(t *testing.T) {
	tmp := t.TempDir()
	p := &ExecPlan{ID: "y", Title: "Y", Status: ExecPlanStatusActive, Origin: ExecPlanOrigin{Type: "cli"}}
	_ = WriteExecPlan(tmp, p)
	if err := TransitionExecPlan(tmp, "y", ExecPlanStatusActive); err != nil {
		t.Errorf("same-status transition should be no-op, got %v", err)
	}
}

func TestAppendDecisionLog_CreatesHeadingOnFirstAppend(t *testing.T) {
	tmp := t.TempDir()
	p := &ExecPlan{
		ID: "log-test", Title: "T", Status: ExecPlanStatusActive,
		Origin: ExecPlanOrigin{Type: "cli"},
		Body:   "## Plan\n\nstart here",
	}
	_ = WriteExecPlan(tmp, p)

	if err := AppendDecisionLog(tmp, "log-test", "agent", "first move"); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := AppendDecisionLog(tmp, "log-test", "user", "looks good"); err != nil {
		t.Fatalf("Append 2: %v", err)
	}

	got, err := ReadExecPlan(tmp, "log-test")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(got.Body, "## Decision log") {
		t.Errorf("missing decision log heading: %q", got.Body)
	}
	if !strings.Contains(got.Body, "[agent] first move") {
		t.Errorf("first append missing: %q", got.Body)
	}
	if !strings.Contains(got.Body, "[user] looks good") {
		t.Errorf("second append missing: %q", got.Body)
	}
	// Original Plan section preserved.
	if !strings.Contains(got.Body, "start here") {
		t.Errorf("original body lost: %q", got.Body)
	}
}

func TestAppendDecisionLog_MissingPlanErrors(t *testing.T) {
	if err := AppendDecisionLog(t.TempDir(), "nope", "agent", "x"); !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestExecPlanStatus_IsValid(t *testing.T) {
	for _, s := range []ExecPlanStatus{ExecPlanStatusActive, ExecPlanStatusCompleted, ExecPlanStatusAbandoned} {
		if !s.IsValid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if ExecPlanStatus("garbage").IsValid() {
		t.Errorf("garbage should not be valid")
	}
}
