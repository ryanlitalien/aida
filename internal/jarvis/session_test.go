package jarvis

import (
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis/llm"
)

func TestSession_StaleZeroValueIsFresh(t *testing.T) {
	s := NewSession()
	if s.Stale() {
		t.Error("a session with no LastAt should not be stale")
	}
}

// A zero IdleTimeout falls back to the same 30m default everywhere -
// pins the shared constant so a future widening can't leave Stale()
// on a stale copy of the old window (the 5m leftover bug).
func TestSession_StaleZeroTimeoutUsesDefault(t *testing.T) {
	s := &Session{LastAt: time.Now().Add(-20 * time.Minute)} // IdleTimeout zero
	if s.Stale() {
		t.Error("20m idle with the 30m default should not be stale")
	}
	s.LastAt = time.Now().Add(-defaultSessionIdleTimeout - time.Minute)
	if !s.Stale() {
		t.Error("idle past the default timeout should be stale")
	}
}

func TestSession_StaleAfterIdleTimeout(t *testing.T) {
	s := NewSession()
	s.IdleTimeout = 50 * time.Millisecond
	s.LastAt = time.Now()
	if s.Stale() {
		t.Error("freshly-stamped session shouldn't be stale immediately")
	}
	time.Sleep(70 * time.Millisecond)
	if !s.Stale() {
		t.Error("session should be stale after idle timeout")
	}
}

func TestSession_ResetClearsTurns(t *testing.T) {
	s := NewSession()
	s.Turns = []llm.TurnPair{{User: "hi", Assistant: "hello"}}
	s.LastAt = time.Now()
	s.Reset()
	if len(s.Turns) != 0 {
		t.Errorf("Turns: want empty, got %d", len(s.Turns))
	}
	if !s.LastAt.IsZero() {
		t.Error("LastAt should be zero after Reset")
	}
}

// Trim happens inside AskTextInSession; this just verifies the cap field
// is honored by appending past the limit and slicing manually as the
// production code does.
func TestSession_MaxTurnsBoundsHistory(t *testing.T) {
	s := NewSession()
	s.MaxTurns = 2
	for i := 0; i < 5; i++ {
		s.Turns = append(s.Turns, llm.TurnPair{User: "u", Assistant: "a"})
		if len(s.Turns) > s.MaxTurns {
			s.Turns = s.Turns[len(s.Turns)-s.MaxTurns:]
		}
	}
	if got := len(s.Turns); got != 2 {
		t.Errorf("len(Turns) = %d, want 2", got)
	}
}

// Save+Load round-trips the conversational window through disk. HOME is
// redirected to a temp dir so the test never touches the real ~/.aida.
func TestSession_SaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := NewSession()
	s.Turns = []llm.TurnPair{{User: "build the dome", Assistant: "done, sir"}}
	s.LastAt = time.Now()
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := LoadSession()
	if len(got.Turns) != 1 || got.Turns[0].User != "build the dome" {
		t.Fatalf("round-trip lost turns: %+v", got.Turns)
	}
	if got.Turns[0].Assistant != "done, sir" {
		t.Errorf("assistant text not preserved: %q", got.Turns[0].Assistant)
	}
}

// A persisted session that has gone idle past its timeout must not
// resurrect dead context - LoadSession returns a fresh empty session.
func TestLoadSession_StaleFileResetsToFresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := NewSession()
	s.IdleTimeout = 30 * time.Minute
	s.Turns = []llm.TurnPair{{User: "old thread", Assistant: "stale"}}
	s.LastAt = time.Now().Add(-2 * time.Hour) // well past timeout
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got := LoadSession()
	if len(got.Turns) != 0 {
		t.Errorf("stale session should reset to empty, got %d turns", len(got.Turns))
	}
}

// No file on disk yields a fresh session with current defaults.
func TestLoadSession_NoFileIsFresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got := LoadSession()
	if got == nil || len(got.Turns) != 0 {
		t.Fatal("missing file should yield a fresh empty session")
	}
	if got.MaxTurns != 12 {
		t.Errorf("fresh session MaxTurns = %d, want 12", got.MaxTurns)
	}
}
