package config

import (
	"reflect"
	"testing"
)

func TestResolveMicPrefer(t *testing.T) {
	s := &ServeConfig{
		MicPrefer: []string{"MacBook"}, // default: built-in on any Mac
		MicPreferByHost: map[string][]string{
			"LavaChicken": {"Logi720pWebcam", "Unknown USB Audio Device"},
		},
	}
	cases := []struct {
		name string
		host string
		want []string
	}{
		{"exact host match", "LavaChicken", []string{"Logi720pWebcam", "Unknown USB Audio Device"}},
		{"case-insensitive substring", "lavachicken.local", []string{"Logi720pWebcam", "Unknown USB Audio Device"}},
		{"other machine falls to default", "AirOfRyan", []string{"MacBook"}},
		{"empty host falls to default", "", []string{"MacBook"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.ResolveMicPrefer(c.host); !reflect.DeepEqual(got, c.want) {
				t.Errorf("ResolveMicPrefer(%q) = %v, want %v", c.host, got, c.want)
			}
		})
	}

	// No per-host map → always the default.
	plain := &ServeConfig{MicPrefer: []string{"Yeti"}}
	if got := plain.ResolveMicPrefer("LavaChicken"); !reflect.DeepEqual(got, []string{"Yeti"}) {
		t.Errorf("no-by-host: got %v, want [Yeti]", got)
	}

	// nil receiver is safe.
	var nilCfg *ServeConfig
	if got := nilCfg.ResolveMicPrefer("x"); got != nil {
		t.Errorf("nil ServeConfig: got %v, want nil", got)
	}
}
