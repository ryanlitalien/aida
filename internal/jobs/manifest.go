package jobs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State enum values for Manifest.State and Job.State.
//
// StateIncomplete means the run stopped without a verified, usable
// result. It is terminal (never automatically resumed) but it is
// NOT a success. Introduced alongside Store.Finalize so a run that
// exhausts its turns/budget mid-task, gets cancelled, or produces a
// final response with no structured verification lands here instead
// of being reported as done. See FinalizeState for the mapping.
const (
	StateQueued           = "queued"
	StateRunning          = "running"
	StateAwaitingInput    = "awaiting_input"
	StateAwaitingApproval = "awaiting_approval"
	StateDone             = "done"
	StateFailed           = "failed"
	StateIncomplete       = "incomplete"
)

// IsTerminalState reports whether s is a final state: done, failed,
// or incomplete. Reapers, vacuum, and queue-view filters key off this.
func IsTerminalState(s string) bool {
	return s == StateDone || s == StateFailed || s == StateIncomplete
}

// IsValidState reports whether s is one of the canonical states.
// Used by the SQL ListOpts filter and any external mutator path that
// accepts a state string from a caller (e.g. a future API).
func IsValidState(s string) bool {
	switch s {
	case StateQueued, StateRunning, StateAwaitingInput, StateAwaitingApproval, StateDone, StateFailed, StateIncomplete:
		return true
	}
	return false
}

// manifestVersion is the schema version stamped onto every new
// manifest. Bump only when the on-disk JSON shape changes in a way
// that older readers can't tolerate. v2 added AwaitingPrompt and
// NotifiedAt for the awaiting_input + voice notification work. v3
// added the approval gate fields. v4 added the per-job governance
// fields (SpendCapUSD/TimeoutSec/ToolAllowlist/SandboxTier). v5 added
// Agent/Model for kind=fleet jobs (aida fleet watch). v6 moved
// ArtifactURL (present since v1) onto the Job/SQL row too, so it is
// no longer manifest-only -- see the note on Manifest.ArtifactURL. v7
// did the same for Destination (present since v1) -- see the note on
// Manifest.Destination. The JSON key itself is unchanged in both
// cases, so older manifests still load fine either way -- Go's
// json.Unmarshal defaults missing keys to zero values. v8 added the
// result contract: TerminationReason/Outcome/Summary/FailureCode
// (Store.Finalize) and the StateIncomplete terminal state -- see
// resultContractVersion.
const manifestVersion = 8

// resultContractVersion is the fixed manifestVersion at which
// TerminationReason/Outcome/Summary/FailureCode were introduced. Unlike
// manifestVersion, this never moves again. It's the line a job's
// recorded Version (or SQL schema_version) is compared against to
// decide whether the record predates the contract at all, regardless
// of how many more manifestVersion bumps come after v8.
//
// A job created before this shipped never had a chance to carry a
// termination reason or structured result, that's a fact about when
// it was written, not a claim about its correctness. EffectiveState
// uses this to downgrade an old "done" to "incomplete" rather than
// treating silence as success. See EffectiveState in outcome.go.
const resultContractVersion = 8

// LegacyUnverifiedReason is the termination reason EffectiveState
// stamps onto a pre-result-contract "done" it downgrades to
// incomplete. Distinct from an empty reason (which means "no reason
// recorded, of any era") so callers can tell "this predates the
// contract" apart from "this predates the contract AND we somehow
// still don't know why."
const LegacyUnverifiedReason = "legacy_unverified"

// Termination mirrors the engine's own Termination enum
// (internal/engine) as plain strings. Defined here rather than
// imported so internal/jobs stays a leaf package: internal/engine
// cannot be imported without risking an import cycle. The caller
// (internal/cli/agent.go) translates its own Termination value into
// one of these constants when it calls Store.Finalize.
const (
	TerminationFinalResponse    = "final_response"
	TerminationAwaitingInput    = "awaiting_input"
	TerminationAwaitingApproval = "awaiting_approval"
	TerminationModelTokenLimit  = "model_token_limit"
	TerminationWallTimeLimit    = "wall_time_limit"
	TerminationCostLimit        = "cost_limit"
	TerminationCancelled        = "cancelled"
	TerminationError            = "error"
)

// Outcome mirrors the engine's structured-result outcome field.
const (
	OutcomeAchieved    = "achieved"
	OutcomeNotAchieved = "not_achieved"
)

