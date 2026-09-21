package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/worktree"
)

// openTaskPR pushes the task's worktree branch to remote (the one the base
// branch tracks, resolved once per run by loopOpts.resolveBase) and opens a PR
// against base (default "main"), requesting review from reviewer when
// non-empty. It is the autonomous-loop's "tag me for review" step - and the
// loop NEVER merges: the PR sits awaiting the human. Returns the PR URL.
//
// gh is invoked with cmd.Dir set to the worktree (gh has no global -C), and all
// args are passed as discrete argv elements (no shell), so the title/body can't
// shell-inject.
func openTaskPR(workDir, branch string, task *brain.TaskRecord, reviewer, base, remote string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("openTaskPR: empty branch (--pr requires --worktree)")
	}
	if remote == "" {
		return "", fmt.Errorf("openTaskPR: base branch %q tracks no remote, nothing to push to", orDefault(base, "main"))
	}
	if err := reviewMirrorRefusal(workDir, remote); err != nil {
		return "", err
	}
	if out, err := exec.Command("git", "-C", workDir, "push", "-u", remote, branch).CombinedOutput(); err != nil {
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

// reviewMirrorRefusal fails fast when pushing to remote would bypass a
// review-mirror flow: remote is a public GitHub repo, and the repo also has
// some other remote that is not on GitHub (a Forgejo or self-hosted mirror
// where PRs are reviewed before anything reaches the public repo). Pushing an
// autonomous branch straight to the public repo in that setup is exactly the
// wrong move, and opening the PR on the mirror instead is a separate feature,
// so --pr is refused outright rather than half-working. Returns nil for the
// ordinary case (every remote on GitHub, or the resolved remote off GitHub).
func reviewMirrorRefusal(workDir, remote string) error {
	resolvedURL := worktree.RemoteURL(workDir, remote)
	if !isGitHubURL(resolvedURL) {
		return nil
	}
	for _, name := range worktree.Remotes(workDir) {
		if name == remote {
			continue
		}
		url := worktree.RemoteURL(workDir, name)
		if url == "" || isGitHubURL(url) {
			continue
		}
		return fmt.Errorf("--pr is not supported in this repo yet: it uses a review-mirror flow "+
			"(base branch tracks the public GitHub remote %q at %s, but remote %q at %s is a non-GitHub mirror where PRs are reviewed first). "+
			"Pushing the branch straight to %q would skip that review. Run the loop without --pr (for example --commit), "+
			"or push the auto/ branch to %q and open the review PR there by hand",
			remote, resolvedURL, name, url, remote, name)
	}
	return nil
}

// isGitHubURL reports whether a git remote URL points at github.com, in any
// of the ssh (git@github.com:o/r.git, ssh://git@github.com/o/r), https, or
// scp-with-alias forms git accepts.
func isGitHubURL(url string) bool {
	u := strings.ToLower(strings.TrimSpace(url))
	return strings.Contains(u, "github.com:") || strings.Contains(u, "github.com/")
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

// worktreeDiff returns `git diff <baseRef>` for the worktree, capped so a huge
// change doesn't blow the reviewer prompt budget. baseRef is the resolved
// remote-tracking ref (e.g. public/main); an empty baseRef falls back to
// whatever main tracks in workDir's repo.
func worktreeDiff(workDir, baseRef string) string {
	if baseRef == "" {
		_, baseRef = worktree.ResolveUpstream(workDir, "main")
	}
	out, _ := exec.Command("git", "-C", workDir, "diff", baseRef).CombinedOutput()
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
	diff := worktreeDiff(workDir, o.baseRef)
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
