package orchestrator

import (
	"sync"

	"github.com/ryanlitalien/aida/internal/engine/orchestrator/model"
)

// Session is a resumable, addressable conversation state. The minimal
// interface stores the rolling message history; richer implementations
// may back it with durable storage.
type Session interface {
	ID() string
	Messages() []model.Message
	Append(msgs ...model.Message)
	Reset()
}

// MemorySession is an in-memory Session. Safe for concurrent use.
type MemorySession struct {
	id   string
	mu   sync.Mutex
	msgs []model.Message
}

// NewMemorySession constructs a MemorySession with the given ID.
func NewMemorySession(id string) *MemorySession {
	return &MemorySession{id: id}
}

// ID implements Session.
func (s *MemorySession) ID() string { return s.id }

// Messages returns a copy of the current message history.
func (s *MemorySession) Messages() []model.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.Message, len(s.msgs))
	copy(out, s.msgs)
	return out
}

// Append adds messages to the history.
func (s *MemorySession) Append(msgs ...model.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msgs...)
}

// Reset clears all messages.
func (s *MemorySession) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = nil
}
