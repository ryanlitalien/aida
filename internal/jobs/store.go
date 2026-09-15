package jobs

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/runs"
)

// Store is the per-profile handle to a jobs queue. Holds a SQLite
// connection plus the profile name (resolved into paths via the
// helpers in paths.go).
type Store struct {
	conn    *sql.DB
	profile string
}

// Job is the queryable shape of a job row. Times are Go time.Time
// here; the manifest serializes them as RFC3339 strings.
type Job struct {
	RunID         string
	Kind          string
	State         string
	TaskSlug      string
	PlanID        string
	Profile       string
	ClaimedByHost string
	ClaimedByPID  int
	EnqueuedAt    time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
	Error         string
	Question      string
	SourceRef     string

	// AwaitingPrompt mirrors the manifest field - set by Pause when
	// the agent yields control back to the user, cleared by Resume.
	AwaitingPrompt string
	// NotifiedAt mirrors the manifest field - set after the daemon
	// surfaces a voice notification for this job's transition. Used
	// for idempotency across daemon restarts.
	NotifiedAt string
	// WorktreePath mirrors the manifest field - set at job_start time
	// for kind=pr_work jobs that own a git worktree.
	WorktreePath string
	// ApprovalAction / ApprovalPayload mirror the manifest fields - set by
	// RequestApproval when the agent pauses for human sign-off before an
	// irreversible action (merge/send/writeback).
	ApprovalAction  string
	ApprovalPayload string

	// Per-job governance (manifest v4). Mirror the manifest fields; set via
	// SetGovernance. Zero/empty = no per-job override. See Governance.
	SpendCapUSD   float64
	TimeoutSec    int
	ToolAllowlist []string
	SandboxTier   string

	// Agent / Model (manifest v5). Mirror the manifest fields; set via
	// SetAgentModel. Empty = not a fleet-watch job, or not yet stamped.
	Agent string
	Model string

	// ArtifactURL (manifest v6). Mirrors the manifest field; set via
	// SetArtifactURL. Empty = no deliverable URL yet. Moved onto the
	// Job/SQL row so every manifest rewrite path (Complete, Fail,
	// MarkNotified, ...) carries it forward instead of dropping it --
	// see the doc comment on Manifest.ArtifactURL.
	ArtifactURL string

	// Destination (manifest v7). Mirrors the manifest field; set via
	// SetDestination. Nil = no typed delivery surface (falls back to the
	// universal Copy/Save modal buttons). Moved onto the Job/SQL row for
	// the same reason ArtifactURL was in v6: Destination used to be
	// manifest-only, so any later manifest rewrite (Complete, Fail,
	// SetGovernance, SetWorktreePath, MarkNotified, SetArtifactURL --
	// every one of which rebuilds the WHOLE manifest via
	// WriteManifest(jobToManifest(j))) would silently drop a
	// previously-set Destination, since jobToManifest had no Destination
	// field on Job to carry forward. See the doc comment on
	// Manifest.Destination.
	Destination *Destination

	// SchemaVersion (manifest v8) is the manifestVersion in effect when
	// this job was first Enqueued. Stamped once and carried through
	// every later rewrite verbatim by jobToManifest, see its doc
	// comment for why that matters. A pre-migration SQL row (ALTER
	// TABLE's DEFAULT 0) or a manifest written before Version existed
	// reads as 0, which is always < resultContractVersion and so always
	// reads as legacy. See EffectiveState.
	SchemaVersion int

	// TerminationReason / Outcome / Summary / FailureCode (manifest v8)
	// are the result contract Store.Finalize writes. Empty on any job
	// finished via Complete/Fail instead, since those callers verify
	// success through their own mechanism (a loop --check gate, a fleet
	// exit code), not a self-reported agent result. See DescribeOutcome.
	TerminationReason string
	Outcome           string
	Summary           string
	FailureCode       string
}

// Governance groups the per-job execution bounds passed to SetGovernance.
// Zero-valued fields leave the corresponding override unset (the job falls
// back to the loop/config default). Persisted to the manifest + jobs.db so
// each job records the confinement and limits it ran under.
type Governance struct {
	SpendCapUSD   float64  // 0 = no per-job cap
	TimeoutSec    int      // 0 = use loop/config default
	ToolAllowlist []string // nil/empty = all tools
	SandboxTier   string   // "", "none", "docker"
}

// ListOpts narrows a List call. Zero values mean "no filter."
type ListOpts struct {
	State    string
	TaskSlug string
	PlanID   string
	Since    time.Time
	Limit    int
}

