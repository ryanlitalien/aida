package brain

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
	"gopkg.in/yaml.v3"
)

// taskFrontmatter is the YAML frontmatter for a task markdown file.
type taskFrontmatter struct {
	Status      string   `yaml:"status"`
	Completed   bool     `yaml:"completed"`
	Tags        []string `yaml:"tags"`
	Created     string   `yaml:"created"`
	CompletedAt string   `yaml:"completed_at,omitempty"`
	IssueNumber int      `yaml:"issue_number,omitempty"`
	TaskID      int      `yaml:"task_id,omitempty"`
}

// AddTask creates a new task as a markdown file and indexes it in brain.db.
// Automatically adds a profile:{name} tag from the active profile.
// If body is non-empty, it is written below the title in the markdown file
// and included in the GitHub Issue body.
func (b *Brain) AddTask(title string, tags []string, body string) (*TaskRecord, error) {
	now := time.Now().UTC()
	slug, pagePath := dedupeTaskSlug(b.Path, slugifyTask(title, now))

	// Auto-add profile tag if not already present
	if b.profile != "" {
		profileTag := "profile:" + b.profile
		hasProfile := false
		for _, t := range tags {
			if t == profileTag {
				hasProfile = true
				break
			}
		}
		if !hasProfile {
			tags = append(tags, profileTag)
		}
	}

	description := title
	if body != "" {
		description = body
	}

	// Reindex from disk right before allocating an ID, so any task file
	// pulled in from another machine (e.g. by a `git pull` on the brain
	// repo just before this call) is in brain.db before we ask it for the
	// next id. This is the main fix: IndexTasks's own upsert-time
	// task_seq bump (UpsertTask, see db.go) does most of the work of
	// getting the counter past whatever is on disk. IndexTasks is cheap -
	// frontmatter parse + upsert, no embeddings.
	if _, skipped := b.IndexTasks(); len(skipped) > 0 {
		ui.PrintVerbose("brain", fmt.Sprintf("%d task file(s) skipped during pre-add reindex, see above", len(skipped)))
	}

	// Belt-and-suspenders on top of the reindex above: scan disk for the
	// max task_id directly and bump task_seq past it if IndexTasks somehow
	// left it behind (e.g. a file whose upsert failed - a collision
	// IndexTasks now reports instead of silently swallowing, but the
	// counter still needs to clear it so the new task doesn't collide
	// too). This call is deliberately made here, in AddTask, rather than
	// inside NextTaskID itself: NextTaskID is a low-level *DB method also
	// called from inside IndexTasks while it is walking the very same
	// files, so resyncing there would be redundant (and, for IndexTasks,
	// circular - it would rescan the files it is already in the middle of
	// scanning). AddTask is the actual external entry point where the
	// drift bug shows up, so this is the one place that needs the guard.
	if err := b.SyncTaskSeqFromDisk(); err != nil {
		ui.PrintVerbose("brain", fmt.Sprintf("sync task_seq from disk failed: %v", err))
	}

	taskID, err := b.DB.NextTaskID()
	if err != nil {
		return nil, fmt.Errorf("allocating task ID: %w", err)
	}

	record := &TaskRecord{
		Slug:        slug,
		Title:       title,
		Status:      "open",
		Completed:   false,
		Tags:        tags,
		Created:     now.Format(time.RFC3339),
		PagePath:    pagePath,
		Description: description,
		TaskID:      taskID,
	}

	// Write markdown file
	if err := writeTaskFile(filepath.Join(b.Path, pagePath), record, body); err != nil {
		return nil, fmt.Errorf("writing task file: %w", err)
	}

	// Index in brain.db
	if err := b.DB.UpsertTask(record); err != nil {
		return nil, fmt.Errorf("indexing task: %w", err)
	}

	// Mirror to GitHub Issue (non-blocking, best-effort)
	b.CreateIssue(record, b.GitHubRepo)

	return record, nil
}