// Manifest is the on-disk source of truth for one job. The SQL row
// in jobs.db is a derived index; rebuilding the DB walks every
// manifest and upserts. Profile is denormalized so a manifest
// surfaced in isolation is self-describing.
type Manifest struct {
	Version       int    `json:"version"`
	RunID         string `json:"run_id"`
	Kind          string `json:"kind"`
	Profile       string `json:"profile"`
	TaskSlug      string `json:"task_slug,omitempty"`
	PlanID        string `json:"plan_id,omitempty"`
	State         string `json:"state"`
	ClaimedByHost string `json:"claimed_by_host,omitempty"`
	ClaimedByPID  int    `json:"claimed_by_pid,omitempty"`
	EnqueuedAt    string `json:"enqueued_at"`
	StartedAt     string `json:"started_at,omitempty"`
	FinishedAt    string `json:"finished_at,omitempty"`
	Error         string `json:"error,omitempty"`
	Question      string `json:"question,omitempty"`
	SourceRef     string `json:"source_ref,omitempty"`

	// Destination is the typed delivery surface resolved at enqueue
	// time from the task's partner tags (issue #58). Nil = fall back
	// to the universal Copy / Save modal buttons.
	//
	// Kept jobs-local (not imported from config) so
	// the jobs package stays a leaf. Kept in lockstep with the config
	// type via the constructor in internal/cli/tasks_ingest.go.
	//
	// Also mirrored on Job/the SQL row (manifest v7) -- it used to be
	// manifest-only, which meant ANY later manifest rewrite that goes
	// through jobToManifest (Complete, Fail, SetGovernance,
	// SetWorktreePath, MarkNotified, SetArtifactURL, ...) would
	// silently drop it, since those all rebuild the manifest from a
	// Job that had no Destination field to carry forward. Mirroring it
	// like ArtifactURL closes that hole for every rewrite path, not
	// just the one that happens to run before whatever sets it.
	Destination *Destination `json:"destination,omitempty"`

	// ArtifactURL is the agent's final deliverable URL when the
	// destination produced one (e.g. a github-pr URL). Surfaced to
	// the modal as the "Open PR" link target. Written by the agent
	// in its final step via Store.SetArtifactURL; empty until then.
	//
	// Also mirrored on Job/the SQL row (manifest v6) -- it used to be
	// manifest-only, which meant ANY later manifest rewrite that goes
	// through jobToManifest (Complete, Fail, MarkNotified, ...) would
	// silently drop it, since those all rebuild the manifest from a
	// Job that had no ArtifactURL field to carry forward. Mirroring it
	// like Agent/Model closes that hole for every rewrite path, not
	// just the ones that happen to run before SetArtifactURL.
	ArtifactURL string `json:"artifact_url,omitempty"`

	// AwaitingPrompt is the agent's question to the user when State ==
	// StateAwaitingInput. Populated by Pause; cleared by Resume. The
	// voice notifier surfaces this on next wake.
	AwaitingPrompt string `json:"awaiting_prompt,omitempty"`

	// NotifiedAt is set by the daemon's job-watch goroutine after it
	// enqueues a voice notification for this manifest's terminal /
	// awaiting_input transition. Idempotency key - on daemon restart
	// the watcher skips manifests that already carry this stamp so
	// the user doesn't re-hear a notification for a job that finished
	// before the crash. RFC3339 UTC string.
	NotifiedAt string `json:"notified_at,omitempty"`

	// WorktreePath is the absolute path to a git worktree created for
	// this job (currently only kind=pr_work). The daemon's job-watch
	// goroutine removes the worktree once the job reaches a terminal
	// state. Empty for jobs that don't own a worktree.
	WorktreePath string `json:"worktree_path,omitempty"`

	// ApprovalAction names the irreversible operation the agent is
	// requesting permission to perform when State == StateAwaitingApproval:
	// "merge" | "send" | "writeback". Set by RequestApproval; empty otherwise.
	ApprovalAction string `json:"approval_action,omitempty"`

	// ApprovalPayload is a human-readable one-liner describing exactly what
	// happens on approval (e.g. "merge PR #83 into main"). Surfaced verbatim
	// in the voice notice and `aida jobs`. Never a secret.
	ApprovalPayload string `json:"approval_payload,omitempty"`

	// --- Per-job governance (manifest v4) ----------------------------------
	// These bound a single job's execution independently of any loop-level
	// aggregate. Zero/empty means "no per-job override - fall back to the
	// loop/config default". The loop stamps the tier + timeout it enforces
	// onto each job it spawns so the confinement is observable per job.

	// SpendCapUSD caps this one job's agent spend. 0 = no per-job cap.
	SpendCapUSD float64 `json:"spend_cap_usd,omitempty"`

	// TimeoutSec is the wall-clock budget for this job's agent, in seconds.
	// 0 = use the loop/config default.
	TimeoutSec int `json:"timeout_sec,omitempty"`

	// ToolAllowlist, when non-empty, restricts the agent to these tool names.
	// nil/empty = all tools. Persisted for governance/audit; enforcement in
	// the agent loop is a follow-on.
	ToolAllowlist []string `json:"tool_allowlist,omitempty"`

	// SandboxTier records the confinement tier this job runs under
	// ("", "none", "docker"). Empty = unconfined / not recorded.
	SandboxTier string `json:"sandbox_tier,omitempty"`

	// --- Fleet watch fields (manifest v5) -----------------------------------
	// Set by `aida fleet watch` (kind=fleet) via SetAgentModel so /runs can
	// show the herdr agent name and the model it ran under directly, instead
	// of parsing them out of SourceRef's "fleet:<host>:<user>:<agent>" or the
	// free-form Question text.

	// Agent is the herdr agent name a fleet-watch job is polling.
	Agent string `json:"agent,omitempty"`

	// Model is the --model alias passed to `aida fleet watch` (e.g. a
	// LiteLLM Bedrock alias). Informational; not validated against any
	// model registry.
	Model string `json:"model,omitempty"`

	// --- Result contract (manifest v8) --------------------------------------
	// Set by Store.Finalize so job_status/job_list/the spoken completion
	// notice have real evidence to read instead of just a state string.
	// Complete/Fail (the other terminal-transition paths) leave these
	// empty -- their callers have their own completion contract (a loop
	// --check gate, a fleet exit code) and aren't claiming a verified
	// agent self-report.

	// TerminationReason is one of the Termination* constants describing
	// why the run's process ended -- not why the TASK did or didn't
	// succeed. Empty on jobs finished via Complete/Fail.
	TerminationReason string `json:"termination_reason,omitempty"`

	// Outcome is one of the Outcome* constants: the agent's own
	// structured verdict on whether it achieved the task. Empty means
	// no structured result was ever produced -- see FinalizeState's
	// "anything else" fallback to StateIncomplete.
	Outcome string `json:"outcome,omitempty"`

	// Summary is the agent's own account of what it did, surfaced
	// verbatim by DescribeOutcome when Outcome is present and the run's
	// output backs it up. Never invented by this package.
	Summary string `json:"summary,omitempty"`

	// FailureCode is a short machine code explaining a not_achieved
	// Outcome. Optional even when Outcome is set.
	FailureCode string `json:"failure_code,omitempty"`
}

