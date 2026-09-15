package library

import (
	"strings"
	"testing"
)

func TestLibraryIssue_Format_OneLine(t *testing.T) {
	iss := LibraryIssue{
		SourceName: "butterstack-github",
		Severity:   IssueSeverityError,
		Type:       IssueTypeRawPassthrough,
		Reason:     "exec.query is a bare {query} placeholder",
	}
	got := iss.Format()
	for _, want := range []string{"ERROR", "raw-passthrough", "butterstack-github", "bare {query}"} {
		if !strings.Contains(got, want) {
			t.Errorf("Format() missing %q: %q", want, got)
		}
	}
	if strings.Count(got, "\n") != 0 {
		t.Errorf("Format() should be single-line, got: %q", got)
	}
}

func TestLibraryIssue_FormatFix_PrefixedAndTrimmed(t *testing.T) {
	iss := LibraryIssue{Fix: "  prepend a real command  \n"}
	got := iss.FormatFix()
	if !strings.HasPrefix(got, "Fix: ") {
		t.Errorf("FormatFix() should start with 'Fix: ', got %q", got)
	}
	if strings.HasSuffix(got, "\n") || strings.HasSuffix(got, " ") {
		t.Errorf("FormatFix() should be trimmed, got %q", got)
	}
}

func TestLibraryIssue_FormatFix_EmptyWhenNoFix(t *testing.T) {
	if got := (LibraryIssue{}).FormatFix(); got != "" {
		t.Errorf("empty Fix should produce empty FormatFix(), got %q", got)
	}
	if got := (LibraryIssue{Fix: "  "}).FormatFix(); got != "" {
		t.Errorf("whitespace-only Fix should produce empty FormatFix(), got %q", got)
	}
}

func TestLibraryIssue_AgentContext_StructuredAndIndented(t *testing.T) {
	iss := LibraryIssue{
		SourceName: "butterstack-github",
		Type:       IssueTypeRawPassthrough,
		Reason:     "exec.query is a bare {query} placeholder",
		Fix:        "Prepend a real command:\n  exec:\n    query: 'gh pr list {query}'",
	}
	got := iss.AgentContext()
	if !strings.HasPrefix(got, "- butterstack-github") {
		t.Errorf("AgentContext should bullet on source name; got %q", got)
	}
	if !strings.Contains(got, "Problem:") {
		t.Errorf("AgentContext missing 'Problem:'; got %q", got)
	}
	if !strings.Contains(got, "Fix:") {
		t.Errorf("AgentContext missing 'Fix:'; got %q", got)
	}
	// Each line of the Fix should be indented under the bullet so
	// the agent reads it as nested guidance, not a top-level claim.
	for _, line := range strings.Split(strings.TrimSpace(iss.Fix), "\n") {
		if !strings.Contains(got, "  "+line) {
			t.Errorf("AgentContext should indent fix line %q; got %q", line, got)
		}
	}
}

func TestLibraryIssue_AgentContext_NoFixOmitsFixSection(t *testing.T) {
	iss := LibraryIssue{
		SourceName: "x",
		Reason:     "y",
		// Fix intentionally empty.
	}
	got := iss.AgentContext()
	if strings.Contains(got, "Fix:") {
		t.Errorf("AgentContext with empty Fix should not include 'Fix:'; got %q", got)
	}
}
