package cli

import (
	"math"
	"regexp"
	"strconv"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis"
)

// hidutil's UserKeyMapping payloads use hex number literals (e.g.
// 0x70000001D) rather than quoted strings, which is not valid JSON -
// encoding/json cannot parse them (hidutil itself uses a lenient parser).
// This regexp pulls the src/dst pairs out of the raw string instead.
var sayoRemapPairRe = regexp.MustCompile(
	`\{"HIDKeyboardModifierMappingSrc":(0x[0-9A-Fa-f]+),"HIDKeyboardModifierMappingDst":(0x[0-9A-Fa-f]+)\}`)

// parseSayoRemap extracts the src->dst mapping pairs from a hidutil
// UserKeyMapping payload string.
func parseSayoRemap(t *testing.T, raw string) map[uint64]uint64 {
	t.Helper()
	matches := sayoRemapPairRe.FindAllStringSubmatch(raw, -1)
	out := make(map[uint64]uint64, len(matches))
	for _, m := range matches {
		src, err := strconv.ParseUint(m[1], 0, 64)
		if err != nil {
			t.Fatalf("parse src %q: %v", m[1], err)
		}
		dst, err := strconv.ParseUint(m[2], 0, 64)
		if err != nil {
			t.Fatalf("parse dst %q: %v", m[2], err)
		}
		out[src] = dst
	}
	return out
}

func TestIsMicButton(t *testing.T) {
	cases := []struct {
		name  string
		usage int
		want  bool
	}{
		{"factory z", 0x1D, true},
		{"remapped F13", 0x68, true},
		{"x button, not mic", 0x1B, false},
		{"zero usage", 0x00, false},
		{"a key, not mic", 0x04, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isMicButton(c.usage); got != c.want {
				t.Errorf("isMicButton(0x%X) = %v, want %v", c.usage, got, c.want)
			}
		})
	}
}

func TestSayoRemapSuppressesMicKeystroke(t *testing.T) {
	mapping := parseSayoRemap(t, sayoRemapJSON)

	const micSrc = 0x70000001D
	const inertDst = 0x700000000
	const f13Dst = 0x700000068

	dst, ok := mapping[micSrc]
	if !ok {
		t.Fatalf("sayoRemapJSON has no mapping for the mic key (src 0x%X)", micSrc)
	}
	if dst == f13Dst {
		t.Fatalf("mic key (0x%X) maps to F13 (0x%X); this is the exact regression this test "+
			"guards against - terminals render F13 as the escape sequence CSI 1;2P, so every "+
			"press spews visible `^[[1;2P` garbage into whatever terminal has focus. It must "+
			"map to the inert usage 0x%X instead", micSrc, f13Dst, inertDst)
	}
	if dst != inertDst {
		t.Fatalf("mic key (0x%X) maps to 0x%X, want inert usage 0x%X", micSrc, dst, inertDst)
	}
}

func TestSayoRemapKeepsPlayPause(t *testing.T) {
	mapping := parseSayoRemap(t, sayoRemapJSON)

	const xSrc = 0x70000001B
	const playPauseDst = 0xC000000CD

	dst, ok := mapping[xSrc]
	if !ok {
		t.Fatalf("sayoRemapJSON has no mapping for x (src 0x%X)", xSrc)
	}
	if dst != playPauseDst {
		t.Fatalf("x (0x%X) maps to 0x%X, want Play/Pause 0x%X", xSrc, dst, playPauseDst)
	}
}