// ErrNotFound is returned by Get when the run id has no row.
var ErrNotFound = errors.New("jobs: run id not found")

// Open creates (or opens) the per-profile jobs store. Runs migrations
// idempotently. Caller must Close when done.
func Open(profile string) (*Store, error) {
	conn, err := openDB(profile)
	if err != nil {
		return nil, err
	}
	return &Store{conn: conn, profile: profile}, nil
}

// Close releases the SQLite connection. Idempotent.
func (s *Store) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	return s.conn.Close()
}

// Profile returns the profile name this store is bound to.
func (s *Store) Profile() string { return s.profile }

// Enqueue creates a new job in state=queued. Writes the manifest to
// disk (source of truth) before inserting the SQL row - if the SQL
// insert fails, Reindex picks up the orphan manifest on next run.
//
// run_id is computed from the question via runs.NewID so it sorts
// chronologically and matches the existing run-id format used by
// internal/runs/.
func (s *Store) Enqueue(kind, taskSlug, planID, question, sourceRef string) (*Job, error) {
	if kind == "" {
		kind = "draft"
	}
	now := time.Now().UTC()
	runID := runs.NewID(now, question)

	j := &Job{
		RunID:         runID,
		Kind:          kind,
		State:         StateQueued,
		TaskSlug:      taskSlug,
		PlanID:        planID,
		Profile:       s.profile,
		EnqueuedAt:    now,
		Question:      question,
		SourceRef:     sourceRef,
		SchemaVersion: manifestVersion,
	}

	// Manifest first (source of truth).
	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}

	if err := s.insertRow(j); err != nil {
		// Manifest survives - Reindex will recover. Surface the
		// SQL error so callers can react (e.g. retry, alert).
		return nil, fmt.Errorf("insert jobs row: %w", err)
	}
	return j, nil
}

