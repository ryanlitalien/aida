package taskstate

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrMalformed is returned by ParseHandoff when the header block is
// missing or unparseable, or when a required section (Task, Done, Left,
// Acceptance) is absent or empty.
var ErrMalformed = errors.New("taskstate: malformed handoff")

// ErrMissing is returned by LoadHandoff when HANDOFF.md does not exist,
// and used as the Check sentinel when h is nil.
var ErrMissing = errors.New("taskstate: handoff missing")

// ErrStaleAttempt is the Check sentinel for a hand-off written before the
// current attempt started: an attempt ran after it, so it may no longer
// describe the real state of the work.
var ErrStaleAttempt = errors.New("taskstate: handoff stale (attempt advanced since it was written)")

// ErrStaleHead is the Check sentinel for a hand-off whose recorded
// worktree HEAD no longer matches the current one.
var ErrStaleHead = errors.New("taskstate: handoff stale (worktree head moved)")

// ErrStaleAge is the Check sentinel for a hand-off older than the caller's
// configured max age.
var ErrStaleAge = errors.New("taskstate: handoff stale (too old)")

// ErrWrongTask is the Check sentinel for a hand-off whose task id or slug
// does not match the state it is being checked against.
var ErrWrongTask = errors.New("taskstate: handoff is for a different task")

// requiredSections are the sections ParseHandoff insists are present and
// non-empty. "Tried and rejected" is informative but optional - a first
// attempt may have nothing to report there yet.
var requiredSections = []string{"Task", "Done", "Left", "Acceptance"}

// sectionOrder is the canonical section order used by Template's output
// and assumed (but not strictly required) by ParseHandoff.
var sectionOrder = []string{"Task", "Done", "Left", "Tried and rejected", "Acceptance"}

var sectionHeadingRE = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)

// Handoff is the parsed HANDOFF.md: a small header the harness can check
// deterministically (task id, slug, lane, attempt, when it was written,
// the worktree head it describes), plus free-text sections written by
// the model while it still has full context. See the package doc comment,
// decision 1: hand-off resumes, it never restarts.
type Handoff struct {
	TaskID       int
	Slug         string
	Lane         string
	Attempt      int
	Written      time.Time
	WorktreeHead string
	Sections     map[string]string
	Raw          string
}

