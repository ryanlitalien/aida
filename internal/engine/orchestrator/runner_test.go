package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// scriptLLM is a scriptable model.LLM for tests. It returns the next
// queued Response on each Complete call and records every Request it
// sees.
type scriptLLM struct {
	name      string
	modelID   string
	responses []model.Response
	calls     []model.Request
	idx       int
}

func (s *scriptLLM) Provider() string { return s.name }
func (s *scriptLLM) Model() string    { return s.modelID }
func (s *scriptLLM) Complete(ctx context.Context, req model.Request) (*model.Response, error) {
	s.calls = append(s.calls, req)
	if s.idx >= len(s.responses) {
		return nil, fmt.Errorf("script exhausted after %d calls", len(s.responses))
	}
	r := s.responses[s.idx]
	s.idx++
	return &r, nil
}

func echoTool(name string) Tool {
	return &FuncTool{
		TName:   name,
		TDesc:   "echo tool",
		TSchema: map[string]interface{}{"type": "object"},
		TFunc: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "echo:" + string(input), nil
		},
	}
}

func countingTool(name string, counter *int64) Tool {
	return &FuncTool{
		TName:   name,
		TDesc:   "counts calls",
		TSchema: map[string]interface{}{"type": "object"},
		TFunc: func(ctx context.Context, input json.RawMessage) (string, error) {
			atomic.AddInt64(counter, 1)
			return "ok", nil
		},
	}
}

func brokenTool(name string) Tool {
	return &FuncTool{
		TName:   name,
		TDesc:   "always fails",
		TSchema: map[string]interface{}{"type": "object"},
		TFunc: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "", errors.New("boom")
		},
	}
}

// TestRunnerBasicEndTurn covers the simplest path: model returns text
// on the first turn; Runner returns it.
func TestRunnerBasicEndTurn(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopEndTurn,
				Blocks:     []model.Block{model.TextBlock("the answer is 42")},
			},
		},
	}
	agent := &Agent{
		Name:         "simple",
		Instructions: "you are simple",
		Model:        llm,
	}
	var r Runner
	res, err := r.Run(context.Background(), agent, "what is the answer?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "the answer is 42" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if got := res.FinalAgent.Name; got != "simple" {
		t.Errorf("FinalAgent = %q", got)
	}
}

// TestRunnerToolUseThenEndTurn covers the core tool-use loop: one
// turn returning tool_use, one turn returning end_turn.
func TestRunnerToolUseThenEndTurn(t *testing.T) {
	var counter int64
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.TextBlock("let me check"),
					model.ToolUseBlock("tu_1", "probe", json.RawMessage(`{"x":1}`)),
				},
			},
			{
				StopReason: model.StopEndTurn,
				Blocks:     []model.Block{model.TextBlock("done")},
			},
		},
	}
	agent := &Agent{
		Name:  "tooluser",
		Model: llm,
		Tools: []Tool{countingTool("probe", &counter)},
	}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt64(&counter); got != 1 {
		t.Errorf("tool calls = %d, want 1", got)
	}
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	if res.FinalOutput != "done" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Tool != "probe" {
		t.Errorf("ToolCalls = %+v", res.ToolCalls)
	}
}

