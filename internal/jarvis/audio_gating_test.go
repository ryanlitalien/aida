package jarvis

import (
	"context"
	"sync"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis/llm"
)

// fakePlayback is a minimal seam-based recorder standing in for playAudio
// (== tts.Play in production). Concurrency-safe since onToolUse and
// progressAck can both be exercised from goroutines in real turns (the
// desk mic and an LMD/Android turn can be in flight on the same process at
// the same time).
type fakePlayback struct {
	mu    sync.Mutex
	calls []string // audio paths passed to playAudio
}

func (f *fakePlayback) play(_ context.Context, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, path)
	return nil
}

func (f *fakePlayback) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// installFakePlayback swaps the package-level playAudio seam for a fake
// recorder and restores the original on test cleanup, so no test can leak
// a fake into another test in the package.
func installFakePlayback(t *testing.T) *fakePlayback {
	t.Helper()
	fake := &fakePlayback{}
	orig := playAudio
	playAudio = fake.play
	t.Cleanup(func() { playAudio = orig })
	return fake
}

// TestOnToolUse_SuppressedSkipsLocalPlayback is the LMD/Android-turn case:
// askAndSpeak marks ctx via withLocalAudioSuppressed whenever speak is
// false (AskTextSilent's path), and that same ctx is what the tool-dispatch
// loop hands to OnToolUse. A slowTools tool call under that ctx must NOT
// play the pre-tool ack locally - the LMD protocol is one-shot
// request/response, so this ack could never reach the phone anyway, and
// playing it on this process's own speakers would leak the phone's turn
// into the room (the bug this fix closes).
func TestOnToolUse_SuppressedSkipsLocalPlayback(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{activity: newActivity(), ackWAV: "ack.wav"}

	ctx := withLocalAudioSuppressed(context.Background())
	a.onToolUse(ctx, llm.ToolUseEvent{Name: "aida_query"})

	if n := fake.count(); n != 0 {
		t.Errorf("onToolUse played local audio %d times under a suppressed ctx, want 0", n)
	}
}

// TestOnToolUse_UnsuppressedPlaysAck is the desk-mic-path case: every
// caller other than AskTextSilent leaves ctx unsuppressed (speak=true),
// and this is a real, daily-driven feature - it must not regress. A
// slowTools tool call under a plain ctx must still play the ack.
func TestOnToolUse_UnsuppressedPlaysAck(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{activity: newActivity(), ackWAV: "ack.wav"}

	a.onToolUse(context.Background(), llm.ToolUseEvent{Name: "aida_query"})

	if n := fake.count(); n != 1 {
		t.Fatalf("onToolUse played local audio %d times under a plain ctx, want 1", n)
	}
	if fake.calls[0] != "ack.wav" {
		t.Errorf("onToolUse played %q, want the ack wav", fake.calls[0])
	}
}

// TestOnToolUse_FastToolNeverAcks guards the pre-existing slowTools gate
// alongside the new suppression gate - a fast tool (current_time) must
// still skip the ack even when nothing is suppressed, so the new check is
// additive, not a replacement.
func TestOnToolUse_FastToolNeverAcks(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{activity: newActivity(), ackWAV: "ack.wav"}

	a.onToolUse(context.Background(), llm.ToolUseEvent{Name: "current_time"})

	if n := fake.count(); n != 0 {
		t.Errorf("onToolUse played local audio %d times for a fast tool, want 0", n)
	}
}

// TestProgressAck_SuppressedSkipsLocalPlayback mirrors
// TestOnToolUse_SuppressedSkipsLocalPlayback for the second ack site: the
// 90s mid-progress beat fired from inside aidaQueryTool's Run. It receives
// the turn's ctx (see aidaQueryTool's doc comment for that plumbing) and
// must stay silent locally under suppression.
func TestProgressAck_SuppressedSkipsLocalPlayback(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{midProgressWAV: "progress.wav"}

	ctx := withLocalAudioSuppressed(context.Background())
	a.progressAck(ctx)

	if n := fake.count(); n != 0 {
		t.Errorf("progressAck played local audio %d times under a suppressed ctx, want 0", n)
	}
}

// TestProgressAck_UnsuppressedPlays is the desk-mic-path case: an
// unsuppressed ctx must still play the mid-progress beat, unchanged from
// before this fix.
func TestProgressAck_UnsuppressedPlays(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{midProgressWAV: "progress.wav"}

	a.progressAck(context.Background())

	if n := fake.count(); n != 1 {
		t.Fatalf("progressAck played local audio %d times under a plain ctx, want 1", n)
	}
	if fake.calls[0] != "progress.wav" {
		t.Errorf("progressAck played %q, want the mid-progress wav", fake.calls[0])
	}
}

// TestProgressAck_EmptyWAVNeverPlays guards the pre-existing
// pre-synthesis-failed gate alongside the new suppression gate: an empty
// midProgressWAV (synth failed at startup) must still skip playback even
// under a plain, unsuppressed ctx.
func TestProgressAck_EmptyWAVNeverPlays(t *testing.T) {
	fake := installFakePlayback(t)
	a := &Assistant{} // midProgressWAV left empty

	a.progressAck(context.Background())

	if n := fake.count(); n != 0 {
		t.Errorf("progressAck played local audio %d times with no pre-synthesized wav, want 0", n)
	}
}
