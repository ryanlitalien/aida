package stt

import (
	"encoding/binary"
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

// TestAudioCtxForDuration covers the -ac sizing formula: clamp(ceil(seconds*50)+64,
// 256, 1500) rounded up to a multiple of 64, with full context (ok=false)
// once a clip reaches 28s.
func TestAudioCtxForDuration(t *testing.T) {
	tests := []struct {
		name    string
		seconds float64
		wantN   int
		wantOK  bool
	}{
		{"1s floors at the 256 minimum", 1, 256, true},
		{"3s still floors at 256", 3, 256, true},
		{"10s", 10, 576, true},
		{"20s", 20, 1088, true},
		{"28s uses full context", 28, 0, false},
		{"40s uses full context", 40, 0, false},
		{"unknown (non-positive) duration uses full context", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := audioCtxForDuration(tt.seconds)
			if ok != tt.wantOK {
				t.Fatalf("audioCtxForDuration(%v) ok = %v, want %v", tt.seconds, ok, tt.wantOK)
			}
			if ok && n != tt.wantN {
				t.Errorf("audioCtxForDuration(%v) = %d, want %d", tt.seconds, n, tt.wantN)
			}
			if ok && n%audioCtxRoundTo != 0 {
				t.Errorf("audioCtxForDuration(%v) = %d, not a multiple of %d", tt.seconds, n, audioCtxRoundTo)
			}
			if ok && (n < minAudioCtx || n > maxAudioCtx) {
				t.Errorf("audioCtxForDuration(%v) = %d, out of range [%d, %d]", tt.seconds, n, minAudioCtx, maxAudioCtx)
			}
		})
	}
}

// writeTestWAV writes a minimal canonical 44-byte-header 16-bit PCM WAV
// file at path with the given format/duration and returns the number of
// audio-data bytes it wrote, so tests can assert wavDurationSeconds's
// result independent of writeTestWAV's own duration math.
func writeTestWAV(t *testing.T, path string, sampleRate uint32, channels, bitsPerSample uint16, dataBytes int) {
	t.Helper()

	bytesPerSample := bitsPerSample / 8
	blockAlign := channels * bytesPerSample
	byteRate := sampleRate * uint32(blockAlign)

	buf := make([]byte, wavHeaderSize+dataBytes)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(36+dataBytes))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16) // fmt chunk size (PCM)
	binary.LittleEndian.PutUint16(buf[20:22], 1)  // audio format: PCM
	binary.LittleEndian.PutUint16(buf[22:24], channels)
	binary.LittleEndian.PutUint32(buf[24:28], sampleRate)
	binary.LittleEndian.PutUint32(buf[28:32], byteRate)
	binary.LittleEndian.PutUint16(buf[32:34], blockAlign)
	binary.LittleEndian.PutUint16(buf[34:36], bitsPerSample)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataBytes))
	// Data payload content doesn't matter for duration - leave zeroed.

	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatalf("write test WAV: %v", err)
	}
}