// CompleteTask marks a task as done in both the markdown file and brain.db.
// Stamps completed_at with the current time. If the task is already terminal
// (done or closed), the existing completed_at is preserved.
func (b *Brain) CompleteTask(slug string) (*TaskRecord, error) {
	// Find by partial slug if needed
	task, err := b.DB.GetTask(slug)
	if err != nil {
		task, err = b.DB.FindTaskByPartialSlug(slug)
		if err != nil {
			return nil, err
		}
	}

	// Stamp completed_at unless already terminal (preserve first-completion date).
	completedAt := task.CompletedAt
	if !task.Completed {
		completedAt = time.Now().UTC().Format(time.RFC3339)
	}

	// Update brain.db
	if err := b.DB.SetTaskStatus(task.Slug, StatusDone, completedAt); err != nil {
		return nil, fmt.Errorf("marking done in db: %w", err)
	}

	// Update markdown frontmatter
	fullPath := filepath.Join(b.Path, task.PagePath)
	if _, body, err := parseTaskFile(fullPath); err == nil {
		task.Completed = true
		task.Status = "done"
		task.CompletedAt = completedAt
		if err := writeTaskFile(fullPath, task, body); err != nil {
			return nil, fmt.Errorf("updating file: %w", err)
		}
	}

	// Close GitHub Issue (non-blocking, best-effort)
	b.CloseIssue(task, b.GitHubRepo)

	return task, nil
}

// ReopenTask flips a task back to open state. Idempotent: reopening an
// already-open task is a no-op. Clears completed_at. Reopens the mirrored
// GitHub Issue if any.
func (b *Brain) ReopenTask(slug string) (*TaskRecord, error) {
	task, err := b.GetTask(slug)
	if err != nil {
		return nil, err
	}
	if !task.Completed {
		return task, nil
	}

	if err := b.DB.SetTaskStatus(task.Slug, StatusOpen, ""); err != nil {
		return nil, fmt.Errorf("marking open in db: %w", err)
	}

	fullPath := filepath.Join(b.Path, task.PagePath)
	if _, body, err := parseTaskFile(fullPath); err == nil {
		task.Completed = false
		task.Status = "open"
		task.CompletedAt = ""
		if err := writeTaskFile(fullPath, task, body); err != nil {
			return nil, fmt.Errorf("updating file: %w", err)
		}
	}

	b.ReopenIssue(task, b.GitHubRepo)
	return task, nil
}

// SetTaskStatus is the generalized status mutator. Updates both the
// markdown file and brain.db, and mirrors transitions to GitHub.
// CompleteTask and ReopenTask remain for the common done/open paths.
//
// completed_at semantics:
//   - non-terminal -> terminal: stamp now()
//   - terminal -> non-terminal: clear
//   - terminal -> terminal (done<->closed swap): preserve existing timestamp
func (b *Brain) SetTaskStatus(slug, status string) (*TaskRecord, error) {
	if !IsValidStatus(status) {
		return nil, fmt.Errorf("invalid status %q (want one of: %s)", status, strings.Join(AllStatuses(), ", "))
	}

	task, err := b.ResolveTaskRef(slug)
	if err != nil {
		return nil, err
	}
	if task.Status == status {
		return task, nil
	}
	wasCompleted := task.Completed
	willBeCompleted := IsTerminalStatus(status)

	completedAt := task.CompletedAt
	switch {
	case !wasCompleted && willBeCompleted:
		completedAt = time.Now().UTC().Format(time.RFC3339)
	case wasCompleted && !willBeCompleted:
		completedAt = ""
		// terminal -> terminal: preserve existing completedAt (already set above)
	}

	if err := b.DB.SetTaskStatus(task.Slug, status, completedAt); err != nil {
		return nil, fmt.Errorf("setting status in db: %w", err)
	}

	fullPath := filepath.Join(b.Path, task.PagePath)
	if _, body, err := parseTaskFile(fullPath); err == nil {
		task.Status = status
		task.Completed = willBeCompleted
		task.CompletedAt = completedAt
		if err := writeTaskFile(fullPath, task, body); err != nil {
			return nil, fmt.Errorf("updating file: %w", err)
		}
	}

	switch {
	case !wasCompleted && task.Completed:
		b.CloseIssue(task, b.GitHubRepo)
	case wasCompleted && !task.Completed:
		b.ReopenIssue(task, b.GitHubRepo)
	}
	return task, nil
}