// Get returns the job with the given run id, or ErrNotFound.
func (s *Store) Get(runID string) (*Job, error) {
	row := s.conn.QueryRow(`
		SELECT run_id, kind, state, task_slug, plan_id, profile,
		       claimed_by_host, claimed_by_pid,
		       enqueued_at, started_at, finished_at,
		       error, question, source_ref,
		       awaiting_prompt, notified_at, worktree_path,
		       approval_action, approval_payload,
		       spend_cap_usd, timeout_sec, tool_allowlist, sandbox_tier,
		       agent, model, artifact_url, destination,
		       schema_version, termination_reason, outcome, summary, failure_code
		  FROM jobs WHERE run_id = ?`, runID)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// List returns matching jobs, newest first by enqueued_at. Filters
// compose with AND. Empty opts returns every job.
func (s *Store) List(opts ListOpts) ([]Job, error) {
	var (
		conds []string
		args  []any
	)
	if opts.State != "" {
		if !IsValidState(opts.State) {
			return nil, fmt.Errorf("List: invalid state %q", opts.State)
		}
		conds = append(conds, "state = ?")
		args = append(args, opts.State)
	}
	if opts.TaskSlug != "" {
		conds = append(conds, "task_slug = ?")
		args = append(args, opts.TaskSlug)
	}
	if opts.PlanID != "" {
		conds = append(conds, "plan_id = ?")
		args = append(args, opts.PlanID)
	}
	if !opts.Since.IsZero() {
		conds = append(conds, "enqueued_at >= ?")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}

	q := `SELECT run_id, kind, state, task_slug, plan_id, profile,
	             claimed_by_host, claimed_by_pid,
	             enqueued_at, started_at, finished_at,
	             error, question, source_ref,
	             awaiting_prompt, notified_at, worktree_path,
	             approval_action, approval_payload,
	             spend_cap_usd, timeout_sec, tool_allowlist, sandbox_tier,
	             agent, model, artifact_url, destination,
	             schema_version, termination_reason, outcome, summary, failure_code
	        FROM jobs`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY enqueued_at DESC"
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}

	rows, err := s.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// Complete transitions a running job to state=done. Writes the
// updated manifest first, then the SQL row, mirroring Enqueue's
// disk-first ordering. Caller is expected to have already written
// output.md before calling this.
func (s *Store) Complete(runID string) error {
	return s.transitionTerminal(runID, StateDone, "")
}

// Fail transitions a job to state=failed and records the error
// message. Same disk-first ordering as Complete.
func (s *Store) Fail(runID, errorMsg string) error {
	return s.transitionTerminal(runID, StateFailed, errorMsg)
}

// FinalizeParams is what a finished agent run hands Store.Finalize. It
// is a small, plain-string struct rather than the engine's own richer
// Termination/AgentResult types, so this package never has to import
// internal/engine, see the Termination* constants in manifest.go for
// why. The caller (internal/cli/agent.go) translates its own typed
// values into this shape.
type FinalizeParams struct {
	// Termination is one of the Termination* constants in manifest.go.
	// Required, an empty or unrecognized value falls into
	// FinalizeState's "anything else" bucket, same as any other
	// termination that isn't final_response or error.
	Termination string

	// HasResult reports whether the agent produced a structured result
	// at all. False is exactly the "recorded done but never verified"
	// failure mode Finalize exists to close: a final_response with
	// HasResult false still lands on StateIncomplete, never StateDone.
	HasResult bool

	// Outcome is one of the Outcome* constants in manifest.go. Only
	// consulted when HasResult is true.
	Outcome string

	// Summary is the agent's own account of what it did. Only
	// persisted when HasResult is true, never invented for a run
	// that produced no structured result.
	Summary string

	// FailureCode is a short machine code explaining a not_achieved
	// Outcome. Optional even when HasResult is true.
	FailureCode string

	// ErrorMsg is recorded on the job's Error field, matching Fail's
	// errorMsg argument. Typically set for Termination ==
	// TerminationError; harmless to set for any other termination too.
	ErrorMsg string
}

// FinalizeState maps a run's outcome to the terminal state it earns:
//
//	final_response + outcome achieved      -> done
//	final_response + outcome not_achieved  -> failed
//	termination error                      -> failed
//	anything else (including final_response
//	  with no structured result at all)    -> incomplete
//
// "Anything else" covers every other Termination* value (awaiting_input,
// awaiting_approval, model_token_limit, wall_time_limit, cost_limit,
// cancelled) and, deliberately, a final_response with HasResult false,
// the exact shape of the bug that motivated this type: a run that
// stopped without ever verifying what it did must never read as done.
//
// Exported (rather than a private helper folded into Finalize) so a
// caller, or a test, can preview the state a given outcome would earn
// without writing anything.
func FinalizeState(p FinalizeParams) string {
	switch {
	case p.Termination == TerminationFinalResponse && p.HasResult && p.Outcome == OutcomeAchieved:
		return StateDone
	case p.Termination == TerminationFinalResponse && p.HasResult && p.Outcome == OutcomeNotAchieved:
		return StateFailed
	case p.Termination == TerminationError:
		return StateFailed
	default:
		return StateIncomplete
	}
}

// Finalize transitions a running job to the terminal state its actual
// outcome earns (FinalizeState), persisting the termination reason and,
// when present, the structured result. This is the path a finished
// agent run takes. Complete and Fail remain for callers with their
// own completion contract (the loop's --check gate, fleet's exit code)
// that isn't a self-reported agent result.
//
// Same disk-first ordering and first-terminal-wins stickiness as
// transitionTerminal: a late Finalize can't overwrite a job some other
// party (a user cancel, the reaper) already closed out.
func (s *Store) Finalize(runID string, p FinalizeParams) error {
	state := FinalizeState(p)
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	if IsTerminalState(j.State) {
		return nil
	}
	j.State = state
	j.FinishedAt = time.Now().UTC()
	j.TerminationReason = p.Termination
	if p.HasResult {
		j.Outcome = p.Outcome
		j.Summary = p.Summary
		j.FailureCode = p.FailureCode
	}
	if p.ErrorMsg != "" {
		j.Error = p.ErrorMsg
	}

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, finished_at = ?, error = ?,
		       termination_reason = ?, outcome = ?, summary = ?, failure_code = ?
		 WHERE run_id = ?`,
		j.State, j.FinishedAt.UTC().Format(time.RFC3339), j.Error,
		j.TerminationReason, j.Outcome, j.Summary, j.FailureCode, runID,
	)
	// Same pid-file cleanup as transitionTerminal: a terminal row should
	// never leave a "present while running" pid file behind.
	removePIDFile(s.profile, runID)
	return err
}

func (s *Store) transitionTerminal(runID, newState, errorMsg string) error {
	if !IsTerminalState(newState) {
		return fmt.Errorf("transitionTerminal: %q is not terminal", newState)
	}
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	// First terminal state wins. Multiple parties can race to finish a
	// row - the agent's own exit path, the spawner's cmd.Wait goroutine,
	// a user cancel - and a late Fail must not overwrite a Complete, nor
	// a killed process's exit error clobber "cancelled by user".
	if IsTerminalState(j.State) {
		return nil
	}
	j.State = newState
	j.FinishedAt = time.Now().UTC()
	if errorMsg != "" {
		j.Error = errorMsg
	}

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, finished_at = ?, error = ?
		 WHERE run_id = ?`,
		j.State, j.FinishedAt.UTC().Format(time.RFC3339), j.Error, runID,
	)
	// The pid file's contract is "present while running" - clear it on
	// any terminal transition so Reap never second-guesses a finished
	// row. Best-effort; absent is fine.
	removePIDFile(s.profile, runID)
	return err
}

