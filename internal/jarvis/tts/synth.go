// Package tts wraps text-to-speech engines behind a single interface so the
// Jarvis pipeline can swap between Piper, ElevenLabs, macOS `say`, etc.
package tts

import (
	"context"
	"time"
)

// Synthesizer turns text into a WAV file path on disk that callers can play.
// The returned path is owned by the caller (delete after use).
type Synthesizer interface {
	Synthesize(ctx context.Context, text string) (wavPath string, err error)
	Name() string
}

// StreamStats reports timing for a SpeakStream call.
type StreamStats struct {
	TTFA  time.Duration // wall time until the first audio bytes arrive (≈ time-to-first-sound)
	Total time.Duration // wall time until playback finishes
}

// StreamingSynthesizer is an optional capability: synthesize and play audio as
// it arrives, rather than synthesizing the whole clip to a file and then
// playing it. For an engine whose API streams (ElevenLabs), this drops
// time-to-first-sound from "whole clip generated + downloaded" to "first audio
// chunk". Implementations play the audio themselves and return when playback
// completes. Callers that don't care about streaming can keep using
// Synthesize + Play.
type StreamingSynthesizer interface {
	Synthesizer
	SpeakStream(ctx context.Context, text string) (StreamStats, error)
}
