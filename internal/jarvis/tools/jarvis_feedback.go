package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

// LastTurnFn returns the most recent non-rating Jarvis turn, or nil if no
// such turn is cached (e.g. fresh daemon start, or the session reset). The
// listener populates this; the feedback tools read it to know which turn
// they're attaching feedback to.
type LastTurnFn func() *audit.Record

type feedbackInput struct {
	Reason string `json:"reason"`
}

type noteInput struct {
	Directive string `json:"directive"`
}

func jarvisThumbsUpTool(b *brain.Brain, lastTurn LastTurnFn) Tool {
	return Tool{
		Name: "jarvis_thumbs_up",
		Description: "Record positive feedback on your most recent reply. Call this when the user says " +
			"'thumbs up', 'good answer', 'that's right', 'perfect', or similar. Reason is optional - " +
			"if the user explained why (e.g. 'I liked that you included the date'), pass it. The " +
			"feedback gets stored so future similar replies can reinforce that style.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "Optional. Why the user approved, in their words if possible.",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in feedbackInput
			_ = json.Unmarshal(raw, &in)
			return recordRating(b, lastTurn, "up", in.Reason)
		},
	}
}

func jarvisThumbsDownTool(b *brain.Brain, lastTurn LastTurnFn) Tool {
	return Tool{
		Name: "jarvis_thumbs_down",
		Description: "Record negative feedback on your most recent reply. Call this when the user says " +
			"'thumbs down', 'wrong', 'that's not right', 'no', or similar correction. Always include " +
			"the user's reason - it's how you avoid making the same mistake on similar future queries. " +
			"Examples of good reasons: 'should have used the weather tool not aida_query', " +
			"'too verbose', 'should have included the date'.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "Required. Why the user is correcting you, in their words.",
				},
			},
			"required": []string{"reason"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in feedbackInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if in.Reason == "" {
				return "", fmt.Errorf("jarvis_thumbs_down requires a reason")
			}
			return recordRating(b, lastTurn, "down", in.Reason)
		},
	}
}

func jarvisNoteTool(b *brain.Brain, lastTurn LastTurnFn) Tool {
	return Tool{
		Name: "jarvis_note",
		Description: "Record a standing directive about how to handle similar future turns, without " +
			"giving a positive or negative rating. Call when the user says 'note that', 'remember', " +
			"'going forward', or gives an instruction that should shape future replies (e.g. " +
			"'use metric units when you talk about distance', 'always include the date with the time').",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"directive": map[string]interface{}{
					"type":        "string",
					"description": "Required. The standing instruction in the user's words.",
				},
			},
			"required": []string{"directive"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in noteInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if in.Directive == "" {
				return "", fmt.Errorf("jarvis_note requires a directive")
			}
			return recordRating(b, lastTurn, "note", in.Directive)
		},
	}
}

// recordRating is the shared body of the three feedback tools. Looks up the
// most recent non-rating turn, writes a jarvis_lessons row, and returns a
// short acknowledgement string. That string is a tool_result block, not
// spoken text: the model reads it and independently decides whether to
// repeat it back, so it does NOT automatically "show up" in the reply the
// user hears. Two real turns went completely silent this way: jarvis_thumbs_up
// and jarvis_note both ran cleanly and the model simply added no text
// afterward. jarvis.askAndSpeak now has a fallback for exactly that case
// (fallbackForSilentReply): when the model returns empty text after a
// successful tool call, it speaks the tool's own result directly, provided
// it's short and single-line, which is why this ack is kept terse and free
// of newlines.
func recordRating(b *brain.Brain, lastTurn LastTurnFn, rating, reason string) (string, error) {
	if b == nil {
		return "I can't record feedback without a brain attached, sir.", nil
	}
	if lastTurn == nil {
		return "I don't recall a recent turn to rate, sir.", nil
	}
	prev := lastTurn()
	if prev == nil {
		return "I don't recall a recent turn to rate, sir.", nil
	}

	toolCalls := make([]brain.JarvisToolCall, len(prev.ToolCalls))
	for i, c := range prev.ToolCalls {
		toolCalls[i] = brain.JarvisToolCall{Name: c.Name, TookMs: c.TookMs, EngineRunID: c.EngineRunID}
	}

	// Engine passthrough: if the rated turn used aida_query and we
	// captured its run id, fire `aida thumbs-* <run-id> --because <reason>`
	// in the background so the engine's lesson table gets the same
	// correction. Fire-and-forget: this tool's return value has already
	// been handed back to the model by the time the goroutine completes,
	// whether or not the model (or the silent-reply fallback in
	// jarvis.askAndSpeak) ends up saying anything to the user, and any
	// failure here is just a missed engine learning, not a broken voice loop.
	if runID := firstEngineRunID(prev.ToolCalls); runID != "" {
		go runEngineFeedback(runID, rating, reason)
	}

	lesson := &brain.JarvisLesson{
		Timestamp:          time.Now().UTC().Format(time.RFC3339),
		RatedTurnStartedAt: prev.StartedAt,
		Transcript:         prev.Transcript,
		Query:              prev.Query,
		Reply:              prev.Reply,
		ToolCalls:          toolCalls,
		Feedback:           rating,
		FeedbackReason:     reason,
	}
	if rating == "note" {
		lesson.FeedbackStyleDirectives = []string{reason}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := b.RecordJarvisLesson(ctx, lesson); err != nil {
		// Don't surface the SQL error to the user - degrade to a spoken
		// note. The error is still in the audit log via the tool error.
		return "", fmt.Errorf("record jarvis lesson: %w", err)
	}
	return "Noted, sir.", nil
}

// IsRatingTool reports whether a tool name is one of the three voice
// feedback tools. Used by the listener to decide whether a turn should
// update the "last turn" cache (rating turns are transparent - they
// don't replace the turn they're rating).
func IsRatingTool(name string) bool {
	switch name {
	case "jarvis_thumbs_up", "jarvis_thumbs_down", "jarvis_note":
		return true
	}
	return false
}

// firstEngineRunID returns the EngineRunID from the first ToolCall that
// has one (typically a aida_query call). Empty when the turn didn't
// touch the engine - disables passthrough rather than firing a racy
// unscoped `aida thumbs-*`.
func firstEngineRunID(calls []audit.ToolCall) string {
	for _, c := range calls {
		if c.EngineRunID != "" {
			return c.EngineRunID
		}
	}
	return ""
}

// runEngineFeedback shells out `aida thumbs-<verb> <run-id> --because
// <reason>` so the engine lesson table gets the same correction the
// jarvis_lessons table just received. The rating string maps to the
// engine subcommand name; "up" → thumbs-up, "down" → thumbs-down,
// "note" → note. Best-effort: errors are swallowed because the user
// has already heard the verbal ack.
func runEngineFeedback(runID, rating, reason string) {
	verb := ""
	switch rating {
	case "up":
		verb = "thumbs-up"
	case "down":
		verb = "thumbs-down"
	case "note":
		verb = "note"
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args := []string{verb, runID}
	if reason != "" {
		args = append(args, "--because", reason)
	}
	_ = exec.CommandContext(ctx, "aida", args...).Run()
}
