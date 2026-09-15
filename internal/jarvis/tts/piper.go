package tts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Piper synthesizes speech by shelling out to the `piper` binary. It writes a
// WAV file to a temp directory and returns the path. Caller deletes when done.
type Piper struct {
	Bin   string // path to piper binary; "piper" → looked up on PATH
	Model string // path to .onnx voice model

	// EspeakData is the path to the espeak-ng-data directory that piper's
	// phonemizer needs. When the binary was vendored by jarvis-deps.sh,
	// this lives next to the binary at $BIN_DIR/espeak-ng-data.
	EspeakData string

	// TempDir overrides the WAV output directory (defaults to os.TempDir).
	TempDir string
}

func (p *Piper) Name() string { return "piper" }

func (p *Piper) Synthesize(ctx context.Context, text string) (string, error) {
	if p.Model == "" {
		return "", fmt.Errorf("piper: no model configured")
	}
	if _, err := os.Stat(p.Model); err != nil {
		return "", fmt.Errorf("piper: model not found at %s: %w", p.Model, err)
	}

	dir := p.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	out := filepath.Join(dir, fmt.Sprintf("jarvis-%d.wav", time.Now().UnixNano()))

	bin := p.Bin
	if bin == "" {
		bin = "piper"
	}

	args := []string{"--model", p.Model, "--output_file", out}
	if p.EspeakData != "" {
		// Only pass when set; the rhasspy build needs it, the
		// pip-installed Python wrapper has its own bundled data.
		if _, err := os.Stat(p.EspeakData); err == nil {
			args = append(args, "--espeak_data", p.EspeakData)
		}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(text)

	// When the binary was vendored next to its dylibs, set
	// DYLD_LIBRARY_PATH as a belt-and-suspenders fallback. The vendoring
	// step also adds @loader_path to the binary's rpath, so this is
	// usually redundant - but it costs nothing and saves us from rpath
	// edge cases (e.g. user manually copied just the binary).
	if filepath.Dir(bin) != "" && filepath.Base(bin) == "piper" {
		dir := filepath.Dir(bin)
		cmd.Env = append(cmd.Environ(), "DYLD_LIBRARY_PATH="+dir)
	}

	if _, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("piper synthesis failed: %w", err)
	}
	return out, nil
}
