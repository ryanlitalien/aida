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
	// (488 MB) - substantially better than tiny.en on technical vocabulary
	// at the cost of ~100-300 ms of extra transcription time. Override
	// to tiny.en for low-latency demos or to medium.en for max accuracy.
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
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".aida", "jarvis", "models", "ggml-small.en.bin")
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