// Destination is the typed draft-delivery target on the manifest. See
// the doc comment on Manifest.Destination for why this is duplicated
// instead of imported.
type Destination struct {
	Type         string `json:"type"`
	Repo         string `json:"repo,omitempty"`
	BranchPrefix string `json:"branch_prefix,omitempty"`
	Channel      string `json:"channel,omitempty"`
}

// Destination type constants match config.Destination* - both keep
// the same canonical strings so a yaml-resolved type round-trips into
// the manifest unchanged.
const (
	DestinationTypeGitHubPR     = "github-pr"
	DestinationTypeSlackMessage = "slack-message"
)

// WriteManifest atomically persists m to <run-dir>/manifest.json.
// The write order is: serialize → write tmp file in same dir → fsync
// the tmp file → rename → fsync the parent dir. Survives crash mid-
// write: readers either see the prior manifest or the new one, never
// a half-written byte stream.
//
// Auto-creates the per-run directory if missing. Caller passes a
// fully-populated Manifest; this function does not default fields.
func WriteManifest(profile string, m *Manifest) error {
	if m == nil {
		return fmt.Errorf("WriteManifest: nil manifest")
	}
	if m.RunID == "" {
		return fmt.Errorf("WriteManifest: empty RunID")
	}
	if m.Profile == "" {
		return fmt.Errorf("WriteManifest: empty Profile")
	}
	if m.Profile != profile {
		return fmt.Errorf("WriteManifest: profile mismatch (manifest=%q dir=%q)", m.Profile, profile)
	}
	dir := RunDir(profile, m.RunID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	data = append(data, '\n')

	tmpName := fmt.Sprintf("manifest.json.tmp.%d.%d", os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("open tmp manifest: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write tmp manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("fsync tmp manifest: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tmp manifest: %w", err)
	}

	if err := os.Rename(tmpPath, ManifestPath(profile, m.RunID)); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename manifest: %w", err)
	}

	// Best-effort parent-dir fsync. POSIX guarantees the rename is
	// durable only after the directory is fsync'd. Errors here are
	// non-fatal - the rename succeeded and a subsequent crash would
	// at worst replay the same write.
	if pdir, err := os.Open(dir); err == nil {
		_ = pdir.Sync()
		pdir.Close()
	}
	return nil
}

// ReadManifest loads the manifest at <run-dir>/manifest.json.
// Returns os.ErrNotExist (wrapped) when the file is absent so
// callers can distinguish "no such job" from a real read error.
func ReadManifest(profile, runID string) (*Manifest, error) {
	if runID == "" {
		return nil, fmt.Errorf("ReadManifest: empty runID")
	}
	data, err := os.ReadFile(ManifestPath(profile, runID))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal manifest %s: %w", runID, err)
	}
	return &m, nil
}

