package cli

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
)

func TestAuditGarden_OrphanedLayer(t *testing.T) {
	known := map[string]bool{"github": true, "snowflake": true}
	layers := map[string]*library.ResolvedLayer{
		"sources/github":    {AbsFile: "/tmp/github.md"}, // alive
		"sources/snowflake": {AbsFile: "/tmp/snowflake.md"},
		"sources/dead-one":  {AbsFile: "/tmp/dead-one.md"}, // orphan
		"global":            {AbsFile: "/tmp/global.md"},   // not a source layer, ignored
		"personal":          {AbsFile: "/tmp/personal.md"},
	}
	got := auditGarden(known, layers, nil, nil, 0)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(got), got)
	}
	if got[0].Category != "orphaned-layer" {
		t.Errorf("Category = %q, want orphaned-layer", got[0].Category)
	}
	if got[0].Subject != "sources/dead-one" {
		t.Errorf("Subject = %q, want sources/dead-one", got[0].Subject)
	}
}

func TestAuditGarden_DeadSourceRef(t *testing.T) {
	known := map[string]bool{"github": true}
	ls := []lessons.Lesson{
		{
			RunID:                   "run-1",
			FeedbackIntendedSources: []string{"snowflake-old", "github"},
		},
		{
			RunID:                  "run-2",
			FeedbackIntendedSource: "chrono-deprecated",
		},
		{
			RunID:                   "run-3",
			FeedbackExcludedSources: []string{"github"}, // alive - not flagged
		},
	}
	got := auditGarden(known, nil, ls, nil, 0)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	for _, f := range got {
		if f.Category != "dead-source-ref" {
			t.Errorf("Category = %q, want dead-source-ref", f.Category)
		}
	}
}

func TestAuditGarden_NoFindingsWhenAllAlive(t *testing.T) {
	known := map[string]bool{"a": true, "b": true}
	layers := map[string]*library.ResolvedLayer{
		"sources/a": {AbsFile: "/tmp/a.md"},
		"sources/b": {AbsFile: "/tmp/b.md"},
	}
	ls := []lessons.Lesson{
		{RunID: "r1", FeedbackIntendedSources: []string{"a"}},
		{RunID: "r2", FeedbackExcludedSources: []string{"b"}},
	}
	got := auditGarden(known, layers, ls, nil, 0)
	if len(got) != 0 {
		t.Errorf("expected 0 findings, got %d: %+v", len(got), got)
	}
}

func TestAuditGarden_LongLayer(t *testing.T) {
	known := map[string]bool{"github": true, "snowflake": true}
	layers := map[string]*library.ResolvedLayer{
		"sources/github":    {AbsFile: "/tmp/github.md"},
		"sources/snowflake": {AbsFile: "/tmp/snowflake.md"},
		"global":            {AbsFile: "/tmp/global.md"},
	}
	linesByPath := map[string]int{
		"/tmp/github.md":    50,  // under cap
		"/tmp/snowflake.md": 350, // over cap
		"/tmp/global.md":    250, // over cap (non-source layer also flagged)
	}
	got := auditGarden(known, layers, nil, linesByPath, 200)
	long := 0
	for _, f := range got {
		if f.Category == "long-layer" {
			long++
		}
	}
	if long != 2 {
		t.Errorf("got %d long-layer findings, want 2: %+v", long, got)
	}
}

func TestAuditGarden_LongLayerDisabledWhenCapZero(t *testing.T) {
	layers := map[string]*library.ResolvedLayer{
		"sources/x": {AbsFile: "/tmp/x.md"},
	}
	linesByPath := map[string]int{"/tmp/x.md": 9999}
	known := map[string]bool{"x": true}
	got := auditGarden(known, layers, nil, linesByPath, 0)
	for _, f := range got {
		if f.Category == "long-layer" {
			t.Errorf("cap=0 should disable long-layer check, got %+v", f)
		}
	}
}

func TestAuditGarden_DeduplicatesDeadNamesPerLesson(t *testing.T) {
	// Same dead source referenced in both intended and excluded
	// should produce ONE finding for that lesson, not two.
	known := map[string]bool{}
	ls := []lessons.Lesson{
		{
			RunID:                   "r1",
			FeedbackIntendedSources: []string{"dead", "also-dead"},
			FeedbackIntendedSource:  "dead",           // duplicate
			FeedbackExcludedSources: []string{"dead"}, // duplicate again
		},
	}
	got := auditGarden(known, nil, ls, nil, 0)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(got))
	}
	// Detail should list each unique dead name once, sorted.
	want := "also-dead, dead"
	if !strings.Contains(got[0].Detail, want) {
		t.Errorf("Detail = %q, want it to contain %q", got[0].Detail, want)
	}
}
