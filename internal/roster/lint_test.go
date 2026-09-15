package roster

import (
	"strings"
	"testing"
)

func findIssue(issues []LintIssue, entry string) *LintIssue {
	for i := range issues {
		if issues[i].Entry == entry {
			return &issues[i]
		}
	}
	return nil
}

func TestLint(t *testing.T) {
	cases := []struct {
		name             string
		dir              string
		availableSources []string
		wantHasError     bool
		check            func(t *testing.T, issues []LintIssue)
	}{
		{
			name:             "good roster: no issues",
			dir:              "testdata/lint/good",
			availableSources: []string{"workouts"},
			wantHasError:     false,
			check: func(t *testing.T, issues []LintIssue) {
				if len(issues) != 0 {
					t.Errorf("issues = %+v, want none", issues)
				}
			},
		},
		{
			name:             "bad kind",
			dir:              "testdata/lint/bad-kind",
			availableSources: nil,
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				issue := findIssue(issues, "mystery")
				if issue == nil {
					t.Fatalf("no issue for entry %q in %+v", "mystery", issues)
				}
				if issue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", issue.Severity, LintError)
				}
				if !strings.Contains(issue.Message, "invalid kind") {
					t.Errorf("Message = %q, want to mention invalid kind", issue.Message)
				}
			},
		},
		{
			name:             "missing spec block",
			dir:              "testdata/lint/missing-spec",
			availableSources: nil,
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				issue := findIssue(issues, "ghost")
				if issue == nil {
					t.Fatalf("no issue for entry %q in %+v", "ghost", issues)
				}
				if issue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", issue.Severity, LintError)
				}
				if !strings.Contains(issue.Message, "subagent:") {
					t.Errorf("Message = %q, want to mention the missing subagent: block", issue.Message)
				}
			},
		},
		{
			name:             "bad slug",
			dir:              "testdata/lint/bad-slug",
			availableSources: []string{"workouts"},
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				issue := findIssue(issues, "bad name!")
				if issue == nil {
					t.Fatalf("no issue for entry %q in %+v", "bad name!", issues)
				}
				if issue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", issue.Severity, LintError)
				}
				if !strings.Contains(issue.Message, "must match") {
					t.Errorf("Message = %q, want to mention the slug pattern", issue.Message)
				}
			},
		},
		{
			name:             "unknown source",
			dir:              "testdata/lint/unknown-source",
			availableSources: []string{"workouts"},
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				issue := findIssue(issues, "ghost-source")
				if issue == nil {
					t.Fatalf("no issue for entry %q in %+v", "ghost-source", issues)
				}
				if issue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", issue.Severity, LintError)
				}
				if !strings.Contains(issue.Message, "nonexistent-source-xyz") {
					t.Errorf("Message = %q, want to mention the unknown source name", issue.Message)
				}
			},
		},
		{
			name:             "duplicate alias",
			dir:              "testdata/lint/dup-alias",
			availableSources: nil,
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				var dupIssue *LintIssue
				for i := range issues {
					if strings.Contains(issues[i].Message, "collides across entries") {
						dupIssue = &issues[i]
						break
					}
				}
				if dupIssue == nil {
					t.Fatalf("no duplicate-collision issue found in %+v", issues)
				}
				if dupIssue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", dupIssue.Severity, LintError)
				}
				if !strings.Contains(dupIssue.Message, "pam") || !strings.Contains(dupIssue.Message, "pamela-two") {
					t.Errorf("Message = %q, want to mention both colliding entries", dupIssue.Message)
				}
			},
		},
		{
			name:             "missing aida entry",
			dir:              "testdata/lint/missing-aida",
			availableSources: []string{"workouts"},
			wantHasError:     true,
			check: func(t *testing.T, issues []LintIssue) {
				issue := findIssue(issues, "roster")
				if issue == nil {
					t.Fatalf("no roster-level issue found in %+v", issues)
				}
				if issue.Severity != LintError {
					t.Errorf("Severity = %q, want %q", issue.Severity, LintError)
				}
				if !strings.Contains(issue.Message, `"aida"`) {
					t.Errorf("Message = %q, want to mention the reserved aida entry", issue.Message)
				}
			},
		},
		{
			name:             "call-sign collides with library source: warn only",
			dir:              "testdata/lint/callsign-collides-with-source",
			availableSources: []string{"workouts"},
			wantHasError:     false,
			check: func(t *testing.T, issues []LintIssue) {
				if len(issues) != 1 {
					t.Fatalf("issues = %+v, want exactly 1", issues)
				}
				if issues[0].Severity != LintWarn {
					t.Errorf("Severity = %q, want %q", issues[0].Severity, LintWarn)
				}
				if !strings.Contains(issues[0].Message, "workouts") {
					t.Errorf("Message = %q, want to mention the colliding source name", issues[0].Message)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issues, err := Lint(tc.dir, tc.availableSources)
			if err != nil {
				t.Fatalf("Lint: %v", err)
			}
			if got := HasError(issues); got != tc.wantHasError {
				t.Errorf("HasError() = %v, want %v (issues: %+v)", got, tc.wantHasError, issues)
			}
			tc.check(t, issues)
		})
	}
}

func TestLint_MissingRosterFile(t *testing.T) {
	issues, err := Lint(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if issues != nil {
		t.Errorf("issues = %+v, want nil", issues)
	}
}

// TestLint_IssuesSortedErrorsFirstThenEntry uses the dup-alias fixture
// (which produces more than one issue) to check the documented sort order:
// errors before warnings, then alphabetically by Entry.
func TestLint_IssuesSortedErrorsFirstThenEntry(t *testing.T) {
	issues, err := Lint("testdata/lint/dup-alias", nil)
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	if len(issues) == 0 {
		t.Fatal("expected at least one issue")
	}
	seenWarn := false
	for i, issue := range issues {
		if issue.Severity == LintWarn {
			seenWarn = true
			continue
		}
		if seenWarn {
			t.Errorf("issue[%d] is severity %q but appears after a warning; want errors sorted first", i, issue.Severity)
		}
	}
	for i := 1; i < len(issues); i++ {
		if issues[i-1].Severity == issues[i].Severity && issues[i-1].Entry > issues[i].Entry {
			t.Errorf("issues not sorted by Entry within severity %q: %q > %q", issues[i].Severity, issues[i-1].Entry, issues[i].Entry)
		}
	}
}

func TestHasError(t *testing.T) {
	if HasError(nil) {
		t.Error("HasError(nil) = true, want false")
	}
	if HasError([]LintIssue{{Severity: LintWarn}}) {
		t.Error("HasError(warn-only) = true, want false")
	}
	if !HasError([]LintIssue{{Severity: LintWarn}, {Severity: LintError}}) {
		t.Error("HasError(warn+error) = false, want true")
	}
}