// jobToManifest folds a Job into the Manifest shape for disk writes.
//
// Version is copied straight from j.SchemaVersion rather than
// re-stamped with the current binary's manifestVersion on every call.
// Every Store method that mutates a job re-reads it via Get first, so
// without this a job enqueued under an older manifestVersion would
// have its Version silently bumped the moment ANY later write touched
// it (e.g. MarkNotified), destroying the exact signal EffectiveState
// needs to tell "predates the result contract" apart from "current."
func jobToManifest(j *Job) *Manifest {
	m := &Manifest{
		Version:           j.SchemaVersion,
		RunID:             j.RunID,
		Kind:              j.Kind,
		Profile:           j.Profile,
		TaskSlug:          j.TaskSlug,
		PlanID:            j.PlanID,
		State:             j.State,
		ClaimedByHost:     j.ClaimedByHost,
		ClaimedByPID:      j.ClaimedByPID,
		Error:             j.Error,
		Question:          j.Question,
		SourceRef:         j.SourceRef,
		AwaitingPrompt:    j.AwaitingPrompt,
		NotifiedAt:        j.NotifiedAt,
		WorktreePath:      j.WorktreePath,
		ApprovalAction:    j.ApprovalAction,
		ApprovalPayload:   j.ApprovalPayload,
		SpendCapUSD:       j.SpendCapUSD,
		TimeoutSec:        j.TimeoutSec,
		ToolAllowlist:     j.ToolAllowlist,
		SandboxTier:       j.SandboxTier,
		Agent:             j.Agent,
		Model:             j.Model,
		ArtifactURL:       j.ArtifactURL,
		Destination:       j.Destination,
		TerminationReason: j.TerminationReason,
		Outcome:           j.Outcome,
		Summary:           j.Summary,
		FailureCode:       j.FailureCode,
	}
	if !j.EnqueuedAt.IsZero() {
		m.EnqueuedAt = j.EnqueuedAt.UTC().Format(time.RFC3339)
	}
	if !j.StartedAt.IsZero() {
		m.StartedAt = j.StartedAt.UTC().Format(time.RFC3339)
	}
	if !j.FinishedAt.IsZero() {
		m.FinishedAt = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	return m
}

// manifestToJob is the inverse - used by Reindex to repopulate the
// SQL row from disk. v1 manifests with missing AwaitingPrompt /
// NotifiedAt fields decode as zero strings - no migration step needed
// because Go's json.Unmarshal defaults missing keys to zero values.
// A pre-v8 manifest's missing TerminationReason/Outcome/Summary/
// FailureCode decode the same way. That absence, combined with
// Version < resultContractVersion, is exactly what EffectiveState
// reads to detect a legacy record.
func manifestToJob(m *Manifest) *Job {
	j := &Job{
		RunID:             m.RunID,
		Kind:              m.Kind,
		State:             m.State,
		TaskSlug:          m.TaskSlug,
		PlanID:            m.PlanID,
		Profile:           m.Profile,
		ClaimedByHost:     m.ClaimedByHost,
		ClaimedByPID:      m.ClaimedByPID,
		Error:             m.Error,
		Question:          m.Question,
		SourceRef:         m.SourceRef,
		AwaitingPrompt:    m.AwaitingPrompt,
		NotifiedAt:        m.NotifiedAt,
		WorktreePath:      m.WorktreePath,
		ApprovalAction:    m.ApprovalAction,
		ApprovalPayload:   m.ApprovalPayload,
		SpendCapUSD:       m.SpendCapUSD,
		TimeoutSec:        m.TimeoutSec,
		ToolAllowlist:     m.ToolAllowlist,
		SandboxTier:       m.SandboxTier,
		Agent:             m.Agent,
		Model:             m.Model,
		ArtifactURL:       m.ArtifactURL,
		Destination:       m.Destination,
		SchemaVersion:     m.Version,
		TerminationReason: m.TerminationReason,
		Outcome:           m.Outcome,
		Summary:           m.Summary,
		FailureCode:       m.FailureCode,
	}
	j.EnqueuedAt, _ = parseTime(m.EnqueuedAt)
	j.StartedAt, _ = parseTime(m.StartedAt)
	j.FinishedAt, _ = parseTime(m.FinishedAt)
	return j
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// SetArtifactURL moved to store.go as a Store method (manifest v6) so it
// can update the SQL row as well as the manifest -- see Store.SetArtifactURL.
//
// SetDestination moved to store.go as a Store method (manifest v7) for the
// same reason -- see Store.SetDestination.