// TaskPatch describes a partial update to a task.
// nil pointer fields are left unchanged. For Tags, passing a non-nil slice
// replaces the full tag list; AddTags/RemoveTags are additive/subtractive
// and are mutually exclusive with a full Tags replace.
type TaskPatch struct {
	Title      *string
	Body       *string // nil = unchanged; empty string = clear body
	Tags       *[]string
	AddTags    []string
	RemoveTags []string
	Status     *string // any value from AllStatuses()
}

// UpdateTask applies a patch to a task's markdown file and brain.db entry.
// Programmatic counterpart to `aida tasks edit` (which shells out to $EDITOR).
func (b *Brain) UpdateTask(slug string, patch TaskPatch) (*TaskRecord, error) {
	task, err := b.ResolveTaskRef(slug)
	if err != nil {
		return nil, err
	}

	if patch.Tags != nil && (len(patch.AddTags) > 0 || len(patch.RemoveTags) > 0) {
		return nil, fmt.Errorf("tags (full replace) is mutually exclusive with add_tags/remove_tags")
	}
	if patch.Status != nil && !IsValidStatus(*patch.Status) {
		return nil, fmt.Errorf("invalid status %q (want one of: %s)", *patch.Status, strings.Join(AllStatuses(), ", "))
	}

	fullPath := filepath.Join(b.Path, task.PagePath)
	current, body, err := parseTaskFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("reading task file: %w", err)
	}
	current.PagePath = task.PagePath
	current.TaskID = task.TaskID // TaskID lives in DB; parsed record may lack it
	oldTitle := current.Title
	oldTags := append([]string(nil), current.Tags...)
	wasCompleted := current.Completed

	if patch.Title != nil {
		current.Title = *patch.Title
		current.Description = current.Title
	}
	if patch.Body != nil {
		body = *patch.Body
		if body != "" {
			current.Description = body
		}
	}
	if patch.Tags != nil {
		current.Tags = append([]string(nil), (*patch.Tags)...)
	}
	if len(patch.AddTags) > 0 {
		current.Tags = mergeTagsDedup(current.Tags, patch.AddTags)
	}
	if len(patch.RemoveTags) > 0 {
		current.Tags = removeTags(current.Tags, patch.RemoveTags)
	}
	if patch.Status != nil {
		willBeCompleted := IsTerminalStatus(*patch.Status)
		switch {
		case !wasCompleted && willBeCompleted:
			current.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		case wasCompleted && !willBeCompleted:
			current.CompletedAt = ""
			// terminal -> terminal (done<->closed): preserve existing CompletedAt.
		}
		current.Status = *patch.Status
		current.Completed = willBeCompleted
	}

	if err := writeTaskFile(fullPath, current, body); err != nil {
		return nil, fmt.Errorf("writing task file: %w", err)
	}
	if err := b.DB.UpsertTask(current); err != nil {
		return nil, fmt.Errorf("indexing update: %w", err)
	}

	// Issue sync:
	// open -> done: close
	// done -> open: reopen (+ UpdateIssue for title/tags)
	// no status change: UpdateIssue if title or tags changed
	transitionedToDone := !wasCompleted && current.Completed
	transitionedToOpen := wasCompleted && !current.Completed
	titleChanged := oldTitle != current.Title
	tagsChanged := !stringSlicesEqual(oldTags, current.Tags)

	switch {
	case transitionedToDone:
		b.CloseIssue(current, b.GitHubRepo)
	case transitionedToOpen:
		b.ReopenIssue(current, b.GitHubRepo)
		if current.IssueNumber > 0 && (titleChanged || tagsChanged) {
			b.UpdateIssue(current, b.GitHubRepo)
		}
	default:
		if current.IssueNumber > 0 && (titleChanged || tagsChanged) {
			b.UpdateIssue(current, b.GitHubRepo)
		}
	}

	return current, nil
}

func mergeTagsDedup(existing, add []string) []string {
	seen := make(map[string]struct{}, len(existing))
	for _, t := range existing {
		seen[t] = struct{}{}
	}
	for _, t := range add {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		existing = append(existing, t)
		seen[t] = struct{}{}
	}
	return existing
}

