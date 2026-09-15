package eval

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// CodeCheck is one build/test/lint command a CodeReviewer runs to verify a
// working tree. Name labels the resulting issue type (e.g. "test" yields a
// "test-failure" Issue); Cmd is the shell command, run via `sh -c` in the
// reviewer's WorkDir.
type CodeCheck struct {
	Name string
	Cmd  string
}

// CodeReviewer is the Reviewer that makes the autonomous loop's "iterate until
// green" gate real. Unlike the answer-grading reviewers (citation/completeness/
// scope) it does not inspect the Answer string - it runs the build/test/lint
// commands in ReviewInput.Commands against ReviewInput.WorkDir and turns any
// non-zero exit into a Fail ReviewRecord whose Issues carry the failing
// command's output. The next fix iteration reads those messages.
//
// It reads WorkDir + Commands from the ReviewInput so a single instance can
// grade different worktrees across loop iterations. With no Commands it returns
// nil (no opinion), so adding it to a reviewer set is a no-op until checks are
// configured - and so it never interferes with the synthesis-side DefaultReviewers.
type CodeReviewer struct {
	// maxOutput caps how much of a failing command's output is captured into
	// an Issue message, so a megabyte of test spew doesn't blow the next fix
	// iteration's prompt budget. 0 means no cap.
	maxOutput int
}

// NewCodeReviewer returns a CodeReviewer with a sensible output cap.
func NewCodeReviewer() *CodeReviewer { return &CodeReviewer{maxOutput: 4000} }

// Name implements Reviewer.
func (r *CodeReviewer) Name() string { return "code" }

// Review runs each configured check and aggregates failures. Returns nil (no
// opinion) when no checks are configured.
func (r *CodeReviewer) Review(ctx context.Context, in ReviewInput) (*ReviewRecord, error) {
	if len(in.Commands) == 0 {
		return nil, nil
	}
	var issues []Issue
	for _, c := range in.Commands {
		cmd := exec.CommandContext(ctx, "sh", "-c", c.Cmd)
		if in.WorkDir != "" {
			cmd.Dir = in.WorkDir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			issues = append(issues, Issue{
				Type:     checkIssueType(c.Name),
				Severity: "error",
				Message:  fmt.Sprintf("`%s` failed: %v\n%s", c.Cmd, err, tailOutput(string(out), r.maxOutput)),
				Anchor:   c.Cmd,
			})
		}
	}
	if len(issues) > 0 {
		return &ReviewRecord{
			Verdict:   VerdictFail,
			Score:     0,
			Issues:    issues,
			Rationale: fmt.Sprintf("%d of %d code checks failed", len(issues), len(in.Commands)),
		}, nil
	}
	return &ReviewRecord{
		Verdict:   VerdictPass,
		Score:     1,
		Rationale: fmt.Sprintf("all %d code checks passed", len(in.Commands)),
	}, nil
}

// checkIssueType turns a check name into a stable Issue.Type the router boost
// and `aida brain analyze` can group on (e.g. "test" -> "test-failure").
func checkIssueType(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		name = "check"
	}
	return name + "-failure"
}

// tailOutput trims output to the last max bytes (failures are usually most
// informative at the end - the panic, the failed assertion, the compiler error).
func tailOutput(s string, max int) string {
	s = strings.TrimSpace(s)
	if max > 0 && len(s) > max {
		return "…(truncated)\n" + s[len(s)-max:]
	}
	return s
}
