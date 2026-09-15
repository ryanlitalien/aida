package ui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// braille animation frames used by the spinner.
var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Quiet, when true, suppresses ALL spinner output globally. Used by
// run-dir mode where TTY output corrupts subprocess capture.
// PersistentPreRun in root.go sets this from the --run-dir flag.
var Quiet bool

// Spinner provides a simple animated progress indicator that writes to stderr
// so that stdout remains clean for piping.
type Spinner struct {
	mu      sync.Mutex
	msg     string
	running bool
	quiet   bool
	done    chan struct{}
}

// NewSpinner returns a ready-to-use Spinner. Call Start to begin
// animation. When ui.Quiet is true (e.g. --run-dir mode), returns a
// no-op spinner so callers don't have to thread a quiet flag.
func NewSpinner() *Spinner {
	if Quiet {
		return &Spinner{quiet: true}
	}
	return &Spinner{}
}

// NewQuietSpinner returns a Spinner whose Start/Stop/Update are no-ops.
// Used by callers that want the same Spinner shape but no terminal
// output - typically subprocess-mode runs writing structured events
// to disk instead of TTY.
func NewQuietSpinner() *Spinner {
	return &Spinner{quiet: true}
}

// Start begins the spinner animation with the given message.
// It is safe to call Start multiple times; subsequent calls are no-ops
// until Stop is called.
func (s *Spinner) Start(msg string) {
	if s.quiet {
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.msg = msg
	s.running = true
	s.done = make(chan struct{})
	s.mu.Unlock()

	go s.animate()
}

// Update changes the message displayed next to the spinner without
// restarting it. If the spinner is not running this is a no-op.
func (s *Spinner) Update(msg string) {
	if s.quiet {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msg = msg
}

// Stop halts the spinner animation and replaces the spinner line with
// the provided final status message.
func (s *Spinner) Stop(msg string) {
	if s.quiet {
		return
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.mu.Unlock()

	// Wait for the animation goroutine to finish.
	<-s.done

	// Clear the current line and write the final message.
	fmt.Fprintf(os.Stderr, "\r\033[K%s\n", msg)
}

// animate runs in a goroutine and cycles through braille frames on stderr.
func (s *Spinner) animate() {
	defer close(s.done)

	idx := 0
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()

	for {
		s.mu.Lock()
		if !s.running {
			s.mu.Unlock()
			return
		}
		msg := s.msg
		s.mu.Unlock()

		frame := frames[idx%len(frames)]
		fmt.Fprintf(os.Stderr, "\r\033[K%s %s", frame, msg)
		idx++

		<-ticker.C
	}
}
