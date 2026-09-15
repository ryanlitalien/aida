package jobs

import (
	"strings"
	"testing"
)

// TestApprovalLifecycle exercises the Phase 5 HITL gate transitions:
// RequestApproval -> awaiting_approval (non-terminal, fields persisted),
// Approve -> running, and RejectApproval -> failed with the reason.
func TestApprovalLifecycle(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("loop", "do-thing", "", "do thing", "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if err := store.RequestApproval(j.RunID, "merge", "merge PR #1 into main"); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateAwaitingApproval {
		t.Fatalf("state = %q, want awaiting_approval", got.State)
	}
	if IsTerminalState(got.State) {
		t.Fatal("awaiting_approval must NOT be terminal (worktree cleanup would fire mid-gate)")
	}
	if got.ApprovalAction != "merge" || got.ApprovalPayload != "merge PR #1 into main" {
		t.Fatalf("approval fields not persisted through SQL: action=%q payload=%q", got.ApprovalAction, got.ApprovalPayload)
	}
	// Round-trips through the manifest too.
	if m, err := ReadManifest("work", j.RunID); err != nil || m.ApprovalPayload != "merge PR #1 into main" {
		t.Fatalf("approval fields not in manifest: %+v err=%v", m, err)
	}

	if err := store.Approve(j.RunID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got, _ = store.Get(j.RunID); got.State != StateRunning {
		t.Fatalf("after Approve state = %q, want running", got.State)
	}

	// Reject path on a fresh job → failed with the reason.
	j2, _ := store.Enqueue("loop", "do-other", "", "do other", "")
	if err := store.RequestApproval(j2.RunID, "send", "post to slack"); err != nil {
		t.Fatalf("RequestApproval#2: %v", err)
	}
	if err := store.RejectApproval(j2.RunID, "too risky"); err != nil {
		t.Fatalf("RejectApproval: %v", err)
	}
	got2, _ := store.Get(j2.RunID)
	if got2.State != StateFailed {
		t.Fatalf("after RejectApproval state = %q, want failed", got2.State)
	}
	if !strings.Contains(got2.Error, "too risky") {
		t.Errorf("reject reason lost: %q", got2.Error)
	}
}
