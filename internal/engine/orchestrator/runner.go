package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

const (
	defaultMaxToolFailures = 3
)

// CompressionTurnThreshold and CompressionMessageThreshold define when
// the context-compression pass engages: once a single agent's loop has
// taken at least this many turns AND accumulated more than this many
// messages.
const (
	CompressionTurnThreshold    = 8
	CompressionMessageThreshold = 15
)

// Termination records precisely why a Run stopped. A nil Go error from
// Run no longer implies the agent finished its work -- callers must
// read RunResult.Termination to tell a genuine final answer apart from
// a run that was cut off, and the incident that motivated this type
// (a run that burned every turn on reconnaissance, never issued its
// upgrade command, and still looked like a clean finish) is exactly
// the case this closes.
type Termination string

const (
	// TerminationFinalResponse means the model ended its turn with no
	// outstanding tool calls, or called submit_result. This is the
	// only reason that pairs with a trustworthy FinalOutput.
	TerminationFinalResponse Termination = "final_response"
	// TerminationAwaitingInput means the run paused for a human reply
	// and has not yet resumed. Reserved for callers that implement a
	// pause/resume tool on top of this package; the runner itself
	// does not currently produce it.
	TerminationAwaitingInput Termination = "awaiting_input"
	// TerminationAwaitingApproval means the run paused for a human
	// approval decision. Reserved, like TerminationAwaitingInput.
	TerminationAwaitingApproval Termination = "awaiting_approval"
	// TerminationModelTokenLimit means a completion was truncated by
	// the provider's max_tokens limit. A truncated response is never
	// dispatched as tool calls and never promoted to a final answer,
	// even when it happens to carry zero tool_use blocks.
	TerminationModelTokenLimit Termination = "model_token_limit"
	// TerminationWallTimeLimit means the run's wall-clock deadline
	// (WithDeadline) passed before the agent finished.
	TerminationWallTimeLimit Termination = "wall_time_limit"
	// TerminationCostLimit means the run's accumulated cost ceiling
	// (WithCostLimit) was reached before the agent finished.
	TerminationCostLimit Termination = "cost_limit"
	// TerminationCancelled means the run's context was cancelled by
	// something outside this package (the caller, a parent deadline)
	// rather than by one of this package's own bounds.
	TerminationCancelled Termination = "cancelled"
	// TerminationError means the run stopped on a genuine failure: a
	// model call errored, or the model produced zero tool calls with a
	// stop reason that is neither end_turn nor max_tokens (e.g. a stop
	// sequence or a provider-specific reason). Never a normal
	// completion.
	TerminationError Termination = "error"
)

// latchTermination records why a run stopped, but only the first time.
// Bound trips cancel the run context as part of stopping it; that
// cancellation can itself surface as a later "context canceled" error
// from an in-flight call. Writing through latchTermination everywhere
// guarantees the original reason (wall_time_limit, cost_limit, ...)
// always wins over whatever secondary error the cancellation causes.
func latchTermination(result *RunResult, t Termination) {
	if result.Termination == "" {
		result.Termination = t
	}
}

