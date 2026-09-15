package brain

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// execPlansDirName is the brain subdirectory under which exec-plans
// are organized: <brain>/exec-plans/<status>/<plan-id>.md.
//
// Source of truth is on disk in the brain git repo so plans sync
// across machines like other brain content. Lifecycle transitions
// (active → completed → ...) are file moves; the brain auto-commit
// cron picks them up automatically.
const execPlansDirName = "exec-plans"

// ExecPlanStatus is the lifecycle bucket of a plan, also the
// directory name where its file lives. Transitioning a plan is a
// file rename across these subdirs.
type ExecPlanStatus string

const (
	ExecPlanStatusActive    ExecPlanStatus = "active"
	ExecPlanStatusCompleted ExecPlanStatus = "completed"
	ExecPlanStatusAbandoned ExecPlanStatus = "abandoned"
)

// IsValid reports whether the status string maps to a known
// directory bucket. Unknown statuses are rejected at write time
// rather than silently creating typo'd folders.
func (s ExecPlanStatus) IsValid() bool {
	switch s {
	case ExecPlanStatusActive, ExecPlanStatusCompleted, ExecPlanStatusAbandoned:
		return true
	}
	return false
}

// ExecPlanOrigin records WHERE the plan was created from. The
// design assumption is that any non-trivial agent task gets an
// exec-plan, and the platform supports many origin adapters
// (CLI, MCP, cron, webhook, Notion meeting transcript, etc.).
// Origin is metadata only - code paths are not switched on it.
type ExecPlanOrigin struct {
	// Type is the adapter that created this plan. Free-form so new
	// adapters don't require code changes here, but the canonical
	// values live in the repo's harness-readiness doc.
	Type string `yaml:"type"`

	// Ref is an adapter-specific reference back to the originating
	// artifact (Notion page id, Slack thread ts, GitHub issue
	// number, raw "cli" for CLI invocations, etc.). Optional.
	Ref string `yaml:"ref,omitempty"`
}

// execPlanFrontmatter is the YAML half of an exec-plan markdown
// file. Internal to this package; callers work with ExecPlan.
type execPlanFrontmatter struct {
	ID        string         `yaml:"id"`
	Title     string         `yaml:"title"`
	Status    ExecPlanStatus `yaml:"status"`
	Origin    ExecPlanOrigin `yaml:"origin"`
	Tags      []string       `yaml:"tags,omitempty"`
	CreatedAt time.Time      `yaml:"created_at"`
	UpdatedAt time.Time      `yaml:"updated_at"`
}

// ExecPlan is the structured view of one durable agent task.
// Body is the free-form markdown that follows the YAML
// frontmatter - the agent appends decision-log entries here as
// it works the plan. Caller mutates Body via AppendDecisionLog
// rather than editing it directly so timestamps and formatting
// stay consistent across appenders.
type ExecPlan struct {
	ID        string
	Title     string
	Status    ExecPlanStatus
	Origin    ExecPlanOrigin
	Tags      []string
	CreatedAt time.Time
	UpdatedAt time.Time

	// Goal is a one-paragraph statement of what the plan is
	// trying to accomplish. Rendered as a "## Goal" section in
	// the markdown body. Optional but encouraged.
	Goal string

	// Body is the markdown that follows the goal. Initially the
	// plan steps; mutated over time as the agent appends to the
	// "## Decision log" section.
	Body string
}

