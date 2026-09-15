package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// runDirSink writes structured NDJSON events to <run-dir>/events.ndjson
// and the final answer to <run-dir>/output.md. Used when `aida --agent`
// is invoked with --run-dir, replacing the TTY/spinner output path
// with one that's safe for subprocess capture and SSE tailing.
//
// Event format: one JSON object per line, newline-terminated.
//
//	{"ts":"2026-05-06T14:12:00Z","type":"start","data":{...}}
//	{"ts":"…","type":"verbose","data":{"label":"…","msg":"…"}}
//	{"ts":"…","type":"tool_call","data":{"turn":N,"tool":"…","input":"…","status":"ok|error","output":"…"}}
//	{"ts":"…","type":"complete","data":{"turns":3,"cost_usd":0.012}}
//	{"ts":"…","type":"error","data":{"message":"…"}}
//
// All writes serialize through a mutex so a stream-sink goroutine
// emitting tool-call events can safely race with the main goroutine
// emitting verbose/complete events.
type runDirSink struct {
	dir   string
	mu    sync.Mutex
	file  *os.File
	start time.Time

	// lastOutput is the content most recently passed to writeOutput.
	// emitComplete reads it to fill final_preview, see the doc comment
	// there for why this exists instead of a new emitComplete parameter.
	lastOutput string
}

// openRunDirSink creates the run dir, opens events.ndjson for append,
// and emits the initial start event. Caller defers Close.
func openRunDirSink(dir, runID, question string) (*runDirSink, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir run-dir: %w", err)
	}
	f, err := os.OpenFile(
		filepath.Join(dir, "events.ndjson"),
		os.O_WRONLY|os.O_CREATE|os.O_APPEND,
		0644,
	)
	if err != nil {
		return nil, fmt.Errorf("open events.ndjson: %w", err)
	}
	s := &runDirSink{
		dir:   dir,
		file:  f,
		start: time.Now().UTC(),
	}
	s.emit("start", map[string]any{
		"version":  1,
		"run_id":   runID,
		"question": question,
	})
	return s, nil
}

// emit appends one event line. Failures are non-fatal and silently
// dropped - a corrupt events log shouldn't abort an agent run.
func (s *runDirSink) emit(eventType string, data any) {
	if s == nil || s.file == nil {
		return
	}
	envelope := map[string]any{
		"ts":   time.Now().UTC().Format(time.RFC3339Nano),
		"type": eventType,
		"data": data,
	}
	line, err := json.Marshal(envelope)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file.Write(line)
	s.file.Write([]byte("\n"))
}

// emitVerbose is a tiny convenience for the ui.PrintVerbose-shaped
// label/msg pair the rest of the codebase uses.
func (s *runDirSink) emitVerbose(label, msg string) {
	s.emit("verbose", map[string]any{"label": label, "msg": msg})
}

// emitToolCall records one completed tool invocation. Output is
// truncated for events; the final answer goes to output.md.
func (s *runDirSink) emitToolCall(turn int, tool, input, output, errMsg string) {
	status := "ok"
	if errMsg != "" {
		status = "error"
	}
	const maxOutput = 4000
	if len(output) > maxOutput {
		output = output[:maxOutput] + "…"
	}
	s.emit("tool_call", map[string]any{
		"turn":   turn,
		"tool":   tool,
		"input":  input,
		"output": output,
		"status": status,
		"error":  errMsg,
	})
}

// maxFinalPreview bounds the final_preview field on the complete
// event -- same truncation scale as emitInputReceived's maxReply, so
// the event log stays scannable without losing the shape of a long
// final answer.
const maxFinalPreview = 2000

// emitComplete is the final "successful run" marker. final_preview
// used to be a hardcoded "" literal (dead code -- every run's complete
// event carried an empty preview, while final_len was computed
// correctly right next to it), so the completion event had no actual
// evidence of what the run produced, just its length. It's filled in
// now from lastOutput, the exact content the preceding writeOutput
// call wrote to output.md, truncated the same way other large fields
// in this file are (emitToolCall's maxOutput, emitInputReceived's
// maxReply) and backed off to a rune boundary like formatJobLine does,
// so a mid-rune slice of an answer containing accented or symbolic
// characters can't yield invalid UTF-8.
func (s *runDirSink) emitComplete(turns int, costUSD float64, finalLen int) {
	s.mu.Lock()
	preview := s.lastOutput
	s.mu.Unlock()
	if len(preview) > maxFinalPreview {
		cut := maxFinalPreview
		for cut > 0 && !utf8.RuneStart(preview[cut]) {
			cut--
		}
		preview = preview[:cut] + "…"
	}
	s.emit("complete", map[string]any{
		"turns":         turns,
		"cost_usd":      costUSD,
		"duration_ms":   time.Since(s.start).Milliseconds(),
		"final_len":     finalLen,
		"final_preview": preview,
	})
}

