package eval

import (
	"context"
	"strings"
	"testing"
)

func TestCodeReviewerNoCommandsSkips(t *testing.T) {
	rec, err := NewCodeReviewer().Review(context.Background(), ReviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("no commands should yield nil record (no opinion), got %+v", rec)
	}
}

func TestCodeReviewerPass(t *testing.T) {
	rec, err := NewCodeReviewer().Review(context.Background(), ReviewInput{
		Commands: []CodeCheck{{Name: "build", Cmd: "true"}, {Name: "test", Cmd: "true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Verdict != VerdictPass {
		t.Fatalf("want pass record, got %+v", rec)
	}
}

func TestCodeReviewerFailCarriesIssue(t *testing.T) {
	rec, err := NewCodeReviewer().Review(context.Background(), ReviewInput{
		Commands: []CodeCheck{
			{Name: "build", Cmd: "true"},
			{Name: "test", Cmd: "echo FAILED_ASSERTION >&2; exit 1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Verdict != VerdictFail {
		t.Fatalf("want fail record, got %+v", rec)
	}
	if len(rec.Issues) != 1 {
		t.Fatalf("want exactly one issue (only the test check failed), got %d: %+v", len(rec.Issues), rec.Issues)
	}
	is := rec.Issues[0]
	if is.Type != "test-failure" {
		t.Errorf("issue type = %q, want test-failure", is.Type)
	}
	if !strings.Contains(is.Message, "FAILED_ASSERTION") {
		t.Errorf("issue message should carry command output, got %q", is.Message)
	}
	// AggregateVerdict over the gate must be fail-loud.
	if AggregateVerdict([]ReviewRecord{*rec}) != VerdictFail {
		t.Errorf("AggregateVerdict should be fail")
	}
}