// DeleteJob removes one row from jobs.db. Caller is responsible for
// removing the run-dir on disk (vacuum does both). Idempotent - a
// missing row is not an error, matching brain.db convention.
func (s *Store) DeleteJob(runID string) error {
	_, err := s.conn.Exec(`DELETE FROM jobs WHERE run_id = ?`, runID)
	return err
}

// Pause transitions a running job to awaiting_input and records the
// agent's prompt to the user. Mirrors Complete/Fail's disk-first
// ordering: manifest write first, SQL row second. The agent-side
// caller (the ask_user tool) blocks polling input.txt after this
// returns; the daemon's job-watch goroutine sees the new state and
// enqueues a voice notification.
func (s *Store) Pause(runID, prompt string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.State = StateAwaitingInput
	j.AwaitingPrompt = prompt
	// Clear NotifiedAt so the watcher fires a fresh notification on
	// each new pause (an agent that asks twice in one run produces
	// two prompts to the user).
	j.NotifiedAt = ""

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, awaiting_prompt = ?, notified_at = ''
		 WHERE run_id = ?`,
		j.State, j.AwaitingPrompt, runID,
	)
	return err
}

// Resume transitions an awaiting_input job back to running and
// clears AwaitingPrompt. Mirror of Pause. Called by the ask_user
// tool after it reads the user's reply from input.txt and is about
// to return that reply to the agent loop.
func (s *Store) Resume(runID string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.State = StateRunning
	j.AwaitingPrompt = ""

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, awaiting_prompt = ''
		 WHERE run_id = ?`,
		j.State, runID,
	)
	return err
}

// RequestApproval transitions a running job to awaiting_approval and records
// the irreversible action (merge/send/writeback) the agent wants to perform.
// Mirrors Pause's disk-first ordering and NotifiedAt clearing so the daemon
// speaks a fresh "ready for approval" notice each time. The agent-side caller
// (the request_approval tool) blocks polling approval.txt after this returns.
func (s *Store) RequestApproval(runID, action, payload string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.State = StateAwaitingApproval
	j.ApprovalAction = action
	j.ApprovalPayload = payload
	j.NotifiedAt = ""

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET state = ?, approval_action = ?, approval_payload = ?, notified_at = ''
		 WHERE run_id = ?`,
		j.State, j.ApprovalAction, j.ApprovalPayload, runID,
	)
	return err
}

// Approve flips an awaiting_approval job back to running so the agent unblocks
// and performs the action it was holding. Mirror of Resume; ApprovalAction /
// ApprovalPayload are left set for the audit trail.
func (s *Store) Approve(runID string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	if j.State != StateAwaitingApproval {
		return fmt.Errorf("Approve: job %s is %q, not awaiting_approval", runID, j.State)
	}
	j.State = StateRunning

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`UPDATE jobs SET state = ? WHERE run_id = ?`, j.State, runID)
	return err
}

// RejectApproval transitions an awaiting_approval job to failed with a reason,
// so the agent's poll loop observes a terminal manifest and aborts WITHOUT
// acting. Terminal => the daemon's worktree/sandbox cleanup fires naturally.
func (s *Store) RejectApproval(runID, reason string) error {
	if reason == "" {
		reason = "rejected by user"
	}
	return s.Fail(runID, "approval rejected: "+reason)
}

// SetWorktreePath stamps the job's WorktreePath field. Called by the
// pr_work job_start path after `git worktree add` succeeds so the
// daemon's job-watch goroutine can locate the worktree to clean up
// once the job terminates.
func (s *Store) SetWorktreePath(runID, path string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.WorktreePath = path

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET worktree_path = ?
		 WHERE run_id = ?`,
		path, runID,
	)
	return err
}

