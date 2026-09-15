package eval

import (
	"context"
	"testing"

	"github.com/ryanlitalien/aida/internal/sources"
)

func mkArtifacts(n int) []sources.Artifact {
	out := make([]sources.Artifact, n)
	for i := 0; i < n; i++ {
		out[i] = sources.Artifact{Type: "row", ID: "x"}
	}
	return out
}

func TestCompletenessReviewer_ListQuestionWithBulletsPasses(t *testing.T) {
	in := ReviewInput{
		Question: "what are my open PRs?",
		Answer:   "You have 3 open PRs.\n\n- PR 1\n- PR 2\n- PR 3",
		Results: []sources.SourceResult{
			{Source: "github", Artifacts: mkArtifacts(3)},
		},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; %+v", rec.Verdict, rec)
	}
}

func TestCompletenessReviewer_ListQuestionNoBulletsWarns(t *testing.T) {
	in := ReviewInput{
		Question: "list all my open issues",
		Answer:   "Your open issues include several items related to the migration.",
		Results: []sources.SourceResult{
			{Source: "github", Artifacts: mkArtifacts(5)},
		},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictWarn {
		t.Errorf("Verdict = %q, want warn; %+v", rec.Verdict, rec)
	}
	if len(rec.Issues) != 1 || rec.Issues[0].Type != "early-stopping" {
		t.Errorf("expected early-stopping issue, got %+v", rec.Issues)
	}
}

func TestCompletenessReviewer_ListQuestionTruncatedListWarns(t *testing.T) {
	in := ReviewInput{
		Question: "show me all my tasks",
		Answer:   "You have 4 tasks.\n\n- The first one is the most urgent",
		Results: []sources.SourceResult{
			{Source: "github", Artifacts: mkArtifacts(4)},
		},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictWarn {
		t.Errorf("Verdict = %q, want warn for truncated list; %+v", rec.Verdict, rec)
	}
}

func TestCompletenessReviewer_NoArtifactsSkipsListCheck(t *testing.T) {
	// Question asks for a list but no results returned. Synth
	// correctly says "no results"; reviewer should pass, not
	// flag early-stopping.
	in := ReviewInput{
		Question: "list all my PRs",
		Answer:   "You have no open PRs.",
		Results:  nil,
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass on empty results; %+v", rec.Verdict, rec)
	}
}

func TestCompletenessReviewer_CountQuestionMissingNumberFails(t *testing.T) {
	in := ReviewInput{
		Question: "how many PRs are open?",
		Answer:   "You have several open pull requests across multiple repositories.",
		Results:  []sources.SourceResult{{Source: "github", Artifacts: mkArtifacts(3)}},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictFail {
		t.Errorf("Verdict = %q, want fail; %+v", rec.Verdict, rec)
	}
	if len(rec.Issues) == 0 || rec.Issues[0].Type != "no-numeric-answer" {
		t.Errorf("expected no-numeric-answer issue, got %+v", rec.Issues)
	}
}

func TestCompletenessReviewer_CountQuestionWithNumberPasses(t *testing.T) {
	in := ReviewInput{
		Question: "how many PRs are open?",
		Answer:   "You have 7 open PRs.",
		Results:  []sources.SourceResult{{Source: "github", Artifacts: mkArtifacts(7)}},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("Verdict = %q, want pass; %+v", rec.Verdict, rec)
	}
}

func TestCompletenessReviewer_FreeFormQuestionPasses(t *testing.T) {
	// "Why did X fail" doesn't trigger any shape rule.
	in := ReviewInput{
		Question: "why did the deploy fail yesterday?",
		Answer:   "The deploy failed because the env var was missing.",
		Results:  []sources.SourceResult{{Source: "chrono", Artifacts: mkArtifacts(2)}},
	}
	rec, _ := NewCompletenessReviewer().Review(context.Background(), in)
	if rec.Verdict != VerdictPass {
		t.Errorf("free-form question should pass; got %+v", rec)
	}
}

func TestCompletenessReviewer_EmptyInputsSkipped(t *testing.T) {
	r := NewCompletenessReviewer()
	if rec, _ := r.Review(context.Background(), ReviewInput{Question: "", Answer: "x"}); rec != nil {
		t.Errorf("empty question should skip, got %+v", rec)
	}
	if rec, _ := r.Review(context.Background(), ReviewInput{Question: "x", Answer: ""}); rec != nil {
		t.Errorf("empty answer should skip, got %+v", rec)
	}
}

func TestCompletenessReviewer_NameIsStable(t *testing.T) {
	if (CompletenessReviewer{}).Name() != "completeness" {
		t.Errorf("Name() = %q, want completeness", (CompletenessReviewer{}).Name())
	}
}
