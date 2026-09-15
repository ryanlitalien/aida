package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// pcm16 builds little-endian 16-bit PCM bytes from int16 samples, the same
// shape RMS and WriteWAV both expect.
func pcm16(samples ...int16) []byte {
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf
}

// TestRMS is table-driven over silence / low-level noise / speech-level
// amplitude, per the LMD silence-gate spec: digital silence measures 0,
// quiet room tone is very low, and real speech is unambiguously higher.
func TestRMS(t *testing.T) {
	cases := []struct {
		name  string
		chunk []byte
		want  float64
	}{
		{"empty", nil, 0},
		{"digital silence (all zero)", pcm16(0, 0, 0, 0, 0, 0), 0},
		{"low-level room-tone noise", pcm16(10, -10, 15, -5, 8, -12), 10.47},
		{"constant speech-level amplitude", pcm16(2000, -2000, 2000, -2000), 2000},
		{"odd trailing byte is ignored, not a panic", append(pcm16(1000, 1000), 0xFF), 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RMS(c.chunk)
			// Allow a little slack for the noise case's irrational sqrt.
			const tol = 0.1
			if diff := got - c.want; diff > tol || diff < -tol {
				t.Errorf("RMS(%v) = %v, want ~%v", c.chunk, got, c.want)
			}
		})
	}
}

// TestRMS_SilenceBelowSpeech is the load-bearing property the LMD gate
// relies on: silence must measure meaningfully lower than real speech, with
// plenty of headroom for a conservative threshold to sit in between.
func TestRMS_SilenceBelowSpeech(t *testing.T) {
	silence := pcm16(0, 0, 0, 0, 0, 0, 0, 0)
	roomTone := pcm16(5, -3, 4, -6, 2, -4, 6, -5)
	speech := pcm16(1500, -1800, 2100, -1600, 1900, -2000)

	rSilence, rTone, rSpeech := RMS(silence), RMS(roomTone), RMS(speech)
	if rSilence != 0 {
		t.Errorf("RMS(silence) = %v, want 0", rSilence)
	}
	if rTone >= rSpeech {
		t.Errorf("RMS(room tone) = %v should be well below RMS(speech) = %v", rTone, rSpeech)
	}
	if rSpeech < 100 {
		t.Errorf("RMS(speech) = %v, want >= 100 for this to be a realistic speech-level fixture", rSpeech)
	}
}

func TestPCMFromWAV_RoundTrip(t *testing.T) {
	pcm := pcm16(100, -100, 32000, -32000, 0, 1, -1)
	path := filepath.Join(t.TempDir(), "round-trip.wav")
	if err := WriteWAV(path, pcm, 16000); err != nil {
		t.Fatalf("WriteWAV: %v", err)
	}
	wavBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	got, err := PCMFromWAV(wavBytes)
	if err != nil {
		t.Fatalf("PCMFromWAV: %v", err)
	}
	if len(got) != len(pcm) {
		t.Fatalf("PCMFromWAV returned %d bytes, want %d", len(got), len(pcm))
	}
	for i := range pcm {
		if got[i] != pcm[i] {
			t.Errorf("byte %d = %d, want %d", i, got[i], pcm[i])
		}
	}
}

func TestPCMFromWAV_Malformed(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"too short", []byte("RIFF")},
		{"empty", nil},
		{"missing RIFF/WAVE magic", []byte("XXXXsizeXXXXfmt more junk here padding")},
		{"not a real wav, just text", []byte("RIFF....WAVEfmt fake-pcm-bytes-not-real-audio")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Must not panic, and must return an error rather than
			// fabricating PCM data from garbage.
			_, err := PCMFromWAV(c.data)
			if err == nil {
				t.Errorf("PCMFromWAV(%q) returned no error, want one", c.data)
			}
		})
	}
}