// TestRunnerCircuitBreaker verifies that after maxFailures consecutive
// errors for the same tool, subsequent invocations are short-circuited.
func TestRunnerCircuitBreaker(t *testing.T) {
	// Script: five consecutive tool_use turns calling the broken
	// tool, then one end_turn. After 3 failures, the tool should be
	// disabled; the model still "calls" it but gets a disabled
	// message back.
	llm := &scriptLLM{}
	for i := 0; i < 5; i++ {
		llm.responses = append(llm.responses, model.Response{
			StopReason: model.StopToolUse,
			Blocks: []model.Block{
				model.ToolUseBlock(fmt.Sprintf("tu_%d", i), "broken", json.RawMessage(`{}`)),
			},
		})
	}
	llm.responses = append(llm.responses, model.Response{
		StopReason: model.StopEndTurn,
		Blocks:     []model.Block{model.TextBlock("gave up")},
	})

	agent := &Agent{
		Name:  "breaker",
		Model: llm,
		Tools: []Tool{brokenTool("broken")},
	}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// We expect exactly 5 recorded tool calls: 3 "boom" errors and 2
	// "disabled" short-circuits. Only the first 3 should have an
	// underlying "boom" error; the later calls should carry the
	// circuit-breaker message.
	if len(res.ToolCalls) != 5 {
		t.Fatalf("ToolCalls count = %d, want 5 (got: %+v)", len(res.ToolCalls), res.ToolCalls)
	}
	boomCount := 0
	disabledCount := 0
	for _, tc := range res.ToolCalls {
		switch {
		case tc.Error == "boom":
			boomCount++
		case strings.Contains(tc.Error, "disabled"):
			disabledCount++
		}
	}
	if boomCount != 3 {
		t.Errorf("expected 3 boom errors, got %d", boomCount)
	}
	if disabledCount != 2 {
		t.Errorf("expected 2 disabled calls, got %d", disabledCount)
	}
}

// TestRunnerUnknownTool makes the model "call" a tool that isn't in
// the agent's palette; we expect an error tool_result to be fed back.
func TestRunnerUnknownTool(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_1", "ghost", json.RawMessage(`{}`)),
				},
			},
			{
				StopReason: model.StopEndTurn,
				Blocks:     []model.Block{model.TextBlock("ok")},
			},
		},
	}
	agent := &Agent{Name: "a", Model: llm}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool call count = %d", len(res.ToolCalls))
	}
	if !strings.Contains(res.ToolCalls[0].Error, "unknown tool") {
		t.Errorf("expected unknown-tool error, got %q", res.ToolCalls[0].Error)
	}
}

// TestRunnerMaxTurnsFieldIgnored verifies that the turn cap was removed:
// Agent.MaxTurns no longer bounds the loop. Only a wall-clock deadline
// or a cost ceiling (WithDeadline / WithCostLimit) can stop a run before
// the model ends its turn. A deliberately low MaxTurns must not truncate
// a longer, well-behaved loop, and must not produce the old "reached
// maximum turns" fabricated answer.
func TestRunnerMaxTurnsFieldIgnored(t *testing.T) {
	llm := &scriptLLM{}
	for i := 0; i < 5; i++ {
		llm.responses = append(llm.responses, model.Response{
			StopReason: model.StopToolUse,
			Blocks: []model.Block{
				model.ToolUseBlock(fmt.Sprintf("tu_%d", i), "echo", json.RawMessage(`{}`)),
			},
		})
	}
	llm.responses = append(llm.responses, model.Response{
		StopReason: model.StopEndTurn,
		Blocks:     []model.Block{model.TextBlock("done")},
	})
	agent := &Agent{
		Name:     "unbounded",
		Model:    llm,
		Tools:    []Tool{echoTool("echo")},
		MaxTurns: 2, // deliberately low; must be ignored entirely
	}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 6 {
		t.Errorf("Turns = %d, want 6 (MaxTurns=2 must not cap the loop)", res.Turns)
	}
	if res.Termination != TerminationFinalResponse {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationFinalResponse)
	}
	if res.FinalOutput != "done" {
		t.Errorf("FinalOutput = %q, want %q", res.FinalOutput, "done")
	}
}

