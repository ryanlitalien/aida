package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// normalizeName strips all common separators and lowercases for fuzzy matching.
// e.g. "acme_widgets", "acme-widgets", "AcmeWidgets", "acme.widgets" all become "acmewidgets".
func normalizeName(s string) string {
	s = strings.ToLower(s)
	for _, sep := range []string{"-", "_", ".", " "} {
		s = strings.ReplaceAll(s, sep, "")
	}
	return s
}

func newTasksCmd() *cobra.Command {
	var tags []string
	var all bool
	var allProfiles bool
	var days int
	var limit int
	var statusFilter []string

	cmd := &cobra.Command{
		Use:     "tasks [ref]",
		Aliases: []string{"task"},
		Short:   "Manage brain tasks",
		Long: `Track tasks across all projects in the brain. Tasks are markdown files with tags, synced via git.

With no arguments, lists tasks. With a single argument that resolves to a task
(` + "`#N`" + `, bare ID, slug, or partial slug), shows that task's body - same as
` + "`aida tasks show <ref>`" + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if shown, err := tryShowTaskByRef(args[0]); err != nil {
					return err
				} else if shown {
					return nil
				}
			}
			return runTasksList(tags, all, allProfiles, days, limit, statusFilter)
		},
	}

	cmd.Flags().StringSliceVar(&tags, "tag", nil, "filter by tag (substring match, repeatable)")
	cmd.Flags().BoolVar(&all, "all", false, "include all statuses (default shows open + in-progress only)")
	cmd.Flags().BoolVar(&allProfiles, "all-profiles", false, "show tasks from every profile (bypasses isolation)")
	cmd.Flags().IntVar(&days, "days", 0, "only tasks created in the last N days")
	cmd.Flags().IntVar(&limit, "limit", 0, "max number of tasks to show")
	cmd.Flags().StringSliceVar(&statusFilter, "status", nil, "filter by status (open, in-progress, hold, deferred, done, closed; repeatable). Overrides --all.")

	cmd.AddCommand(newTasksAddCmd())
	cmd.AddCommand(newTasksDoneCmd())
	cmd.AddCommand(newTasksReopenCmd())
	cmd.AddCommand(newTasksEditCmd())
	cmd.AddCommand(newTasksShowCmd())
	cmd.AddCommand(newTasksStatusCmd())
	cmd.AddCommand(newTasksWebCmd())
	cmd.AddCommand(newTasksIngestCmd())
	cmd.AddCommand(newTasksDraftsCmd())
	cmd.AddCommand(newTasksReindexCmd())

	return cmd
}

// tryShowTaskByRef attempts to resolve ref as a single task and print its
// body. Returns (true, nil) when the task was shown, (false, nil) when ref
// did not resolve (so the caller can fall back to listing), or (false, err)
// for unrecoverable errors like a brain.Open failure.
func tryShowTaskByRef(ref string) (bool, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return false, err
	}
	_, profileName := cfg.ActiveProfileConfig()
	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return false, err
	}
	defer b.Close()

	task, err := resolveTaskRef(ref, b)
	if err != nil {
		return false, nil
	}
	data, err := os.ReadFile(b.TaskFilePath(task.Slug))
	if err != nil {
		return false, fmt.Errorf("reading task: %w", err)
	}
	fmt.Println(string(data))
	return true, nil
}

