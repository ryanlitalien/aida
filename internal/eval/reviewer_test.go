package eval

import (
	"context"
	"errors"
	"testing"
)

// fakeReviewer is a test double that returns canned records.
type fakeReviewer struct {
	name   string
	out    *ReviewRecord
	err    error
	called bool
}

func (f *fakeReviewer) Name() string { return f.name }
func (f *fakeReviewer) Review(_ context.Context, _ ReviewInput) (*ReviewRecord, error) {
	f.called = true
	return f.out, f.err
}

func TestLoopRunsAllReviewersInOrder(t *testing.T) {
	a := &fakeReviewer{name: "a", out: &ReviewRecord{Verdict: VerdictPass}}
	b := &fakeReviewer{name: "b", out: &ReviewRecord{Verdict: VerdictWarn}}
	c := &fakeReviewer{name: "c", out: &ReviewRecord{Verdict: VerdictFail}}

	got, err := Loop(context.Background(), []Reviewer{a, b, c}, ReviewInput{})
	if err != nil {
		t.Fatalf("Loop: %v", err)
	}
	if !a.called || !b.called || !c.called {
		t.Errorf("not all reviewers called: a=%v b=%v c=%v", a.called, b.called, c.called)
	}
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	if got[0].Reviewer != "a" || got[1].Reviewer != "b" || got[2].Reviewer != "c" {
		t.Errorf("records out of order: %+v", got)
	}
}

func TestLoopFillsReviewerNameWhenEmpty(t *testing.T) {
	a := &fakeReviewer{name: "explicit-name", out: &ReviewRecord{Verdict: VerdictPass}}
	got, err := Loop(context.Background(), []Reviewer{a}, ReviewInput{})
	if err != nil {
		t.Fatalf("Loop: %v", err)
	}
	if got[0].Reviewer != "explicit-name" {
		t.Errorf("Reviewer = %q, want %q", got[0].Reviewer, "explicit-name")
	}
}

func TestLoopRespectsExplicitReviewerName(t *testing.T) {
	a := &fakeReviewer{
		name: "factory-name",
		out:  &ReviewRecord{Reviewer: "override-name", Verdict: VerdictPass},
	}
	got, _ := Loop(context.Background(), []Reviewer{a}, ReviewInput{})
	if got[0].Reviewer != "override-name" {
		t.Errorf("Reviewer = %q, want %q", got[0].Reviewer, "override-name")
	}
}

func TestLoopSkipsNilRecords(t *testing.T) {
	a := &fakeReviewer{name: "skipper", out: nil}
	b := &fakeReviewer{name: "speaker", out: &ReviewRecord{Verdict: VerdictPass}}
	got, _ := Loop(context.Background(), []Reviewer{a, b}, ReviewInput{})
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1 (nil should be skipped)", len(got))
	}
	if got[0].Reviewer != "speaker" {
		t.Errorf("wrong reviewer survived: %q", got[0].Reviewer)
	}
}

func TestLoopReturnsErrorWithoutContinuing(t *testing.T) {
	boom := errors.New("boom")
	a := &fakeReviewer{name: "a", out: &ReviewRecord{Verdict: VerdictPass}}
	b := &fakeReviewer{name: "b", err: boom}
	c := &fakeReviewer{name: "c", out: &ReviewRecord{Verdict: VerdictPass}}

	got, err := Loop(context.Background(), []Reviewer{a, b, c}, ReviewInput{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	// 'a' ran before the error; partial results are returned.
	if len(got) != 1 || got[0].Reviewer != "a" {
		t.Errorf("partial results = %+v, want only [a]", got)
	}
	if c.called {
		t.Errorf("Loop should stop on error, but c was called")
	}
}

func TestAggregateVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   []ReviewRecord
		want Verdict
	}{
		{"empty → pass", nil, VerdictPass},
		{"all pass", []ReviewRecord{{Verdict: VerdictPass}, {Verdict: VerdictPass}}, VerdictPass},
		{"any warn → warn", []ReviewRecord{{Verdict: VerdictPass}, {Verdict: VerdictWarn}}, VerdictWarn},
		{"any fail → fail", []ReviewRecord{{Verdict: VerdictWarn}, {Verdict: VerdictFail}, {Verdict: VerdictPass}}, VerdictFail},
		{"fail beats warn regardless of order", []ReviewRecord{{Verdict: VerdictFail}, {Verdict: VerdictWarn}}, VerdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateVerdict(tc.in); got != tc.want {
				t.Errorf("AggregateVerdict = %q, want %q", got, tc.want)
			}
		})
	}
}