// TestDriveAgentTerminationClassification is a table-driven check of the
// stop-reason -> Termination mapping described in the Termination doc
// comment. Calls driveAgent directly (same package) so it can inspect
// Termination even on the branches where Run() would return a nil
// RunResult alongside a non-nil error.
func TestDriveAgentTerminationClassification(t *testing.T) {
	tests := []struct {
		name       string
		resp       model.Response
		wantTerm   Termination
		wantErr    bool
		wantOutput string
	}{
		{
			name:       "end_turn is final_response",
			resp:       model.Response{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("42")}},
			wantTerm:   TerminationFinalResponse,
			wantOutput: "42",
		},
		{
			// The regression this whole change closes: a truncated,
			// tool-call-free response must never be promoted to a
			// final answer.
			name:     "max_tokens with zero tool calls is not final_response",
			resp:     model.Response{StopReason: model.StopMaxTokens, Blocks: []model.Block{model.TextBlock("cut off mid-")}},
			wantTerm: TerminationModelTokenLimit,
		},
		{
			name:     "stop_sequence with zero tool calls is an error, not a completion",
			resp:     model.Response{StopReason: model.StopSequence, Blocks: []model.Block{model.TextBlock("...")}},
			wantTerm: TerminationError,
			wantErr:  true,
		},
		{
			name:     "provider-specific stop reason with zero tool calls is an error",
			resp:     model.Response{StopReason: model.StopOther},
			wantTerm: TerminationError,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			llm := &scriptLLM{responses: []model.Response{tt.resp}}
			agent := &Agent{Name: "a", Model: llm}
			result := &RunResult{}
			options := runOptions{
				session: NewMemorySession("t"),
				tracer:  NoopTracer{},
				cancel:  func() {},
			}
			var r Runner
			out, handoff, err := r.driveAgent(context.Background(), agent, options, result)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if handoff != nil {
				t.Fatalf("unexpected handoff signal")
			}
			if result.Termination != tt.wantTerm {
				t.Errorf("Termination = %q, want %q", result.Termination, tt.wantTerm)
			}
			if tt.wantTerm == TerminationFinalResponse {
				if out != tt.wantOutput {
					t.Errorf("output = %q, want %q", out, tt.wantOutput)
				}
			} else if out != "" {
				t.Errorf("output = %q, want empty (no fabricated final answer)", out)
			}
		})
	}
}

// TestLatchTerminationFirstWriteWins verifies the ordering guarantee a
// bound trip depends on: once a reason is latched, nothing later --
// including an error caused by the trip's own context cancellation --
// can overwrite it.
func TestLatchTerminationFirstWriteWins(t *testing.T) {
	result := &RunResult{}
	latchTermination(result, TerminationWallTimeLimit)
	latchTermination(result, TerminationError) // simulates a later error
	if result.Termination != TerminationWallTimeLimit {
		t.Errorf("Termination = %q, want %q (first latch must win)", result.Termination, TerminationWallTimeLimit)
	}
}

// TestRunnerWallClockDeadlineTripped verifies that a deadline already in
// the past stops the run before it makes a single model call, and that
// no answer is fabricated.
func TestRunnerWallClockDeadlineTripped(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("should never be produced")}},
		},
	}
	agent := &Agent{Name: "slow", Model: llm}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go", WithDeadline(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Termination != TerminationWallTimeLimit {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationWallTimeLimit)
	}
	if res.FinalOutput != "" {
		t.Errorf("FinalOutput = %q, want empty (no fabricated answer on a bound trip)", res.FinalOutput)
	}
	if len(llm.calls) != 0 {
		t.Errorf("model called %d times, want 0 (deadline already passed before the first call)", len(llm.calls))
	}
}