// Template renders the HANDOFF.md skeleton for a fresh attempt. The
// harness pre-fills Task and Acceptance, since it already knows those
// facts (the task title/body and the Acceptance struct); the model fills
// in Done, Left, and Tried and rejected before it runs out of context.
func Template(taskID int, slug, lane string, attempt int, taskTitle, taskBody string, acc Acceptance) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "task: %d\n", taskID)
	fmt.Fprintf(&b, "slug: %s\n", slug)
	fmt.Fprintf(&b, "lane: %s\n", lane)
	fmt.Fprintf(&b, "attempt: %d\n", attempt)
	fmt.Fprintf(&b, "written: %s\n", Now().UTC().Format(time.RFC3339))
	b.WriteString("worktree_head: (optional)\n")
	b.WriteString("---\n\n")

	b.WriteString("<!-- fill Done, Left, Tried and rejected; keep the header -->\n\n")

	b.WriteString("## Task\n\n")
	fmt.Fprintf(&b, "%s\n", taskTitle)
	if taskBody != "" {
		fmt.Fprintf(&b, "\n%s\n", taskBody)
	}
	b.WriteString("\n## Done\n\n\n## Left\n\n\n## Tried and rejected\n\n\n## Acceptance\n\n")
	if len(acc.Checks) > 0 {
		b.WriteString("Checks:\n")
		for _, c := range acc.Checks {
			fmt.Fprintf(&b, "- %s\n", c)
		}
	}
	if len(acc.Deliverables) > 0 {
		b.WriteString("Deliverables:\n")
		for _, d := range acc.Deliverables {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(acc.Checks) == 0 && len(acc.Deliverables) == 0 {
		// Acceptance is a required section for ParseHandoff, so a task
		// with no --check and no named deliverables must still render
		// something here, or every hand-off for it would be malformed
		// by construction. Say so explicitly rather than leaving it blank:
		// the next model should know the gate is the agent's own report.
		b.WriteString(NoMachineAcceptance + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

// NoMachineAcceptance is the Acceptance line Template renders when the
// task carries neither --check commands nor named deliverables.
const NoMachineAcceptance = "No machine acceptance configured (no --check commands, no deliverables named); the gate is the agent's own report."

// ParseHandoff parses HANDOFF.md text into a Handoff. Returns ErrMalformed
// when the YAML-ish header block is missing/unparseable or a required
// section (Task, Done, Left, Acceptance) is absent or empty after
// trimming whitespace and the harness's own instruction comment.
func ParseHandoff(text string) (*Handoff, error) {
	if !strings.HasPrefix(text, "---\n") {
		return nil, fmt.Errorf("%w: no header block", ErrMalformed)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("%w: unterminated header block", ErrMalformed)
	}
	headerBlock := rest[:end]
	body := rest[end+len("\n---\n"):]

	h := &Handoff{Raw: text, Sections: map[string]string{}}
	for _, line := range strings.Split(headerBlock, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("%w: bad header line %q", ErrMalformed, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "task":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("%w: bad task id %q: %v", ErrMalformed, val, err)
			}
			h.TaskID = n
		case "slug":
			h.Slug = val
		case "lane":
			h.Lane = val
		case "attempt":
			n, err := strconv.Atoi(val)
			if err != nil {
				return nil, fmt.Errorf("%w: bad attempt %q: %v", ErrMalformed, val, err)
			}
			h.Attempt = n
		case "written":
			ts, err := time.Parse(time.RFC3339, val)
			if err != nil {
				return nil, fmt.Errorf("%w: bad written timestamp %q: %v", ErrMalformed, val, err)
			}
			h.Written = ts
		case "worktree_head":
			if val != "(optional)" && val != "" {
				h.WorktreeHead = val
			}
		}
	}
	if h.Slug == "" {
		return nil, fmt.Errorf("%w: header missing slug", ErrMalformed)
	}
	if h.Written.IsZero() {
		return nil, fmt.Errorf("%w: header missing written timestamp", ErrMalformed)
	}

	// Sections: split on "## Heading" lines, case-sensitive match to
	// Template's own headings (a resuming model is instructed to "keep
	// the header" but may retitle sections - we accept any heading text,
	// only the required ones are enforced below).
	matches := sectionHeadingRE.FindAllStringSubmatchIndex(body, -1)
	for i, m := range matches {
		name := body[m[2]:m[3]]
		start := m[1]
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		content := strings.TrimSpace(body[start:end])
		h.Sections[name] = content
	}

	var missing []string
	for _, name := range requiredSections {
		if strings.TrimSpace(h.Sections[name]) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%w: empty or missing section(s): %s", ErrMalformed, strings.Join(missing, ", "))
	}

	return h, nil
}

// LoadHandoff reads and parses HANDOFF.md for a slug. Returns ErrMissing
// when the file does not exist.
func LoadHandoff(brainPath, slug string) (*Handoff, error) {
	path := HandoffPath(brainPath, slug)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %w", path, ErrMissing)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return ParseHandoff(string(data))
}

// Check validates a hand-off against the current state and worktree
// before the next model is allowed to resume from it. A stale or missing
// hand-off must be detected, never silently ignored (package doc comment,
// decision 1). Returns nil when the hand-off is fresh and usable.
//
// maxAge <= 0 disables the age check.
func Check(h *Handoff, s *State, currentHead string, now time.Time, maxAge time.Duration) error {
	if h == nil {
		return ErrMissing
	}
	if s != nil {
		if h.Slug != "" && s.Slug != "" && h.Slug != s.Slug {
			return fmt.Errorf("%w: handoff slug %q != state slug %q", ErrWrongTask, h.Slug, s.Slug)
		}
		if h.TaskID != 0 && s.TaskID != 0 && h.TaskID != s.TaskID {
			return fmt.Errorf("%w: handoff task %d != state task %d", ErrWrongTask, h.TaskID, s.TaskID)
		}
		if h.Attempt < s.Attempt {
			return fmt.Errorf("%w: handoff written for attempt %d but state is on attempt %d", ErrStaleAttempt, h.Attempt, s.Attempt)
		}
	}
	if h.WorktreeHead != "" && currentHead != "" && h.WorktreeHead != currentHead {
		return fmt.Errorf("%w: handoff head %s != current head %s", ErrStaleHead, h.WorktreeHead, currentHead)
	}
	if maxAge > 0 && !h.Written.IsZero() {
		age := now.Sub(h.Written)
		if age > maxAge {
			return fmt.Errorf("%w: handoff is %s old (max %s)", ErrStaleAge, age, maxAge)
		}
	}
	return nil
}

// IsStale reports whether err is one of the three staleness sentinels
// (ErrStaleAttempt, ErrStaleHead, ErrStaleAge). ErrMissing and
// ErrWrongTask are deliberately excluded - those are "there is nothing
// usable here" / "this is not this task's hand-off", not "there was a
// hand-off but it aged out".
func IsStale(err error) bool {
	return errors.Is(err, ErrStaleAttempt) || errors.Is(err, ErrStaleHead) || errors.Is(err, ErrStaleAge)
}

// ResumePrompt is the instruction given to the next model taking over a
// task from a validated hand-off. It inlines every section so the model
// never needs to re-read HANDOFF.md itself, and it is explicit that this
// is a continuation, not a fresh start (docs/arbiter-plan.md section 6,
// decision 1).
func ResumePrompt(h *Handoff, s *State) string {
	var b strings.Builder
	taskID := h.TaskID
	if taskID == 0 && s != nil {
		taskID = s.TaskID
	}
	lane := h.Lane
	fmt.Fprintf(&b, "You are resuming task #%d on lane %s after a hand-off. ", taskID, lane)
	b.WriteString("Read the hand-off below, do not restart, continue from Left.\n\n")

	for _, name := range sectionOrder {
		content, ok := h.Sections[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n\n", name, content)
	}

	path := HandoffPath("<brain>", h.Slug)
	fmt.Fprintf(&b, "Rewrite HANDOFF.md at %s before you finish if anything in it changed.\n", path)
	return b.String()
}

// ReconstructPrompt is the instruction given to a model when the hand-off
// is missing or failed Check: rebuild HANDOFF.md from git history and
// STATE.json first, then continue. reason is the staleness detail from
// Check (or "missing"), included verbatim so the model knows what to
// distrust.
func ReconstructPrompt(taskID int, slug, path, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The hand-off for task #%d (%s) could not be used: %s\n\n", taskID, slug, reason)
	b.WriteString("Before continuing, rebuild HANDOFF.md yourself:\n")
	b.WriteString("1. Run `git log` and `git diff` against the task's worktree to see what was actually done.\n")
	fmt.Fprintf(&b, "2. Read STATE.json for the task (phase, attempt, last verdict, acceptance).\n")
	fmt.Fprintf(&b, "3. Write a new HANDOFF.md at %s with the Task, Done, Left, Tried and rejected, and Acceptance sections filled from what you found.\n", path)
	b.WriteString("4. Only then continue the work - do not restart from scratch if the git history shows real progress.\n")
	return b.String()
}
