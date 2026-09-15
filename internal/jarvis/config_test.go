package jarvis

import "testing"

// TestDefaultConfigVolume guards against a future edit that drops or zeroes
// the Volume field: 0 silently falls back to the tts package default (see
// afplayVolumeFor), so a regression here would not fail loudly, it would
// just quietly change how loud Jarvis's replies are.
func TestDefaultConfigVolume(t *testing.T) {
	if v := DefaultConfig().Volume; v <= 0 {
		t.Errorf("DefaultConfig().Volume = %v, want > 0", v)
	}
}
