package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/ryanlitalien/aida/internal/worktree"
)

// Swarm is the natural-language front door to the autonomous loop. Instead of
// driving `aida loop` with a fistful of flags, the user says what they mean -
// "swarm on PR 456", "work through my prio tasks" - and aida recognizes the
// autonomous-work intent, confirms a plan in one line, and hands off to the
// machinery: `runLoopCtx` for a task set, a PR-continuation agent for a PR.
//
// "swarm" is the user's generic verb for "go work on this autonomously"; the
// system, not the user, decides the shape (a single PR → one agent; a task set
// → the loop). The flags don't go away - they become the smart defaults filled
// in here (PR-per-task for review, an auto-detected quality gate, never merge).
//
// This is the typed surface (the engine path). The Jarvis voice tool layers on
// top of the same recognize → confirm → dispatch functions.

// swarmVerbRE matches the lead verbs that mean "go work on this autonomously."
// Anchored at the start (after an optional "please"/vocative) so it fires on a
// real request - "swarm on PR 456" - and NOT on an ordinary question that
// happens to contain the word ("how does the swarm scheduler work?"). Precision
// over recall: this spawns agents, so a missed phrasing (user restates) is far
// cheaper than a false trigger.
var swarmVerbRE = regexp.MustCompile(`(?i)^\s*(?:(?:hey\s+|ok(?:ay)?\s+)?jarvis[,\s]+)?(?:please\s+|can you\s+|could you\s+|go\s+)?(swarm|loop\s+(?:on|over|through)|work\s+through|grind\s+through|churn\s+through)\b`)

// swarmPRNumRE extracts the first PR number from the request. Digit form only
// for the typed surface ("PR 456", "pull request #456"); voice word-numbers are
// a later refinement.
var swarmPRNumRE = regexp.MustCompile(`(?i)\b(?:pr|pull\s*request)\s*#?\s*(\d{1,6})\b`)

// tagExplicitRE matches an explicit tag callout: "tagged prio", "tag prio".
var tagExplicitRE = regexp.MustCompile(`(?i)\b(?:tagged|tag)\s+["']?([a-z0-9][\w-]*)`)

// tagAdjRE captures the adjective immediately before "tasks": "my prio tasks",
// "the cleanup tasks", "csv tasks". The captured word is rejected when it's a
// stopword (an article/quantifier, not a real tag) and the request falls
// through to the all-open-tasks check.
var tagAdjRE = regexp.MustCompile(`(?i)\b(?:the|my|all|those|these)?\s*([a-z0-9][\w-]*)\s+tasks?\b`)

// allTasksRE recognizes "every open task" phrasings that carry no tag filter.
var allTasksRE = regexp.MustCompile(`(?i)\b(?:all\s+(?:open\s+)?tasks|my\s+tasks|the\s+tasks|open\s+tasks|everything|my\s+plate|the\s+backlog|to-?dos?)\b`)

// swarmStopwords are words that can land in the tagAdjRE capture slot but are
// not real tags - they signal "all open tasks", not a filter.
var swarmStopwords = map[string]bool{
	"open": true, "all": true, "my": true, "the": true, "those": true,
	"these": true, "remaining": true, "pending": true, "current": true,
	"new": true, "more": true, "other": true, "done": true, "closed": true,
	"terminal": true, "in": true, "next": true, "some": true,
}

type swarmTargetKind int

const (
	targetNone swarmTargetKind = iota
	targetPR
	targetTag
	targetAllTasks
)

// swarmPlan is the resolved, confirmable description of an autonomous-work
// request: what to work on (kind + prNum/tag), how many tasks it covers, and
// the auto-detected quality gate.
type swarmPlan struct {
	kind      swarmTargetKind
	prNum     string   // targetPR
	tag       string   // targetTag
	taskCount int      // targetTag / targetAllTasks
	checks    []string // auto-detected quality gate
	repoRoot  string
}

// recognizeSwarm reports whether the request leads with a swarm verb. Pure, so
// it's unit-tested directly.
func recognizeSwarm(question string) bool {
	return swarmVerbRE.MatchString(question)
}

// resolveSwarmTarget extracts the work target from the request. Pure (no IO) so
// the parsing is unit-tested in isolation from the brain/task count. Order is
// most-specific-first: a PR number wins over a tag, which wins over the
// all-open-tasks catch-all.
func resolveSwarmTarget(question string) (kind swarmTargetKind, prNum, tag string) {
	if m := swarmPRNumRE.FindStringSubmatch(question); len(m) >= 2 {
		return targetPR, m[1], ""
	}
	if m := tagExplicitRE.FindStringSubmatch(question); len(m) >= 2 {
		return targetTag, "", strings.ToLower(m[1])
	}
	if m := tagAdjRE.FindStringSubmatch(question); len(m) >= 2 {
		if cand := strings.ToLower(m[1]); !swarmStopwords[cand] {
			return targetTag, "", cand
		}
	}
	if allTasksRE.MatchString(question) {
		return targetAllTasks, "", ""
	}
	return targetNone, "", ""
}