func runTasksList(tags []string, showAll, allProfiles bool, days, limit int, statusFilter []string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	_, profileName := cfg.ActiveProfileConfig()

	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return err
	}
	defer b.Close()

	// Resolve effective status filter:
	//   --status wins when set
	//   --all means "all statuses" (no filter)
	//   default = active set (open + in-progress)
	var statuses []string
	switch {
	case len(statusFilter) > 0:
		for _, s := range statusFilter {
			s = strings.TrimSpace(s)
			if !brain.IsValidStatus(s) {
				return fmt.Errorf("invalid status %q (want one of: %s)", s, strings.Join(brain.AllStatuses(), ", "))
			}
			statuses = append(statuses, s)
		}
	case showAll:
		statuses = nil // no filter - show everything
	default:
		statuses = brain.ActiveStatuses()
	}

	// Profile isolation is implicit in b.ListTasks*. --all-profiles is the
	// only sanctioned escape hatch and the header below makes it loud.
	var tasks []brain.TaskRecord
	switch {
	case allProfiles && len(statuses) > 0:
		tasks, err = b.ListTasksAllProfilesByStatus(statuses, tags, days, limit)
	case allProfiles:
		tasks, err = b.ListTasksAllProfiles(true, tags, days, limit)
	case len(statuses) > 0:
		tasks, err = b.ListTasksByStatus(statuses, tags, days, limit)
	default:
		tasks, err = b.ListTasks(true, tags, days, limit)
	}
	if err != nil {
		return err
	}

	if len(tasks) == 0 {
		if len(tags) > 0 || days > 0 || len(statusFilter) > 0 {
			fmt.Println("No tasks match the filters.")
		} else {
			fmt.Println("No open tasks. Add one with: aida tasks add \"description\" --tag project:name --tag p1")
		}
		return nil
	}

	activeProfile := profileName
	if allProfiles {
		activeProfile = "" // signal "all" in the display header
	}
	runTasksDisplay(tasks, tags, showAll, days, limit, activeProfile, statusFilter)
	return nil
}

func newTasksAddCmd() *cobra.Command {
	var tags []string
	var more bool
	cmd := &cobra.Command{
		Use:   "add [title]",
		Short: "Add a new task",
		Long: `Add a new task. With --more, opens your editor so you can paste in
context (Slack messages, links, notes). An LLM call extracts a concise
title from whatever you paste, and the full content becomes the task body.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			var title, body string

			if more {
				// Open editor for pasting content, then LLM-extract the title.
				body, err = editTaskContent(cfg.GetEditor())
				if err != nil {
					return err
				}
				if strings.TrimSpace(body) == "" {
					return fmt.Errorf("no content provided - task not created")
				}

				// Use any positional args as a title hint; otherwise LLM extracts one.
				if len(args) > 0 {
					title = strings.Join(args, " ")
				} else {
					title, err = extractTaskTitle(cfg, body)
					if err != nil {
						// Fallback: first line, truncated.
						title = firstLine(body, 80)
						ui.PrintVerbose("Title extraction", "LLM failed, using first line: "+err.Error())
					}
				}
			} else {
				if len(args) == 0 {
					return fmt.Errorf("provide a title, or use --more to paste content")
				}
				title = strings.Join(args, " ")
			}

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			if dryRunGuard("add task", "title: "+title) {
				return nil
			}

			if cfg.Brain.AutoSync {
				brain.PullAndWait(cfg.BrainPath())
			}

			task, err := b.AddTask(title, tags, body)
			if err != nil {
				return err
			}

			fmt.Printf("%s Task #%d added: %s\n", ui.SuccessIcon, task.TaskID, task.Slug)
			fmt.Println("  Edit with: aida tasks edit " + task.Slug)

			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "task tag (repeatable, e.g. --tag project:aida --tag p1)")
	cmd.Flags().BoolVar(&more, "more", false, "open editor to paste context; LLM extracts the title")
	return cmd
}

// editTaskContent opens $EDITOR with a temp file for the user to paste content.
// Returns the content after stripping comment lines.
func editTaskContent(editor string) (string, error) {
	tmpDir := os.TempDir()
	tmpFile := filepath.Join(tmpDir, "aida-task-content.md")

	prompt := `<!-- Paste your content below this line. Save and close when done. -->
<!-- Lines starting with <!-- will be stripped. -->

`
	if err := os.WriteFile(tmpFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	editorCmd := exec.Command(editor, tmpFile)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return "", fmt.Errorf("editor failed: %w", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return "", fmt.Errorf("reading temp file: %w", err)
	}

	// Strip HTML comment lines
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->") {
			continue
		}
		lines = append(lines, line)
	}

	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

// extractTaskTitle uses an LLM call to extract a concise task title from pasted content.
func extractTaskTitle(cfg *config.Config, content string) (string, error) {
	apiKey := cfg.GetAPIKey()
	if apiKey == "" {
		return "", fmt.Errorf("no API key available")
	}

	client := llm.NewClient(apiKey, llm.DefaultModel, false)

	systemPrompt := `You extract concise task titles from raw content.
Rules:
- Return ONLY the title, nothing else - no quotes, no explanation
- Under 80 characters
- Imperative form (e.g., "Reply to Devin about open loop channels")
- Capture the core action and subject`

	userPrompt := "Extract a task title from this content:\n\n" + content

	ctx := context.Background()
	title, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", err
	}

	title = strings.TrimSpace(title)
	// Strip quotes the LLM might add
	title = strings.Trim(title, "\"'`")

	if title == "" {
		return "", fmt.Errorf("LLM returned empty title")
	}
	return title, nil
}

// firstLine returns the first line of text, truncated to maxLen.
func firstLine(s string, maxLen int) string {
	line := strings.SplitN(strings.TrimSpace(s), "\n", 2)[0]
	if len(line) > maxLen {
		line = line[:maxLen-3] + "..."
	}
	return line
}

func newTasksDoneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "done [slug]",
		Short: "Mark a task as completed",
		Long:  "Accepts partial slug match. E.g., 'aida tasks done checkout' matches '2026-04-12-checkout-timeout-spike'.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			// Resolve #N stable ID references
			ref := args[0]
			resolved, err := resolveTaskRef(ref, b)
			if err != nil {
				return err
			}

			if dryRunGuard("complete task", resolved.Slug) {
				return nil
			}

			task, err := b.CompleteTask(resolved.Slug)
			if err != nil {
				return err
			}

			fmt.Printf("%s Done: %s\n", ui.SuccessIcon, task.Slug)

			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
			return nil
		},
	}
}