// ToolCallRecord records one tool invocation for observability.
type ToolCallRecord struct {
	Turn   int    `json:"turn"`
	Agent  string `json:"agent"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// RunResult is the outcome of a Runner.Run.
type RunResult struct {
	// FinalOutput is the final text answer. Only trustworthy when
	// Termination == TerminationFinalResponse; every other Termination
	// leaves this empty rather than fabricating an answer from
	// whatever partial work happened before the run stopped.
	FinalOutput string
	// FinalAgent is the agent that produced FinalOutput. After a
	// handoff, this is the target agent, not the original.
	FinalAgent *Agent
	// Turns is the total number of LLM completions across all agents
	// in this run. Kept for reporting even though nothing bounds the
	// loop by turn count any more.
	Turns int
	// ToolCalls records every tool/handoff dispatch.
	ToolCalls []ToolCallRecord
	// Usage aggregates token counts across all completions.
	Usage model.Usage
	// CostUSD aggregates the USD cost of every completion, as computed
	// by the CostFunc supplied to WithCostLimit. Zero when no CostFunc
	// was supplied, even if completions happened.
	CostUSD float64
	// History is the final rolling message history.
	History []model.Message
	// HandoffPath names each agent touched, in order. Always contains
	// at least the starting agent.
	HandoffPath []string
	// Termination records precisely why the run stopped. See the
	// Termination type.
	Termination Termination
	// Result is the structured outcome an agent declared via the
	// submit_result tool. Nil whenever the run ended any other way --
	// free final text is never promoted into a Result, and downstream
	// callers must treat a nil Result as unverified.
	Result *SubmitResult
}

// SubmitResultToolName is the reserved tool name a Runner treats as a
// terminal declaration of outcome. An Agent that wants to let its
// model declare a verified result -- rather than relying on free
// prose a downstream reader might mistake for success -- lists a tool
// with this name (see internal/engine/agent_tools.go's submit_result
// tool for the reference implementation of the Execute side; the
// Runner itself intercepts the tool_use block before normal
// dispatch).
const SubmitResultToolName = "submit_result"

// Outcome is the caller-declared result of a submit_result call.
type Outcome string

const (
	OutcomeAchieved    Outcome = "achieved"
	OutcomeNotAchieved Outcome = "not_achieved"
)

// SubmitResult is the structured payload an agent supplies via the
// submit_result tool to end its run with a verified outcome.
// FailureCode is required when Outcome is OutcomeNotAchieved -- a
// failed run must say why in a stable, machine-readable way, not just
// prose.
type SubmitResult struct {
	Outcome     Outcome `json:"outcome"`
	Summary     string  `json:"summary"`
	FailureCode string  `json:"failure_code,omitempty"`
}

// ParseSubmitResult validates a submit_result tool call's raw JSON
// input against the schema: outcome must be "achieved" or
// "not_achieved", and failure_code is required when outcome is
// "not_achieved". The returned error is meant to be fed back to the
// model as an ordinary tool error so it can correct and resubmit.
func ParseSubmitResult(raw json.RawMessage) (*SubmitResult, error) {
	var sr SubmitResult
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("submit_result: invalid input: %w", err)
	}
	switch sr.Outcome {
	case OutcomeAchieved, OutcomeNotAchieved:
	default:
		return nil, fmt.Errorf("submit_result: outcome must be %q or %q, got %q", OutcomeAchieved, OutcomeNotAchieved, sr.Outcome)
	}
	if sr.Outcome == OutcomeNotAchieved && strings.TrimSpace(sr.FailureCode) == "" {
		return nil, fmt.Errorf("submit_result: failure_code is required when outcome is %q", OutcomeNotAchieved)
	}
	return &sr, nil
}

// RunOption is a functional option for Runner.Run.
type RunOption func(*runOptions)

type runOptions struct {
	session    Session
	tracer     Tracer
	maxHandoff int
	streamSink func(model.StreamEvent) // nil = non-streaming

	// deadline bounds wall-clock time; zero means unbounded. Set via
	// WithDeadline.
	deadline time.Time
	// maxCostUSD and costFunc together bound accumulated LLM cost;
	// maxCostUSD <= 0 or a nil costFunc means unbounded. Set via
	// WithCostLimit.
	maxCostUSD float64
	costFunc   CostFunc

	// cancel is set internally by Run before driving any agent, never
	// by a RunOption. Bound trips call it as part of latching a
	// termination reason, so anything still watching this run's
	// context (a slow tool, a streaming call) unwinds promptly.
	cancel context.CancelFunc
}

// CostFunc converts a single completion's token usage into a USD cost
// estimate. Supplied by the caller via WithCostLimit -- this package
// has no pricing tables of its own, since those live with the model
// adapters and vary by provider/model.
type CostFunc func(model.Usage) float64

// WithSession runs against a caller-supplied Session. If omitted, a
// fresh MemorySession is created.
func WithSession(s Session) RunOption {
	return func(o *runOptions) { o.session = s }
}

// WithTracer routes span events to a caller-supplied Tracer. If
// omitted, NoopTracer is used.
func WithTracer(t Tracer) RunOption {
	return func(o *runOptions) { o.tracer = t }
}

// WithMaxHandoffs caps the number of sequential handoffs allowed
// within a single Run. Zero uses the default (5).
func WithMaxHandoffs(n int) RunOption {
	return func(o *runOptions) { o.maxHandoff = n }
}

// WithStreamSink registers a per-event callback invoked for every
// model.StreamEvent while the agent is generating a response. Only
// meaningful when the active agent's Model implements model.Streamer;
// if the model does not support streaming, Complete is used silently
// and the sink is never called. Pass nil to disable streaming.
func WithStreamSink(sink func(model.StreamEvent)) RunOption {
	return func(o *runOptions) { o.streamSink = sink }
}

// WithDeadline bounds a run's wall-clock time. Once time.Now() reaches
// deadline, the run stops with TerminationWallTimeLimit rather than
// making another model call or tool dispatch -- it never keeps going
// on the theory that "it'll probably finish soon." A zero Time (the
// default) means unbounded.
func WithDeadline(deadline time.Time) RunOption {
	return func(o *runOptions) { o.deadline = deadline }
}

// WithCostLimit bounds a run's accumulated LLM cost in USD. costFn
// converts each completion's token usage into a dollar estimate; once
// the running total (RunResult.CostUSD) reaches maxUSD, the run stops
// with TerminationCostLimit. maxUSD <= 0 or a nil costFn disables the
// bound (the default) -- callers that care about cost must supply
// both.
func WithCostLimit(maxUSD float64, costFn CostFunc) RunOption {
	return func(o *runOptions) {
		o.maxCostUSD = maxUSD
		o.costFunc = costFn
	}
}

// checkBounds reports whether the wall-clock deadline or cost ceiling
// configured on options has been exceeded, and which one. A disabled
// bound (zero deadline; maxCostUSD <= 0 or a nil costFunc) never trips.
func checkBounds(options runOptions, result *RunResult) (Termination, bool) {
	if !options.deadline.IsZero() && !time.Now().Before(options.deadline) {
		return TerminationWallTimeLimit, true
	}
	if options.maxCostUSD > 0 && options.costFunc != nil && result.CostUSD >= options.maxCostUSD {
		return TerminationCostLimit, true
	}
	return "", false
}

// Runner drives an Agent or a graph of Agents connected by handoffs.
// A zero Runner is valid; fields exist only for future extensions.
type Runner struct{}

// Run executes the agent loop starting from agent with the given user
// input. It returns when the active agent produces a final text
// answer (stop_reason == end_turn or a validated submit_result call),
// when a configured bound (WithDeadline / WithCostLimit) trips, or
// when a guardrail fails. See RunResult.Termination for precisely
// which of those happened -- a nil error here does not by itself mean
// the agent finished its work.
func (r *Runner) Run(ctx context.Context, agent *Agent, input string, opts ...RunOption) (*RunResult, error) {
	if agent == nil {
		return nil, errors.New("runner: agent is nil")
	}
	if agent.Model == nil {
		return nil, errors.New("runner: agent.Model is nil")
	}

	options := runOptions{
		tracer:     NoopTracer{},
		maxHandoff: 5,
	}
	for _, o := range opts {
		o(&options)
	}
	if options.session == nil {
		options.session = NewMemorySession(agent.Name + ":session")
	}

	ctx, endRun := options.tracer.StartRun(ctx, agent.Name)
	defer endRun()

	// Every driveAgent call shares this cancelable context so a bound
	// trip (see latchTermination call sites in driveAgent) can cancel
	// any in-flight work as part of stopping the run, rather than
	// merely returning while a tool or stream keeps running.
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	options.cancel = cancelRun

	// Input guardrails run once against the raw user input.
	if err := runGuardrailsParallel(ctx, agent.InputGuardrails, input); err != nil {
		return nil, err
	}

	// Seed the session with the user's first message.
	options.session.Append(model.UserText(input))

	result := &RunResult{
		FinalAgent:  agent,
		HandoffPath: []string{agent.Name},
	}

	currentAgent := agent
	handoffCount := 0
	// The agent loop. We may switch agents mid-loop on handoff; the
	// outer for-range-handoffs semantic is expressed via currentAgent.
	for {
		final, handoff, err := r.driveAgent(ctx, currentAgent, options, result)
		if err != nil {
			return nil, err
		}

		if handoff != nil {
			if handoffCount >= options.maxHandoff {
				return nil, fmt.Errorf("runner: max handoffs (%d) exceeded", options.maxHandoff)
			}
			handoffCount++
			// Push a synthetic user message carrying the handoff
			// context so the target agent sees something to respond to.
			if handoff.payload != "" {
				options.session.Append(model.UserText(handoff.payload))
			}
			currentAgent = handoff.target
			result.FinalAgent = currentAgent
			result.HandoffPath = append(result.HandoffPath, currentAgent.Name)
			continue
		}

		result.FinalOutput = final

		// Output guardrails only make sense against a genuine final
		// answer; a bound trip or an unexpected-stop-reason error
		// leaves FinalOutput empty and Termination not-final_response,
		// so there is nothing here worth checking.
		if result.Termination == TerminationFinalResponse {
			if err := runGuardrailsParallel(ctx, currentAgent.OutputGuardrails, final); err != nil {
				return nil, err
			}
		}
		result.History = options.session.Messages()
		return result, nil
	}
}

// handoffSignal is the internal signal from driveAgent back to Run
// that the agent has requested a handoff.
type handoffSignal struct {
	target  *Agent
	payload string
}

// driveAgent is the single-agent tool-use loop. It returns either a
// final text answer, a handoff signal, or an error. There is no turn
// cap: the loop runs until the model ends its turn, calls
// submit_result, hits an unrecoverable error, or a bound configured on
// options (deadline / cost) trips. Every exit path records exactly why
// on result.Termination via latchTermination before returning.
func (r *Runner) driveAgent(
	ctx context.Context,
	agent *Agent,
	options runOptions,
	result *RunResult,
) (finalOutput string, handoff *handoffSignal, err error) {
	maxFails := agent.MaxToolFailures
	if maxFails <= 0 {
		maxFails = defaultMaxToolFailures
	}

	tools := agent.toolMap()
	handoffs := agent.handoffMap()
	toolDefs := agent.modelToolDefs()

	failureCount := make(map[string]int)

	// localTurn counts this driveAgent call's own completions, starting
	// back at 1 after every handoff, distinct from result.Turns, which
	// is cumulative across the whole run (including agents earlier in a
	// handoff chain) and is what gets reported on RunResult. Context
	// compression is keyed off localTurn so a freshly handed-off agent
	// gets its own compression budget rather than inheriting whatever
	// turn count the prior agent had already run up.
	localTurn := 0

	for {
		// Check before spending another model call: a bound that
		// tripped while we were dispatching the previous turn's tools
		// must stop us here, before we pay for a call we won't use.
		if reason, tripped := checkBounds(options, result); tripped {
			latchTermination(result, reason)
			options.cancel()
			return "", nil, nil
		}

		result.Turns++
		localTurn++
		turnCtx, endTurn := options.tracer.StartTurn(ctx, agent.Name, result.Turns)

		req := model.Request{
			SystemPrefix: agent.InstructionsPrefix,
			System:       agent.Instructions,
			Messages:     options.session.Messages(),
			Tools:        toolDefs,
			MaxTokens:    agent.MaxTokens,
		}
		resp, callErr := callModel(turnCtx, agent.Model, req, options.streamSink)
		if callErr != nil {
			endTurn()
			// If our own context is already done, the model call
			// failing is a symptom, not the cause: either a bound
			// already latched and cancelled us (latchTermination is a
			// no-op here, preserving that reason) or something
			// outside this package cancelled/timed out the run.
			if ctx.Err() != nil {
				latchTermination(result, TerminationCancelled)
			} else {
				latchTermination(result, TerminationError)
			}
			return "", nil, fmt.Errorf("agent %q turn %d: %w", agent.Name, result.Turns, callErr)
		}
		result.Usage.InputTokens += resp.Usage.InputTokens
		result.Usage.OutputTokens += resp.Usage.OutputTokens
		result.Usage.CacheReadTokens += resp.Usage.CacheReadTokens
		result.Usage.CacheWriteTokens += resp.Usage.CacheWriteTokens
		if options.costFunc != nil {
			result.CostUSD += options.costFunc(resp.Usage)
		}

		// Check again now that this call's cost is on the books: a
		// response that pushes us over the ceiling must not be
		// dispatched or promoted to a final answer, no matter what it
		// contains.
		if reason, tripped := checkBounds(options, result); tripped {
			latchTermination(result, reason)
			options.cancel()
			endTurn()
			return "", nil, nil
		}

		toolCalls := resp.ToolCalls()

		switch {
		case resp.StopReason == model.StopEndTurn:
			// A final answer. Any stray tool_use blocks alongside
			// end_turn (providers should not send these, but we don't
			// trust that) are not dispatched -- the model said it was
			// done.
			latchTermination(result, TerminationFinalResponse)
			endTurn()
			return resp.TextOnly(), nil, nil
		case resp.StopReason == model.StopMaxTokens:
			// Truncated output. Never dispatch tool calls parsed from
			// a cut-off response, and never treat truncated text as a
			// final answer -- this is exactly the bug this type
			// closes: a truncated response used to satisfy
			// len(toolCalls) == 0 and get promoted to "done".
			latchTermination(result, TerminationModelTokenLimit)
			endTurn()
			return "", nil, nil
		case len(toolCalls) == 0:
			// Zero tool calls with a stop reason that is neither
			// end_turn nor max_tokens (a stop sequence, or a
			// provider-specific "other") is not a normal completion.
			latchTermination(result, TerminationError)
			endTurn()
			return "", nil, fmt.Errorf("agent %q turn %d: model stopped with reason %q and produced no tool calls", agent.Name, result.Turns, resp.StopReason)
		}

		// Persist the assistant turn (text + tool_use blocks) on the session.
		options.session.Append(model.Message{
			Role:   model.RoleAssistant,
			Blocks: resp.Blocks,
		})

		// Dispatch tool calls. First, check if any of them is a
		// handoff or a validated submit_result call: either one
		// short-circuits out of this agent (submit_result ends the
		// whole run; a handoff just switches agents).
		for _, tc := range toolCalls {
			if h, ok := handoffs[tc.ToolName]; ok {
				payload, perr := parseHandoffInput(tc.ToolInput)
				if perr != nil {
					endTurn()
					latchTermination(result, TerminationError)
					return "", nil, perr
				}
				// Record the handoff as a tool call so it is visible
				// in the run log.
				result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
					Turn:   result.Turns,
					Agent:  agent.Name,
					Tool:   tc.ToolName,
					Input:  truncateForLog(string(tc.ToolInput), 500),
					Output: "(handoff to " + h.Target.Name + ")",
				})
				// Persist a synthetic tool_result so the rolling
				// history matches what the next model call expects -
				// the Anthropic API requires a tool_result after a
				// tool_use block before the next user turn.
				options.session.Append(model.UserToolResults(
					model.ToolResultBlock(tc.ToolUseID, "handoff accepted", false),
				))
				endTurn()
				return "", &handoffSignal{target: h.Target, payload: payload}, nil
			}
			if tc.ToolName == SubmitResultToolName {
				if sr, perr := ParseSubmitResult(tc.ToolInput); perr == nil {
					result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
						Turn:   result.Turns,
						Agent:  agent.Name,
						Tool:   tc.ToolName,
						Input:  truncateForLog(string(tc.ToolInput), 500),
						Output: fmt.Sprintf("(run ended: outcome=%s)", sr.Outcome),
					})
					options.session.Append(model.UserToolResults(
						model.ToolResultBlock(tc.ToolUseID, "result recorded", false),
					))
					result.Result = sr
					latchTermination(result, TerminationFinalResponse)
					endTurn()
					return sr.Summary, nil, nil
				}
				// Malformed submit_result input falls through to
				// normal tool dispatch below, where the registered
				// tool's own validation (if the caller wired one up)
				// returns the same error as an ordinary tool_result --
				// giving the model a chance to correct and resubmit
				// instead of the run ending on bad data.
			}
		}

		// Normal tool dispatch: run each tool and collect results. A
		// bound that trips mid-dispatch stops us from launching any
		// further tool in this batch, but we still record a
		// consistent tool_result for every remaining call so the
		// session never carries an orphaned tool_use block.
		var boundTripped Termination
		results := make([]model.Block, 0, len(toolCalls))
		for _, tc := range toolCalls {
			if boundTripped == "" {
				if reason, tripped := checkBounds(options, result); tripped {
					boundTripped = reason
				}
			}
			if boundTripped != "" {
				msg := fmt.Sprintf("run stopped (%s) before this tool executed", boundTripped)
				results = append(results, model.ToolResultBlock(tc.ToolUseID, msg, true))
				result.ToolCalls = append(result.ToolCalls, ToolCallRecord{
					Turn:  result.Turns,
					Agent: agent.Name,
					Tool:  tc.ToolName,
					Input: truncateForLog(string(tc.ToolInput), 500),
					Error: msg,
				})
				continue
			}

			record := ToolCallRecord{
				Turn:  result.Turns,
				Agent: agent.Name,
				Tool:  tc.ToolName,
				Input: truncateForLog(string(tc.ToolInput), 500),
			}

			// Circuit breaker.
			if failureCount[tc.ToolName] >= maxFails {
				msg := fmt.Sprintf("Tool %q has been disabled after %d consecutive failures. Use a different tool.", tc.ToolName, maxFails)
				record.Error = msg
				results = append(results, model.ToolResultBlock(tc.ToolUseID, msg, true))
				result.ToolCalls = append(result.ToolCalls, record)
				continue
			}

			tool, ok := tools[tc.ToolName]
			if !ok {
				msg := fmt.Sprintf("unknown tool %q", tc.ToolName)
				record.Error = msg
				results = append(results, model.ToolResultBlock(tc.ToolUseID, "Error: "+msg, true))
				result.ToolCalls = append(result.ToolCalls, record)
				continue
			}

			toolCtx, endTool := options.tracer.StartTool(turnCtx, agent.Name, tc.ToolName, tc.ToolUseID)
			output, runErr := tool.Run(toolCtx, tc.ToolInput)
			endTool()
			if runErr != nil {
				failureCount[tc.ToolName]++
				errContent := "Error: " + runErr.Error()
				if failureCount[tc.ToolName] >= 2 {
					errContent += fmt.Sprintf(" (warning: this tool has failed %d times - consider using a different approach)", failureCount[tc.ToolName])
				}
				record.Error = runErr.Error()
				results = append(results, model.ToolResultBlock(tc.ToolUseID, errContent, true))
			} else {
				failureCount[tc.ToolName] = 0
				record.Output = truncateForLog(output, 1000)
				results = append(results, model.ToolResultBlock(tc.ToolUseID, output, false))
			}
			result.ToolCalls = append(result.ToolCalls, record)
		}

		// Persist tool results as a user message.
		options.session.Append(model.UserToolResults(results...))

		if boundTripped != "" {
			latchTermination(result, boundTripped)
			options.cancel()
			endTurn()
			return "", nil, nil
		}

		// Context compression: once the history is long enough, replace
		// middle messages with a deterministic summary.
		if shouldCompress(localTurn, options.session.Messages()) {
			compressed := compressMessages(options.session.Messages())
			options.session.Reset()
			options.session.Append(compressed...)
		}

		endTurn()
	}
}

// callModel dispatches one completion, using streaming when the model
// supports it and a sink is registered, falling back to Complete otherwise.
func callModel(ctx context.Context, llm model.LLM, req model.Request, sink func(model.StreamEvent)) (*model.Response, error) {
	if sink != nil {
		if streamer, ok := llm.(model.Streamer); ok {
			return streamedComplete(ctx, streamer, req, sink)
		}
	}
	return llm.Complete(ctx, req)
}

// streamedComplete drives a single streaming completion, calls sink for
// every event, and reconstructs a *model.Response from the events so the
// rest of the turn loop can work without branching on streaming vs. non-streaming.
func streamedComplete(ctx context.Context, s model.Streamer, req model.Request, sink func(model.StreamEvent)) (*model.Response, error) {
	ch, err := s.Stream(ctx, req)
	if err != nil {
		return nil, err
	}

	resp := &model.Response{}
	var textBuf strings.Builder

	for ev := range ch {
		sink(ev)
		switch ev.Kind {
		case model.StreamTextDelta:
			textBuf.WriteString(ev.Text)
		case model.StreamBlockDone:
			if ev.Block != nil {
				resp.Blocks = append(resp.Blocks, *ev.Block)
			}
		case model.StreamDone:
			if ev.Err != nil {
				return nil, ev.Err
			}
			resp.StopReason = ev.StopReason
			if ev.Usage != nil {
				resp.Usage = *ev.Usage
			}
		}
	}

	// For providers (like OpenAI) that don't emit a block_done for text,
	// assemble the accumulated text into a TextBlock and prepend it.
	if textBuf.Len() > 0 {
		hasTextBlock := false
		for _, b := range resp.Blocks {
			if b.Type == model.BlockText {
				hasTextBlock = true
				break
			}
		}
		if !hasTextBlock {
			textBlock := model.TextBlock(textBuf.String())
			resp.Blocks = append([]model.Block{textBlock}, resp.Blocks...)
		}
	}

	// Infer stop reason when the stream didn't set one explicitly.
	if resp.StopReason == "" {
		if len(resp.ToolCalls()) > 0 {
			resp.StopReason = model.StopToolUse
		} else {
			resp.StopReason = model.StopEndTurn
		}
	}

	return resp, nil
}

// truncateForLog trims whitespace and caps length with an ellipsis.
func truncateForLog(s string, max int) string {
	s = trimSpace(s)
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

// trimSpace is strings.TrimSpace inlined to avoid a one-function import.
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// shouldCompress returns true when context compression should engage.
func shouldCompress(turn int, msgs []model.Message) bool {
	return turn >= CompressionTurnThreshold && len(msgs) > CompressionMessageThreshold
}

// compressMessages keeps the first message (original user input) and
// the last keepTail messages, replacing the middle with a compact
// summary. The summary is deterministic - no LLM call.
func compressMessages(msgs []model.Message) []model.Message {
	if len(msgs) <= CompressionMessageThreshold {
		return msgs
	}
	const keepTail = 6
	tail := msgs[len(msgs)-keepTail:]
	first := msgs[0]

	var buf []byte
	buf = append(buf, []byte("[Context compressed]\nSummary of earlier tool calls:\n")...)
	for _, m := range msgs[1 : len(msgs)-keepTail] {
		for _, b := range m.Blocks {
			switch b.Type {
			case model.BlockToolUse:
				buf = append(buf, []byte("- Called tool \""+b.ToolName+"\"\n")...)
			case model.BlockToolResult:
				snippet := b.ToolResultContent
				if len(snippet) > 150 {
					snippet = snippet[:150] + "..."
				}
				buf = append(buf, []byte("  Result: "+snippet+"\n")...)
			case model.BlockText:
				snippet := b.Text
				if len(snippet) > 100 {
					snippet = snippet[:100] + "..."
				}
				if snippet != "" {
					buf = append(buf, []byte("  Text: "+snippet+"\n")...)
				}
			}
		}
	}

	summary := model.UserText(string(buf))
	out := make([]model.Message, 0, 2+keepTail)
	out = append(out, first)
	out = append(out, summary)
	out = append(out, tail...)
	return out
}