// emitError is the final "failed run" marker.
func (s *runDirSink) emitError(msg string) {
	s.emit("error", map[string]any{
		"message":     msg,
		"duration_ms": time.Since(s.start).Milliseconds(),
	})
}

// Event kind constants for the run-dir event log. Kept as constants
// (vs. string literals scattered through callers) so a typo in a new
// emitter site is a compile error, and so the SSE consumer in
// tasks_web has one place to look for the full set.
const (
	EventKindStart         = "start"
	EventKindVerbose       = "verbose"
	EventKindToolCall      = "tool_call"
	EventKindToolCallStart = "tool_call_start"
	EventKindComplete      = "complete"
	EventKindError         = "error"
	// EventKindAwaitingInput fires when the agent's ask_user tool is
	// invoked. The agent loop is suspended (the SQL row + manifest
	// state both transition to awaiting_input) until the user's
	// reply lands in input.txt.
	EventKindAwaitingInput = "awaiting_input"
	// EventKindInputReceived fires when ask_user has read input.txt
	// and is about to return the reply to the agent. The pair
	// (awaiting_input, input_received) brackets one user-in-the-loop
	// detour in the SSE stream.
	EventKindInputReceived = "input_received"
	// EventKindAwaitingApproval fires when request_approval suspends the run
	// pending a human merge/send/writeback decision; EventKindApprovalGranted
	// fires when the user approves and the agent is about to act. Their
	// presence/absence in events.ndjson is the audit trail for the HITL gate
	// (the negative test asserts no irreversible action precedes a granted).
	EventKindAwaitingApproval = "awaiting_approval"
	EventKindApprovalGranted  = "approval_granted"
)

// emitAwaitingInput records that the agent has paused and is waiting
// for the user to drop a reply into <run-dir>/input.txt. The prompt
// is the agent's question; the voice notifier surfaces it on next
// wake.
func (s *runDirSink) emitAwaitingInput(prompt string) {
	s.emit(EventKindAwaitingInput, map[string]any{
		"prompt": prompt,
	})
}

// emitInputReceived records the inverse - the agent has read the
// user's reply and is resuming. Reply is included (truncated) so
// the SSE consumer can show what was sent.
func (s *runDirSink) emitInputReceived(reply string) {
	const maxReply = 2000
	if len(reply) > maxReply {
		reply = reply[:maxReply] + "…"
	}
	s.emit(EventKindInputReceived, map[string]any{
		"reply": reply,
	})
}

// writeOutput writes the final answer markdown. Called once at the
// end of a run, before Close. Also stashes content as lastOutput so a
// following emitComplete can fill final_preview with the real thing
// that got written, not a hardcoded stand-in. See the doc comment
// there.
func (s *runDirSink) writeOutput(content string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.lastOutput = content
	s.mu.Unlock()
	return os.WriteFile(filepath.Join(s.dir, "output.md"), []byte(content), 0644)
}

// Close releases the events.ndjson file handle. Idempotent.
func (s *runDirSink) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// makeRunDirStreamSink returns a stream-sink callback that writes
// one tool_call event per BlockDone event with a tool-use block.
// Text deltas are intentionally dropped - the final answer goes to
// output.md, not the event log, to keep events.ndjson scan-friendly.
//
// Compatible with engine.Run's WithStreamSink option, so the run-dir
// mode can opt in to live event emission without --stream's TTY
// printing side effects.
func makeRunDirStreamSink(s *runDirSink) func(model.StreamEvent) {
	if s == nil {
		return func(model.StreamEvent) {}
	}
	turn := 0
	return func(ev model.StreamEvent) {
		if ev.Kind != model.StreamBlockDone || ev.Block == nil {
			return
		}
		if ev.Block.Type != model.BlockToolUse {
			return
		}
		turn++
		// Tool input is JSON bytes; render as string for the event.
		// Truncate large inputs so events.ndjson stays scannable.
		input := string(ev.Block.ToolInput)
		const maxInput = 2000
		if len(input) > maxInput {
			input = input[:maxInput] + "…"
		}
		s.emit("tool_call_start", map[string]any{
			"turn":  turn,
			"tool":  ev.Block.ToolName,
			"input": input,
		})
	}
}
