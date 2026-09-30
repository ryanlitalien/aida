package arbiter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// stopHookPayload is the subset of a Claude Code Stop hook's stdin JSON
// this package needs. docs/arbiter-plan.md section 5's open question --
// whether Stop fires on a usage-limit refusal or only on a normal turn
// end -- is exactly what a hook built on ParseClaudeStopHook is meant to
// help answer; this parser doesn't presume an answer either way, it just
// extracts what the hook was given.
type stopHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// transcriptLine is one JSONL record in a Claude Code transcript file.
// Only Type and Message are read; every other field (uuid, timestamp,
// parentUuid, ...) is left alone -- an unrecognized or malformed line is
// tolerated, never a hard failure, since a hook running mid-session must
// never crash on a transcript shape it hasn't seen before.
type transcriptLine struct {
	Type    string         `json:"type"`
	Message *transcriptMsg `json:"message,omitempty"`
}

type transcriptMsg struct {
	Content json.RawMessage `json:"content"`
}

// transcriptContentBlock is one element of an assistant message's content
// array (the shape Claude Code uses for anything beyond a single string
// -- "text" blocks interleaved with "tool_use", "thinking", etc).
type transcriptContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseClaudeStopHook reads a Claude Code Stop hook's JSON payload from r
// (session_id, transcript_path, stop_hook_active) and returns the
// transcript path plus the text of the last assistant message found in
// the JSONL transcript at that path. An empty lastAssistantText with a
// nil error means the transcript had no assistant messages at all --
// callers must not treat that as VerdictEmpty on its own; ClassifySignal
// already treats empty text as Ambiguous.
func ParseClaudeStopHook(r io.Reader) (transcriptPath, lastAssistantText string, err error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", "", fmt.Errorf("arbiter: reading stop hook payload: %w", err)
	}
	var payload stopHookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", "", fmt.Errorf("arbiter: parsing stop hook payload: %w", err)
	}
	if payload.TranscriptPath == "" {
		return "", "", fmt.Errorf("arbiter: stop hook payload has no transcript_path")
	}

	text, err := lastAssistantMessage(payload.TranscriptPath)
	if err != nil {
		return payload.TranscriptPath, "", err
	}
	return payload.TranscriptPath, text, nil
}

// lastAssistantMessage scans path's JSONL transcript top to bottom and
// returns the text content of the last line whose type is "assistant".
// Lines this package doesn't recognize (unknown type, malformed JSON, a
// message whose content isn't a string or a text-block array) are
// skipped rather than failing the whole read -- a transcript accumulates
// over an entire session, so one odd line must never blank out every
// assistant turn before it.
func lastAssistantMessage(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("arbiter: opening transcript %q: %w", path, err)
	}
	defer f.Close()

	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			continue
		}
		if tl.Type != "assistant" || tl.Message == nil {
			continue
		}
		if text := extractText(tl.Message.Content); text != "" {
			last = text
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("arbiter: reading transcript %q: %w", path, err)
	}
	return last, nil
}

// extractText pulls plain text out of a transcript message's content
// field, which Claude Code renders either as a bare string or as an
// array of typed blocks (only "text" blocks contribute; "tool_use",
// "thinking", and anything else are silently skipped).
func extractText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var blocks []transcriptContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}