// SetGovernance stamps the per-job governance fields (manifest v4) onto a
// job. Mirrors SetWorktreePath's disk-first ordering: manifest write first,
// then the SQL row. Called by the loop after Enqueue to record the tier +
// timeout each spawned agent runs under, and available to any other creator
// (voice job_start, web UI) that wants to bound a single job. Idempotent.
func (s *Store) SetGovernance(runID string, g Governance) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.SpendCapUSD = g.SpendCapUSD
	j.TimeoutSec = g.TimeoutSec
	j.ToolAllowlist = g.ToolAllowlist
	j.SandboxTier = g.SandboxTier

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET spend_cap_usd = ?, timeout_sec = ?, tool_allowlist = ?, sandbox_tier = ?
		 WHERE run_id = ?`,
		j.SpendCapUSD, j.TimeoutSec, encodeAllowlist(j.ToolAllowlist), j.SandboxTier, runID,
	)
	return err
}

// SetAgentModel stamps the herdr agent name and model alias (manifest v5)
// onto a job. Mirrors SetGovernance's disk-first ordering. Called by
// `aida fleet watch` right after Enqueue/MarkRunning (or right after
// resolving an existing --run-id) so /runs can show both without parsing
// SourceRef or Question. Idempotent; either argument may be "".
func (s *Store) SetAgentModel(runID, agent, model string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.Agent = agent
	j.Model = model

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET agent = ?, model = ?
		 WHERE run_id = ?`,
		j.Agent, j.Model, runID,
	)
	return err
}

// SetArtifactURL stamps the job's final deliverable URL (manifest v6)
// onto both the SQL row and the manifest. Mirrors SetAgentModel's
// disk-first ordering. Used by the agent at the end of a
// destination-typed run (e.g. github-pr produces a PR URL) to record
// the deliverable so the modal can render an "Open PR" link without
// parsing the full draft body, and by `aida fleet watch` for the same
// purpose.
//
// ArtifactURL used to be manifest-only, which meant Complete/Fail/
// MarkNotified -- every one of which rewrites the WHOLE manifest via
// WriteManifest(jobToManifest(j)) -- would silently drop it, since
// jobToManifest had no ArtifactURL to carry forward from a Job that
// didn't have the field. Now that it's a Job/SQL field too, every one
// of those rewrite paths carries it forward correctly; see
// TestCompleteThenMarkNotifiedKeepsArtifactURL.
//
// Idempotent: setting the same URL twice is a no-op write.
func (s *Store) SetArtifactURL(runID, url string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	if j.ArtifactURL == url {
		return nil
	}
	j.ArtifactURL = url

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET artifact_url = ?
		 WHERE run_id = ?`,
		j.ArtifactURL, runID,
	)
	return err
}

// SetDestination stamps the job's typed delivery surface (manifest v7)
// onto both the SQL row and the manifest. Mirrors SetArtifactURL's
// disk-first ordering and idempotency shape. Used by the auto-solve
// paths (tasks_ingest.go, tasks_web.go) right after Enqueue, once the
// task's tags have resolved to a typed destination, so
// the web modal can pick the right renderer.
//
// Destination used to be manifest-only, which meant Complete/Fail/
// SetGovernance/SetWorktreePath/MarkNotified/SetArtifactURL -- every
// one of which rewrites the WHOLE manifest via
// WriteManifest(jobToManifest(j)) -- would silently drop it, since
// jobToManifest had no Destination to carry forward from a Job that
// didn't have the field. Now that it's a Job/SQL field too, every one
// of those rewrite paths carries it forward correctly.
//
// Idempotent: setting an equivalent Destination twice is a no-op write.
func (s *Store) SetDestination(runID string, dest *Destination) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	if destinationsEqual(j.Destination, dest) {
		return nil
	}
	j.Destination = dest

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET destination = ?
		 WHERE run_id = ?`,
		encodeDestination(j.Destination), runID,
	)
	return err
}

// destinationsEqual reports whether a and b encode the same delivery
// surface, treating nil and a nil-valued pointer as equal. Used by
// SetDestination's idempotency check.
func destinationsEqual(a, b *Destination) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// MarkNotified stamps the job's NotifiedAt field with the given UTC
// timestamp. The daemon's job-watch goroutine calls this after it
// hands a notification to the notifier so the watcher doesn't
// re-enqueue the same notification on the next poll tick (and so a
// daemon restart doesn't re-speak old notifications).
func (s *Store) MarkNotified(runID, when string) error {
	j, err := s.Get(runID)
	if err != nil {
		return err
	}
	j.NotifiedAt = when

	if err := WriteManifest(s.profile, jobToManifest(j)); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	_, err = s.conn.Exec(`
		UPDATE jobs
		   SET notified_at = ?
		 WHERE run_id = ?`,
		when, runID,
	)
	return err
}

