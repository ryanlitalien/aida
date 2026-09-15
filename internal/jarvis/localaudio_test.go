package jarvis

import (
	"context"
	"testing"
)

// TestLocalAudioSuppressed_DefaultFalse guards the safe default: a plain,
// never-marked context (what every desk-mic/typed-CLI/HTTP-text caller
// uses) must report unsuppressed, or the desk mic's ack would go silent
// by accident.
func TestLocalAudioSuppressed_DefaultFalse(t *testing.T) {
	if localAudioSuppressed(context.Background()) {
		t.Error("a plain context.Background() must not report suppressed")
	}
}

// TestLocalAudioSuppressed_NilContext guards the nil-safe path - callers
// (tests, defensive code) may pass a nil context and must get the same
// safe default rather than a panic.
func TestLocalAudioSuppressed_NilContext(t *testing.T) {
	if localAudioSuppressed(nil) {
		t.Error("a nil context must not report suppressed")
	}
}

// TestWithLocalAudioSuppressed_MarksTrue is the core contract: after
// wrapping, the flag reads back true on the SAME context.
func TestWithLocalAudioSuppressed_MarksTrue(t *testing.T) {
	ctx := withLocalAudioSuppressed(context.Background())
	if !localAudioSuppressed(ctx) {
		t.Error("withLocalAudioSuppressed(ctx) did not mark ctx as suppressed")
	}
}

// TestLocalAudioSuppressed_DerivedContextInherits mirrors what actually
// happens across a real turn: askAndSpeak marks the ctx once, then
// AskWithHistory, the tool-dispatch loop, and individual tools each
// derive further contexts (WithTimeout, WithCancel, WithValue for
// unrelated keys) from it. Every one of those descendants must still see
// the suppression - this is exactly why context.Value, not a
// side-channel, is the right carrier here.
func TestLocalAudioSuppressed_DerivedContextInherits(t *testing.T) {
	suppressed := withLocalAudioSuppressed(context.Background())

	withTimeout, cancel := context.WithTimeout(suppressed, 0)
	defer cancel()
	if !localAudioSuppressed(withTimeout) {
		t.Error("a context.WithTimeout child of a suppressed context lost the flag")
	}

	withCancel, cancel2 := context.WithCancel(suppressed)
	defer cancel2()
	if !localAudioSuppressed(withCancel) {
		t.Error("a context.WithCancel child of a suppressed context lost the flag")
	}

	type otherKey struct{}
	withOtherValue := context.WithValue(suppressed, otherKey{}, "unrelated")
	if !localAudioSuppressed(withOtherValue) {
		t.Error("layering an unrelated context.WithValue lost the suppression flag")
	}

	// The flag must survive cancellation too - progressAck's 90s-later
	// check may run well after the turn's own ctx has been cancelled, and
	// ctx.Value must stay readable regardless.
	cancel()
	if !localAudioSuppressed(withTimeout) {
		t.Error("suppression flag became unreadable after the derived context was cancelled")
	}
}

// TestLocalAudioSuppressed_SiblingDoesNotLeak guards the concurrency
// property the whole design depends on: the desk mic and an LMD/phone
// turn can be in flight on the SAME process at the SAME time, sharing one
// Assistant/Registry. Marking one turn's context must never bleed into a
// completely independent context.Background()-rooted turn.
func TestLocalAudioSuppressed_SiblingDoesNotLeak(t *testing.T) {
	suppressed := withLocalAudioSuppressed(context.Background())
	sibling := context.Background()

	if !localAudioSuppressed(suppressed) {
		t.Fatal("test setup: suppressed context did not report suppressed")
	}
	if localAudioSuppressed(sibling) {
		t.Error("an independent context.Background() reported suppressed - flag leaked across turns")
	}
}

// TestWithLocalAudioSuppressed_Idempotent guards against a double-wrap
// (e.g. a future call site accidentally wrapping twice) silently flipping
// the flag off or panicking.
func TestWithLocalAudioSuppressed_Idempotent(t *testing.T) {
	ctx := withLocalAudioSuppressed(withLocalAudioSuppressed(context.Background()))
	if !localAudioSuppressed(ctx) {
		t.Error("double-wrapping withLocalAudioSuppressed should still report suppressed")
	}
}
