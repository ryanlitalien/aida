package brain

import (
	"os"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/runs"
)

func TestBuildTrajectory_NilRunReturnsNil(t *testing.T) {
	if got := BuildTrajectory(nil, nil, ""); got != nil {
		t.Errorf("expected nil for nil run, got %+v", got)
	}
}

func TestBuildTrajectory_DeterministicRunSingleAssistantTurn(t *testing.T) {
	r := &runs.Run{
		ID:        "20260505-123000-test",
		Question:  "what are my open PRs?",
		Profile:   "home",
		Strategy:  "query",
		Answer:    "You have 2 open PRs.",
		StartedAt: time.Date(2026, 5, 5, 12, 30, 0, 0, time.UTC),
		TotalMs:   3500,
	}
	tj := BuildTrajectory(r, nil, "claude-haiku-4-5")
	if tj == nil {
		t.Fatal("BuildTrajectory returned nil")
	}
	if tj.ID != r.ID {
		t.Errorf("ID = %q, want %q", tj.ID, r.ID)
	}
	if tj.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", tj.Model)
	}
	if len(tj.Messages) != 2 {
		t.Fatalf("expected 2 messages (user + assistant), got %d", len(tj.Messages))
	}
	if tj.Messages[0].Role != "user" {
		t.Errorf("first message role = %q, want user", tj.Messages[0].Role)
	}
	if tj.Messages[1].Role != "assistant" {
		t.Errorf("last message role = %q, want assistant", tj.Messages[1].Role)
	}
	if tj.Messages[1].Content != "You have 2 open PRs." {
		t.Errorf("assistant content mismatch: got %q", tj.Messages[1].Content)
	}
	if tj.Metadata.Strategy != "query" {
		t.Errorf("Strategy metadata = %q, want query", tj.Metadata.Strategy)
	}
}

func TestBuildTrajectory_AgentRunMultiTurn(t *testing.T) {
	r := &runs.Run{
		ID:       "20260505-test-agent",
		Question: "list my PRs",
		Strategy: "agent",
		Answer:   "Found 3 PRs.",
		AgentToolCalls: []runs.AgentToolCallRun{
			{Turn: 1, Tool: "github", Input: `{"q":"list"}`, Output: `[{"id":"pr-1"}]`},
			{Turn: 2, Tool: "github", Input: `{"q":"more"}`, Output: `[{"id":"pr-2"},{"id":"pr-3"}]`},
		},
	}
	tj := BuildTrajectory(r, nil, "")
	// user + (assistant+tool)*2 + final assistant = 6
	if len(tj.Messages) != 6 {
		t.Fatalf("expected 6 messages, got %d: %+v", len(tj.Messages), tj.Messages)
	}
	wantRoles := []string{"user", "assistant", "tool", "assistant", "tool", "assistant"}
	for i, want := range wantRoles {
		if tj.Messages[i].Role != want {
			t.Errorf("message[%d].Role = %q, want %q", i, tj.Messages[i].Role, want)
		}
	}
	// Tool calls have stable IDs that match their tool turns.
	if len(tj.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant turn 1 should have 1 tool call")
	}
	if tj.Messages[1].ToolCalls[0].ID != tj.Messages[2].ToolCallID {
		t.Errorf("tool_call_id mismatch: assistant=%q tool=%q",
			tj.Messages[1].ToolCalls[0].ID, tj.Messages[2].ToolCallID)
	}
	if tj.Messages[1].ToolCalls[0].Arguments != `{"q":"list"}` {
		t.Errorf("arguments not preserved verbatim")
	}
}

func TestBuildTrajectory_ErrorTurnPrefixedWithBracket(t *testing.T) {
	r := &runs.Run{
		ID:       "id1",
		Question: "q",
		AgentToolCalls: []runs.AgentToolCallRun{
			{Turn: 1, Tool: "github", Input: "{}", Error: "rate limited"},
		},
		// Answer left empty - agent crashed before producing one,
		// which is exactly when the error tool turn is most useful.
	}
	tj := BuildTrajectory(r, nil, "")
	// user + assistant(tool_call) + tool(error) = 3
	if len(tj.Messages) != 3 {
		t.Fatalf("expected 3 messages (user/assistant/tool), got %d: %+v",
			len(tj.Messages), tj.Messages)
	}
	toolMsg := tj.Messages[2]
	if toolMsg.Role != "tool" {
		t.Fatalf("third message role = %q, want tool", toolMsg.Role)
	}
	if toolMsg.Content != "[error] rate limited" {
		t.Errorf("error tool content = %q, want '[error] rate limited'", toolMsg.Content)
	}
}

func TestBuildTrajectory_PopulatesRewardFromEvalRun(t *testing.T) {
	r := &runs.Run{ID: "id1", Question: "q", Answer: "a"}
	er := &EvalRun{
		AggregateVerdict: "fail",
		Reviewers: []eval.ReviewRecord{
			{Reviewer: "citation", Score: 0.5},
			{Reviewer: "scope", Score: 1.0},
		},
	}
	tj := BuildTrajectory(r, er, "")
	if tj.Reward.AggregateVerdict != "fail" {
		t.Errorf("AggregateVerdict = %q, want fail", tj.Reward.AggregateVerdict)
	}
	if got := tj.Reward.ReviewerScores["citation"]; got != 0.5 {
		t.Errorf("citation score = %v, want 0.5", got)
	}
	if got := tj.Reward.ReviewerScores["scope"]; got != 1.0 {
		t.Errorf("scope score = %v, want 1.0", got)
	}
}

func TestBuildTrajectory_NoEvalRunMeansNoReward(t *testing.T) {
	r := &runs.Run{ID: "id1", Question: "q", Answer: "a"}
	tj := BuildTrajectory(r, nil, "")
	if tj.Reward.AggregateVerdict != "" {
		t.Errorf("expected empty Reward when no eval, got %+v", tj.Reward)
	}
	if tj.Reward.ReviewerScores != nil {
		t.Errorf("expected nil ReviewerScores, got %+v", tj.Reward.ReviewerScores)
	}
}

func TestWriteAndReadTrajectory_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	original := &Trajectory{
		ID:       "20260505-test",
		Model:    "claude-haiku",
		Profile:  "home",
		Question: "hi",
		Messages: []TrajectoryMessage{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	}
	if err := WriteTrajectory(tmp, original); err != nil {
		t.Fatalf("WriteTrajectory: %v", err)
	}
	got, err := ReadTrajectory(tmp, original.ID)
	if err != nil {
		t.Fatalf("ReadTrajectory: %v", err)
	}
	if got.ID != original.ID || got.Model != original.Model || len(got.Messages) != 2 {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("Timestamp should be auto-filled on write")
	}
}

func TestWriteTrajectory_RejectsEmptyID(t *testing.T) {
	if err := WriteTrajectory(t.TempDir(), &Trajectory{ID: ""}); err == nil {
		t.Errorf("expected error for empty ID")
	}
}

func TestWriteTrajectory_RejectsNil(t *testing.T) {
	if err := WriteTrajectory(t.TempDir(), nil); err == nil {
		t.Errorf("expected error for nil trajectory")
	}
}

func TestReadTrajectory_MissingFile(t *testing.T) {
	_, err := ReadTrajectory(t.TempDir(), "no-such-id")
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}