// TestWavDurationSeconds covers the header parser against a generated
// 16kHz/mono/16-bit WAV (the LMD protocol's format, see
// docs/lmd-protocol.md) at a duration chosen to be exact under that format
// (32000 bytes/sec), plus the error paths it must fall back on.
func TestWavDurationSeconds(t *testing.T) {
	t.Run("3 second clip at 16kHz mono 16-bit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "clip.wav")
		writeTestWAV(t, path, 16000, 1, 16, 3*32000) // exactly 3.0s of data
		got, err := wavDurationSeconds(path)
		if err != nil {
			t.Fatalf("wavDurationSeconds() error = %v", err)
		}
		if got != 3.0 {
			t.Errorf("wavDurationSeconds() = %v, want 3.0", got)
		}
	})

	t.Run("stale data chunk size field is ignored in favor of file size", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "clip.wav")
		writeTestWAV(t, path, 16000, 1, 16, 32000) // 1.0s of real data
		// Corrupt the declared data-chunk size to simulate a streamed
		// writer that never went back to fix it up.
		buf, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read back test WAV: %v", err)
		}
		binary.LittleEndian.PutUint32(buf[40:44], 0)
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			t.Fatalf("rewrite test WAV: %v", err)
		}
		got, err := wavDurationSeconds(path)
		if err != nil {
			t.Fatalf("wavDurationSeconds() error = %v", err)
		}
		if got != 1.0 {
			t.Errorf("wavDurationSeconds() = %v, want 1.0 (from file size, not the stale data-chunk field)", got)
		}
	})

	t.Run("missing file", func(t *testing.T) {
		if _, err := wavDurationSeconds(filepath.Join(t.TempDir(), "does-not-exist.wav")); err == nil {
			t.Error("wavDurationSeconds() on a missing file: want error, got nil")
		}
	})

	t.Run("too short to hold a header", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "short.wav")
		if err := os.WriteFile(path, []byte("RIFF"), 0o600); err != nil {
			t.Fatalf("write short file: %v", err)
		}
		if _, err := wavDurationSeconds(path); err == nil {
			t.Error("wavDurationSeconds() on a too-short file: want error, got nil")
		}
	})

	t.Run("not a RIFF/WAVE container", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notwav.bin")
		if err := os.WriteFile(path, make([]byte, wavHeaderSize+100), 0o600); err != nil {
			t.Fatalf("write non-WAV file: %v", err)
		}
		if _, err := wavDurationSeconds(path); err == nil {
			t.Error("wavDurationSeconds() on a non-RIFF file: want error, got nil")
		}
	})
}

// TestWhisper_AudioCtxFlag covers AudioCtx's three modes end to end against
// audioCtxFlag, the resolver Transcribe consults for the -ac value.
func TestWhisper_AudioCtxFlag(t *testing.T) {
	shortClip := filepath.Join(t.TempDir(), "short.wav")
	writeTestWAV(t, shortClip, 16000, 1, 16, 3*32000) // 3.0s

	longClip := filepath.Join(t.TempDir(), "long.wav")
	writeTestWAV(t, longClip, 16000, 1, 16, 40*32000) // 40.0s

	t.Run("auto (zero value) sizes from a short clip's duration", func(t *testing.T) {
		w := &Whisper{}
		n, ok := w.audioCtxFlag(shortClip)
		if !ok {
			t.Fatal("audioCtxFlag() ok = false, want true for a short clip")
		}
		if n != minAudioCtx {
			t.Errorf("audioCtxFlag() = %d, want %d (the 256 floor for a 3s clip)", n, minAudioCtx)
		}
	})

	t.Run("auto (zero value) omits the flag for a long clip", func(t *testing.T) {
		w := &Whisper{}
		if _, ok := w.audioCtxFlag(longClip); ok {
			t.Error("audioCtxFlag() ok = true, want false (full context) for a 40s clip")
		}
	})

	t.Run("auto (zero value) omits the flag when duration can't be read", func(t *testing.T) {
		w := &Whisper{}
		if _, ok := w.audioCtxFlag(filepath.Join(t.TempDir(), "missing.wav")); ok {
			t.Error("audioCtxFlag() ok = true, want false when the WAV can't be parsed")
		}
	})

	t.Run("negative AudioCtx never passes the flag, even for a short clip", func(t *testing.T) {
		w := &Whisper{AudioCtx: -1}
		if _, ok := w.audioCtxFlag(shortClip); ok {
			t.Error("audioCtxFlag() ok = true, want false when AudioCtx is -1")
		}
	})

	t.Run("positive AudioCtx is a fixed override, ignoring duration", func(t *testing.T) {
		w := &Whisper{AudioCtx: 768}
		n, ok := w.audioCtxFlag(longClip)
		if !ok || n != 768 {
			t.Errorf("audioCtxFlag() = (%d, %v), want (768, true)", n, ok)
		}
	})
}
