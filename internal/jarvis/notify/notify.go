// Package notify is Jarvis's proactive voice notification queue.
//
// Long-running background agents (job_start in the Jarvis tools, the
// aida --agent subprocess underneath) need to surface progress and
// awaiting_input prompts to the user. We deliberately do NOT speak
// them as soon as they arrive - that risks Whisper picking up Jarvis's
// own TTS while it's listening, plus it interrupts whatever the user
// is doing. Instead, notifications accumulate in a buffered queue and
// drain on the user's NEXT wake-triggered turn (right after the wake
// regex hits, before the LLM runs). The user is already engaged at
// that point and the mic will catch their reply, not Jarvis's voice.
//
// One Notifier per daemon process. Constructed in serve.go alongside
// the jarvis.Assistant; passed to the listener so it can call
// DrainOnNextWake at the start of each turn, and passed to the
// daemon's job-watch goroutine so it can Enqueue when transitions
// fire.
package notify

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/ryanlitalien/aida/internal/jarvis/tts"
)

// queueCap is the buffered channel capacity. Sized for several
// concurrent long-running jobs each producing a few notifications
// before the user gets back - generous enough that Enqueue never
// blocks in practice, small enough that a runaway producer can't
// pin the process's memory.
const queueCap = 64

// Notifier is the goroutine-safe handle the daemon hands to the
// job-watch loop (Enqueue) and to the listener (DrainOnNextWake).
type Notifier struct {
	queue chan string
	tts   tts.Synthesizer

	// volume is the playback gain passed to tts.PlayAt for spoken
	// notifications, matching whichever persona's TTS handle this
	// Notifier shares (see SetVolume). 0 falls back to the tts package
	// default.
	volume float64

	// desktop enables the immediate banner+chime side effect in
	// Enqueue (see desktop.go). Set via EnableDesktop by the HTTP
	// daemon; stays false in tests and stdio mode.
	desktop bool

	// mu guards drainInFlight so two simultaneous wakes (in theory)
	// can't double-speak the same queue. In practice the listener
	// is single-goroutine so only one DrainOnNextWake runs at a
	// time, but the lock is cheap and forecloses a class of bugs.
	mu            sync.Mutex
	drainInFlight bool
}

// New constructs a Notifier with the given TTS synthesizer. nil tts
// is tolerated - DrainOnNextWake becomes a no-op. Useful for tests
// and for daemons running in --no-jarvis mode.
func New(t tts.Synthesizer) *Notifier {
	return &Notifier{
		queue: make(chan string, queueCap),
		tts:   t,
	}
}

// Enqueue adds one utterance to the queue. Non-blocking: if the
// queue is full (>64 pending) the message is dropped on the floor
// with a stderr warning. That ceiling is high enough that hitting
// it indicates a runaway loop, not normal traffic.
func (n *Notifier) Enqueue(msg string) {
	if n == nil || msg == "" {
		return
	}
	select {
	case n.queue <- msg:
		// Immediate out-of-band ping: banner + chime the moment the
		// event fires, speech still deferred to the next wake. Own
		// goroutine so osascript/afplay latency never stalls the
		// 2s job-watch tick.
		if n.desktop {
			go notifyDesktop(msg)
		}
	default:
		fmt.Fprintf(os.Stderr, "notify: queue full, dropping: %s\n", msg)
	}
}

// SetVolume sets the playback gain used for spoken notifications, so they
// match the level of whichever persona's TTS handle this Notifier shares
// (see serve.go, which calls this with the Jarvis persona's configured
// Volume right after notify.New). Safe on a nil receiver.
func (n *Notifier) SetVolume(v float64) {
	if n != nil {
		n.volume = v
	}
}

// Pending returns the number of queued utterances. Useful for the
// listener to decide whether to play a "stand by, sir" beat before
// drilling into the queue. Concurrent-safe - len on a channel is
// atomic.
func (n *Notifier) Pending() int {
	if n == nil {
		return 0
	}
	return len(n.queue)
}

// DrainOnNextWake synthesizes and plays every queued utterance in
// order. Blocks until the queue is empty or ctx is cancelled
// (cancellation is treated as a graceful early exit - partially
// drained queues will resume on the next wake).
//
// Idempotent under concurrent callers via drainInFlight: a second
// caller returns immediately rather than racing against the first
// for the same channel reads.
func (n *Notifier) DrainOnNextWake(ctx context.Context) {
	if n == nil || n.tts == nil {
		return
	}
	n.mu.Lock()
	if n.drainInFlight {
		n.mu.Unlock()
		return
	}
	n.drainInFlight = true
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.drainInFlight = false
		n.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-n.queue:
			n.speak(ctx, msg)
		default:
			// Queue empty - done draining.
			return
		}
	}
}

// speak synthesizes one utterance and plays it. Errors are logged
// and swallowed; one botched notification shouldn't stop the rest
// of the queue from delivering.
func (n *Notifier) speak(ctx context.Context, msg string) {
	wav, err := n.tts.Synthesize(ctx, msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "notify: synth failed for %q: %v\n", msg, err)
		return
	}
	defer os.Remove(wav)
	if err := tts.PlayAt(ctx, wav, n.volume); err != nil {
		fmt.Fprintf(os.Stderr, "notify: play failed: %v\n", err)
	}
}
