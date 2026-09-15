package brain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/eval"
)

func TestWriteAndReadEvalRun_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	original := &EvalRun{
		RunID:            "20260504-101530-test-question",
		Question:         "test question?",
		Model:            "claude-haiku-4-5",
		Profile:          "home",
		AggregateVerdict: "pass",
		Reviewers: []eval.ReviewRecord{
			{
				Reviewer:  "citation",
				Verdict:   eval.VerdictPass,
				Score:     1.0,
				Rationale: "all 3 citations resolved",
			},
		},
	}

	if err := WriteEvalRun(tmp, original); err != nil {
		t.Fatalf("WriteEvalRun: %v", err)
	}
	got, err := ReadEvalRun(tmp, original.RunID)
	if err != nil {
		t.Fatalf("ReadEvalRun: %v", err)
	}
	if got.RunID != original.RunID {
		t.Errorf("RunID = %q, want %q", got.RunID, original.RunID)
	}
	if got.AggregateVerdict != "pass" {
		t.Errorf("AggregateVerdict = %q, want pass", got.AggregateVerdict)
	}
	if len(got.Reviewers) != 1 || got.Reviewers[0].Reviewer != "citation" {
		t.Errorf("reviewer round-trip lost data: %+v", got.Reviewers)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("Timestamp should be auto-set on write, but is zero on read")
	}
}

func TestWriteEvalRun_FillsTimestampWhenZero(t *testing.T) {
	tmp := t.TempDir()
	r := &EvalRun{RunID: "20260504-test", AggregateVerdict: "pass"}
	before := time.Now().UTC().Add(-time.Second)
	if err := WriteEvalRun(tmp, r); err != nil {
		t.Fatalf("WriteEvalRun: %v", err)
	}
	got, _ := ReadEvalRun(tmp, "20260504-test")
	if got.Timestamp.Before(before) {
		t.Errorf("Timestamp not auto-filled: got %v, expected ≥ %v", got.Timestamp, before)
	}
}

func TestWriteEvalRun_PreservesExplicitTimestamp(t *testing.T) {
	tmp := t.TempDir()
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	r := &EvalRun{RunID: "20260102-test", Timestamp: when, AggregateVerdict: "pass"}
	_ = WriteEvalRun(tmp, r)
	got, _ := ReadEvalRun(tmp, "20260102-test")
	if !got.Timestamp.Equal(when) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, when)
	}
}

func TestWriteEvalRun_RejectsEmptyRunID(t *testing.T) {
	if err := WriteEvalRun(t.TempDir(), &EvalRun{RunID: ""}); err == nil {
		t.Errorf("expected error for empty RunID, got nil")
	}
}

func TestWriteEvalRun_RejectsNil(t *testing.T) {
	if err := WriteEvalRun(t.TempDir(), nil); err == nil {
		t.Errorf("expected error for nil record, got nil")
	}
}

func TestReadEvalRun_MissingFile(t *testing.T) {
	_, err := ReadEvalRun(t.TempDir(), "nonexistent-run-id")
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}

func TestListRecentEvalRuns_NewestFirst(t *testing.T) {
	tmp := t.TempDir()
	// IDs use the runs.NewID format: "<UTC timestamp>-<slug>"
	// Lexical reverse sort matches chronological newest-first.
	ids := []string{
		"20260504-100000-old-one",
		"20260504-200000-newer-one",
		"20260504-150000-middle",
	}
	for _, id := range ids {
		_ = WriteEvalRun(tmp, &EvalRun{RunID: id, AggregateVerdict: "pass"})
	}
	got, err := ListRecentEvalRuns(tmp, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if got[0].RunID != "20260504-200000-newer-one" {
		t.Errorf("first record = %q, want newest first", got[0].RunID)
	}
	if got[2].RunID != "20260504-100000-old-one" {
		t.Errorf("last record = %q, want oldest last", got[2].RunID)
	}
}

func TestListRecentEvalRuns_RespectsLimit(t *testing.T) {
	tmp := t.TempDir()
	for i := 0; i < 5; i++ {
		_ = WriteEvalRun(tmp, &EvalRun{
			RunID: time.Now().UTC().Format("20060102-150405") + "-" + string(rune('a'+i)),
		})
	}
	got, _ := ListRecentEvalRuns(tmp, 3)
	if len(got) != 3 {
		t.Errorf("limit=3, got %d records", len(got))
	}
}

func TestListRecentEvalRuns_EmptyDirIsNotError(t *testing.T) {
	tmp := t.TempDir()
	got, err := ListRecentEvalRuns(tmp, 10)
	if err != nil {
		t.Errorf("ListRecentEvalRuns on empty dir: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 records, got %d", len(got))
	}
}

func TestListRecentEvalRuns_SkipsMalformed(t *testing.T) {
	tmp := t.TempDir()
	// One valid record.
	_ = WriteEvalRun(tmp, &EvalRun{RunID: "20260504-good", AggregateVerdict: "pass"})
	// One malformed file in the same dir.
	bad := filepath.Join(EvalRunsDir(tmp), "20260504-broken.json")
	_ = os.WriteFile(bad, []byte("{not valid json"), 0644)

	got, err := ListRecentEvalRuns(tmp, 10)
	if err != nil {
		t.Fatalf("List should not error on a malformed entry: %v", err)
	}
	// Malformed entry skipped, valid one survives.
	if len(got) != 1 || got[0].RunID != "20260504-good" {
		t.Errorf("expected 1 valid record, got %+v", got)
	}
}
