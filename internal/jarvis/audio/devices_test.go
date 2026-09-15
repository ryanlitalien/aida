package audio

import "testing"

// Real `ffmpeg -f avfoundation -list_devices true -i ""` output captured on the
// dev machine (after plugging in the Logi 720p webcam). Note the video and
// audio sections share `[N]` numbering - the parser must only return audio rows.
const sampleListDevices = `[AVFoundation indev @ 0x11f304080] AVFoundation video devices:
[AVFoundation indev @ 0x11f304080] [0] Logitech Webcam C925e
[AVFoundation indev @ 0x11f304080] [1] USB Camera VID:1133 PID:2085
[AVFoundation indev @ 0x11f304080] [2] MacBook Pro Camera
[AVFoundation indev @ 0x11f304080] [3] Capture screen 0
[AVFoundation indev @ 0x11f304080] AVFoundation audio devices:
[AVFoundation indev @ 0x11f304080] [0] Unknown USB Audio Device
[AVFoundation indev @ 0x11f304080] [1] MacBook Pro Microphone
[AVFoundation indev @ 0x11f304080] [2] Logitech Webcam C925e
[AVFoundation indev @ 0x11f304080] [3] Yeti Stereo Microphone`

func TestParseInputDevices(t *testing.T) {
	devs := parseInputDevices(sampleListDevices)
	want := []InputDevice{
		{0, "Unknown USB Audio Device"},
		{1, "MacBook Pro Microphone"},
		{2, "Logitech Webcam C925e"},
		{3, "Yeti Stereo Microphone"},
	}
	if len(devs) != len(want) {
		t.Fatalf("parsed %d devices, want %d: %+v", len(devs), len(want), devs)
	}
	for i, w := range want {
		if devs[i] != w {
			t.Errorf("device[%d] = %+v, want %+v", i, devs[i], w)
		}
	}
}

func TestPickDevice(t *testing.T) {
	devs := parseInputDevices(sampleListDevices)
	cases := []struct {
		name       string
		prefer     []string
		wantDevice string
		wantName   string
	}{
		{"webcam first", []string{"Unknown USB Audio Device", "MacBook Pro Microphone"}, ":0", "Unknown USB Audio Device"},
		{"falls to macbook when first absent", []string{"Logi BRIO", "MacBook Pro Microphone"}, ":1", "MacBook Pro Microphone"},
		{"case-insensitive substring", []string{"yeti"}, ":3", "Yeti Stereo Microphone"},
		{"no match falls back to :0", []string{"Nonexistent Mic"}, ":0", ""},
		{"priority order respected", []string{"MacBook Pro Microphone", "Yeti"}, ":1", "MacBook Pro Microphone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dev, name := pickDevice(devs, c.prefer)
			if dev != c.wantDevice || name != c.wantName {
				t.Errorf("pickDevice(%v) = (%q,%q), want (%q,%q)", c.prefer, dev, name, c.wantDevice, c.wantName)
			}
		})
	}
}
