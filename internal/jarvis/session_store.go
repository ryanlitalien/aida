package jarvis

// Disk persistence for the listener Session. Without this the short-term
// conversational window lives only in the listener goroutine's stack, so a
// `aida serve` restart (or a crash) starts every conversation blank - the
// "each session starts fresh for me" failure. Persisting the rolling window
// to a small JSON file lets a restart resume the in-flight thread; the
// durable brain thread-memory (jarvis_threads.go) backstops anything older
// than the idle window.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// sessionStatePath returns the on-disk location for the persisted listener
// session: ~/.aida/brain/jarvis/session.json. Lives under the brain repo
// (like the audit log) so the brain auto-commit backs it up across machines.
// Returns "" if the home dir can't be resolved - callers treat that as
// "persistence unavailable" and fall back to an in-memory session.
func sessionStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aida", "brain", "jarvis", "session.json")
}

// LoadSession rehydrates the persisted listener session so a restart resumes
// the in-flight conversation thread. Any failure (missing file, parse error,
// no home dir) degrades to a fresh NewSession() - persistence is purely
// additive. A loaded-but-stale session (idle past its timeout) is discarded
// so a day-old session.json never resurrects dead context.
func LoadSession() *Session {
	path := sessionStatePath()
	if path == "" {
		return NewSession()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return NewSession()
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return NewSession()
	}
	// Restore zero-valued tunables to current defaults so a file written
	// before the window was widened doesn't pin the old small window.
	if s.MaxTurns <= 0 {
		s.MaxTurns = defaultSessionMaxTurns
	}
	if s.IdleTimeout <= 0 {
		s.IdleTimeout = defaultSessionIdleTimeout
	}
	if s.Stale() {
		return NewSession()
	}
	return &s
}

// Save persists the session atomically (tmp + rename). Best-effort and
// concurrency-safe: a non-nil error is advisory (caller logs and continues)
// - a failed save must never break the voice loop. Safe on a nil receiver.
func (s *Session) Save() error {
	if s == nil {
		return nil
	}
	path := sessionStatePath()
	if path == "" {
		return nil
	}
	s.mu.Lock()
	data, err := json.Marshal(s)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
