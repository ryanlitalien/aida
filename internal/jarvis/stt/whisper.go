// Package stt wraps speech-to-text engines. The current implementation shells
// out to whisper-cpp's `whisper-cli` (installed via `brew install
// whisper-cpp`) - adequate for the demo, swappable for whisper.cpp Go
// bindings later when we want streaming partials.
package stt

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Whisper transcribes a WAV file via the whisper-cpp CLI.
type Whisper struct {
	// Bin is the whisper-cpp binary; empty means look up "whisper-cli" then
	// "whisper-cpp" on PATH.
	Bin string
	// Model path. Empty defaults to ~/.aida/jarvis/models/ggml-small.en.bin
	// (488 MB) - substantially better than tiny.en on technical vocabulary,
	// at a cost that's only ~100-300 ms of extra transcription time on
	// GPU-accelerated hardware (e.g. Apple Silicon Metal). On weak/headless
	// hardware with no GPU, that gap is far larger (small.en measured 94s vs
	// tiny.en's 10.6s on an 11s clip on a 2-core Pentium) - see
	// TinyEnModelPath for such callers. Override to medium.en for max
	// accuracy where both compute and time are available.
	Model string
	// Language hint, default "en".
	Language string
	// Prompt biases whisper's decoding toward this vocabulary/spelling -
	// small/tiny models otherwise mis-hear uncommon proper nouns as
	// near-homophones (e.g. "ryanlitalien/aida" -> "Ryan the Talion slash
	// Ada", "pull request" -> "polar quest"). Empty defaults to
	// defaultVocabPrompt.
	Prompt string
	// AudioCtx controls whisper.cpp's -ac/--audio-ctx flag, which shrinks
	// the encoder's context window below its default full-30-second size.
	// On a CPU-only host, whisper.cpp always encodes a full 30s window
	// regardless of the clip's actual length, so a 3s clip costs as much to
	// transcribe as a 30s one unless -ac sizes the window to what's
	// actually there. Measured on minty (a 2-core Pentium, tiny.en, 3s
	// clip): 8.0s at full context vs 3.3s at -ac 512, with identical or
	// better transcripts - a smaller window seems to give the model less
	// room to hallucinate padding, not just less to compute.
	//
	//   - 0 (default/auto): Transcribe reads the WAV's duration and sizes
	//     -ac from it - see audioCtxForDuration - falling back to full
	//     context (flag omitted) when the duration can't be determined or
	//     is >= 28s, where shrinking stops being worth the risk of an edge
	//     case clipping real audio.
	//   - -1: never pass -ac; always use whisper's full context. Use this
	//     on GPU-accelerated hardware (e.g. Apple Silicon Metal) where the
	//     encode is already fast enough that a truncated window isn't worth
	//     the risk.
	//   - >0: pass this fixed value on every call, bypassing auto-sizing.
	AudioCtx int
}

// Audio-context sizing constants for AudioCtx's auto (0) mode - see
// audioCtxForDuration.
const (
	// audioCtxPerSecond is whisper.cpp's context-units-per-second-of-audio
	// ratio: -ac 1500 covers whisper's full 30s window, so roughly 50
	// units per second of audio.
	audioCtxPerSecond = 50
	// audioCtxHeadroom is added on top of the raw per-second estimate so a
	// clip isn't sized right at its own boundary.
	audioCtxHeadroom = 64
	// minAudioCtx / maxAudioCtx bound the auto-sized value to whisper.cpp's
	// sane range; 1500 is whisper's own full-context ceiling.
	minAudioCtx = 256
	maxAudioCtx = 1500
	// audioCtxRoundTo rounds the computed value up to a multiple of this,
	// matching the granularity -ac is normally tuned in (e.g. 512/768).
	audioCtxRoundTo = 64
	// fullContextSeconds is the clip length at or above which auto mode
	// omits -ac entirely and uses whisper's full context.
	fullContextSeconds = 28.0
)

// audioCtxForDuration computes the -ac value for a clip of the given
// duration (in seconds): clamp(ceil(seconds*audioCtxPerSecond)+audioCtxHeadroom,
// minAudioCtx, maxAudioCtx), rounded up to a multiple of audioCtxRoundTo.
// ok is false - meaning "omit -ac, use full context" - when seconds is
// unknown (<= 0) or >= fullContextSeconds.
func audioCtxForDuration(seconds float64) (n int, ok bool) {
	if seconds <= 0 || seconds >= fullContextSeconds {
		return 0, false
	}
	raw := int(math.Ceil(seconds*audioCtxPerSecond)) + audioCtxHeadroom
	if raw < minAudioCtx {
		raw = minAudioCtx
	} else if raw > maxAudioCtx {
		raw = maxAudioCtx
	}
	n = ((raw + audioCtxRoundTo - 1) / audioCtxRoundTo) * audioCtxRoundTo
	return n, true
}

// wavHeaderSize is the length of the canonical (no extension chunks)
// 16-bit-PCM WAV header: "RIFF" + size(4) + "WAVE" + "fmt " + size(4) + 16
// fmt bytes + "data" + size(4) = 44 bytes. AudioRecord (Android) and Aida's
// own capture path both write this shape.
const wavHeaderSize = 44

