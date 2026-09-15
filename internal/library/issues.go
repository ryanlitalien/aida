package library

import (
	"fmt"
	"strings"
)

// IssueSeverity classifies a library issue's blast radius.
type IssueSeverity string

const (
	IssueSeverityError IssueSeverity = "error" // source dropped from registry; would shell-inject or crash
	IssueSeverityWarn  IssueSeverity = "warn"  // source loaded but routing quality degraded
)

// IssueType groups issues for telemetry, doc-gardening, and the
// agent's remediation prompt. Stable strings so cross-machine
// brain analysis can group on them.
const (
	IssueTypeRawPassthrough = "raw-passthrough"
)

// LibraryIssue is a structured report of one problem found when
// loading a source config. Issues with severity "error" cause the
// source to be dropped from the registry; "warn" issues let the
// source load but are surfaced to users and agents as remediation
// guidance.
//
// Captured during Registry.LoadSources(); read back via
// Registry.Issues(). Inspired by the OpenAI harness-engineering
// pattern of writing validator messages as remediation prompts so
// the agent can act on them rather than silently routing around
// the broken source.
type LibraryIssue struct {
	// SourceName is the YAML key under the manifest's `sources:` map.
	SourceName string

	Severity IssueSeverity

	// Type is a short machine-readable identifier (one of the
	// IssueType* constants above). Stable for cross-run analysis.
	Type string

	// Reason is a one-sentence diagnosis: what's wrong and why
	// we drop the source. Plain text, no markdown.
	Reason string

	// Fix is multi-line remediation guidance: how to make the
	// source valid. Reads as a how-to, not a description. May
	// contain example YAML in indented form.
	Fix string

	// Path is the absolute filesystem path to the source's YAML.
	// Empty when the issue is registry-level rather than file-level.
	Path string
}

// Format returns a one-line "Severity Type: Reason" rendering for
// verbose console output. The Fix is intentionally NOT included
// here so verbose lines stay scannable; callers that want the fix
// should print Format() and FormatFix() on separate lines or use
// AgentContext().
func (i LibraryIssue) Format() string {
	return fmt.Sprintf("%s %s: %s [%s]",
		strings.ToUpper(string(i.Severity)), i.Type, i.Reason, i.SourceName)
}

// FormatFix returns the multi-line remediation block, prefixed
// with "Fix:". Empty string when the issue has no Fix populated.
func (i LibraryIssue) FormatFix() string {
	if strings.TrimSpace(i.Fix) == "" {
		return ""
	}
	return "Fix: " + strings.TrimSpace(i.Fix)
}

// AgentContext renders the issue as a remediation block suitable
// for injection into an agent's system prompt. Format follows the
// OpenAI harness pattern: state the source, the problem, and the
// concrete fix in one block so the agent can surface it to the
// user when answering questions where this source would have been
// relevant.
func (i LibraryIssue) AgentContext() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s [%s]\n", i.SourceName, i.Type)
	fmt.Fprintf(&b, "  Problem: %s\n", i.Reason)
	if fix := strings.TrimSpace(i.Fix); fix != "" {
		// Indent each fix line by 2 spaces so it nests under the bullet.
		lines := strings.Split(fix, "\n")
		for j, line := range lines {
			lines[j] = "  " + line
		}
		fmt.Fprintf(&b, "  Fix:\n%s\n", strings.Join(lines, "\n"))
	}
	return b.String()
}