func removeTags(existing, remove []string) []string {
	drop := make(map[string]struct{}, len(remove))
	for _, t := range remove {
		drop[strings.TrimSpace(t)] = struct{}{}
	}
	out := existing[:0]
	for _, t := range existing {
		if _, gone := drop[t]; gone {
			continue
		}
		out = append(out, t)
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ListTasks returns tasks from brain.db with optional filtering. Profile
// isolation is automatic: tasks tagged with a different profile than the
// brain's active profile are dropped before the tag filter runs. Use
// ListTasksAllProfiles for explicit cross-profile views.
func (b *Brain) ListTasks(showCompleted bool, tags []string, days, limit int) ([]TaskRecord, error) {
	return b.DB.ListTasks(showCompleted, tags, days, limit, b.profile, nil)
}

// ListTasksByStatus returns tasks filtered to exactly the given statuses.
// Bypasses the showCompleted heuristic - pass an explicit set.
func (b *Brain) ListTasksByStatus(statuses []string, tags []string, days, limit int) ([]TaskRecord, error) {
	return b.DB.ListTasks(false, tags, days, limit, b.profile, statuses)
}

// ListTasksAllProfiles bypasses profile isolation. Reserved for explicit
// cross-profile commands like `aida tasks --all-profiles` and the equivalent
// MCP arg - never call this from generic agent or task-intercept paths.
func (b *Brain) ListTasksAllProfiles(showCompleted bool, tags []string, days, limit int) ([]TaskRecord, error) {
	return b.DB.ListTasks(showCompleted, tags, days, limit, "", nil)
}

// ListTasksAllProfilesByStatus is the cross-profile equivalent of
// ListTasksByStatus.
func (b *Brain) ListTasksAllProfilesByStatus(statuses []string, tags []string, days, limit int) ([]TaskRecord, error) {
	return b.DB.ListTasks(false, tags, days, limit, "", statuses)
}

// GetTask returns a single task by slug (exact or partial match).
func (b *Brain) GetTask(slug string) (*TaskRecord, error) {
	task, err := b.DB.GetTask(slug)
	if err != nil {
		return b.DB.FindTaskByPartialSlug(slug)
	}
	return task, nil
}

// GetTaskByID returns a single task by its stable sequential ID.
func (b *Brain) GetTaskByID(taskID int) (*TaskRecord, error) {
	return b.DB.GetTaskByID(taskID)
}

// ResolveTaskRef resolves a "#N" stable ID or a slug/partial-slug to a task record.
// Matches tasks regardless of status (open or done) - callers that need to
// act on completed tasks (reopen, edit) rely on this.
// Shared by MCP handlers and CLI helpers that need a structured lookup.
func (b *Brain) ResolveTaskRef(ref string) (*TaskRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty task ref")
	}
	if strings.HasPrefix(ref, "#") {
		n, err := strconv.Atoi(strings.TrimPrefix(ref, "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid task ref %q", ref)
		}
		task, err := b.GetTaskByID(n)
		if err != nil {
			return nil, fmt.Errorf("task #%d not found", n)
		}
		return task, nil
	}
	if task, err := b.DB.GetTask(ref); err == nil {
		return task, nil
	}
	return b.DB.FindTaskByPartialSlugAny(ref)
}

// TaskFilePath returns the absolute path to a task's markdown file.
func (b *Brain) TaskFilePath(slug string) string {
	return filepath.Join(b.Path, "tasks", slug+".md")
}

// TaskBody returns a task's markdown body with the frontmatter and title
// heading stripped. Callers that want to append to a task (e.g. recording a
// recurrence) without clobbering the existing text use this to read it back
// first.
func (b *Brain) TaskBody(slug string) (string, error) {
	_, body, err := parseTaskFile(b.TaskFilePath(slug))
	if err != nil {
		return "", err
	}
	return body, nil
}

// IndexTasks reads all task markdown files and upserts them into brain.db.
// Returns the count of indexed tasks and one error per file that was
// skipped (parse failure, or a DB upsert failure - most notably the
// idx_tasks_task_id UNIQUE index rejecting a second file that claims a
// task_id another file already claims). Each error is prefixed with the
// file name so a caller can report or log exactly which files need
// attention; the errors are collected rather than returned as a single
// combined error so a caller that only cares about the count (the common
// case) can ignore the second return value entirely.
//
// This used to `continue` past both kinds of failure with no signal at
// all - the actual root cause of a real incident where a task file pulled
// in from another machine (task_id 480) silently lost the UNIQUE-index
// race to a local file that had wrongly been assigned the same id, and
// `aida tasks show '#480'` reported "not found" despite the file sitting
// right there on disk.
func (b *Brain) IndexTasks() (int, []error) {
	tasksDir := filepath.Join(b.Path, "tasks")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, []error{err}
	}

	count := 0
	var skipped []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(tasksDir, entry.Name())
		record, body, err := parseTaskFile(path)
		if err != nil {
			skipped = append(skipped, fmt.Errorf("%s: parse failed: %w", entry.Name(), err))
			continue
		}
		record.PagePath = filepath.Join("tasks", entry.Name())

		// Allocate a stable ID if the file doesn't have one
		if record.TaskID == 0 {
			// Check if DB already has an ID for this slug
			if existing, err := b.DB.GetTask(record.Slug); err == nil && existing.TaskID > 0 {
				record.TaskID = existing.TaskID
			} else {
				newID, err := b.DB.NextTaskID()
				if err == nil {
					record.TaskID = newID
				}
			}
			// Write the ID back to the file so it persists across git sync
			if record.TaskID > 0 {
				writeTaskFile(path, record, body)
			}
		}

		if err := b.DB.UpsertTask(record); err != nil {
			skipped = append(skipped, fmt.Errorf("%s: upsert failed (task_id %d, slug %q): %w", entry.Name(), record.TaskID, record.Slug, err))
			continue
		}
		count++
	}
	for _, skipErr := range skipped {
		ui.PrintVerbose("IndexTasks", "skipped "+skipErr.Error())
	}
	return count, skipped
}

