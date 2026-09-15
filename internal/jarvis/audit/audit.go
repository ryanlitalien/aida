// Package audit appends a single NDJSON line per real Jarvis interaction to
// ~/.aida/brain/jarvis/audit.ndjson. Intended for offline review: which
// queries fired, which replies got synthesized, where Jarvis got things
// wrong. Lives under the brain repo so the brain auto-commit picks it up
// for cross-machine recovery - it is NOT vector-indexed into brain.db.
//
// Only logs interactions that triggered the wake phrase - skipped utterances,
// VAD blips, and whisper hallucinations are NOT logged. The point is to
// audit Jarvis's actual behavior, not to mirror raw mic input.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ToolCall captures the name and duration of one in-loop tool execution so
// audit records can show "the wait was aida_query, eight seconds". Error
// is the tool's error string if it failed; omitted on success.
// EngineRunID is set only when the tool was aida_query - it captures
// the underlying `aida` run id so voice feedback tools can target the
// exact engine run rather than guessing "most recent aida invocation".
type ToolCall struct {
	Name        string `json:"name"`
	TookMs      int64  `json:"took_ms"`
	Error       string `json:"error,omitempty"`
	EngineRunID string `json:"engine_run_id,omitempty"`
}

// Record is one row in the audit log. Per-stage timings let you spot where
// long round-trips spent their time without re-running the query.
type Record struct {
	Timestamp  string     `json:"ts"`
	StartedAt  string     `json:"started_at,omitempty"` // wake-fire time (RFC3339)
	Transcript string     `json:"transcript"`           // raw whisper output
	Query      string     `json:"query"`                // text after wake phrase
	Reply      string     `json:"reply,omitempty"`      // Jarvis's spoken answer
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"` // per-tool timings
	LLMMs      int64      `json:"llm_ms,omitempty"`     // sum of Anthropic round-trips
	ToolMs     int64      `json:"tool_ms,omitempty"`    // sum of tool runs
	TTSMs      int64      `json:"tts_ms,omitempty"`     // Piper synthesis
	PlayMs     int64      `json:"play_ms,omitempty"`    // afplay duration of the reply
	TookMs     int64      `json:"took_ms,omitempty"`    // total wall-clock round-trip
	Error      string     `json:"error,omitempty"`
	WakeOnly   bool       `json:"wake_only,omitempty"`

	// GroundingRewritten is true when the grounding guard replaced a phantom
	// background-job claim before synthesis. OriginalReply preserves the
	// model's pre-rewrite text for offline review.
	GroundingRewritten bool   `json:"grounding_rewritten,omitempty"`
	OriginalReply      string `json:"original_reply,omitempty"`

	// EmptyReplyFallback is true when the model returned no text at all
	// despite a tool succeeding this turn, and Jarvis substituted a
	// spoken fallback instead of going completely silent. See
	// jarvis.fallbackForSilentReply.
	EmptyReplyFallback bool `json:"empty_reply_fallback,omitempty"`
}

// Logger writes audit records to disk. Concurrency-safe.
type Logger struct {
	path string
	mu   sync.Mutex
}

// New returns a Logger writing to ~/.aida/brain/jarvis/audit.ndjson by
// default. Pass an explicit path to override (e.g. for tests).
//
// One-shot migration: if the legacy ~/.aida/jarvis/audit.ndjson exists
// and the new path does not, the legacy file is moved into place so prior
// turns are preserved across the relocation.
func New(path string) (*Logger, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".aida", "brain", "jarvis", "audit.ndjson")
		legacy := filepath.Join(home, ".aida", "jarvis", "audit.ndjson")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if _, lerr := os.Stat(legacy); lerr == nil {
				if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr == nil {
					// Best-effort: rename works within the same fs.
					// Failure is non-fatal - caller can keep logging
					// to the new path; legacy file stays for manual
					// recovery.
					_ = os.Rename(legacy, path)
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Logger{path: path}, nil
}

// Path returns the absolute log file path.
func (l *Logger) Path() string { return l.path }

// Append writes one record. Errors writing the log are intentionally
// non-fatal - auditing should never break the voice loop.
func (l *Logger) Append(r Record) {
	if r.Timestamp == "" {
		r.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-audit: open %s: %v\n", l.path, err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	if err := enc.Encode(r); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis-audit: encode: %v\n", err)
	}
}
