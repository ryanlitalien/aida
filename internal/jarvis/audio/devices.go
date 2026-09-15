package audio

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MachineName returns a stable, distinctive identifier for this Mac, used to
// key per-machine mic preferences. It prefers scutil's LocalHostName /
// ComputerName (user-set, stable) over os.Hostname(), which on macOS is often a
// transient network name like "Mac.lan" that can't tell two machines apart.
func MachineName() string {
	for _, args := range [][]string{{"--get", "LocalHostName"}, {"--get", "ComputerName"}} {
		if out, err := exec.Command("scutil", args...).Output(); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" {
				return s
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// InputDevice is an avfoundation audio input as ffmpeg enumerates it. Index is
// the avfoundation device index used in the `:N` capture spec - but it is NOT
// stable across device connect/disconnect (a Yeti seen at :0 can become :3 when
// a webcam is plugged in), so callers should resolve by Name, not Index.
type InputDevice struct {
	Index int
	Name  string
}

// avfDeviceLineRE matches a device row like "[AVFoundation indev @ 0x..] [0]
// MacBook Pro Microphone". The `[AVFoundation ...]` prefix has non-digit bracket
// content so only the `[0]` index bracket matches.
var avfDeviceLineRE = regexp.MustCompile(`\[(\d+)\]\s+(.+)$`)

// ListInputDevices enumerates avfoundation audio input devices via
// `ffmpeg -f avfoundation -list_devices true -i ""`. ffmpeg prints the device
// table to stderr and exits non-zero (no real input was given) - both expected,
// so the exit error is ignored and only the parsed table matters. Only rows
// under the "AVFoundation audio devices:" header are returned (video indices
// share the same `[N]` numbering and must not leak in).
func ListInputDevices(ctx context.Context) ([]InputDevice, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg",
		"-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "")
	out, _ := cmd.CombinedOutput() // non-zero exit is expected; parse regardless

	devices := parseInputDevices(string(out))
	if len(devices) == 0 {
		return nil, fmt.Errorf("no avfoundation audio devices parsed")
	}
	return devices, nil
}

// parseInputDevices extracts the audio-device rows from ffmpeg's
// `-list_devices` output. Pure (no IO) so the parsing is unit-tested directly.
// Only rows under the "AVFoundation audio devices:" header are kept - video
// rows share the same `[N]` numbering and must not leak in.
func parseInputDevices(raw string) []InputDevice {
	var devices []InputDevice
	inAudio := false
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "AVFoundation audio devices:"):
			inAudio = true
			continue
		case strings.Contains(line, "AVFoundation video devices:"):
			inAudio = false
			continue
		}
		if !inAudio {
			continue
		}
		m := avfDeviceLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		idx, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		devices = append(devices, InputDevice{Index: idx, Name: strings.TrimSpace(m[2])})
	}
	return devices
}

// SelectInputDevice resolves the avfoundation `:N` capture spec for the first
// connected audio device whose name contains one of the prefer substrings
// (case-insensitive, in priority order), returning the matched name too for
// logging. Falls back to ":0" (system default) when prefer is empty or nothing
// matches. Selection is by NAME precisely because avfoundation indices shift.
func SelectInputDevice(ctx context.Context, prefer []string) (device, name string) {
	if len(prefer) == 0 {
		return ":0", ""
	}
	devices, err := ListInputDevices(ctx)
	if err != nil || len(devices) == 0 {
		return ":0", ""
	}
	return pickDevice(devices, prefer)
}

// pickDevice returns the `:N` spec + name of the first device matching the
// prefer list (case-insensitive substring, priority order), else (":0", "").
// Pure, so the matching logic is unit-tested without running ffmpeg.
func pickDevice(devices []InputDevice, prefer []string) (device, name string) {
	for _, p := range prefer {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		for _, d := range devices {
			if strings.Contains(strings.ToLower(d.Name), p) {
				return fmt.Sprintf(":%d", d.Index), d.Name
			}
		}
	}
	return ":0", ""
}