func TestSayoRemapBaseKeepsOnlyPlayPause(t *testing.T) {
	mapping := parseSayoRemap(t, sayoRemapBaseJSON)

	if len(mapping) != 1 {
		t.Fatalf("sayoRemapBaseJSON has %d mappings, want exactly 1: %v", len(mapping), mapping)
	}

	const xSrc = 0x70000001B
	const playPauseDst = 0xC000000CD
	const micSrc = 0x70000001D

	dst, ok := mapping[xSrc]
	if !ok {
		t.Fatalf("sayoRemapBaseJSON has no mapping for x (src 0x%X): %v", xSrc, mapping)
	}
	if dst != playPauseDst {
		t.Fatalf("x (0x%X) maps to 0x%X, want Play/Pause 0x%X", xSrc, dst, playPauseDst)
	}
	if _, ok := mapping[micSrc]; ok {
		t.Fatalf("sayoRemapBaseJSON must not remap the mic key (src 0x%X); the restore path "+
			"should never reintroduce a z remap", micSrc)
	}
}

// TestJarvisVolumeIsTenPercentLouderThanAida encodes the requirement
// "Jarvis is 10% louder than Aida" as a checked invariant, so a future edit
// to either persona's default Volume can't silently drift the ratio - per
// internal/jarvis/config.go, Jarvis's ElevenLabs voice reads quieter than
// Aida's at the same gain, so his default multiplier is set higher to
// compensate.
func TestJarvisVolumeIsTenPercentLouderThanAida(t *testing.T) {
	jarvisVolume := jarvis.DefaultConfig().Volume
	aidaVolume := aidaPersonaConfig().Volume

	if jarvisVolume <= 0 {
		t.Fatalf("jarvis.DefaultConfig().Volume = %v, want > 0", jarvisVolume)
	}
	if aidaVolume <= 0 {
		t.Fatalf("aidaPersonaConfig().Volume = %v, want > 0", aidaVolume)
	}

	const wantRatio = 1.1
	const tolerance = 0.001
	if ratio := jarvisVolume / aidaVolume; math.Abs(ratio-wantRatio) >= tolerance {
		t.Errorf("jarvis volume %v / aida volume %v = %v, want %v (+/- %v)",
			jarvisVolume, aidaVolume, ratio, wantRatio, tolerance)
	}
}

// TestPersonasUseDifferentTTSModels pins the deliberate asymmetry introduced
// 2026-07-21: Aida runs eleven_turbo_v2_5 (faster to first audio, half the
// credit cost) while Jarvis stays on eleven_multilingual_v2. The split looks
// like an oversight, so it is the kind of thing a future "unify the persona
// configs" refactor would helpfully collapse - and collapsing it either
// re-slows Aida or silently re-voices Jarvis, whose IVC replica of the MCU
// lines does not survive turbo. Fail loudly instead.
func TestPersonasUseDifferentTTSModels(t *testing.T) {
	jarvisModel := jarvis.DefaultConfig().ElevenLabsModelID
	aidaModel := aidaPersonaConfig().ElevenLabsModelID

	if want := "eleven_multilingual_v2"; jarvisModel != want {
		t.Errorf("jarvis.DefaultConfig().ElevenLabsModelID = %q, want %q "+
			"(his voice is an IVC replica; turbo/flash re-render it as a different person)",
			jarvisModel, want)
	}
	if want := "eleven_turbo_v2_5"; aidaModel != want {
		t.Errorf("aidaPersonaConfig().ElevenLabsModelID = %q, want %q "+
			"(see docs/tts-model-ab/ for the measurements behind this choice)",
			aidaModel, want)
	}
	if jarvisModel == aidaModel {
		t.Errorf("both personas resolved to %q; the per-persona TTS model split "+
			"has been collapsed - see docs/tts-model-ab/", jarvisModel)
	}
}

func TestSayoRemapConstantsParse(t *testing.T) {
	if mapping := parseSayoRemap(t, sayoRemapJSON); len(mapping) == 0 {
		t.Fatal("sayoRemapJSON produced zero mappings - regexp likely doesn't match the " +
			"constant's format (a typo here would make other tests vacuously pass)")
	}
	if mapping := parseSayoRemap(t, sayoRemapBaseJSON); len(mapping) == 0 {
		t.Fatal("sayoRemapBaseJSON produced zero mappings - regexp likely doesn't match the " +
			"constant's format (a typo here would make other tests vacuously pass)")
	}
}