// TestRunnerCostLimitTripped verifies that an accumulated-cost ceiling
// stops the run as soon as a completion's cost pushes the running total
// over the limit, before that turn's tool calls are dispatched.
func TestRunnerCostLimitTripped(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks:     []model.Block{model.ToolUseBlock("tu_1", "echo", json.RawMessage(`{}`))},
				Usage:      model.Usage{InputTokens: 1000, OutputTokens: 1000},
			},
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("done")}},
		},
	}
	var toolCalled int64
	agent := &Agent{Name: "pricey", Model: llm, Tools: []Tool{countingTool("echo", &toolCalled)}}
	costFn := func(u model.Usage) float64 { return float64(u.InputTokens+u.OutputTokens) * 0.001 }

	var r Runner
	res, err := r.Run(context.Background(), agent, "go", WithCostLimit(1.0, costFn))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Termination != TerminationCostLimit {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationCostLimit)
	}
	if got := atomic.LoadInt64(&toolCalled); got != 0 {
		t.Errorf("tool was dispatched %d times, want 0 (cost ceiling trips before dispatch)", got)
	}
	if len(llm.calls) != 1 {
		t.Errorf("model called %d times, want 1 (cost ceiling should stop before the second call)", len(llm.calls))
	}
}

// ---------------------------------------------------------------------
// submit_result: the terminal tool that lets an agent declare a
// verified outcome instead of relying on free prose.
// ---------------------------------------------------------------------

func TestParseSubmitResultRequiresFailureCodeWhenNotAchieved(t *testing.T) {
	_, err := ParseSubmitResult(json.RawMessage(`{"outcome":"not_achieved","summary":"tried but couldn't"}`))
	if err == nil {
		t.Fatal("expected error for not_achieved without failure_code")
	}
}

