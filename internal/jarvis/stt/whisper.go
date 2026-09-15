// Package stt wraps speech-to-text engines. The current implementation shells
// out to whisper-cpp's `whisper-cli` (installed via `brew install
// whisper-cpp`) - adequate for the demo, swappable for whisper.cpp Go
// bindings later when we want streaming partials.
package stt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := exec.CommandContext(ctx, bin,
		"-m", model,
		"-f", wavPath,
		"-l", lang,
		"-np", "-nt", "-otxt",
		"--prompt", prompt,
	)
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