// newTasksReindexCmd runs the tasks-only reindex (frontmatter parse +
// upsert, no embeddings), so a human can pick up task files another
// machine pushed - after a manual `git -C ~/.aida/brain pull`, say -
// without paying for the full `aida brain index` (~3 minutes with
// several hundred lessons, most of which reindex has nothing to do with).
func newTasksReindexCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: "Reindex task markdown files into brain.db (cheap, no embeddings)",
		Long: "Re-parses every tasks/*.md file and upserts it into brain.db.\n" +
			"Much cheaper than `aida brain index` (which also re-embeds lessons) -\n" +
			"run this after a manual `git pull` on the brain repo to make sure\n" +
			"task files someone else pushed are actually reflected in the DB\n" +
			"before you add another task.\n\n" +
			"Any file that could not be indexed (a parse failure, or a task_id\n" +
			"collision with another file) is reported, not silently dropped -\n" +
			"see CLAUDE.md's Task CLI surface section for the fix.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			count, skipped := b.IndexTasks()

			fmt.Printf("%s Indexed %d task(s)\n", ui.SuccessIcon, count)
			if len(skipped) == 0 {
				return nil
			}
			fmt.Printf("%s %d file(s) skipped:\n", ui.WarnIcon, len(skipped))
			for _, skipErr := range skipped {
				fmt.Printf("  - %s\n", skipErr)
			}
			return nil
		},
	}
}

func newTasksReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reopen [slug]",
		Short: "Reopen a completed task",
		Long:  "Flips a task back to open. Accepts slug, partial slug, or '#N' task ID.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			resolved, err := resolveTaskRef(args[0], b)
			if err != nil {
				return err
			}

			if dryRunGuard("reopen task", resolved.Slug) {
				return nil
			}

			task, err := b.ReopenTask(resolved.Slug)
			if err != nil {
				return err
			}

			fmt.Printf("%s Reopened: %s\n", ui.SuccessIcon, task.Slug)

			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
			return nil
		},
	}
}

func newTasksStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [slug] [value]",
		Short: "Set a task's status",
		Long: `Set a task's status. Valid values: open, in-progress, hold, deferred, done, closed.

  done   = I completed it (counts as my work)
  closed = no longer relevant (coworker did it, outdated, won't do)
  hold   = waiting on someone/something external
  defer  = pushed out, not blocked on anything specific

Accepts slug, partial slug, or '#N' task ID.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			resolved, err := resolveTaskRef(args[0], b)
			if err != nil {
				return err
			}
			status := strings.TrimSpace(args[1])
			if !brain.IsValidStatus(status) {
				return fmt.Errorf("invalid status %q (want one of: %s)", status, strings.Join(brain.AllStatuses(), ", "))
			}

			if dryRunGuard("set status "+status, resolved.Slug) {
				return nil
			}

			task, err := b.SetTaskStatus(resolved.Slug, status)
			if err != nil {
				return err
			}

			fmt.Printf("%s %s: %s\n", ui.SuccessIcon, task.Status, task.Slug)

			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
			return nil
		},
	}
}

func newTasksEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit [slug]",
		Short: "Edit a task in $EDITOR",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			task, err := resolveTaskRef(args[0], b)
			if err != nil {
				return err
			}

			if dryRunGuard("edit task", task.Slug) {
				return nil
			}

			editor := cfg.GetEditor()
			filePath := b.TaskFilePath(task.Slug)

			editorCmd := exec.Command(editor, filePath)
			editorCmd.Stdin = os.Stdin
			editorCmd.Stdout = os.Stdout
			editorCmd.Stderr = os.Stderr
			if err := editorCmd.Run(); err != nil {
				return fmt.Errorf("editor failed: %w", err)
			}

			// Re-index the task after editing
			if err := b.ReindexTask(task.Slug); err != nil {
				return fmt.Errorf("re-index failed: %w", err)
			}

			// Update GitHub Issue if it exists
			updated, _ := b.GetTask(task.Slug)
			if updated != nil {
				b.UpdateIssue(updated, b.GitHubRepo)
			}

			fmt.Printf("%s Task updated and re-indexed\n", ui.SuccessIcon)

			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
			return nil
		},
	}
}

func newTasksShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [slug]",
		Short: "Show a task's full content",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			task, err := resolveTaskRef(args[0], b)
			if err != nil {
				return err
			}

			data, err := os.ReadFile(b.TaskFilePath(task.Slug))
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		},
	}
}

// HandleTaskIntent handles parsed task intents from the LLM parser.
// Called after Parse (step 1) when intent.Action == "task".
// Returns true if the task was handled (caller should skip the rest of the pipeline).
func HandleTaskIntent(intent *engine.Intent, cfg *config.Config, profileName string) bool {
	startedAt := time.Now()
	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return false
	}
	defer b.Close()

	switch intent.TaskAction {
	case "lookup":
		// Show a single task by ref (typically `#N` from raw_entities,
		// but resolveTaskRef also accepts slug / partial slug).
		ref := strings.TrimSpace(intent.TaskTitle)
		if ref == "" {
			for _, e := range intent.RawEntities {
				e = strings.TrimSpace(e)
				if e != "" && (strings.HasPrefix(e, "#") || !strings.ContainsAny(e, " \t")) {
					ref = e
					break
				}
			}
		}
		if ref == "" {
			ref = strings.TrimSpace(strings.Join(intent.Keywords, " "))
		}
		if ref == "" {
			fmt.Println("Could not determine which task to show.")
			saveTaskRun(intent, profileName, "lookup", startedAt,
				"could not determine task ref", []string{"no task reference"})
			return true
		}
		task, err := resolveTaskRef(ref, b)
		if err != nil {
			fmt.Printf("%s\n", err)
			saveTaskRun(intent, profileName, "lookup", startedAt,
				fmt.Sprintf("error resolving task ref %q: %s", ref, err), []string{err.Error()})
			return true
		}
		data, err := os.ReadFile(b.TaskFilePath(task.Slug))
		if err != nil {
			fmt.Printf("Could not read task: %s\n", err)
			saveTaskRun(intent, profileName, "lookup", startedAt,
				fmt.Sprintf("error reading task %q: %s", task.Slug, err), []string{err.Error()})
			return true
		}
		fmt.Println(string(data))
		saveTaskRun(intent, profileName, "lookup", startedAt,
			fmt.Sprintf("Showed task #%d: %s", task.TaskID, task.Slug), nil)
		return true

	case "create":
		title := intent.TaskTitle
		if title == "" {
			title = strings.Join(intent.Keywords, " ")
		}
		if title == "" {
			fmt.Println("Could not determine task title.")
			return true
		}
		// Auto-generate project tags from parsed entities matching library sources.
		// When multiple sources match (e.g., "acmewidgets" matches both
		// "acme-widgets" and "acme-widgets-dependabot"), prefer the closest
		// name length (shortest match wins).
		var tags []string
		srcs, _, _ := resolveSources()
		for _, entity := range intent.RawEntities {
			entityNorm := normalizeName(entity)
			bestMatch := ""
			bestLen := 999
			for name := range srcs {
				nameNorm := normalizeName(name)
				if strings.Contains(nameNorm, entityNorm) || strings.Contains(entityNorm, nameNorm) {
					if len(nameNorm) < bestLen {
						bestMatch = name
						bestLen = len(nameNorm)
					}
				}
			}
			if bestMatch != "" {
				tags = append(tags, "project:"+bestMatch)
			}
		}
		if dryRunGuard("add task", "title: "+title) {
			return true
		}
		if cfg.Brain.AutoSync {
			brain.PullAndWait(cfg.BrainPath())
		}
		task, err := b.AddTask(title, tags, "")
		if err != nil {
			fmt.Printf("Could not create task: %s\n", err)
			saveTaskRun(intent, profileName, "create", startedAt,
				fmt.Sprintf("error creating task: %s", err), []string{err.Error()})
			return true
		}
		fmt.Printf("%s Task #%d added: %s\n", ui.SuccessIcon, task.TaskID, task.Slug)
		fmt.Println("  Edit with: aida tasks edit " + task.Slug)
		if cfg.Brain.AutoSync {
			brain.CommitAndPush(cfg.BrainPath(), profileName)
		}
		saveTaskRun(intent, profileName, "create", startedAt,
			fmt.Sprintf("Task #%d added: %s", task.TaskID, task.Slug), nil)
		return true

	case "done":
		// Find the task reference: prefer task_title, then entities, then keywords
		ref := intent.TaskTitle
		if ref == "" {
			for _, e := range intent.RawEntities {
				ref = e
				break
			}
		}
		if ref == "" {
			ref = strings.Join(intent.Keywords, " ")
		}
		// Strip action/completion words the LLM may have left in
		ref = strings.ToLower(ref)
		for _, strip := range []string{" done", " complete", " completed", " finished", " as done"} {
			ref = strings.TrimSuffix(ref, strip)
		}
		for _, prefix := range []string{"mark ", "complete ", "finish ", "close "} {
			ref = strings.TrimPrefix(ref, prefix)
		}
		ref = strings.TrimSpace(ref)
		if ref == "" {
			fmt.Println("Could not determine which task to complete.")
			saveTaskRun(intent, profileName, "done", startedAt,
				"could not determine which task to complete", []string{"no task reference"})
			return true
		}

		task, err := resolveTaskRef(ref, b)
		if err != nil {
			fmt.Printf("Could not complete task: %s\n", err)
			saveTaskRun(intent, profileName, "done", startedAt,
				fmt.Sprintf("error resolving task ref %q: %s", ref, err), []string{err.Error()})
			return true
		}
		if dryRunGuard("complete task", task.Slug) {
			return true
		}
		completed, err := b.CompleteTask(task.Slug)
		if err != nil {
			fmt.Printf("Could not complete task: %s\n", err)
			saveTaskRun(intent, profileName, "done", startedAt,
				fmt.Sprintf("error completing task %q: %s", task.Slug, err), []string{err.Error()})
			return true
		}
		fmt.Printf("%s Done: %s\n", ui.SuccessIcon, completed.Slug)
		if cfg.Brain.AutoSync {
			brain.CommitAndPush(cfg.BrainPath(), profileName)
		}
		saveTaskRun(intent, profileName, "done", startedAt,
			fmt.Sprintf("Completed task #%d: %s", completed.TaskID, completed.Slug), nil)
		return true

	case "list", "":
		// Extract filters from keywords and entities
		var tags []string
		days := 0
		limit := 0
		lower := strings.ToLower(intent.RawQuery)

		// Priority filter from keywords. Profile filtering is intentionally
		// NOT inferred from keywords - "work" matches the verb in "what
		// should I work on next?" and a prior version cross-leaked work
		// tasks onto the home machine. Profile isolation is enforced by
		// brain.ListTasks against the active profile; explicit overrides
		// go through `AIDA_PROFILE=… aida tasks` or `--all-profiles`.
		for _, kw := range intent.Keywords {
			kwl := strings.ToLower(kw)
			if kwl == "p1" || kwl == "p2" || kwl == "p3" {
				tags = append(tags, kwl)
			}
		}

		// Project filter from parsed entities - prefer closest name length match
		srcs, _, _ := resolveSources()
		for _, entity := range intent.RawEntities {
			entityNorm := normalizeName(entity)
			bestMatch := ""
			bestLen := 999
			for name := range srcs {
				nameNorm := normalizeName(name)
				if strings.Contains(nameNorm, entityNorm) || strings.Contains(entityNorm, nameNorm) {
					if len(nameNorm) < bestLen {
						bestMatch = name
						bestLen = len(nameNorm)
					}
				}
			}
			if bestMatch != "" {
				tags = append(tags, "project:"+bestMatch)
			}
		}

		// Timeframe from intent
		switch intent.Timeframe {
		case "today":
			days = 1
		case "this week", "1w":
			days = 7
		}

		// Recency/limit hints
		if strings.Contains(lower, "latest") || strings.Contains(lower, "recent") {
			limit = 5
		}

		// Top N detection
		for _, pattern := range []string{"top ", "first "} {
			if idx := strings.Index(lower, pattern); idx >= 0 {
				rest := lower[idx+len(pattern):]
				for _, nw := range []struct {
					w string
					n int
				}{{"1", 1}, {"2", 2}, {"3", 3}, {"4", 4}, {"5", 5}, {"10", 10}} {
					if strings.HasPrefix(rest, nw.w) {
						limit = nw.n
						break
					}
				}
			}
		}

		// Profile isolation is implicit in b.ListTasks (see brain/tasks.go).

		showCompleted := strings.Contains(lower, "completed") || strings.Contains(lower, "finished")
		tasks, err := b.ListTasks(showCompleted, tags, days, limit)
		if err != nil {
			return false
		}
		if len(tasks) == 0 {
			fmt.Println("No matching tasks found.")
			saveTaskRun(intent, profileName, "list", startedAt,
				fmt.Sprintf("no tasks matching filters %v", tags), nil)
			return true
		}
		runTasksDisplay(tasks, tags, showCompleted, days, limit, profileName, nil)
		saveTaskRun(intent, profileName, "list", startedAt,
			fmt.Sprintf("%d task(s) listed with filters %v", len(tasks), tags), nil)
		return true
	}

	return false
}