// NewExecPlan constructs a new active plan with timestamps set to
// now. ID, Title, and Origin are required; Goal and Tags are
// optional. The returned plan has not yet been written - caller
// invokes WriteExecPlan to persist.
func NewExecPlan(id, title string, origin ExecPlanOrigin) *ExecPlan {
	now := time.Now().UTC()
	return &ExecPlan{
		ID:        id,
		Title:     title,
		Status:    ExecPlanStatusActive,
		Origin:    origin,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// ExecPlansDir returns the absolute path to the exec-plans root.
func ExecPlansDir(brainPath string) string {
	return filepath.Join(brainPath, execPlansDirName)
}

// execPlanPath returns the on-disk path for a plan with the given
// status and id. Caller is responsible for ensuring the parent
// directory exists when writing.
func execPlanPath(brainPath, id string, status ExecPlanStatus) string {
	return filepath.Join(ExecPlansDir(brainPath), string(status), id+".md")
}

// WriteExecPlan persists the plan to <brain>/exec-plans/<status>/<id>.md.
// Auto-creates the status directory on first write. Updates
// plan.UpdatedAt before serializing. Rejects unknown statuses and
// empty IDs so typo'd state can't end up on disk.
func WriteExecPlan(brainPath string, p *ExecPlan) error {
	if p == nil {
		return fmt.Errorf("WriteExecPlan: nil plan")
	}
	if p.ID == "" {
		return fmt.Errorf("WriteExecPlan: empty ID")
	}
	if !p.Status.IsValid() {
		return fmt.Errorf("WriteExecPlan: unknown status %q", p.Status)
	}
	dir := filepath.Join(ExecPlansDir(brainPath), string(p.Status))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	p.UpdatedAt = time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = p.UpdatedAt
	}

	fm := execPlanFrontmatter{
		ID:        p.ID,
		Title:     p.Title,
		Status:    p.Status,
		Origin:    p.Origin,
		Tags:      p.Tags,
		CreatedAt: p.CreatedAt,
		UpdatedAt: p.UpdatedAt,
	}
	yamlBytes, err := yaml.Marshal(fm)
	if err != nil {
		return fmt.Errorf("marshal frontmatter: %w", err)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.Write(yamlBytes)
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", p.Title)
	if strings.TrimSpace(p.Goal) != "" {
		b.WriteString("## Goal\n\n")
		b.WriteString(strings.TrimSpace(p.Goal))
		b.WriteString("\n\n")
	}
	if strings.TrimSpace(p.Body) != "" {
		b.WriteString(strings.TrimSpace(p.Body))
		b.WriteString("\n")
	}

	return os.WriteFile(execPlanPath(brainPath, p.ID, p.Status), []byte(b.String()), 0644)
}

// ReadExecPlan looks up a plan by id across all status directories
// and returns it. Returns os.ErrNotExist (wrapped) when no file
// matches - callers can distinguish "not yet created" from a real
// read failure.
func ReadExecPlan(brainPath, id string) (*ExecPlan, error) {
	if id == "" {
		return nil, fmt.Errorf("ReadExecPlan: empty id")
	}
	for _, status := range []ExecPlanStatus{
		ExecPlanStatusActive, ExecPlanStatusCompleted, ExecPlanStatusAbandoned,
	} {
		path := execPlanPath(brainPath, id, status)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		return parseExecPlan(data)
	}
	return nil, os.ErrNotExist
}

// ListExecPlans returns plans in the given status bucket, ordered
// newest-first by UpdatedAt. Empty / missing directory yields an
// empty slice and no error so callers don't need to special-case
// "no plans yet."
func ListExecPlans(brainPath string, status ExecPlanStatus) ([]ExecPlan, error) {
	if !status.IsValid() {
		return nil, fmt.Errorf("ListExecPlans: unknown status %q", status)
	}
	dir := filepath.Join(ExecPlansDir(brainPath), string(status))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ExecPlan
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		p, err := parseExecPlan(data)
		if err != nil || p == nil {
			continue
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out, nil
}

// TransitionExecPlan moves a plan from its current status to a new
// status. The on-disk file is moved and its frontmatter Status is
// updated to match. Errors when the plan doesn't exist or the
// target status is unknown.
//
// Allowed transitions are not gated here - a plan can move from
// any status to any other valid status. Lifecycle policy lives in
// callers (the agent, the meeting workflow, the user).
func TransitionExecPlan(brainPath, id string, to ExecPlanStatus) error {
	if !to.IsValid() {
		return fmt.Errorf("TransitionExecPlan: unknown status %q", to)
	}
	plan, err := ReadExecPlan(brainPath, id)
	if err != nil {
		return err
	}
	if plan.Status == to {
		return nil
	}
	oldPath := execPlanPath(brainPath, id, plan.Status)
	plan.Status = to
	// Re-serialize at the new location; this also refreshes
	// UpdatedAt automatically inside WriteExecPlan.
	if err := WriteExecPlan(brainPath, plan); err != nil {
		return err
	}
	// Best-effort cleanup of the old file. If removal fails, the
	// plan now exists in two places - caller should re-run
	// transition or clean up manually rather than have us silently
	// hide the error.
	if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove old plan file %s: %w", oldPath, err)
	}
	return nil
}

// AppendDecisionLog appends a timestamped line to the plan's body
// under a "## Decision log" heading. The heading is created on
// first append. Each line gets a UTC RFC3339 timestamp and an
// author tag so multi-actor logs stay legible.
//
// author is free-form ("agent", "user", "claude-code") and
// surfaces in the rendered log as a bracketed prefix. entry is
// the message itself; newlines are preserved.
//
// Reads the current plan, mutates Body, writes back. Not safe for
// concurrent appenders on the same plan id - caller must serialize
// (or accept that a lost append is non-fatal for the v1 use case).
func AppendDecisionLog(brainPath, id, author, entry string) error {
	plan, err := ReadExecPlan(brainPath, id)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("- %s [%s] %s", stamp, author, strings.TrimSpace(entry))

	const heading = "## Decision log"
	body := strings.TrimRight(plan.Body, "\n")
	if !strings.Contains(body, heading) {
		if body != "" {
			body += "\n\n"
		}
		body += heading + "\n\n" + line + "\n"
	} else {
		body = strings.TrimRight(body, "\n") + "\n" + line + "\n"
	}
	plan.Body = body
	return WriteExecPlan(brainPath, plan)
}

// parseExecPlan splits raw bytes into frontmatter + body and
// reconstructs an ExecPlan. Returns an error when the
// frontmatter delimiters are missing or YAML parsing fails;
// callers handle by skipping the bad file in batch reads.
func parseExecPlan(data []byte) (*ExecPlan, error) {
	s := string(data)
	const delim = "---"
	if !strings.HasPrefix(s, delim+"\n") && !strings.HasPrefix(s, delim+"\r\n") {
		return nil, fmt.Errorf("missing leading frontmatter delimiter")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(s, delim+"\n"), delim+"\r\n")
	end := strings.Index(rest, "\n"+delim)
	if end < 0 {
		return nil, fmt.Errorf("missing trailing frontmatter delimiter")
	}
	yamlPart := rest[:end]
	body := strings.TrimLeft(rest[end+len("\n"+delim):], "\n")

	var fm execPlanFrontmatter
	if err := yaml.Unmarshal([]byte(yamlPart), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	// Strip the title line and the optional Goal section out of
	// the body so they round-trip cleanly through write/read.
	stripped, goal := splitGoal(stripFirstHeading(body))

	return &ExecPlan{
		ID:        fm.ID,
		Title:     fm.Title,
		Status:    fm.Status,
		Origin:    fm.Origin,
		Tags:      fm.Tags,
		CreatedAt: fm.CreatedAt,
		UpdatedAt: fm.UpdatedAt,
		Goal:      goal,
		Body:      strings.TrimSpace(stripped),
	}, nil
}

// stripFirstHeading removes the leading "# Title" line from the
// markdown body so the parsed body matches what was passed to
// WriteExecPlan (which adds the heading on serialization).
func stripFirstHeading(body string) string {
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "# ") {
		return body
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.TrimLeft(lines[1], "\n")
}

// splitGoal extracts a "## Goal" section from the head of the body
// and returns (rest, goal). When no Goal section is present, goal
// is empty and rest is the original body.
func splitGoal(body string) (rest, goal string) {
	const heading = "## Goal"
	if !strings.HasPrefix(body, heading) {
		return body, ""
	}
	afterHeading := strings.TrimLeft(strings.TrimPrefix(body, heading), "\n")
	// Goal is everything until the next "## " heading or EOF.
	end := strings.Index(afterHeading, "\n## ")
	if end < 0 {
		return "", strings.TrimSpace(afterHeading)
	}
	return strings.TrimLeft(afterHeading[end:], "\n"), strings.TrimSpace(afterHeading[:end])
}