// HandleSwarmIntent is the deterministic pre-pipeline intercept for
// autonomous-work requests. It returns handled=true when it took ownership of
// the request - whether it ran the swarm or the user declined - so the caller
// skips the normal query pipeline. A non-swarm request, or one in a
// non-interactive context, returns (false, nil) and falls straight through.
//
// Confirmation is mandatory and needs a TTY (the human-in-the-loop gate), so
// piped stdin and `--agent`/`--run-dir` runs are never hijacked.
func HandleSwarmIntent(ctx context.Context, question string, cfg *config.Config, profileName string) (bool, error) {
	if !recognizeSwarm(question) {
		return false, nil
	}
	// The confirm needs an interactive terminal. Don't hijack a piped or
	// agent-driven run (the agent loop is itself a swarm worker; intercepting
	// there would recurse).
	if agentMode || agentRunDir != "" || !isTerminal(os.Stdin) {
		return false, nil
	}

	repoRoot, rootErr := worktree.RepoRoot(".")
	if rootErr != nil {
		return true, fmt.Errorf("swarm needs to run inside a git repo: %w", rootErr)
	}

	kind, prNum, tag := resolveSwarmTarget(question)
	if kind == targetNone {
		fmt.Printf("%s I can swarm a PR or a set of tasks, but I couldn't tell what to target.\n"+
			"  Try: aida \"swarm on PR 456\"  ·  aida \"work through my prio tasks\"  ·  aida \"swarm my tasks\"\n", ui.WarnIcon)
		return true, nil
	}

	plan := swarmPlan{kind: kind, prNum: prNum, tag: tag, repoRoot: repoRoot}
	plan.checks = detectChecks(repoRoot)

	// Count the task set up front so the confirm is honest about scope and we
	// can bail before prompting when there's nothing to do.
	if kind == targetTag || kind == targetAllTasks {
		n, err := countSwarmTasks(cfg, profileName, tag)
		if err != nil {
			return true, fmt.Errorf("count tasks: %w", err)
		}
		if n == 0 {
			if tag != "" {
				fmt.Printf("%s No open tasks tagged %q - nothing to swarm.\n", ui.WarnIcon, tag)
			} else {
				fmt.Printf("%s No open tasks - nothing to swarm.\n", ui.WarnIcon)
			}
			return true, nil
		}
		plan.taskCount = n
	}

	printSwarmPlan(plan)

	// --dry-run: show the plan, don't prompt, don't launch.
	if dryRun {
		fmt.Println("  [dry-run] not launching.")
		return true, nil
	}

	if !confirmYes("Proceed?") {
		fmt.Println("Cancelled.")
		return true, nil
	}

	switch kind {
	case targetPR:
		return true, runSwarmPR(ctx, plan, profileName)
	default:
		return true, runSwarmTasks(ctx, plan)
	}
}

// printSwarmPlan renders the one-line-per-step plan the user confirms against.
func printSwarmPlan(p swarmPlan) {
	gate := "no quality gate (agent self-report only)"
	if len(p.checks) > 0 {
		gate = "gate: " + strings.Join(p.checks, " && ")
	}
	switch p.kind {
	case targetPR:
		fmt.Printf("\nswarm - PR #%s\n", p.prNum)
		fmt.Printf("  • check out the PR branch in an isolated worktree, work it with an agent\n")
		fmt.Printf("  • commit + push when it reaches a stopping point (never merges)\n")
		fmt.Printf("  • %s\n", gate)
	case targetTag:
		fmt.Printf("\nswarm - %d task(s) tagged %q\n", p.taskCount, p.tag)
		fmt.Printf("  • each task in its own worktree off main, highest-priority first\n")
		fmt.Printf("  • opens a PR per task for your review (never merges)\n")
		fmt.Printf("  • %s\n", gate)
	case targetAllTasks:
		fmt.Printf("\nswarm - %d open task(s)\n", p.taskCount)
		fmt.Printf("  • each task in its own worktree off main, highest-priority first\n")
		fmt.Printf("  • opens a PR per task for your review (never merges)\n")
		fmt.Printf("  • %s\n", gate)
	}
}

// runSwarmTasks maps a task-set plan onto the autonomous loop with the agreed
// defaults: a PR per passing task for review, an isolated worktree per task,
// the auto-detected gate, never merge. The maxIterations bound is sized to the
// set (plus slack) so a single `swarm` run can drain it while still bounding a
// task that keeps re-spinning.
func runSwarmTasks(ctx context.Context, p swarmPlan) error {
	o := defaultLoopOpts()
	if p.tag != "" {
		o.tags = []string{p.tag}
	}
	o.pr = true // implies worktree; opens a PR per task for review, never merges
	o.worktree = true
	o.base = "main"
	o.checks = p.checks
	o.maxIterations = p.taskCount + 5
	return runLoopCtx(ctx, o)
}

