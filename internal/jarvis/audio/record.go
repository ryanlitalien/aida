// Package audio handles microphone capture and playback for the Jarvis
// pipeline. The current implementation shells out to ffmpeg with the
// avfoundation input - adequate for the demo, swappable for malgo later when
// we want continuous capture and barge-in.
package audio

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// RecordOptions configures a single push-to-talk recording.
type RecordOptions struct {
	Seconds  int    // wall-clock duration
	Device   string // avfoundation index, default ":0" (default mic)
	OutDir   string // dest dir, default os.TempDir()
	Format   string // "wav" only for now
	Loglevel string // ffmpeg loglevel, default "error"
}

// Record captures `opts.Seconds` of audio from the macOS default microphone
// and returns the resulting WAV path. Caller deletes when done.
func Record(ctx context.Context, opts RecordOptions) (string, error) {
	if opts.Seconds <= 0 {
		opts.Seconds = 5
	}
	if opts.Device == "" {
		opts.Device = ":0"
	}
	if opts.OutDir == "" {
		opts.OutDir = os.TempDir()
	}
	if opts.Loglevel == "" {
		opts.Loglevel = "error"
	}
	out := filepath.Join(opts.OutDir, fmt.Sprintf("jarvis-rec-%d.wav", time.Now().UnixNano()))

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-loglevel", opts.Loglevel,
		"-f", "avfoundation",
		"-i", opts.Device,
		"-t", fmt.Sprintf("%d", opts.Seconds),
		"-ar", "16000", // 16kHz, what whisper expects
		"-ac", "1", // mono
		"-y", // overwrite without asking
		out,
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg record failed: %w (%s)", err, string(combined))
	}
	return out, nil
}