// saveTaskRun records a run log entry for a task-intent invocation so that
// later `aida ok` / `aida thumbs-down` commands can attach feedback to the
// correct run via runs.Latest() rather than silently landing on whatever
// query ran before the task intercept fired.
func saveTaskRun(intent *engine.Intent, profileName, taskAction string, startedAt time.Time, answer string, errs []string) {
	cwd, _ := os.Getwd()
	run := &runs.Run{
		StartedAt: startedAt,
		TotalMs:   time.Since(startedAt).Milliseconds(),
		Question:  intent.RawQuery,
		Cwd:       cwd,
		Profile:   profileName,
		Action:    "task",
		Strategy:  taskAction,
		Entities:  append([]string(nil), intent.RawEntities...),
		Answer:    answer,
		Errors:    errs,
	}
	if _, err := runs.Save(run); err != nil {
		ui.PrintVerbose("Run log", "task save error: "+err.Error())
	}
}

// statusCode returns the single-character display indicator for a status.
//
//	open         -> ' '   in-progress  -> '>'
//	hold         -> 'H'   deferred     -> 'D'
//	done         -> 'x'   closed       -> 'X'
func statusCode(status string) string {
	switch status {
	case brain.StatusInProgress:
		return ">"
	case brain.StatusHold:
		return "H"
	case brain.StatusDeferred:
		return "D"
	case brain.StatusDone:
		return "x"
	case brain.StatusClosed:
		return "X"
	default:
		return " "
	}
}

