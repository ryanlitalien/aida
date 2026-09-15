package jarvis

import "context"

// localAudioSuppressedKey is the context key carrying whether the CURRENT
// turn must not play any acknowledgement/progress audio through THIS
// process's local speakers.
//
// This exists because a turn's origin - desk mic vs. the Android LMD
// client - determines whether the pre-tool "One moment, sir." ack and the
// 90s mid-progress beat are allowed to play locally. The LMD protocol is a
// one-shot request/response (see docs/lmd-protocol.md), so a mid-turn ack
// can never reach the phone; playing it on this process's own speakers
// instead would leak a remote user's turn into the room. speak=false
// callers (AskTextSilent, the LMD entry point) suppress; every other
// caller - the desk-mic listener, typed CLI, HTTP text-only callers - is
// unaffected and keeps the existing ack behavior.
//
// Deliberately a request-scoped context value rather than a new bool
// threaded through every layer by hand: the turn's ctx already flows from
// askAndSpeak down through AskWithHistory, the tool-dispatch loop, and
// into each tool's Run, so piggybacking on it reaches every ack site for
// free instead of inventing a parallel plumbing mechanism. The key type is
// unexported so nothing outside this package can forge suppression (or
// the lack of it) for a turn it doesn't own.
type localAudioSuppressedKey struct{}

// withLocalAudioSuppressed returns a context derived from ctx that marks
// the current turn as one whose acknowledgement/progress audio must NOT
// play through this process's local speakers. Call this once, before the
// LLM/tool-dispatch pipeline starts (askAndSpeak, when speak is false), so
// every downstream consumer - tool dispatch, the pre-tool ack, the
// mid-progress ack - observes the same decision for the whole turn.
func withLocalAudioSuppressed(ctx context.Context) context.Context {
	return context.WithValue(ctx, localAudioSuppressedKey{}, true)
}

// localAudioSuppressed reports whether ctx (or an ancestor it was derived
// from) was marked via withLocalAudioSuppressed. A nil ctx, or one that
// was never marked, reports false - the safe default that preserves the
// desk mic's existing ack behavior for every caller that doesn't opt in.
func localAudioSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(localAudioSuppressedKey{}).(bool)
	return v
}