// runSwarmPR works an existing PR foreground: a detached worktree, `gh pr
// checkout`, then a single `aida --agent` pass that reviews the branch, makes
// progress, and commits/pushes. Streams live to the terminal and inherits stdin
// so the agent's ask_user prompts can be answered inline. The worktree is
// removed on a clean finish; on agent failure it's left in place (with the path
// printed) so in-flight work isn't lost.
func runSwarmPR(ctx context.Context, p swarmPlan, profileName string) error {
	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return fmt.Errorf("locate aida on PATH: %w", err)
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh not on PATH (needed to check out the PR): %w", err)
	}

	worktreeDir := filepath.Join(config.Dir(), "worktrees", profileName,
		fmt.Sprintf("swarm-pr-%s-%d", p.prNum, time.Now().Unix()))
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return fmt.Errorf("mkdir worktrees parent: %w", err)
	}

	fmt.Printf("\nChecking out PR #%s…\n", p.prNum)
	if out, err := exec.Command("git", "-C", p.repoRoot, "worktree", "add", "--detach", worktreeDir).CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	checkout := exec.Command("gh", "pr", "checkout", p.prNum)
	checkout.Dir = worktreeDir
	if out, err := checkout.CombinedOutput(); err != nil {
		_ = exec.Command("git", "-C", p.repoRoot, "worktree", "remove", "--force", worktreeDir).Run()
		return fmt.Errorf("gh pr checkout %s: %w (%s)", p.prNum, err, strings.TrimSpace(string(out)))
	}

	fmt.Printf("Working PR #%s in %s\n\n", p.prNum, worktreeDir)
	agent := exec.CommandContext(ctx, aidaBin, "--agent", swarmPRPrompt(p.prNum))
	agent.Dir = worktreeDir
	agent.Stdin = os.Stdin
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	agent.Env = append(os.Environ(),
		"AIDA_PROFILE="+profileName,
	)
	runErr := agent.Run()
	if runErr != nil {
		fmt.Printf("\n%s Agent exited with an error; leaving the worktree in place so nothing is lost:\n  %s\n",
			ui.WarnIcon, worktreeDir)
		return fmt.Errorf("agent on PR #%s: %w", p.prNum, runErr)
	}
	if out, err := exec.Command("git", "-C", p.repoRoot, "worktree", "remove", "--force", worktreeDir).CombinedOutput(); err != nil {
		ui.PrintVerbose("swarm", "worktree remove failed: "+strings.TrimSpace(string(out)))
	}
	fmt.Printf("\n%s Done with PR #%s.\n", ui.SuccessIcon, p.prNum)
	return nil
}

// swarmPRPrompt is the agent instruction body for a PR swarm - written for the
// engine-side agent, not the user.
func swarmPRPrompt(prNum string) string {
	return fmt.Sprintf(
		"You are continuing work on PR #%s. The branch is already checked out in the "+
			"current working directory. Review the existing changes (git status, git log, "+
			"git diff origin/main...HEAD), the PR description (gh pr view %s), and any review "+
			"comments (gh pr view %s --comments). Make further progress. Use the ask_user "+
			"tool when you need a decision from me. Commit and push when you reach a stopping "+
			"point or finish. Do NOT merge the PR.",
		prNum, prNum, prNum,
	)
}

// detectChecks picks a sensible default quality gate for the repo so the user
// doesn't have to pass --check. A Makefile with a `test:` target wins (most
// projects' canonical gate); otherwise a Go module falls back to `go build`.
// Empty means no gate - the loop warns and relies on agent self-report.
func detectChecks(repoRoot string) []string {
	if data, err := os.ReadFile(filepath.Join(repoRoot, "Makefile")); err == nil {
		if regexp.MustCompile(`(?m)^test:`).Match(data) {
			return []string{"make test"}
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		return []string{"go build ./..."}
	}
	return nil
}

// countSwarmTasks counts open + in-progress tasks matching the swarm's tag
// filter (empty tag = all), so the confirm can state scope and we can bail when
// the set is empty.
func countSwarmTasks(cfg *config.Config, profileName, tag string) (int, error) {
	brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return 0, err
	}
	defer brn.Close()
	var tags []string
	if tag != "" {
		tags = []string{tag}
	}
	tasks, err := brn.ListTasksByStatus([]string{brain.StatusOpen, brain.StatusInProgress}, tags, 0, 0)
	if err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// confirmYes prints prompt and returns true only on an explicit yes. Reads a
// single line; the caller has already confirmed stdin is a TTY, so this is safe
// to interleave with a subprocess that later inherits stdin.
func confirmYes(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