// wavDurationSeconds returns a WAV file's audio duration in seconds by
// reading its canonical 44-byte header via encoding/binary. It trusts the
// header's own sample-rate/channels/bits-per-sample fields (whisper's input
// is always 16 kHz/mono/16-bit PCM per docs/lmd-protocol.md, but this
// doesn't hardcode that), then computes duration from the file's actual
// size minus the 44-byte header rather than the header's own "data" chunk
// size field, since some WAV writers leave that field zeroed or stale for
// streamed captures - the file's real byte count is the more reliable
// source of truth for how much audio is actually there.
//
// Returns an error - callers should treat that as "duration unknown" and
// omit -ac entirely - when the file is too short to hold a header, isn't a
// RIFF/WAVE/fmt container, or doesn't have a "data" sub-chunk at the
// canonical 44-byte offset (a WAV with extra chunks before "fmt "/"data"
// isn't handled by this simple fixed-offset parser; none of whisper's
// callers in this codebase produce one).
func wavDurationSeconds(path string) (float64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.Size() < wavHeaderSize {
		return 0, fmt.Errorf("wav: file too small (%d bytes) for a %d-byte header", info.Size(), wavHeaderSize)
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var header [wavHeaderSize]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return 0, fmt.Errorf("wav: read header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" || string(header[12:16]) != "fmt " {
		return 0, fmt.Errorf("wav: not a canonical RIFF/WAVE/fmt header")
	}
	if string(header[36:40]) != "data" {
		return 0, fmt.Errorf("wav: no data sub-chunk at the canonical 44-byte offset")
	}

	channels := binary.LittleEndian.Uint16(header[22:24])
	sampleRate := binary.LittleEndian.Uint32(header[24:28])
	bitsPerSample := binary.LittleEndian.Uint16(header[34:36])
	if channels == 0 || sampleRate == 0 || bitsPerSample == 0 {
		return 0, fmt.Errorf("wav: invalid fmt fields (channels=%d sampleRate=%d bitsPerSample=%d)", channels, sampleRate, bitsPerSample)
	}

	bytesPerSecond := float64(sampleRate) * float64(channels) * float64(bitsPerSample/8)
	if bytesPerSecond <= 0 {
		return 0, fmt.Errorf("wav: zero bytes-per-second")
	}

	dataBytes := info.Size() - wavHeaderSize
	if dataBytes < 0 {
		dataBytes = 0
	}
	return float64(dataBytes) / bytesPerSecond, nil
}

// defaultVocabPrompt seeds whisper's initial context with the proper nouns
// and terms of art that recur in the user's spoken queries, so decoding
// favors the correct spelling over an acoustically-similar guess.
const defaultVocabPrompt = "Technical terms: aida, ryanlitalien slash aida on GitHub, pull request, " +
	"Bifrost, Heimdall, herdr, ButterStack, Camp Butz, First Chair, ViewerCore, Jarvis."

func (w *Whisper) bin() (string, error) {
	candidates := []string{w.Bin, "whisper-cli", "whisper-cpp"}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("stt: whisper-cli not found on PATH (try `brew install whisper-cpp`)")
}

func (w *Whisper) modelPath() string {
	if w.Model != "" {
		return w.Model
	}
	return defaultModelPath("ggml-small.en.bin")
}

// TinyEnModelPath returns the path to the tiny.en whisper.cpp model under the
// standard ~/.aida/jarvis/models directory (downloaded by
// scripts/jarvis-deps.sh alongside small.en). Latency-sensitive callers that
// can't assume Whisper's own zero-value default (ggml-small.en.bin - see
// modelPath's doc comment) is cheap enough - e.g. the LMD/Android turn
// handler in internal/jarvis/lmd, which may run on weak/headless hardware
// with no GPU - set Model to this explicitly instead.
func TinyEnModelPath() string {
	return defaultModelPath("ggml-tiny.en.bin")
}

func defaultModelPath(filename string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aida", "jarvis", "models", filename)
}

// Transcribe runs whisper-cpp on the given WAV file and returns the
// transcribed text (no timestamps, single block).
func (w *Whisper) Transcribe(ctx context.Context, wavPath string) (string, error) {
	bin, err := w.bin()
	if err != nil {
		return "", err
	}
	model := w.modelPath()
	if _, err := os.Stat(model); err != nil {
		return "", fmt.Errorf("stt: model missing at %s - run `make jarvis-deps`", model)
	}
	lang := w.Language
	if lang == "" {
		lang = "en"
	}
	prompt := w.Prompt
	if prompt == "" {
		prompt = defaultVocabPrompt
	}
	// -np: no progress, -nt: no timestamps, -otxt: write .txt sidecar
	args := []string{
		"-m", model,
		"-f", wavPath,
		"-l", lang,
		"-np", "-nt", "-otxt",
		"--prompt", prompt,
	}
	if ac, ok := w.audioCtxFlag(wavPath); ok {
		args = append(args, "-ac", strconv.Itoa(ac))
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if _, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("whisper-cli failed: %w", err)
	}
	txt, err := os.ReadFile(wavPath + ".txt")
	if err != nil {
		return "", fmt.Errorf("read whisper output: %w", err)
	}
	defer os.Remove(wavPath + ".txt")
	return strings.TrimSpace(string(txt)), nil
}

// audioCtxFlag resolves the -ac value (if any) Transcribe should pass, per
// AudioCtx's doc comment: negative means never pass it, positive is a fixed
// override, and zero (the default) auto-sizes from the clip's own duration.
func (w *Whisper) audioCtxFlag(wavPath string) (n int, ok bool) {
	switch {
	case w.AudioCtx < 0:
		return 0, false
	case w.AudioCtx > 0:
		return w.AudioCtx, true
	default:
		seconds, err := wavDurationSeconds(wavPath)
		if err != nil {
			// Duration unknown - keep full context rather than guess.
			return 0, false
		}
		return audioCtxForDuration(seconds)
	}
}
