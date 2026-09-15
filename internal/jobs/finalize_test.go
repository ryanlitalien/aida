package jobs

import "testing"

// TestFinalizeState covers the mapping table Store.Finalize is built
// on: the two "verified" rows, the outright-error row, and the
// catch-all that must swallow everything else -- most importantly a
// final_response with no structured result at all, which is exactly
// the shape of the bug that motivated this type (a run recorded done
// with nothing behind it).
func TestFinalizeState(t *testing.T) {
	cases := []struct {
		name string
		p    FinalizeParams
		want string
	}{
		{
			name: "final_response achieved -> done",
			p:    FinalizeParams{Termination: TerminationFinalResponse, HasResult: true, Outcome: OutcomeAchieved},
			want: StateDone,
		},
		{
			name: "final_response not_achieved -> failed",
			p:    FinalizeParams{Termination: TerminationFinalResponse, HasResult: true, Outcome: OutcomeNotAchieved},
			want: StateFailed,
		},
		{
			name: "termination error -> failed",
			p:    FinalizeParams{Termination: TerminationError},
			want: StateFailed,
		},
		{
			name: "final_response with no structured result -> incomplete",
			p:    FinalizeParams{Termination: TerminationFinalResponse, HasResult: false},
			want: StateIncomplete,
		},
		// Additional "anything else" coverage beyond the four required rows.
		{
			name: "wall_time_limit -> incomplete",
			p:    FinalizeParams{Termination: TerminationWallTimeLimit},
			want: StateIncomplete,
		},
		{
			name: "cost_limit -> incomplete",
			p:    FinalizeParams{Termination: TerminationCostLimit},
			want: StateIncomplete,
		},
		{
			name: "model_token_limit -> incomplete",
			p:    FinalizeParams{Termination: TerminationModelTokenLimit},
			want: StateIncomplete,
		},
		{
			name: "cancelled -> incomplete",
			p:    FinalizeParams{Termination: TerminationCancelled},
			want: StateIncomplete,
		},
		{
			name: "awaiting_input -> incomplete",
			p:    FinalizeParams{Termination: TerminationAwaitingInput},
			want: StateIncomplete,
		},
		{
			name: "empty/unrecognized termination -> incomplete",
			p:    FinalizeParams{},
			want: StateIncomplete,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FinalizeState(c.p); got != c.want {
				t.Errorf("FinalizeState(%+v) = %q, want %q", c.p, got, c.want)
			}
		})
	}
}

// TestFinalizeWritesResultContract exercises Store.Finalize end to
// end: state, termination reason, and structured result all land on
// both the SQL row and the on-disk manifest, and the job's
// SchemaVersion marks it current (not legacy) so DescribeOutcome
// doesn't second-guess it later.
func TestFinalizeWritesResultContract(t *testing.T) {
	withTempHome(t)
	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("agent", "", "", "upgrade the server", "jarvis:voice")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
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
	if got.State != StateDone {
		t.Errorf("state = %q, want %q", got.State, StateDone)
	}
	if got.TerminationReason != TerminationFinalResponse {
		t.Errorf("termination_reason = %q, want %q", got.TerminationReason, TerminationFinalResponse)
	}
	if got.Outcome != OutcomeAchieved || got.Summary != "upgraded to v2.3.1" {
		t.Errorf("outcome/summary lost through SQL: %+v", got)
	}
	if got.SchemaVersion < resultContractVersion {
		t.Errorf("SchemaVersion = %d, want >= %d (a fresh job must never read as legacy)", got.SchemaVersion, resultContractVersion)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.TerminationReason != TerminationFinalResponse || m.Outcome != OutcomeAchieved || m.Summary != "upgraded to v2.3.1" {
		t.Errorf("result contract not in manifest: %+v", m)
	}

	// First-terminal-wins: a second Finalize call must be a no-op.
	if err := store.Finalize(j.RunID, FinalizeParams{Termination: TerminationError, ErrorMsg: "should not apply"}); err != nil {
		t.Fatalf("second Finalize: %v", err)
	}
	again, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.State != StateDone || again.Error == "should not apply" {
		t.Errorf("Finalize overwrote an already-terminal job: %+v", again)
	}
}

// TestFinalizeIncompleteNeverClaimsDone is the direct regression test
// for the reported bug: a run that stops mid-task with no structured
// result must land on incomplete, never done, even though the
// termination is final_response (the agent DID produce some final
// text -- it just never verified it).
func TestFinalizeIncompleteNeverClaimsDone(t *testing.T) {
	withTempHome(t)
	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("agent", "", "", "upgrade the server behind the backup gate", "jarvis:voice")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.Finalize(j.RunID, FinalizeParams{
		Termination: TerminationFinalResponse,
		HasResult:   false,
	}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateIncomplete {
		t.Errorf("state = %q, want %q (must never be %q with no verified result)", got.State, StateIncomplete, StateDone)
	}
}
