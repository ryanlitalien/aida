package tts

import "testing"

func TestStreamFilter(t *testing.T) {
	// Streaming applies a volume gain only - no loudnorm (its lookahead gets
	// truncated by ffplay -autoexit, cutting the reply's tail).
	cases := []struct {
		name   string
		volume string
		want   string
	}{
		{"default volume", "", "volume=1.3"},
		{"explicit boost", "1.5", "volume=1.5"},
		{"unity volume omitted", "1.0", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AIDA_TTS_VOLUME", c.volume)
			if got := streamFilter(0); got != c.want {
				t.Errorf("streamFilter(0) = %q, want %q", got, c.want)
			}
		})
	}
}