// SyncTaskSeqFromDisk scans tasks/*.md frontmatter for the highest task_id
// present on disk and, if it is higher than brain.db's task_seq counter,
// bumps the counter to max+1. This is cheap - a few hundred small file reads
// - and exists because task_seq is otherwise only re-seeded from disk during
// a full IndexTasks run: a task file that arrives via `git pull` (written by
// another machine, already carrying a task_id) does not bump the counter
// until that next reindex, so an allocation in between can hand out an ID
// that collides with (or falls behind) one already on disk.
func (b *Brain) SyncTaskSeqFromDisk() error {
	tasksDir := filepath.Join(b.Path, "tasks")
	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	maxID := 0
	taskIDRe := regexp.MustCompile(`(?m)^task_id:\s*(\d+)\s*$`)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(tasksDir, entry.Name()))
		if err != nil {
			continue
		}
		content := string(data)
		if !strings.HasPrefix(content, "---\n") {
			continue
		}
		rest := content[4:]
		end := strings.Index(rest, "\n---\n")
		if end < 0 {
			continue
		}
		m := taskIDRe.FindStringSubmatch(rest[:end])
		if m == nil {
			continue
		}
		id, err := strconv.Atoi(m[1])
		if err != nil || id <= maxID {
			continue
		}
		maxID = id
	}

	if maxID == 0 {
		return nil
	}
	return b.DB.BumpTaskSeq(maxID + 1)
}

// ReindexTask re-parses a single task file and updates brain.db.
func (b *Brain) ReindexTask(slug string) error {
	path := b.TaskFilePath(slug)
	record, _, err := parseTaskFile(path)
	if err != nil {
		return err
	}
	record.PagePath = filepath.Join("tasks", slug+".md")
	return b.DB.UpsertTask(record)
}

// dedupeTaskSlug returns a slug (and its tasks/ page path) guaranteed not to
// collide with an existing task file. slugifyTask's prefix is date-granularity
// only, so two tasks created the same day with the same (truncated) title -
// e.g. two identical SaveToolErrorTask calls a few seconds apart - hash to
// the exact same slug. Without this check, AddTask would silently overwrite
// the first task's markdown file on disk while NextTaskID still handed out a
// fresh sequential ID for the second call, orphaning that ID and losing the
// first task's content with no error surfaced.
//
// On collision, -2, -3, ... is appended until a free slug is found. The
// non-colliding (common) case returns the base slug unchanged, so existing
// callers and on-disk slugs are unaffected.
func dedupeTaskSlug(root, slug string) (string, string) {
	pagePath := filepath.Join("tasks", slug+".md")
	if _, err := os.Stat(filepath.Join(root, pagePath)); err != nil {
		return slug, pagePath
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", slug, i)
		candidatePath := filepath.Join("tasks", candidate+".md")
		if _, err := os.Stat(filepath.Join(root, candidatePath)); err != nil {
			return candidate, candidatePath
		}
	}
}

