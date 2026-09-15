package notify

// Desktop banner + chime for the moment a notification is ENQUEUED.
// The spoken utterance still waits for the next wake (see the package
// doc - speaking immediately risks Whisper transcribing Jarvis's own
// voice); the banner/chime is the "outside a wake" channel: the user
// learns a job finished NOW, and the voice confirmation follows when
// they next talk to Jarvis.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// fallbackChime ships with every macOS install - a suitably heroic
// default until the user drops a custom sound (see chimePath).
const fallbackChime = "/System/Library/Sounds/Hero.aiff"

// EnableDesktop turns on the banner+chime side effect in Enqueue.
// Off by default so tests and --no-jarvis daemons never fork
// osascript/afplay. Safe on a nil receiver.
func (n *Notifier) EnableDesktop() {
	if n != nil {
		n.desktop = true
	}
}

// notifyDesktop posts a Notification Center banner and plays the chime.
// Audio goes through afplay rather than the banner's `sound name`: an
// osascript banner posts under the "Script Editor" app identity, whose
// notification sound is frequently disabled or muted by Focus - the
// reason a `sound name "Glass"` banner can arrive silently. afplay
// depends on nothing but the output device, and accepts any audio file
// (aiff/mp3/m4a/wav), which is what makes the chime user-swappable.
// Best-effort: failures log and are otherwise ignored.
func notifyDesktop(msg string) {
	if runtime.GOOS != "darwin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// argv-passing sidesteps AppleScript string escaping - job questions
	// and error messages routinely carry quotes.
	banner := exec.CommandContext(ctx, "osascript",
		"-e", "on run argv",
		"-e", `display notification (item 1 of argv) with title "Jarvis"`,
		"-e", "end run",
		msg)
	if err := banner.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "notify: desktop banner failed: %v\n", err)
	}
	if err := exec.CommandContext(ctx, "afplay", chimePath()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "notify: chime failed: %v\n", err)
	}
}

// chimePath returns the notification sound to play: the user's custom
// file when present - anything matching ~/.aida/jarvis/notify-sound.*
// (drop in an .mp3/.m4a/.wav/.aiff; afplay decodes all of them, no
// conversion needed) - else the built-in Hero chime. Multiple matches
// pick the lexicographically first, deterministically.
func chimePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return fallbackChime
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".aida", "jarvis", "notify-sound.*"))
	if len(matches) == 0 {
		return fallbackChime
	}
	sort.Strings(matches)
	return matches[0]
}
