package brain

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ryanlitalien/aida/internal/runs"
)

// trajectoriesDirName is the brain subdirectory holding
// per-invocation tool-calling trajectories. Source of truth lives
// in the brain git repo so trajectories accumulate across machines
// and survive brain.db rebuilds.
const trajectoriesDirName = "trajectories"

// TrajectoryMessage is one turn in a ChatML / OpenAI-compatible
// tool-calling conversation. The shape is what RL training
// pipelines like Nous Research's Atropos consume - keeping the
// canonical four-role pattern (system / user / assistant / tool)
// lets these files feed into trajectory-grading and
// smaller-model fine-tuning workflows without a transform step.
type TrajectoryMessage struct {
	Role       string           `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content    string           `json:"content,omitempty"`
	ToolCalls  []TrajectoryCall `json:"tool_calls,omitempty"`   // populated on assistant turns that invoke tools
	ToolCallID string           `json:"tool_call_id,omitempty"` // populated on tool turns
	Name       string           `json:"name,omitempty"`         // tool name on tool turns
}

// TrajectoryCall is one tool invocation embedded in an
// assistant turn. Arguments are stored as a string (the raw
// JSON the agent emitted) rather than a structured object;
// downstream consumers can re-parse if needed.
type TrajectoryCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// TrajectoryReward captures the aida eval signal in a shape
// suitable for RL reward modeling. AggregateVerdict is the
// pass/warn/fail bucket; per-reviewer scores let downstream code
// weight specialists differently.
type TrajectoryReward struct {
	AggregateVerdict string             `json:"aggregate_verdict,omitempty"`
	ReviewerScores   map[string]float64 `json:"reviewer_scores,omitempty"`
}

// TrajectoryMetadata is bookkeeping that's useful for filtering
// trajectories during analysis or training (cost, latency,
// strategy taken). Fields are optional - zero values are skipped
// in JSON output via omitempty.
type TrajectoryMetadata struct {
	DurationMs   int64   `json:"duration_ms,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	InputTokens  int     `json:"input_tokens,omitempty"`
	OutputTokens int     `json:"output_tokens,omitempty"`
	Strategy     string  `json:"strategy,omitempty"` // "lookup" | "query" | "agent" | etc.
}

// Trajectory is the durable export of one aida invocation in a
// format compatible with tool-calling RL training pipelines.
// Files live under brain/trajectories/<run_id>.json and sync
// across machines via the brain git repo, like eval-runs.
type Trajectory struct {
	ID        string              `json:"id"`
	Timestamp time.Time           `json:"timestamp"`
	Model     string              `json:"model,omitempty"`
	Profile   string              `json:"profile,omitempty"`
	Question  string              `json:"question"`
	Messages  []TrajectoryMessage `json:"messages"`
	Reward    TrajectoryReward    `json:"reward,omitempty"`
	Metadata  TrajectoryMetadata  `json:"metadata,omitempty"`
}

// BuildTrajectory converts a aida run record (plus optional
// eval signal) into a portable Trajectory. Both deterministic
// 6-step pipeline runs and agent-mode runs are supported:
//
//   - deterministic runs produce user → assistant (the synthesizer
//     output is the assistant turn);
//   - agent runs produce user → (assistant→tool)* → final assistant,
//     reflecting the full tool-use loop captured in
//     run.AgentToolCalls.
//
// The system prompt is intentionally NOT included today - it is
// large, model-specific, and rebuildable from the source code at
// the run's git commit. Including it would inflate every trajectory
// file by 3-5kb without payoff for v1 analysis. Add it later if a
// fine-tuning workflow needs it.
//
// Returns nil if run is nil. evalRun may be nil; the trajectory
// is still produced, just without a Reward signal.
func BuildTrajectory(run *runs.Run, evalRun *EvalRun, model string) *Trajectory {
	if run == nil {
		return nil
	}
	t := &Trajectory{
		ID:        run.ID,
		Timestamp: run.StartedAt.UTC(),
		Model:     model,
		Profile:   run.Profile,
		Question:  run.Question,
	}

	// User turn always present.
	t.Messages = append(t.Messages, TrajectoryMessage{
		Role:    "user",
		Content: run.Question,
	})

	// Agent tool-call turns: each AgentToolCallRun becomes one
	// assistant turn with a tool_call followed by a tool turn
	// carrying the result.
	for _, tc := range run.AgentToolCalls {
		callID := fmt.Sprintf("call-%d", tc.Turn)
		t.Messages = append(t.Messages, TrajectoryMessage{
			Role: "assistant",
			ToolCalls: []TrajectoryCall{{
				ID:        callID,
				Name:      tc.Tool,
				Arguments: tc.Input,
			}},
		})
		// Tool turn carries either the output or the error
		// verbatim. Errors get an "[error]" prefix so consumers
		// can distinguish without parsing the original Tool
		// adapter's error format.
		content := tc.Output
		if tc.Error != "" {
			content = "[error] " + tc.Error
		}
		t.Messages = append(t.Messages, TrajectoryMessage{
			Role:       "tool",
			Name:       tc.Tool,
			ToolCallID: callID,
			Content:    content,
		})
	}

	// Final assistant turn - the answer that aida actually
	// returned to the user. For deterministic runs this is the
	// synthesizer output; for agent runs it's the agent's last
	// reply after the tool loop ended.
	if run.Answer != "" {
		t.Messages = append(t.Messages, TrajectoryMessage{
			Role:    "assistant",
			Content: run.Answer,
		})
	}

	// Reward signal from eval - only populated when an EvalRun
	// was successfully recorded for this run id. Reviewer scores
	// preserved per-reviewer so downstream weighting is possible.
	if evalRun != nil {
		t.Reward.AggregateVerdict = evalRun.AggregateVerdict
		if len(evalRun.Reviewers) > 0 {
			scores := make(map[string]float64, len(evalRun.Reviewers))
			for _, r := range evalRun.Reviewers {
				scores[r.Reviewer] = r.Score
			}
			t.Reward.ReviewerScores = scores
		}
	}

	// Metadata from the run record + tracing data.
	t.Metadata.DurationMs = run.TotalMs
	t.Metadata.Strategy = run.Strategy
	if run.Tracing != nil {
		t.Metadata.CostUSD = run.Tracing.TotalCost
		t.Metadata.InputTokens = run.Tracing.TotalIn
		t.Metadata.OutputTokens = run.Tracing.TotalOut
	}

	return t
}

// TrajectoriesDir returns the absolute path to the trajectories
// directory under the given brain root.
func TrajectoriesDir(brainPath string) string {
	return filepath.Join(brainPath, trajectoriesDirName)
}

// WriteTrajectory persists a Trajectory to disk. Caller is
// responsible for ensuring the brain root exists; the trajectories
// subdir is auto-created on first write.
func WriteTrajectory(brainPath string, t *Trajectory) error {
	if t == nil {
		return fmt.Errorf("WriteTrajectory: nil trajectory")
	}
	if t.ID == "" {
		return fmt.Errorf("WriteTrajectory: empty ID")
	}
	dir := TrajectoriesDir(brainPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if t.Timestamp.IsZero() {
		t.Timestamp = time.Now().UTC()
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trajectory: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, t.ID+".json"), data, 0644)
}

// ReadTrajectory reads a single Trajectory by run id. Returns
// the wrapped os.ErrNotExist when the file is missing.
func ReadTrajectory(brainPath, id string) (*Trajectory, error) {
	if id == "" {
		return nil, fmt.Errorf("ReadTrajectory: empty id")
	}
	path := filepath.Join(TrajectoriesDir(brainPath), id+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Trajectory
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &t, nil
}
