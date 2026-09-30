package taskstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Phase values for State.Phase. See docs/arbiter-plan.md section 6: the
// harness advances these, never the model.
const (
	PhaseClaimed = "claimed"
	PhaseRunning = "running"
	PhaseGate    = "gate"
	PhaseHandoff = "handoff"
	PhaseResumed = "resumed"
	PhaseDone    = "done"
	PhaseHold    = "hold"
)

// ErrNoState is returned by Load when no STATE.json exists yet for the
// slug. It wraps os.ErrNotExist so callers can use errors.Is(err,
// os.ErrNotExist) as well as errors.Is(err, ErrNoState).
var ErrNoState = errors.New("taskstate: no state file")

// Now is injected so tests can control timestamps deterministically.
// Production code leaves this at its default (time.Now).
var Now = time.Now

// State is the harness's per-task record: what attempt is running, on
// which lane, at what step, and what the last verdict was. It is written
// ONLY by the harness loop (internal/cli), never by a model -- see the
// package doc comment, decision 1. Field names use snake_case JSON tags
// to match the rest of the arbiter's on-disk formats (signals.ndjson,
// ledger.ndjson).
type State struct {
	Slug             string     `json:"slug"`
	TaskID           int        `json:"task_id"`
	Phase            string     `json:"phase"`
	Step             int        `json:"step"`
	Attempt          int        `json:"attempt"`
	AttemptStartedAt time.Time  `json:"attempt_started_at"`
	Lane             string     `json:"lane"`
	Model            string     `json:"model"`
	Host             string     `json:"host"`
	Started          time.Time  `json:"started"`
	Updated          time.Time  `json:"updated"`
	NextAction       string     `json:"next_action"`
	RunIDs           []string   `json:"run_ids,omitempty"`
	WorktreeHead     string     `json:"worktree_head,omitempty"`
	Acceptance       Acceptance `json:"acceptance"`
	LastVerdict      string     `json:"last_verdict,omitempty"`
	HandoffWrittenAt time.Time  `json:"handoff_written_at,omitempty"`
	HandoffAttempt   int        `json:"handoff_attempt,omitempty"`
}

// Dir returns the per-task state directory under the brain repo:
// <brainPath>/arbiter/<slug>. Deliberately NOT under <brain>/tasks/ --
// IndexTasks walks tasks/*.md and would choke on these files (plan
// section 6).
func Dir(brainPath, slug string) string {
	return filepath.Join(brainPath, "arbiter", slug)
}

// StatePath returns the STATE.json path for a task.
func StatePath(brainPath, slug string) string {
	return filepath.Join(Dir(brainPath, slug), "STATE.json")
}

// HandoffPath returns the HANDOFF.md path for a task.
func HandoffPath(brainPath, slug string) string {
	return filepath.Join(Dir(brainPath, slug), "HANDOFF.md")
}

// LogPath returns the LOG.md path for a task.
func LogPath(brainPath, slug string) string {
	return filepath.Join(Dir(brainPath, slug), "LOG.md")
}

// Load reads a task's STATE.json. Returns ErrNoState (wrapping
// os.ErrNotExist) when the file does not exist yet, so callers can
// distinguish "no state" from a real read/parse error.
func Load(brainPath, slug string) (*State, error) {
	path := StatePath(brainPath, slug)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Wrap both ErrNoState and the original os.ErrNotExist-family
			// error so callers can match on either sentinel.
			return nil, fmt.Errorf("%s: %w: %w", path, ErrNoState, err)
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &s, nil
}

// Save writes STATE.json atomically (temp file + rename, so a reader
// never observes a partially-written file) and always stamps Updated to
// Now(). The caller is the harness loop -- this function does not decide
// phase transitions, it just persists whatever the caller set.
func Save(brainPath string, s *State) error {
	if s.Slug == "" {
		return fmt.Errorf("taskstate: Save requires a non-empty Slug")
	}
	s.Updated = Now()

	dir := Dir(brainPath, s.Slug)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	target := StatePath(brainPath, s.Slug)
	tmp, err := os.CreateTemp(dir, ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp state file into place: %w", err)
	}
	return nil
}

// BeginAttempt advances the state to a new attempt: increments Attempt,
// stamps AttemptStartedAt, and sets Phase to running. It does not persist
// the change -- callers still call Save.
func (s *State) BeginAttempt(lane, model, host string, now time.Time) {
	s.Attempt++
	s.AttemptStartedAt = now
	s.Lane = lane
	s.Model = model
	s.Host = host
	s.Phase = PhaseRunning
}

// AppendLog appends one timestamped line to LOG.md, the human narrative
// for a task. LOG.md is descriptive only -- nothing in the arbiter reads
// it back to make a decision; STATE.json and HANDOFF.md are the sources
// of truth for that. Creates the directory and file if needed.
func AppendLog(brainPath, slug, line string) error {
	dir := Dir(brainPath, slug)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.OpenFile(LogPath(brainPath, slug), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("opening log: %w", err)
	}
	defer f.Close()
	ts := Now().UTC().Format(time.RFC3339)
	if _, err := fmt.Fprintf(f, "[%s] %s\n", ts, line); err != nil {
		return fmt.Errorf("writing log line: %w", err)
	}
	return nil
}
