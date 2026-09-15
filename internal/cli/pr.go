package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/jobs"
)

// openTaskPR pushes the task's worktree branch to origin and opens a PR against
// base (default "main"), requesting review from reviewer when non-empty. It is
// the autonomous-loop's "tag me for review" step - and the loop NEVER merges:
// the PR sits awaiting the human. Returns the PR URL.
//
// gh is invoked with cmd.Dir set to the worktree (gh has no global -C), and all
// args are passed as discrete argv elements (no shell), so the title/body can't
// shell-inject.
func openTaskPR(workDir, branch string, task *brain.TaskRecord, reviewer, base string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("openTaskPR: empty branch (--pr requires --worktree)")
	}
	if out, err := exec.Command("git", "-C", workDir, "push", "-u", "origin", branch).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git push %s: %w (%s)", branch, err, strings.TrimSpace(string(out)))
	}
	if base == "" {
		base = "main"
	}
	title := fmt.Sprintf("#%d %s", task.TaskID, task.Title)
	body := fmt.Sprintf("Autonomous-loop work for task #%d: %s\n\n"+
		"Opened by `aida loop --pr`. Awaiting human review + merge - the loop never merges on its own.",
		task.TaskID, task.Title)
	args := []string{"pr", "create", "--head", branch, "--base", base, "--title", title, "--body", body}
	if reviewer != "" {
		args = append(args, "--reviewer", reviewer)
	}
	c := exec.Command("gh", args...)
	c.Dir = workDir
	out, err := c.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("gh pr create: %w (%s)", err, s)
	}
	return extractPRURL(s), nil
}

// extractPRURL pulls the PR URL line out of `gh pr create` output.
func extractPRURL(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); strings.Contains(ln, "/pull/") || strings.Contains(ln, "github.com") {
			return ln
		}
	}
	return s
}

// worktreeDiff returns `git diff <base>` for the worktree, capped so a huge
// change doesn't blow the reviewer prompt budget.
func worktreeDiff(workDir, base string) string {
	if base == "" {
		base = "origin/main"
	}
	out, _ := exec.Command("git", "-C", workDir, "diff", base).CombinedOutput()
	d := strings.TrimSpace(string(out))
	const max = 14000
	if len(d) > max {
		d = d[:max] + "\n…(diff truncated)"
	}
	return d
}

// reviewLens is one adversarial review perspective.
type reviewLens struct {
	name   string
	prompt string
}

var reviewLenses = []reviewLens{
	{"correctness", "Does the change correctly accomplish the task with no bugs, regressions, or missed edge cases?"},
	{"security", "Any injection, secret leakage, unsafe shell/exec, path traversal, or unsafe input handling?"},
	{"test-coverage", "Is the new/changed behavior covered by tests that actually assert it (not just compile)?"},
}

// runReviewPanel spawns n adversarial reviewer agents (distinct lenses, cycling)
// over the worktree diff and returns whether a MAJORITY approved. A reviewer
// that errors or produces no clear verdict counts as a REJECT (fail-safe).
// Reviewers run sequentially in the worktree to avoid concurrent edits to a
// shared tree, and are told to review only. Returns true immediately when the
// panel is disabled (n<=0) or the diff is empty.
func (o loopOpts) runReviewPanel(ctx context.Context, store *jobs.Store, aidaBin, profileName, workDir string, task *brain.TaskRecord, state *loopState) bool {
	n := o.reviewPanel
	if n <= 0 {
		return true
	}
	diff := worktreeDiff(workDir, "origin/"+orDefault(o.base, "main"))
	if diff == "" {
		fmt.Println("  review panel: empty diff - nothing to review, approving")
		return true
	}
	approvals := 0
	for i := 0; i < n; i++ {
		lens := reviewLenses[i%len(reviewLenses)]
		prompt := fmt.Sprintf(
			"You are an adversarial code reviewer. Lens: %s - %s\n\n"+
				"Task #%d: %s\n\nReview ONLY this diff (do NOT modify any files):\n\n```diff\n%s\n```\n\n"+
				"Reply with a single line starting with APPROVE or REJECT, then a one-line reason.",
			lens.name, lens.prompt, task.TaskID, task.Title, diff)
		runID, err := spawnLoopAgent(ctx, store, aidaBin, profileName, task, prompt, o.perTask, workDir, o.sbxTier, o.sandboxMemory, o.linuxAidaBin)
		if err != nil {
			fmt.Printf("  review[%s]: agent error (%v) - counting as REJECT\n", lens.name, err)
			continue
		}
		if state != nil {
			state.addCost(readRunCostUSD(profileName, runID))
		}
		verdict := parseReviewVerdict(profileName, runID)
		fmt.Printf("  review[%s]: %s\n", lens.name, verdict)
		if verdict == "APPROVE" {
			approvals++
		}
	}
	majority := approvals*2 > n
	fmt.Printf("  review panel: %d/%d approved → %v\n", approvals, n, majority)
	return majority
}

// parseReviewVerdict reads a reviewer run's output and returns "APPROVE" or
// "REJECT". No clear verdict is fail-safe REJECT.
func parseReviewVerdict(profileName, runID string) string {
	out, err := os.ReadFile(jobs.OutputPath(profileName, runID))
	if err != nil {
		return "REJECT"
	}
	up := strings.ToUpper(string(out))
	ai := strings.Index(up, "APPROVE")
	ri := strings.Index(up, "REJECT")
	switch {
	case ai >= 0 && (ri < 0 || ai < ri):
		return "APPROVE"
	case ri >= 0:
		return "REJECT"
	default:
		return "REJECT"
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