func runTasksDisplay(tasks []brain.TaskRecord, tags []string, showAll bool, days, limit int, activeProfile string, statusFilter []string) {
	// Show status column whenever the result set could include non-active
	// statuses - otherwise the default open+in-progress view stays slim.
	showStatus := showAll || len(statusFilter) > 0

	header := "Open tasks"
	switch {
	case len(statusFilter) > 0:
		header = "Tasks (" + strings.Join(statusFilter, ", ") + ")"
	case showAll:
		header = "All tasks"
	}
	var filters []string
	if activeProfile != "" {
		filters = append(filters, "profile: "+activeProfile)
	} else {
		filters = append(filters, "ALL profiles")
	}
	if len(tags) > 0 {
		filters = append(filters, strings.Join(tags, ", "))
	}
	if days > 0 {
		filters = append(filters, fmt.Sprintf("last %d days", days))
	}
	header += " (" + strings.Join(filters, ", ") + ")"
	fmt.Println(header + ":\n")
	if showStatus {
		fmt.Printf("  %-5s %-3s %-10s %-4s %-24s %s\n", "#", " ", "DATE", "PRI", "TITLE", "TAGS")
		fmt.Printf("  %-5s %-3s %-10s %-4s %-24s %s\n", "-----", "---", "----------", "----", "------------------------", "----")
	} else {
		fmt.Printf("  %-5s %-10s %-4s %-24s %s\n", "#", "DATE", "PRI", "TITLE", "TAGS")
		fmt.Printf("  %-5s %-10s %-4s %-24s %s\n", "-----", "----------", "----", "------------------------", "----")
	}

	for _, t := range tasks {
		date := t.Created
		if len(date) >= 10 {
			date = date[:10]
		}
		displayTags := t.DisplayTags()
		num := fmt.Sprintf("#%d", t.TaskID)
		if showStatus {
			fmt.Printf("  %-5s [%s] %s  %-4s %-24s %s\n", num, statusCode(t.Status), date, t.PriorityTag(), t.Title, displayTags)
		} else {
			fmt.Printf("  %-5s %s  %-4s %-24s %s\n", num, date, t.PriorityTag(), t.Title, displayTags)
		}
	}

	count := fmt.Sprintf("\n%d tasks", len(tasks))
	if limit > 0 && len(tasks) >= limit {
		count += fmt.Sprintf(" (showing top %d)", limit)
	}
	fmt.Println(count)
}

