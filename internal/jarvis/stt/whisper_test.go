package stt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWhisper_ModelPath_DefaultsToSmallEn locks in Whisper's zero-value
// default (small.en, chosen for accuracy on GPU-accelerated hardware like
// the desk-mic listener's Mac) so a future change to modelPath doesn't
// silently also change TinyEnModelPath's independent default below.
func TestWhisper_ModelPath_DefaultsToSmallEn(t *testing.T) {
	w := &Whisper{}
	got := w.modelPath()
	if !strings.HasSuffix(got, filepath.Join(".aida", "jarvis", "models", "ggml-small.en.bin")) {
		t.Errorf("modelPath() = %q, want a path ending in .aida/jarvis/models/ggml-small.en.bin", got)
	}
}

// TestWhisper_ModelPath_ExplicitOverrideWins covers Whisper.Model taking
// priority over the small.en default - this is the override path LMD's
// config knob (ServeConfig.LMDWhisperModel) and the pre-existing
// low-latency-demo override both rely on.
func TestWhisper_ModelPath_ExplicitOverrideWins(t *testing.T) {
	w := &Whisper{Model: "/custom/path/ggml-medium.en.bin"}
	if got := w.modelPath(); got != "/custom/path/ggml-medium.en.bin" {
		t.Errorf("modelPath() = %q, want the explicit override", got)
	}
}

// TestTinyEnModelPath covers the LMD default (see internal/jarvis/lmd and
// stt.TinyEnModelPath's doc comment): it must resolve under the same
// ~/.aida/jarvis/models directory as the small.en default, just naming the
// tiny.en file instead, so both models are found side by side wherever
// scripts/jarvis-deps.sh downloaded them.
func TestTinyEnModelPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	want := filepath.Join(home, ".aida", "jarvis", "models", "ggml-tiny.en.bin")
	if got := TinyEnModelPath(); got != want {
		t.Errorf("TinyEnModelPath() = %q, want %q", got, want)
	}

	// It must differ from the small.en default so LMD and the listener
	// don't silently end up pointed at the same (slow) model.
	if got := (&Whisper{}).modelPath(); got == TinyEnModelPath() {
		t.Error("TinyEnModelPath() must not equal the Whisper zero-value default (ggml-small.en.bin)")
	}
}