func TestParseSubmitResultAchievedNeedsNoFailureCode(t *testing.T) {
	sr, err := ParseSubmitResult(json.RawMessage(`{"outcome":"achieved","summary":"done"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sr.Outcome != OutcomeAchieved {
		t.Errorf("Outcome = %q, want %q", sr.Outcome, OutcomeAchieved)
	}
}

func TestParseSubmitResultRejectsUnknownOutcome(t *testing.T) {
	_, err := ParseSubmitResult(json.RawMessage(`{"outcome":"maybe","summary":"?"}`))
	if err == nil {
		t.Fatal("expected error for an outcome that is neither achieved nor not_achieved")
	}
}

func TestParseSubmitResultRejectsMalformedJSON(t *testing.T) {
	_, err := ParseSubmitResult(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestRunnerSubmitResultEndsRunWithStructuredResult verifies that a
// valid submit_result call ends the run immediately with
// TerminationFinalResponse and attaches the parsed payload to
// RunResult.Result.
func TestRunnerSubmitResultEndsRunWithStructuredResult(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_1", SubmitResultToolName, json.RawMessage(`{"outcome":"achieved","summary":"upgraded the server"}`)),
				},
			},
		},
	}
	agent := &Agent{Name: "verified", Model: llm}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Termination != TerminationFinalResponse {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationFinalResponse)
	}
	if res.Result == nil {
		t.Fatal("Result is nil, want a structured SubmitResult")
	}
	if res.Result.Outcome != OutcomeAchieved || res.Result.Summary != "upgraded the server" {
		t.Errorf("Result = %+v", res.Result)
	}
	if res.FinalOutput != "upgraded the server" {
		t.Errorf("FinalOutput = %q, want %q", res.FinalOutput, "upgraded the server")
	}
}

// TestRunnerSubmitResultMalformedInputLetsModelRetry verifies that an
// invalid submit_result call (not_achieved with no failure_code) does
// NOT end the run -- it comes back as an ordinary failed tool call the
// model can act on, exactly like any other tool validation failure,
// and a corrected retry then ends the run with the structured result.
func TestRunnerSubmitResultMalformedInputLetsModelRetry(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_1", SubmitResultToolName, json.RawMessage(`{"outcome":"not_achieved","summary":"couldn't do it"}`)),
				},
			},
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_2", SubmitResultToolName, json.RawMessage(`{"outcome":"not_achieved","summary":"couldn't do it","failure_code":"backup_gate_closed"}`)),
				},
			},
		},
	}
	submitTool := &FuncTool{
		TName:   SubmitResultToolName,
		TDesc:   "declare the final outcome",
		TSchema: map[string]interface{}{"type": "object"},
		TFunc: func(ctx context.Context, input json.RawMessage) (string, error) {
			if _, err := ParseSubmitResult(input); err != nil {
				return "", err
			}
			return "recorded", nil
		},
	}
	agent := &Agent{Name: "retrier", Model: llm, Tools: []Tool{submitTool}}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Termination != TerminationFinalResponse {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationFinalResponse)
	}
	if res.Result == nil || res.Result.FailureCode != "backup_gate_closed" {
		t.Errorf("Result = %+v, want FailureCode=backup_gate_closed", res.Result)
	}
	foundMalformedAttempt := false
	for _, tc := range res.ToolCalls {
		if tc.Tool == SubmitResultToolName && tc.Error != "" {
			foundMalformedAttempt = true
		}
	}
	if !foundMalformedAttempt {
		t.Errorf("expected a recorded failed submit_result attempt before the successful retry, got: %+v", res.ToolCalls)
	}
}

// TestRunnerNoSubmitResultCallLeavesResultNil verifies that a run which
// ends on ordinary free text (no submit_result call) carries no
// structured Result -- downstream must not synthesize one from
// FinalOutput.
func TestRunnerNoSubmitResultCallLeavesResultNil(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("the answer is 42")}},
		},
	}
	agent := &Agent{Name: "plain", Model: llm}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Result != nil {
		t.Errorf("Result = %+v, want nil (no submit_result call was made)", res.Result)
	}
	if res.Termination != TerminationFinalResponse {
		t.Errorf("Termination = %q, want %q", res.Termination, TerminationFinalResponse)
	}
}

// TestRunnerInputGuardrailFails aborts the run before any LLM call.
func TestRunnerInputGuardrailFails(t *testing.T) {
	called := int64(0)
	llm := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("hi")}},
		},
	}
	agent := &Agent{
		Name:  "guarded",
		Model: llm,
		InputGuardrails: []Guardrail{
			&GuardrailFunc{
				GName: "ban-hello",
				GFn: func(ctx context.Context, text string) error {
					atomic.AddInt64(&called, 1)
					if strings.Contains(text, "hello") {
						return errors.New("banned word")
					}
					return nil
				},
			},
		},
	}
	var r Runner
	_, err := r.Run(context.Background(), agent, "hello world")
	if err == nil {
		t.Fatal("expected guardrail violation, got nil")
	}
	var g *GuardrailViolation
	if !errors.As(err, &g) {
		t.Fatalf("expected GuardrailViolation, got %T: %v", err, err)
	}
	if g.Guardrail != "ban-hello" {
		t.Errorf("Guardrail name = %q", g.Guardrail)
	}
	if atomic.LoadInt64(&called) != 1 {
		t.Errorf("guardrail called %d times", called)
	}
	if len(llm.calls) != 0 {
		t.Error("LLM should not have been called when input guardrail fails")
	}
}

// TestRunnerOutputGuardrailFails rejects the final text.
func TestRunnerOutputGuardrailFails(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("forbidden text")}},
		},
	}
	agent := &Agent{
		Name:  "guarded",
		Model: llm,
		OutputGuardrails: []Guardrail{
			&GuardrailFunc{
				GName: "ban-forbidden",
				GFn: func(ctx context.Context, text string) error {
					if strings.Contains(text, "forbidden") {
						return errors.New("nope")
					}
					return nil
				},
			},
		},
	}
	var r Runner
	_, err := r.Run(context.Background(), agent, "go")
	if err == nil {
		t.Fatal("expected guardrail violation")
	}
	if !strings.Contains(err.Error(), "ban-forbidden") {
		t.Errorf("err = %v", err)
	}
}

// TestRunnerHandoff: agent A decides to hand off to agent B via a
// handoff tool, B produces the final answer.
func TestRunnerHandoff(t *testing.T) {
	bLLM := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("B answers")}},
		},
	}
	b := &Agent{Name: "B", Model: bLLM}

	aLLM := &scriptLLM{
		responses: []model.Response{
			{
				StopReason: model.StopToolUse,
				Blocks: []model.Block{
					model.ToolUseBlock("tu_1", "handoff_to_B", json.RawMessage(`{"context":"please solve"}`)),
				},
			},
		},
	}
	a := &Agent{
		Name:     "A",
		Model:    aLLM,
		Handoffs: []Handoff{{Target: b}},
	}

	var r Runner
	res, err := r.Run(context.Background(), a, "start")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "B answers" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if res.FinalAgent.Name != "B" {
		t.Errorf("FinalAgent = %q, want B", res.FinalAgent.Name)
	}
	if len(res.HandoffPath) != 2 || res.HandoffPath[0] != "A" || res.HandoffPath[1] != "B" {
		t.Errorf("HandoffPath = %v", res.HandoffPath)
	}
	// Turns should include A's turn and B's turn.
	if res.Turns != 2 {
		t.Errorf("Turns = %d, want 2", res.Turns)
	}
	// B should have seen the handoff context as its first user message.
	if len(bLLM.calls) != 1 {
		t.Fatalf("B should have been called once")
	}
	// The session passed to B should contain the handoff context as a
	// user message near the end.
	msgs := bLLM.calls[0].Messages
	foundContext := false
	for _, m := range msgs {
		if m.Role != model.RoleUser {
			continue
		}
		for _, b := range m.Blocks {
			if b.Type == model.BlockText && strings.Contains(b.Text, "please solve") {
				foundContext = true
			}
		}
	}
	if !foundContext {
		t.Errorf("handoff context not passed to target agent; got messages: %+v", msgs)
	}
}

// TestRunnerMaxHandoffsExceeded: A hands off to A hands off to A -
// the Runner must cap the chain.
func TestRunnerMaxHandoffsExceeded(t *testing.T) {
	var a *Agent
	a = &Agent{
		Name: "loop",
	}
	llm := &scriptLLM{}
	// Every turn, A hands off to itself.
	for i := 0; i < 10; i++ {
		llm.responses = append(llm.responses, model.Response{
			StopReason: model.StopToolUse,
			Blocks: []model.Block{
				model.ToolUseBlock(fmt.Sprintf("tu_%d", i), "handoff_to_loop", json.RawMessage(`{"context":"again"}`)),
			},
		})
	}
	a.Model = llm
	a.Handoffs = []Handoff{{Target: a}}

	var r Runner
	_, err := r.Run(context.Background(), a, "start", WithMaxHandoffs(3))
	if err == nil {
		t.Fatal("expected max handoffs error")
	}
	if !strings.Contains(err.Error(), "max handoffs") {
		t.Errorf("err = %v", err)
	}
}

// TestRunnerContextCompression verifies that once turn >= 8 and
// messages > 15, compression engages. We drive a very long scripted
// loop and assert the session shrinks.
func TestRunnerContextCompression(t *testing.T) {
	llm := &scriptLLM{}
	for i := 0; i < 10; i++ {
		llm.responses = append(llm.responses, model.Response{
			StopReason: model.StopToolUse,
			Blocks: []model.Block{
				model.ToolUseBlock(fmt.Sprintf("tu_%d", i), "echo", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))),
			},
		})
	}
	llm.responses = append(llm.responses, model.Response{
		StopReason: model.StopEndTurn,
		Blocks:     []model.Block{model.TextBlock("done")},
	})
	agent := &Agent{
		Name:     "compress",
		Model:    llm,
		Tools:    []Tool{echoTool("echo")},
		MaxTurns: 11,
	}
	var r Runner
	res, err := r.Run(context.Background(), agent, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "done" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	// After compression, the message history should contain at least
	// one message whose text begins with [Context compressed].
	sawCompressed := false
	for _, m := range res.History {
		for _, b := range m.Blocks {
			if b.Type == model.BlockText && strings.HasPrefix(b.Text, "[Context compressed]") {
				sawCompressed = true
			}
		}
	}
	if !sawCompressed {
		t.Errorf("expected compressed summary in history, got: %+v", res.History)
	}
}

// TestTruncateForLog validates truncation behavior.
func TestTruncateForLog(t *testing.T) {
	tests := []struct {
		input    string
		max      int
		expected string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is a long string that should be truncated", 20, "this is a long st..."},
		{"", 10, ""},
		{"  spaces  ", 10, "spaces"},
	}
	for _, tt := range tests {
		got := truncateForLog(tt.input, tt.max)
		if got != tt.expected {
			t.Errorf("truncateForLog(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
		}
	}
}

func TestRunnerNilAgent(t *testing.T) {
	var r Runner
	if _, err := r.Run(context.Background(), nil, "x"); err == nil {
		t.Error("expected error for nil agent")
	}
}

func TestRunnerNilModel(t *testing.T) {
	var r Runner
	if _, err := r.Run(context.Background(), &Agent{Name: "x"}, "y"); err == nil {
		t.Error("expected error for nil model")
	}
}

func TestSessionBasics(t *testing.T) {
	s := NewMemorySession("test")
	if s.ID() != "test" {
		t.Errorf("ID = %q", s.ID())
	}
	s.Append(model.UserText("a"), model.UserText("b"))
	if got := s.Messages(); len(got) != 2 {
		t.Errorf("len = %d", len(got))
	}
	s.Reset()
	if got := s.Messages(); len(got) != 0 {
		t.Errorf("after reset len = %d", len(got))
	}
}

// ---------------------------------------------------------------------
// Streaming helpers and tests
// ---------------------------------------------------------------------

// scriptStreamer is a model.Streamer whose Stream() emits a pre-built
// slice of StreamEvents. Complete delegates to scriptLLM for non-streaming callers.
type scriptStreamer struct {
	scriptLLM
	events []model.StreamEvent
}

func (s *scriptStreamer) Stream(_ context.Context, req model.Request) (<-chan model.StreamEvent, error) {
	s.calls = append(s.calls, req)
	ch := make(chan model.StreamEvent, len(s.events)+1)
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// Compile-time check that scriptStreamer satisfies model.Streamer.
var _ model.Streamer = (*scriptStreamer)(nil)

// TestStreamedCompleteText verifies that streamedComplete assembles a
// text response from StreamTextDelta + StreamBlockDone + StreamDone.
func TestStreamedCompleteText(t *testing.T) {
	textBlk := model.TextBlock("hello world")
	usage := model.Usage{InputTokens: 10, OutputTokens: 5}
	s := &scriptStreamer{
		events: []model.StreamEvent{
			{Kind: model.StreamTextDelta, Text: "hello "},
			{Kind: model.StreamTextDelta, Text: "world"},
			{Kind: model.StreamBlockDone, Block: &textBlk},
			{Kind: model.StreamDone, StopReason: model.StopEndTurn, Usage: &usage},
		},
	}

	var captured []model.StreamEvent
	resp, err := streamedComplete(context.Background(), s, model.Request{}, func(ev model.StreamEvent) {
		captured = append(captured, ev)
	})
	if err != nil {
		t.Fatalf("streamedComplete: %v", err)
	}
	if resp.TextOnly() != "hello world" {
		t.Errorf("TextOnly = %q", resp.TextOnly())
	}
	if resp.StopReason != model.StopEndTurn {
		t.Errorf("StopReason = %s", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if len(captured) != 4 {
		t.Errorf("sink received %d events, want 4", len(captured))
	}
}

// TestStreamedCompleteToolUse verifies that streamedComplete assembles
// a tool_use response from StreamToolUseDelta + StreamBlockDone + StreamDone.
func TestStreamedCompleteToolUse(t *testing.T) {
	toolBlk := model.ToolUseBlock("tu_1", "search", json.RawMessage(`{"q":"go"}`))
	usage := model.Usage{InputTokens: 8, OutputTokens: 12}
	s := &scriptStreamer{
		events: []model.StreamEvent{
			{Kind: model.StreamToolUseDelta, ToolUseID: "tu_1", ToolName: "search", ToolPartial: `{"q":"go"}`},
			{Kind: model.StreamBlockDone, ToolUseID: "tu_1", ToolName: "search", Block: &toolBlk},
			{Kind: model.StreamDone, StopReason: model.StopToolUse, Usage: &usage},
		},
	}

	resp, err := streamedComplete(context.Background(), s, model.Request{}, func(model.StreamEvent) {})
	if err != nil {
		t.Fatalf("streamedComplete: %v", err)
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(calls))
	}
	if calls[0].ToolName != "search" || calls[0].ToolUseID != "tu_1" {
		t.Errorf("tool call = %+v", calls[0])
	}
	if resp.StopReason != model.StopToolUse {
		t.Errorf("StopReason = %s", resp.StopReason)
	}
}

// TestStreamedCompleteError verifies that a StreamDone with Err set is
// propagated as an error from streamedComplete.
func TestStreamedCompleteError(t *testing.T) {
	s := &scriptStreamer{
		events: []model.StreamEvent{
			{Kind: model.StreamDone, Err: errors.New("network reset")},
		},
	}
	_, err := streamedComplete(context.Background(), s, model.Request{}, func(model.StreamEvent) {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "network reset") {
		t.Errorf("err = %q", err.Error())
	}
}

// TestCallModelFallsBackToComplete verifies that callModel falls back to
// Complete when the model doesn't implement Streamer.
func TestCallModelFallsBackToComplete(t *testing.T) {
	llm := &scriptLLM{
		responses: []model.Response{
			{StopReason: model.StopEndTurn, Blocks: []model.Block{model.TextBlock("done")}},
		},
	}
	var sinkCalled bool
	resp, err := callModel(context.Background(), llm, model.Request{}, func(model.StreamEvent) {
		sinkCalled = true
	})
	if err != nil {
		t.Fatalf("callModel: %v", err)
	}
	if resp.TextOnly() != "done" {
		t.Errorf("TextOnly = %q", resp.TextOnly())
	}
	// A non-Streamer model must never call the sink.
	if sinkCalled {
		t.Error("sink was called for a non-Streamer model")
	}
}

// TestWithStreamSinkRunnerEndTurn verifies that Runner.Run honours
// WithStreamSink and text deltas flow through the sink.
func TestWithStreamSinkRunnerEndTurn(t *testing.T) {
	textBlk := model.TextBlock("stream answer")
	usage := model.Usage{InputTokens: 5, OutputTokens: 3}
	s := &scriptStreamer{
		scriptLLM: scriptLLM{
			responses: []model.Response{
				{StopReason: model.StopEndTurn, Blocks: []model.Block{textBlk}},
			},
		},
		events: []model.StreamEvent{
			{Kind: model.StreamTextDelta, Text: "stream "},
			{Kind: model.StreamTextDelta, Text: "answer"},
			{Kind: model.StreamBlockDone, Block: &textBlk},
			{Kind: model.StreamDone, StopReason: model.StopEndTurn, Usage: &usage},
		},
	}
	agent := &Agent{
		Name:         "streamer",
		Instructions: "stream",
		Model:        s,
	}

	var textPieces []string
	sink := func(ev model.StreamEvent) {
		if ev.Kind == model.StreamTextDelta {
			textPieces = append(textPieces, ev.Text)
		}
	}

	var r Runner
	res, err := r.Run(context.Background(), agent, "hello", WithStreamSink(sink))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalOutput != "stream answer" {
		t.Errorf("FinalOutput = %q", res.FinalOutput)
	}
	if got := strings.Join(textPieces, ""); got != "stream answer" {
		t.Errorf("streamed text = %q", got)
	}
}