// HandleTaskShortcut handles #N and anaphor references pre-parse (e.g.,
// "mark #1 as done", "mark that one as done"). #N is positional against
// the current task list, not something the parser would understand;
// anaphors are resolved against prior-turn run context inside
// resolveTaskRef. Saves a run record at every exit so that a follow-up
// `aida thumbs-down` attaches feedback to this action - not to whatever
// query ran before the shortcut fired.
// Returns true if the query was handled.
func HandleTaskShortcut(question string, cfg *config.Config, profileName string) bool {
	lower := strings.ToLower(question)

	// Only intercept if it looks like a #N completion
	var ref string
	for _, pattern := range []string{"mark ", "complete ", "finish ", "close "} {
		if idx := strings.Index(lower, pattern); idx >= 0 {
			rest := strings.TrimSpace(lower[idx+len(pattern):])
			for _, suffix := range []string{" as done", " as complete", " as completed", " complete", " completed", " done", " task"} {
				rest = strings.TrimSuffix(rest, suffix)
			}
			ref = strings.TrimSpace(rest)
			break
		}
	}

	// Only handle #N, "latest", "last", "that one", "it" - let the LLM handle everything else
	if ref == "" {
		return false
	}
	isShortcut := strings.HasPrefix(ref, "#")
	for _, word := range []string{"latest", "last", "that one", "first one", "the latest", "the last", "that", "it"} {
		if ref == word {
			isShortcut = true
			break
		}
	}
	if !isShortcut {
		return false
	}

	startedAt := time.Now()
	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return false
	}
	defer b.Close()

	task, err := resolveTaskRef(ref, b)
	if err != nil {
		fmt.Printf("Could not complete task: %s\n", err)
		saveTaskShortcutRun(question, profileName, "done", ref, startedAt,
			fmt.Sprintf("error resolving task ref %q: %s", ref, err), []string{err.Error()})
		return true
	}

	if dryRunGuard("complete task", task.Slug) {
		return true
	}
	completed, err := b.CompleteTask(task.Slug)
	if err != nil {
		fmt.Printf("Could not complete task: %s\n", err)
		saveTaskShortcutRun(question, profileName, "done", ref, startedAt,
			fmt.Sprintf("error completing task %q: %s", task.Slug, err), []string{err.Error()})
		return true
	}

	fmt.Printf("%s Done: %s\n", ui.SuccessIcon, completed.Slug)
	if cfg.Brain.AutoSync {
		brain.CommitAndPush(cfg.BrainPath(), profileName)
	}
	saveTaskShortcutRun(question, profileName, "done", ref, startedAt,
		fmt.Sprintf("Completed task #%d: %s", completed.TaskID, completed.Slug), nil)
	return true
}

