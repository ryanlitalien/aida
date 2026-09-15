package jobs

import (
	"os"
	"strings"
	"testing"
)

// TestEffectiveState_LegacyDone is the Change 4 regression test: a job
// written before the result contract existed (SchemaVersion below
// resultContractVersion) that recorded "done" must never read back as
// a verified success. It downgrades to incomplete with
// LegacyUnverifiedReason, while the ORIGINALLY stored state is kept
// around for auditing.
func TestEffectiveState_LegacyDone(t *testing.T) {
	j := &Job{RunID: "20250101-old-job", State: StateDone, SchemaVersion: 0}
	state, reason, orig := EffectiveState(j)
	if state != StateIncomplete {
		t.Errorf("state = %q, want %q", state, StateIncomplete)
	}
	if reason != LegacyUnverifiedReason {
		t.Errorf("reason = %q, want %q", reason, LegacyUnverifiedReason)
	}
	if orig != StateDone {
		t.Errorf("original state = %q, want %q (must be preserved for auditing)", orig, StateDone)
	}
}

// TestEffectiveState_LegacyFailedStaysFailed covers Change 4's other
// row: a legacy "failed" job stays failed, and EffectiveState never
// invents a reason it doesn't have.
func TestEffectiveState_LegacyFailedStaysFailed(t *testing.T) {
	j := &Job{RunID: "20250101-old-job", State: StateFailed, SchemaVersion: 0, Error: "boom"}
	state, reason, orig := EffectiveState(j)
	if state != StateFailed {
		t.Errorf("state = %q, want %q", state, StateFailed)
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty (never invented)", reason)
	}
	if orig != StateFailed {
		t.Errorf("original state = %q, want %q", orig, StateFailed)
	}
}

// TestEffectiveState_LegacyNonTerminalUnchanged covers the "other
// states -> unchanged" row: a legacy job still queued/running/etc.
// isn't a completion claim at all, so it passes through untouched.
func TestEffectiveState_LegacyNonTerminalUnchanged(t *testing.T) {
	j := &Job{RunID: "20250101-old-job", State: StateRunning, SchemaVersion: 0}
	state, reason, orig := EffectiveState(j)
	if state != StateRunning || reason != "" || orig != StateRunning {
		t.Errorf("EffectiveState = (%q, %q, %q), want (%q, \"\", %q)", state, reason, orig, StateRunning, StateRunning)
	}
}

// TestEffectiveState_CurrentSchemaUnchanged verifies a job written
// under the current schema passes through unchanged even with an
// empty TerminationReason -- a job finished via Complete/Fail (its own
// verification contract, e.g. a loop --check gate) must not be
// downgraded just because it never populated the result-contract
// fields. Only records that PREDATE the contract get downgraded.
func TestEffectiveState_CurrentSchemaUnchanged(t *testing.T) {
	j := &Job{RunID: "20260908-new-job", State: StateDone, SchemaVersion: resultContractVersion}
	state, reason, orig := EffectiveState(j)
	if state != StateDone || reason != "" || orig != StateDone {
		t.Errorf("EffectiveState = (%q, %q, %q), want (%q, \"\", %q)", state, reason, orig, StateDone, StateDone)
	}
}

// TestDescribeOutcome_UnverifiedWording checks that an incomplete job
// gets the exact verbatim notice Change 2 requires, with the real stop
// reason appended -- not a bare state name.
func TestDescribeOutcome_UnverifiedWording(t *testing.T) {
	withTempHome(t)
	j := &Job{
		RunID:             "20260908-incomplete-job",
		Profile:           "work",
		State:             StateIncomplete,
		SchemaVersion:     resultContractVersion,
		TerminationReason: TerminationWallTimeLimit,
	}
	got := DescribeOutcome(j)
	if !strings.Contains(got, UnverifiedResultNotice) {
		t.Errorf("DescribeOutcome = %q, missing the verbatim unverified notice", got)
	}
	if !strings.Contains(got, TerminationWallTimeLimit) {
		t.Errorf("DescribeOutcome = %q, missing the actual stop reason", got)
	}
}

// TestDescribeOutcome_LegacyDoneGetsUnverifiedWording ties Change 2 and
// Change 4 together: a legacy "done" record must produce the same
// verbatim notice as a fresh "incomplete" one, since EffectiveState has
// already downgraded it before DescribeOutcome ever sees the outcome
// fields.
func TestDescribeOutcome_LegacyDoneGetsUnverifiedWording(t *testing.T) {
	withTempHome(t)
	j := &Job{RunID: "20250101-legacy-done", Profile: "work", State: StateDone, SchemaVersion: 0}
	got := DescribeOutcome(j)
	if !strings.Contains(got, UnverifiedResultNotice) {
		t.Errorf("DescribeOutcome = %q, missing the verbatim unverified notice", got)
	}
	if strings.Contains(got, "Verified outcome: achieved") {
		t.Errorf("DescribeOutcome = %q, must not claim a verified success for a legacy record", got)
	}
}

// TestDescribeOutcome_VerifiedDoneOmitsNotice is the positive case: a
// current-schema job with an achieved outcome AND a real output file
// must read as a clean success, not get buried under the disclaimer.
func TestDescribeOutcome_VerifiedDoneOmitsNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	j, err := store.Enqueue("agent", "", "", "upgrade the server", "test")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := os.MkdirAll(RunDir("work", j.RunID), 0755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	if err := os.WriteFile(OutputPath("work", j.RunID), []byte("upgraded successfully to v2.3.1"), 0644); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if err := store.Finalize(j.RunID, FinalizeParams{
		Termination: TerminationFinalResponse,
		HasResult:   true,
		Outcome:     OutcomeAchieved,
		Summary:     "upgraded to v2.3.1",
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	desc := DescribeOutcome(got)
	if strings.Contains(desc, UnverifiedResultNotice) {
		t.Errorf("DescribeOutcome = %q, a verified done with real output must not carry the unverified notice", desc)
	}
	if !strings.Contains(desc, "Verified outcome: achieved") {
		t.Errorf("DescribeOutcome = %q, missing the verified outcome", desc)
	}
	if !strings.Contains(desc, "upgraded to v2.3.1") {
		t.Errorf("DescribeOutcome = %q, missing the summary", desc)
	}
}
