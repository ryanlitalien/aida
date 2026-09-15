package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ryanlitalien/aida/internal/jobs"
)

// ─── approve_job / reject_job ───────────────────────────────────────────────
//
// The voice side of the HITL approval gate (autonomous-loop Phase 5). A job
// that called the engine-side request_approval tool is parked in
// state=awaiting_approval, blocking on <run-dir>/approval.txt. These tools
// write that file atomically (same shape as job_send_input → input.txt); the
// agent's request_approval poll picks it up and either proceeds ("approve") or
// aborts without acting (anything else).

type approveJobInput struct {
	Ref string `json:"ref"`
}

func approveJobTool(store *jobs.Store) Tool {
	return Tool{
		Name: "approve_job",
		Description: "Approve a job awaiting your sign-off before an IRREVERSIBLE action " +
			"(state=awaiting_approval) - merging a PR, sending a Slack/email message, or " +
			"writing back to Notion. Use when the user says \"approve PR 583\", \"yes, merge " +
			"it\", \"go ahead and send it\". Ref is the run-id Jarvis announced or a substring " +
			"of the job's question. The agent proceeds on its next poll.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{"type": "string", "description": "run-id (preferred) or substring of the job's question"},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in approveJobInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			j, err := resolveApprovalJob(store, strings.TrimSpace(in.Ref))
			if err != nil {
				return "", err
			}
			if err := writeApprovalVerdict(store, j.RunID, "approve"); err != nil {
				return "", err
			}
			return fmt.Sprintf("Approved - %s will proceed.", approvalWhat(j)), nil
		},
	}
}

type rejectJobInput struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

func rejectJobTool(store *jobs.Store) Tool {
	return Tool{
		Name: "reject_job",
		Description: "Reject a job awaiting approval (state=awaiting_approval) so it STOPS " +
			"without performing the irreversible action. Use when the user says \"reject PR " +
			"583\", \"no, don't send that\", \"cancel the merge\". Optional reason is recorded.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref":    map[string]interface{}{"type": "string", "description": "run-id (preferred) or substring of the job's question"},
				"reason": map[string]interface{}{"type": "string", "description": "optional reason for the rejection"},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in rejectJobInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			j, err := resolveApprovalJob(store, strings.TrimSpace(in.Ref))
			if err != nil {
				return "", err
			}
			verdict := "reject"
			if reason := strings.TrimSpace(in.Reason); reason != "" {
				verdict = "reject: " + reason
			}
			if err := writeApprovalVerdict(store, j.RunID, verdict); err != nil {
				return "", err
			}
			return fmt.Sprintf("Rejected - %s will not proceed.", approvalWhat(j)), nil
		},
	}
}

// resolveApprovalJob finds a job by ref and verifies it's awaiting approval.
func resolveApprovalJob(store *jobs.Store, ref string) (*jobs.Job, error) {
	if ref == "" {
		return nil, fmt.Errorf("ref is required")
	}
	j, err := resolveJobRef(store, ref)
	if err != nil {
		return nil, err
	}
	if j.State != jobs.StateAwaitingApproval {
		return nil, fmt.Errorf("job %s is not awaiting approval (state=%s)", j.RunID, j.State)
	}
	return j, nil
}

// writeApprovalVerdict atomically writes the verdict to the run-dir's
// approval.txt; the paused agent's request_approval tool reads it next poll.
func writeApprovalVerdict(store *jobs.Store, runID, verdict string) error {
	runDir := jobs.RunDir(store.Profile(), runID)
	tmp := fmt.Sprintf("%s/.approval.txt.tmp", runDir)
	if err := os.WriteFile(tmp, []byte(verdict), 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, runDir+"/approval.txt"); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func approvalWhat(j *jobs.Job) string {
	switch {
	case j.ApprovalPayload != "":
		return j.ApprovalPayload
	case j.ApprovalAction != "":
		return "the " + j.ApprovalAction
	default:
		return "run " + j.RunID
	}
}
