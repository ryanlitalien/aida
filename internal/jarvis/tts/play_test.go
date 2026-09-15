package tts

import (
	"testing"
)

func TestLoudnormTarget(t *testing.T) {
	cases := []struct {
		env       string
		wantValue float64
		wantOn    bool
	}{
		{"", loudnormDefaultI, true},
		{"-14", -14, true},
		{"-10.5", -10.5, true},
		{"off", 0, false},
		{"none", 0, false},
		{"0", 0, false},
		{"garbage", loudnormDefaultI, true}, // unparseable → default
	}
	for _, c := range cases {
		t.Setenv("AIDA_TTS_LOUDNORM_I", c.env)
		gotValue, gotOn := loudnormTarget()
		if gotOn != c.wantOn {
			t.Errorf("env=%q: enabled %v, want %v", c.env, gotOn, c.wantOn)
		}
		if gotOn && gotValue != c.wantValue {
			t.Errorf("env=%q: value %v, want %v", c.env, gotValue, c.wantValue)
		}
	}
}

func TestAfplayVolume(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"", "1.3"},
		{"1.5", "1.5"},
		{"0.8", "0.8"},
		{"2", "2"},
		{"nope", "1.3"}, // garbage, default
	}
	for _, c := range cases {
		t.Setenv("AIDA_TTS_VOLUME", c.env)
		if got := afplayVolumeFor(0); got != c.want {
			t.Errorf("env=%q: got %q, want %q", c.env, got, c.want)
		}
	}
}

// TestAfplayVolumeForPersonaArg verifies the volume-arg precedence used by
// PlayAt: a positive per-persona volume is used when AIDA_TTS_VOLUME is
// unset, but the env var still wins over a non-zero arg (the escape hatch
// must override any persona, not just the default).
func TestAfplayVolumeForPersonaArg(t *testing.T) {
	t.Setenv("AIDA_TTS_VOLUME", "")
	if got, want := afplayVolumeFor(1.43), "1.43"; got != want {
		t.Errorf("afplayVolumeFor(1.43) with no env = %q, want %q", got, want)
	}

	t.Setenv("AIDA_TTS_VOLUME", "2")
	if got, want := afplayVolumeFor(1.43), "2"; got != want {
		t.Errorf("afplayVolumeFor(1.43) with env override = %q, want %q", got, want)
	}
}