func slugifyTask(title string, t time.Time) string {
	prefix := t.Format("2006-01-02")
	return prefix + "-" + SlugifyTaskTitle(title)
}

// SlugifyTaskTitle applies the lowercase-and-hyphenate normalization used
// for task slugs: keep [a-z0-9], turn spaces/hyphens/underscores into single
// hyphens, trim leading/trailing hyphens, collapse repeated hyphens, and
// truncate to 50 characters (trimming any trailing hyphen the truncation
// exposes). slugifyTask calls this and prepends today's date to produce the
// on-disk task filename. internal/jarvis/tools normalizes conversational
// task references ("water the plants") the same way, through this same
// function, so a spoken reference and the stored slug it needs to match
// always land in the same shape - see resolveTaskRef in
// internal/jarvis/tools/registry.go.
//
// This is the single source of truth for both callers. Do not change its
// output for any input: slugifyTask already generated the on-disk filenames
// for every existing task file, and a changed output here would orphan
// those files.
func SlugifyTaskTitle(title string) string {
	// Lowercase, replace non-alnum with hyphens
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' || r == '_' {
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")

	// Collapse multiple hyphens
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}

	// Truncate to keep filename reasonable
	if len(slug) > 50 {
		slug = slug[:50]
		slug = strings.TrimRight(slug, "-")
	}

	return slug
}

func writeTaskFile(path string, record *TaskRecord, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	fm := taskFrontmatter{
		Status:      record.Status,
		Completed:   record.Completed,
		Tags:        record.Tags,
		Created:     record.Created,
		CompletedAt: record.CompletedAt,
		IssueNumber: record.IssueNumber,
		TaskID:      record.TaskID,
	}

	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return err
	}

	var content strings.Builder
	content.WriteString("---\n")
	content.Write(yamlBytes)
	content.WriteString("---\n\n")
	content.WriteString("# " + record.Title + "\n")
	if body != "" {
		content.WriteString("\n" + body)
	}

	return os.WriteFile(path, []byte(content.String()), 0644)
}

func parseTaskFile(path string) (*TaskRecord, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	content := string(data)

	// Split frontmatter: ---\n...\n---\n
	if !strings.HasPrefix(content, "---\n") {
		return nil, "", fmt.Errorf("no frontmatter found in %s", path)
	}
	rest := content[4:]
	endIdx := strings.Index(rest, "\n---\n")
	if endIdx < 0 {
		return nil, "", fmt.Errorf("unterminated frontmatter in %s", path)
	}
	fmStr := rest[:endIdx]
	bodyStr := strings.TrimSpace(rest[endIdx+5:])

	var fm taskFrontmatter
	if err := yaml.Unmarshal([]byte(fmStr), &fm); err != nil {
		return nil, "", fmt.Errorf("parsing frontmatter in %s: %w", path, err)
	}

	// Extract title from first # heading in body
	title := ""
	body := ""
	lines := strings.SplitN(bodyStr, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "# ") {
		title = strings.TrimPrefix(lines[0], "# ")
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		}
	} else {
		title = bodyStr
		if len(title) > 100 {
			title = title[:100]
		}
	}

	slug := strings.TrimSuffix(filepath.Base(path), ".md")

	record := &TaskRecord{
		Slug:        slug,
		Title:       title,
		Status:      fm.Status,
		Completed:   fm.Completed,
		Tags:        fm.Tags,
		Created:     fm.Created,
		CompletedAt: fm.CompletedAt,
		PagePath:    "", // caller sets this
		Description: title,
		IssueNumber: fm.IssueNumber,
		TaskID:      fm.TaskID,
	}

	return record, body, nil
}

// --- GitHub Issue Mirroring ---

// ghAvailable checks if the gh CLI is on PATH.
func ghAvailable() bool {
	_, err := exec.LookPath("gh")
	return err == nil
}

