package tts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// loudnormDefaultI is the integrated loudness target in LUFS. Streaming
// platforms target -14, broadcast -23, podcasts often -16. -12 is
// "noticeably louder than streaming" without being broadcast-shouty.
// Override via AIDA_TTS_LOUDNORM_I=<float|"off">.
const loudnormDefaultI = -12.0

// afplayDefaultVolume is afplay's volume multiplier on top of the
// system volume. 1.0 is system level; 1.3 = +30%. afplay clamps
// internally so over-driving past clipping just hits the ceiling.
// Override via AIDA_TTS_VOLUME=<float>. This is the fallback used when
// no per-persona volume is set (see afplayVolumeFor).
const afplayDefaultVolume = 1.3

// Play streams an audio file (WAV or MP3) to the default speakers via
// afplay (macOS), using the package default volume. Before playback, the
// file is run through an ffmpeg loudnorm pass to equalize loudness across
// utterances, ElevenLabs's raw output drifts noticeably between phrases
// and the inconsistency was annoying to listen to.
//
// Blocks until playback completes or ctx is cancelled (cancellation
// kills afplay for barge-in).
//
// Non-macOS callers must port both the ffmpeg invocation (which is
// portable) and the afplay step (which is not).
func Play(ctx context.Context, audioPath string) error {
	return PlayAt(ctx, audioPath, 0)
}

// PlayAt is Play with an explicit per-persona volume. volume <= 0 falls
// back to the package default (see afplayVolumeFor); AIDA_TTS_VOLUME still
// overrides either way.
func PlayAt(ctx context.Context, audioPath string, volume float64) error {
	playPath := audioPath
	if normalized, ok := normalizeLoudness(ctx, audioPath); ok {
		playPath = normalized
		defer os.Remove(normalized)
	}

	args := []string{"-v", afplayVolumeFor(volume), playPath}
	cmd := exec.CommandContext(ctx, "afplay", args...)
	if err := cmd.Run(); err != nil {
		// Treat ctx.Done as success, barge-in is a normal flow.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("afplay failed: %w", err)
	}
	return nil
}

// normalizeLoudness runs ffmpeg's single-pass loudnorm filter and
// returns the path to a freshly-written normalized WAV. Failures
// (ffmpeg missing, env override set to "off", filter error) return
// ok=false so the caller falls back to the raw file rather than
// going silent.
func normalizeLoudness(ctx context.Context, src string) (string, bool) {
	targetI, enabled := loudnormTarget()
	if !enabled {
		return "", false
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", false
	}

	out := filepath.Join(os.TempDir(),
		fmt.Sprintf("jarvis-norm-%d.wav", time.Now().UnixNano()))

	// loudnorm single-pass: I=integrated LUFS target, LRA=loudness range
	// budget (lower = more compression), TP=true-peak ceiling. -y to
	// overwrite the temp file without prompting.
	filter := fmt.Sprintf("loudnorm=I=%.1f:LRA=11:TP=-1", targetI)
	normCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(normCtx, "ffmpeg",
		"-y", "-loglevel", "error",
		"-i", src,
		"-af", filter,
		"-ar", "22050",
		out,
	)
	if err := cmd.Run(); err != nil {
		// Clean up partial output, fall through.
		os.Remove(out)
		return "", false
	}
	return out, true
}

// loudnormTarget reads AIDA_TTS_LOUDNORM_I. Values "off"/"none"/"0"
// disable normalization; any parseable float overrides the default.
// Returns (target, enabled).
func loudnormTarget() (float64, bool) {
	v := strings.TrimSpace(os.Getenv("AIDA_TTS_LOUDNORM_I"))
	if v == "" {
		return loudnormDefaultI, true
	}
	switch strings.ToLower(v) {
	case "off", "none", "0":
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return loudnormDefaultI, true
	}
	return f, true
}

// afplayVolumeFor resolves the volume to pass to afplay's -v flag, as a
// string. Precedence: AIDA_TTS_VOLUME wins if set and parseable (the
// escape hatch for overriding any persona at runtime); else v if it's
// positive (the caller's per-persona volume); else afplayDefaultVolume.
func afplayVolumeFor(v float64) string {
	if env := strings.TrimSpace(os.Getenv("AIDA_TTS_VOLUME")); env != "" {
		if _, err := strconv.ParseFloat(env, 64); err == nil {
			return env
		}
	}
	if v > 0 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(afplayDefaultVolume, 'f', -1, 64)
}