// saveTaskShortcutRun records a run log entry for the pre-parse shortcut
// path, where no Intent has been parsed yet. Mirrors saveTaskRun so that
// `aida thumbs-up` / `aida thumbs-down` attach feedback to the shortcut
// invocation rather than the previous run.
func saveTaskShortcutRun(question, profileName, taskAction, ref string,
	startedAt time.Time, answer string, errs []string) {
	cwd, _ := os.Getwd()
	var entities []string
	if ref != "" {
		entities = []string{ref}
	}
	run := &runs.Run{
		StartedAt: startedAt,
		TotalMs:   time.Since(startedAt).Milliseconds(),
		Question:  question,
		Cwd:       cwd,
		Profile:   profileName,
		Action:    "task",
		Strategy:  taskAction,
		Entities:  entities,
		Answer:    answer,
		Errors:    errs,
	}
	if _, err := runs.Save(run); err != nil {
		ui.PrintVerbose("Run log", "task shortcut save error: "+err.Error())
	}
}

// resolveTaskRef resolves a task reference to a TaskRecord.
// Supports: "#N" / "N" (stable task ID), "latest"/"last"/"that one"/"first one" (positional), or partial slug.
// Bare numeric refs are unambiguous against slugs (slugs start with dates like "2026-04-28-..."),
// so they fall through to GetTaskByID after the `#` and "task " prefix paths.
func resolveTaskRef(ref string, b *brain.Brain) (*brain.TaskRecord, error) {
	ref = strings.TrimSpace(ref)
	hadTaskPrefix := strings.HasPrefix(ref, "task ")
	ref = strings.TrimPrefix(ref, "task ")
	hadHashPrefix := strings.HasPrefix(ref, "#")
	ref = strings.TrimPrefix(ref, "#")

	// Numeric ID lookup: try whenever ref is purely digits, regardless of how
	// we got here. `#125`, `task 125`, and bare `125` all resolve the same.
	n := 0
	isNum := len(ref) > 0
	for _, c := range ref {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		} else {
			isNum = false
			break
		}
	}
	if isNum && n > 0 {
		task, err := b.GetTaskByID(n)
		if err == nil {
			return task, nil
		}
		// Only error out here if the user explicitly asked for an ID
		// (`#N` or `task N`); otherwise fall through to slug match.
		if hadHashPrefix || hadTaskPrefix {
			return nil, fmt.Errorf("task #%d not found", n)
		}
	}

	// Anaphor / positional references - anaphors first try to resolve
	// against the task most recently shown/created/completed in this
	// cwd (the "that one" the user just referred to). Cold invocations
	// fall back to "first open task" so e.g. `aida tasks done last`
	// from a fresh terminal still works. Profile isolation is implicit
	// in b.ListTasks; LatestTaskRefInCwd is filtered to task-action runs
	// so unrelated query mentions don't leak in.
	for _, word := range []string{"latest", "last", "that one", "first one", "the latest", "the last", "that", "it"} {
		if ref == word {
			if cwd, err := os.Getwd(); err == nil {
				if id := runs.LatestTaskRefInCwd(cwd, 10); id > 0 {
					if task, err := b.GetTaskByID(id); err == nil {
						return task, nil
					}
				}
			}
			tasks, err := b.ListTasks(false, nil, 0, 1)
			if err != nil {
				return nil, err
			}
			if len(tasks) == 0 {
				return nil, fmt.Errorf("no open tasks")
			}
			return &tasks[0], nil
		}
	}

	// Normalize spaces to hyphens for slug matching
	normalized := strings.ReplaceAll(ref, " ", "-")
	task, err := b.GetTask(normalized)
	if err == nil {
		return task, nil
	}
	// Try original as-is
	return b.GetTask(ref)
}
