package config

import "testing"

func TestActiveClaudeFallbackConfig_Defaults(t *testing.T) {
	t.Setenv("AIDA_PROFILE", "")
	c := &Config{} // no profiles configured
	fb := c.ActiveClaudeFallbackConfig()
	if fb.Enabled == nil || !*fb.Enabled {
		t.Errorf("default Enabled should be true (ON), got %v", fb.Enabled)
	}
	if fb.TimeoutSeconds != 90 {
		t.Errorf("default TimeoutSeconds = %d, want 90", fb.TimeoutSeconds)
	}
}

func TestActiveClaudeFallbackConfig_ExplicitHonored(t *testing.T) {
	t.Setenv("AIDA_PROFILE", "")
	off := false
	c := &Config{
		ActiveProfile: "p",
		Profiles: map[string]Profile{
			"p": {ClaudeFallback: &ClaudeFallbackConfig{
				Enabled:        &off,
				TimeoutSeconds: 30,
				Model:          "claude-haiku-4-5",
			}},
		},
	}
	fb := c.ActiveClaudeFallbackConfig()
	if fb.Enabled == nil || *fb.Enabled {
		t.Errorf("explicit enabled:false not honored, got %v", fb.Enabled)
	}
	if fb.TimeoutSeconds != 30 {
		t.Errorf("explicit TimeoutSeconds not honored: %d", fb.TimeoutSeconds)
	}
	if fb.Model != "claude-haiku-4-5" {
		t.Errorf("explicit Model not honored: %q", fb.Model)
	}
}