// CreateIssue creates a GitHub Issue mirroring a brain task.
// Stores the issue number back in the task's frontmatter and brain.db.
func (b *Brain) CreateIssue(task *TaskRecord, repo string) {
	if repo == "" || !ghAvailable() {
		return
	}

	// Ensure labels exist and build label flags
	var labelArgs []string
	for _, tag := range task.Tags {
		// Create label if it doesn't exist (ignore errors - label may already exist)
		exec.Command("gh", "label", "create", tag, "--repo", repo, "--force").Run()
		labelArgs = append(labelArgs, "--label", tag)
	}

	// Write body to temp file (with clickable link to brain file)
	fileURL := fmt.Sprintf("https://github.com/%s/blob/main/%s", repo, task.PagePath)
	bodyContent := fmt.Sprintf("Brain file: [`%s`](%s)\n\n%s", task.PagePath, fileURL, task.Description)
	bodyFile := fmt.Sprintf("/tmp/aida-issue-%s.md", task.Slug)
	if err := os.WriteFile(bodyFile, []byte(bodyContent), 0644); err != nil {
		ui.PrintVerbose("Issue create", "body write error: "+err.Error())
		return
	}
	defer os.Remove(bodyFile)

	args := []string{"issue", "create", "--repo", repo, "--title", task.Title, "--body-file", bodyFile}
	args = append(args, labelArgs...)

	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ui.PrintVerbose("Issue create", "failed: "+err.Error()+": "+string(out))
		return
	}

	// Parse issue number from output URL (e.g., "https://github.com/owner/repo/issues/4")
	issueNum := parseIssueNumber(string(out))
	if issueNum > 0 {
		task.IssueNumber = issueNum
		// Update frontmatter and DB
		fullPath := filepath.Join(b.Path, task.PagePath)
		if _, body, err := parseTaskFile(fullPath); err == nil {
			_ = writeTaskFile(fullPath, task, body)
		}
		_ = b.DB.UpsertTask(task)
		ui.PrintVerbose("Issue create", fmt.Sprintf("#%d created", issueNum))
	}
}

// CloseIssue closes the GitHub Issue associated with a brain task. Tasks in
// status "closed" pass --reason not_planned to record that they were
// abandoned rather than completed.
func (b *Brain) CloseIssue(task *TaskRecord, repo string) {
	if repo == "" || task.IssueNumber == 0 || !ghAvailable() {
		return
	}

	args := []string{"issue", "close", strconv.Itoa(task.IssueNumber), "--repo", repo}
	if task.Status == StatusClosed {
		args = append(args, "--reason", "not planned")
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ui.PrintVerbose("Issue close", "failed: "+err.Error()+": "+string(out))
		return
	}
	ui.PrintVerbose("Issue close", fmt.Sprintf("#%d closed", task.IssueNumber))
}

// ReopenIssue reopens the GitHub Issue associated with a brain task.
func (b *Brain) ReopenIssue(task *TaskRecord, repo string) {
	if repo == "" || task.IssueNumber == 0 || !ghAvailable() {
		return
	}

	cmd := exec.Command("gh", "issue", "reopen", strconv.Itoa(task.IssueNumber), "--repo", repo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ui.PrintVerbose("Issue reopen", "failed: "+err.Error()+": "+string(out))
		return
	}
	ui.PrintVerbose("Issue reopen", fmt.Sprintf("#%d reopened", task.IssueNumber))
}

// UpdateIssue updates the GitHub Issue title and labels to match the brain task.
func (b *Brain) UpdateIssue(task *TaskRecord, repo string) {
	if repo == "" || task.IssueNumber == 0 || !ghAvailable() {
		return
	}

	// Update title
	args := []string{"issue", "edit", strconv.Itoa(task.IssueNumber), "--repo", repo, "--title", task.Title}

	// Add current tags as labels
	for _, tag := range task.Tags {
		args = append(args, "--add-label", tag)
	}

	cmd := exec.Command("gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		ui.PrintVerbose("Issue update", "failed: "+err.Error()+": "+string(out))
		return
	}
	ui.PrintVerbose("Issue update", fmt.Sprintf("#%d updated", task.IssueNumber))
}

var issueURLPattern = regexp.MustCompile(`/issues/(\d+)`)

func parseIssueNumber(output string) int {
	matches := issueURLPattern.FindStringSubmatch(output)
	if len(matches) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(matches[1])
	return n
}