// insertRow is the SQL half of Enqueue. Split out so future callers
// (e.g. Reindex) can write rows without going through manifest write.
func (s *Store) insertRow(j *Job) error {
	_, err := s.conn.Exec(`
		INSERT INTO jobs (
			run_id, kind, state, task_slug, plan_id, profile,
			claimed_by_host, claimed_by_pid,
			enqueued_at, started_at, finished_at,
			error, question, source_ref,
			awaiting_prompt, notified_at, worktree_path,
			approval_action, approval_payload,
			spend_cap_usd, timeout_sec, tool_allowlist, sandbox_tier,
			agent, model, artifact_url, destination,
			schema_version, termination_reason, outcome, summary, failure_code
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.RunID, j.Kind, j.State, j.TaskSlug, j.PlanID, j.Profile,
		j.ClaimedByHost, j.ClaimedByPID,
		fmtTime(j.EnqueuedAt), fmtTime(j.StartedAt), fmtTime(j.FinishedAt),
		j.Error, j.Question, j.SourceRef,
		j.AwaitingPrompt, j.NotifiedAt, j.WorktreePath,
		j.ApprovalAction, j.ApprovalPayload,
		j.SpendCapUSD, j.TimeoutSec, encodeAllowlist(j.ToolAllowlist), j.SandboxTier,
		j.Agent, j.Model, j.ArtifactURL, encodeDestination(j.Destination),
		j.SchemaVersion, j.TerminationReason, j.Outcome, j.Summary, j.FailureCode,
	)
	return err
}

// rowScanner is the subset of *sql.Row / *sql.Rows used by scanJob,
// matching the brain.scanner pattern in memory_types.go:377.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(r rowScanner) (*Job, error) {
	var j Job
	var enqueued, started, finished, allowlist, destination string
	if err := r.Scan(
		&j.RunID, &j.Kind, &j.State, &j.TaskSlug, &j.PlanID, &j.Profile,
		&j.ClaimedByHost, &j.ClaimedByPID,
		&enqueued, &started, &finished,
		&j.Error, &j.Question, &j.SourceRef,
		&j.AwaitingPrompt, &j.NotifiedAt, &j.WorktreePath,
		&j.ApprovalAction, &j.ApprovalPayload,
		&j.SpendCapUSD, &j.TimeoutSec, &allowlist, &j.SandboxTier,
		&j.Agent, &j.Model, &j.ArtifactURL, &destination,
		&j.SchemaVersion, &j.TerminationReason, &j.Outcome, &j.Summary, &j.FailureCode,
	); err != nil {
		return nil, err
	}
	j.EnqueuedAt, _ = parseTime(enqueued)
	j.StartedAt, _ = parseTime(started)
	j.FinishedAt, _ = parseTime(finished)
	j.ToolAllowlist = decodeAllowlist(allowlist)
	j.Destination = decodeDestination(destination)
	return &j, nil
}

// encodeAllowlist serializes a tool allowlist for the jobs.db TEXT column.
// nil/empty encodes as "" (not "null"/"[]") so the default-empty column and
// an unset allowlist round-trip to the same nil slice via decodeAllowlist.
func encodeAllowlist(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	b, err := json.Marshal(tools)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeAllowlist is the inverse of encodeAllowlist. "" => nil.
func decodeAllowlist(s string) []string {
	if s == "" {
		return nil
	}
	var tools []string
	if err := json.Unmarshal([]byte(s), &tools); err != nil {
		return nil
	}
	return tools
}

// encodeDestination serializes a Destination for the jobs.db TEXT
// column. nil encodes as "" (not "null") so the default-empty column
// and an unset destination round-trip to the same nil pointer via
// decodeDestination, matching encodeAllowlist's convention.
func encodeDestination(d *Destination) string {
	if d == nil {
		return ""
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeDestination is the inverse of encodeDestination. "" => nil.
func decodeDestination(s string) *Destination {
	if s == "" {
		return nil
	}
	var d Destination
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return nil
	}
	return &d
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
