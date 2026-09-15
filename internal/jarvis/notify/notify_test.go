package notify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// stubSynth implements tts.Synthesizer by writing a marker file per
// call. We can't exercise the audio path (no afplay in CI), but we
// can verify ordering, idempotency, and queue-empty behavior.
type stubSynth struct {
	dir   string
	calls []string
}

func (s *stubSynth) Name() string { return "stub" }

func (s *stubSynth) Synthesize(_ context.Context, text string) (string, error) {
	s.calls = append(s.calls, text)
	// Return a path to an empty file so tts.Play has something to
	// hand to afplay; afplay will fail but the notifier swallows it.
	p := filepath.Join(s.dir, "stub.wav")
	_ = os.WriteFile(p, nil, 0644)
	return p, nil
}

func TestNotifyEnqueueAndPending(t *testing.T) {
	n := New(nil)
	if n.Pending() != 0 {
		t.Fatalf("expected empty queue, got %d", n.Pending())
	}
	n.Enqueue("hello")
	n.Enqueue("world")
	if n.Pending() != 2 {
		t.Errorf("expected 2 pending, got %d", n.Pending())
	}
}

func TestNotifyDrainSpeaksInOrder(t *testing.T) {
	dir := t.TempDir()
	s := &stubSynth{dir: dir}
	n := New(s)
	n.Enqueue("one")
	n.Enqueue("two")
	n.Enqueue("three")

	// Drain. The Play call will error (no audio device) but the
	// Synthesize calls should still happen in order.
	n.DrainOnNextWake(context.Background())

	if len(s.calls) != 3 {
		t.Fatalf("expected 3 synth calls, got %d (%v)", len(s.calls), s.calls)
	}
	for i, want := range []string{"one", "two", "three"} {
		if s.calls[i] != want {
			t.Errorf("call %d: want %q got %q", i, want, s.calls[i])
		}
	}
	if n.Pending() != 0 {
		t.Errorf("expected drained queue, got %d", n.Pending())
	}
}

func TestNotifyDrainEmptyQueueIsFastNoOp(t *testing.T) {
	dir := t.TempDir()
	s := &stubSynth{dir: dir}
	n := New(s)
	n.DrainOnNextWake(context.Background())
	if len(s.calls) != 0 {
		t.Errorf("expected zero calls on empty drain, got %d", len(s.calls))
	}
}

func TestNotifyEnqueueNilNotifier(t *testing.T) {
	var n *Notifier
	// Should not panic on a nil receiver - the daemon may run in
	// --no-jarvis mode where the notifier was never constructed.
	n.Enqueue("ignore me")
	n.DrainOnNextWake(context.Background())
}

func TestNotifyDrainNoSynthesizer(t *testing.T) {
	// nil synthesizer is tolerated; drain becomes a no-op so the
	// listener can call it unconditionally.
	n := New(nil)
	n.Enqueue("queued but never spoken")
	n.DrainOnNextWake(context.Background())
	// The message stays in the queue because we returned early.
	if n.Pending() != 1 {
		t.Errorf("expected queue intact when no synth, got %d", n.Pending())
	}
}

// chimePath prefers a user-dropped ~/.aida/jarvis/notify-sound.* file
// (lexicographically first when several exist) and falls back to the
// built-in Hero chime.
func TestChimePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := chimePath(); got != fallbackChime {
		t.Errorf("no custom file: chimePath() = %q, want fallback %q", got, fallbackChime)
	}

	dir := filepath.Join(os.Getenv("HOME"), ".aida", "jarvis")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "notify-sound.mp3")
	if err := os.WriteFile(custom, []byte("not really audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := chimePath(); got != custom {
		t.Errorf("chimePath() = %q, want custom %q", got, custom)
	}

	// Two candidates: deterministic lexicographic pick.
	second := filepath.Join(dir, "notify-sound.aiff")
	if err := os.WriteFile(second, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := chimePath(); got != second { // .aiff < .mp3
		t.Errorf("chimePath() = %q, want lexicographically first %q", got, second)
	}
}

// EnableDesktop is nil-safe and Enqueue with desktop off must not fork
// anything (implicitly covered: tests would hang/fail loudly on missing
// binaries if it did - the flag defaults to false).
func TestEnableDesktopNilSafe(t *testing.T) {
	var n *Notifier
	n.EnableDesktop() // must not panic
	n2 := New(nil)
	n2.EnableDesktop()
	if !n2.desktop {
		t.Error("EnableDesktop did not set the flag")
	}
}
