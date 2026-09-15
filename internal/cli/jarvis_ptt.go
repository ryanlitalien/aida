package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/hid"
	"github.com/ryanlitalien/aida/internal/jarvis/stt"
	"github.com/spf13/cobra"
)

// SayoDevice O2L V2 - the two-button USB HID pad used as Aida's push-to-talk
// key. Matched by USB vendor/product so only this device drives the mic; the
// same letters on a real keyboard are ignored.
const (
	sayoVendorID  = 0x8089
	sayoProductID = 0x000c
)

// IOHIDManager's input-value callbacks deliver the device's raw element
// values, which are upstream of any hidutil UserKeyMapping remap. That means
// aida always observes the factory usage 0x1D for the mic button ('z'), no
// matter what that usage is remapped to for the rest of the system. usageF13
// is kept only so a device whose firmware is configured to emit F13 directly
// (instead of z) is still recognised as the mic button.
const (
	usageZ   = 0x1D
	usageF13 = 0x68
)

func isMicButton(usage int) bool { return usage == usageZ || usage == usageF13 }

// checkPTTAccess prints a warning to stderr if this process lacks macOS Input
// Monitoring for HID listen events. This exists because IOHIDManagerOpen can
// succeed - and the "push-to-talk ready" banner still prints - even when TCC
// is silently withholding every keyboard-page event, so a dead button is
// otherwise indistinguishable from a working one. Does not abort: hid.Open is
// still attempted afterward regardless of what this reports.
func checkPTTAccess() {
	access := hid.CheckAccess()
	if access == hid.AccessUnknown {
		// Unknown means macOS hasn't decided yet - requesting it now triggers
		// the one-time prompt (or registers this process in the Input
		// Monitoring list) instead of leaving the button silently dead.
		hid.RequestAccess()
		access = hid.CheckAccess()
	}
	if access == hid.AccessDenied {
		fmt.Fprintln(os.Stderr, "⚠️  push-to-talk: macOS Input Monitoring is DENIED for this process - the button will be silent. Fix in System Settings > Privacy & Security > Input Monitoring (grant BOTH your terminal app and this aida binary), then restart serve.")
	}
}

// hidutil match + remap payloads. The remap is scoped to the SayoDevice by
// vendor/product, so a real keyboard's z/x are never touched. While PTT is
// active, z (the mic button) is remapped to keyboard usage 0, "Reserved (no
// event indicated)", so the press types nothing anywhere - it is not merely
// silenced in some apps, it produces no key event at all.
//
// WARNING: an earlier version of this remap targeted F13 instead. That was
// wrong: terminals render F13 as the escape sequence `CSI 1;2P`, so every
// press spewed a visible `^[[1;2P` into whatever terminal had focus. Do not
// go back to F13 for the mic button.
//
// x is remapped to Consumer usage 0xCD (Play/Pause), which macOS handles
// itself as a media key. On exit we restore the x->Play/Pause baseline
// instead of clearing, so the media key survives an `aida serve` restart and
// never fights the standalone login LaunchAgent that also owns x->Play/Pause.
const (
	sayoMatchJSON = `{"VendorID":0x8089,"ProductID":0x000c}`
	sayoRemapJSON = `{"UserKeyMapping":[` +
		`{"HIDKeyboardModifierMappingSrc":0x70000001D,"HIDKeyboardModifierMappingDst":0x700000000},` +
		`{"HIDKeyboardModifierMappingSrc":0x70000001B,"HIDKeyboardModifierMappingDst":0xC000000CD}]}`
	// sayoRemapBaseJSON is the resting state - just x->Play/Pause. Restored on
	// exit so releasing the mic's z remap never wipes the media key.
	sayoRemapBaseJSON = `{"UserKeyMapping":[` +
		`{"HIDKeyboardModifierMappingSrc":0x70000001B,"HIDKeyboardModifierMappingDst":0xC000000CD}]}`
)

// Capture format matches StartPCMStream: 16 kHz, mono, signed 16-bit LE.
const (
	pttBytesPerMs = 16000 * 2 / 1000 // 32 bytes of PCM per millisecond
	pttMaxSeconds = 120              // hard cap on a single held clip
)

// pttState tracks the one in-flight turn so a new button press can barge in
// (cancel its ctx, which kills TTS playback) before starting the next clip.
type pttState struct {
	mu      sync.Mutex
	current *pttTurn
}

type pttTurn struct{ cancel context.CancelFunc }

func (s *pttState) bargeIn() {
	s.mu.Lock()
	if s.current != nil {
		s.current.cancel()
		s.current = nil
	}
	s.mu.Unlock()
}

func (s *pttState) begin(parent context.Context) (context.Context, *pttTurn) {
	ctx, cancel := context.WithCancel(parent)
	t := &pttTurn{cancel: cancel}
	s.mu.Lock()
	s.current = t
	s.mu.Unlock()
	return ctx, t
}

func (s *pttState) finish(t *pttTurn) {
	s.mu.Lock()
	if s.current == t {
		s.current = nil
	}
	s.mu.Unlock()
}

// aidaPersonaConfig returns DefaultConfig tuned for the Aida persona - her
// Voice Design voice, display name, tone, and wake phrasing. Shared by the
// serve listener (spoken wake word) and the ptt button so the two paths are
// the same Aida. The wake fields only matter to the voice listener; the button
// ignores them.
func aidaPersonaConfig() jarvis.Config {
	c := jarvis.DefaultConfig()
	c.Persona = "Aida"
	c.WakeWord = "hey aida"
	// Several natural prefixes so she still answers by voice ("hi aida",
	// "hello aida", "ok aida") when away from the push-to-talk button - e.g.
	// travelling. The name pattern keeps this from colliding with "hey jarvis".
	c.WakePrefix = "hey|hi|hello|ok|okay"
	c.WakePattern = "aida|ada|ayda|ida"
	c.ElevenLabsVoiceID = "Lopz0RdZlxPXALj2qGr3" // Aida v3 - elite/sarcastic (Voice Design)
	c.ElevenLabsSpeed = 1.0
	c.ElevenLabsStability = 0.40 // lower → she inflects (needed for wit/sarcasm)
	c.ElevenLabsStyle = 0.40     // higher → amplify the elite/arch character
	// Aida alone moves off eleven_multilingual_v2: turbo is ~2.7x faster to
	// first audio (309ms vs 827ms mean on a short reply, and ~430ms vs ~2020ms
	// on a paragraph, since multilingual's TTFA scales with text length while
	// turbo's does not) at half the credit cost. Jarvis deliberately stays on
	// multilingual - his voice is an IVC replica built from MCU lines, and
	// turbo/flash re-render it into a noticeably different person. Aida was
	// generated inside ElevenLabs via Voice Design, so turbo reproduces her
	// essentially unchanged. Turbo over flash despite ElevenLabs' docs
	// recommending flash "in all use cases": on this network path flash was
	// both slower on average and far jitterier (stdev 162ms vs turbo's 31ms).
	// Measured 2026-07-21; harness, clips, and full results in docs/tts-model-ab/.
	c.ElevenLabsModelID = "eleven_turbo_v2_5"
	c.Greeting = "Good morning."
	c.Tone = "concise, warm, and personable" // warmer than Jarvis's dry butler register
	c.Volume = 1.3                           // Aida's voice is inherently louder, so she stays at the baseline gain
	return c
}

func newJarvisPTTCmd() *cobra.Command {
	var textOnly, seize, remap bool
	var minMs int
	var micOverride string

	cmd := &cobra.Command{
		Use:   "ptt",
		Short: "Push-to-talk via the SayoDevice button (hold z to record, release to send)",
		Long: `Hold-to-talk hot mic driven by the SayoDevice O2L V2 button.

Hold the z button and speak; release to send the clip through the standard
Jarvis pipeline (whisper -> Claude -> tools -> TTS). Pressing the button while
Aida is speaking barges in: it cuts her off and starts a new recording.

Only this specific USB device drives the mic - pressing z on a real keyboard is
ignored. With --remap (default), the device's z is remapped to an inert keyboard
usage that produces no keystroke, and x to the system Play/Pause media key, so
the presses no longer type stray characters into whatever app is focused.

Requires macOS Input Monitoring permission for the process (System Settings ->
Privacy & Security -> Input Monitoring). Press Ctrl-C to quit.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			// The button talks to AIDA (her voice + persona), not Jarvis. Build
			// the primary Jarvis first so the Aida twin can borrow its MCP
			// discovery + jobs store, then route button turns to the twin. Going
			// through NewTwin (not New) is what guarantees Aida keeps her own
			// configured voice even when $ELEVENLABS_VOICE_ID revoices the primary.
			jcfg := jarvis.DefaultConfig()
			jcfg.TextOnly = textOnly
			jcfg.AutoSync = cfg.Brain.AutoSync
			base, err := jarvis.New(jcfg, b, os.Getenv("ANTHROPIC_API_KEY"))
			if err != nil {
				return err
			}
			defer base.Close()

			aidaCfg := aidaPersonaConfig()
			aidaCfg.TextOnly = textOnly
			aidaCfg.AutoSync = cfg.Brain.AutoSync
			a, err := base.NewTwin(aidaCfg, os.Getenv("ANTHROPIC_API_KEY"))
			if err != nil {
				return err
			}
			defer a.Close()

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			// Resolve the mic the same way `aida serve` does: by name, honoring
			// the profile's serve.mic_prefer, unless --mic overrides it.
			micDevice := micOverride
			micName := ""
			if micDevice == "" {
				var micPrefer []string
				if p, ok := cfg.Profiles[profileName]; ok && p.Serve != nil {
					micPrefer = p.Serve.ResolveMicPrefer(audio.MachineName())
				}
				micDevice, micName = audio.SelectInputDevice(ctx, micPrefer)
			}

			// Best-effort key remap so presses don't leak z/x into focused apps.
			if remap {
				if err := pttApplyRemap(); err != nil {
					fmt.Fprintf(os.Stderr, "⚠  key remap failed (presses will still type z/x): %v\n", err)
				} else {
					defer pttRestoreRemap()
				}
			}

			checkPTTAccess()
			mon, err := hid.Open(sayoVendorID, sayoProductID, seize)
			if err != nil {
				return fmt.Errorf("open SayoDevice button: %w", err)
			}
			defer mon.Close()

			stream, err := audio.StartPCMStream(ctx, micDevice)
			if err != nil {
				return fmt.Errorf("start mic: %w", err)
			}
			defer stream.Stop()

			// Warm-mic capture: one always-running ffmpeg stream owned SOLELY by
			// this reader goroutine. Read and Flush must never run concurrently -
			// Flush sets a read deadline on the pipe that would abort an in-flight
			// Read ("i/o timeout") - so the event loop never touches the stream.
			// It signals capture start/stop via wantCapture; finished clips come
			// back over clips.
			var wantCapture atomic.Bool
			clips := make(chan []byte, 4)
			maxBytes := pttMaxSeconds * 1000 * pttBytesPerMs

			readErr := make(chan error, 1)
			go func() {
				chunk := make([]byte, 3200) // ~100ms @ 16kHz mono s16
				capturing := false
				var buf []byte
				for {
					n, rerr := stream.Read(chunk)
					want := wantCapture.Load() // single load: consistent per iteration

					if want && !capturing {
						// Press just began: drop stale pipe backlog (e.g. the tail
						// of Aida's own TTS on a barge-in) and start clean. This
						// pre-press chunk is discarded.
						stream.Flush()
						buf = buf[:0]
						capturing = true
					} else if capturing {
						if n > 0 && len(buf) < maxBytes {
							buf = append(buf, chunk[:n]...)
						}
						if !want {
							// Release: hand the finished clip to the event loop.
							capturing = false
							select {
							case clips <- append([]byte(nil), buf...):
							default:
							}
						}
					}

					if rerr != nil {
						if ctx.Err() == nil {
							select {
							case readErr <- rerr:
							default:
							}
						}
						return
					}
				}
			}()

			micLabel := "default mic"
			if micName != "" {
				micLabel = micName
			}
			fmt.Fprintf(os.Stderr, "🎧 push-to-talk ready - mic: %s\n", micLabel)
			fmt.Fprintln(os.Stderr, "   hold the z button and speak; release to send. Ctrl-C to quit.")

			var turns pttState
			for {
				select {
				case <-ctx.Done():
					turns.bargeIn()
					return nil
				case rerr := <-readErr:
					return fmt.Errorf("mic stream: %w", rerr)
				case ev, ok := <-mon.Events():
					if !ok {
						return nil
					}
					if !isMicButton(ev.Usage) {
						continue
					}
					if ev.Down {
						// Barge-in: silence any in-flight reply, then arm capture.
						turns.bargeIn()
						wantCapture.Store(true)
						fmt.Fprintln(os.Stderr, "🎙  recording…")
					} else {
						wantCapture.Store(false)
					}
				case clip := <-clips:
					if ms := len(clip) / pttBytesPerMs; ms < minMs {
						fmt.Fprintf(os.Stderr, "…  too short (%dms), ignored\n", ms)
						continue
					}
					turnCtx, t := turns.begin(ctx)
					go func(tctx context.Context, turn *pttTurn, pcm []byte) {
						defer turns.finish(turn)
						pttHandleClip(tctx, a, pcm, textOnly)
					}(turnCtx, t, clip)
				}
			}
		},
	}

	cmd.Flags().BoolVar(&textOnly, "text", false, "print the reply instead of speaking it")
	cmd.Flags().BoolVar(&seize, "seize", false, "exclusively grab the device (stops z/x entirely; requires root)")
	cmd.Flags().BoolVar(&remap, "remap", true, "remap z (mic) to an inert usage and x to Play/Pause so presses don't type stray characters")
	cmd.Flags().IntVar(&minMs, "min-ms", 250, "ignore presses shorter than this many milliseconds")
	cmd.Flags().StringVar(&micOverride, "mic", "", "avfoundation input device (e.g. ':0'); default honors serve.mic_prefer")
	return cmd
}

// pttHandleClip transcribes one PCM clip and runs it through the assistant.
// ctx cancellation (barge-in or Ctrl-C) aborts transcription/reply/playback.
func pttHandleClip(ctx context.Context, a *jarvis.Assistant, pcm []byte, textOnly bool) {
	wav := filepath.Join(os.TempDir(), fmt.Sprintf("aida-ptt-%d.wav", time.Now().UnixNano()))
	if err := audio.WriteWAV(wav, pcm, 16000); err != nil {
		fmt.Fprintf(os.Stderr, "✗  write wav: %v\n", err)
		return
	}
	defer os.Remove(wav)

	w := &stt.Whisper{}
	text, err := w.Transcribe(ctx, wav)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "✗  transcribe: %v\n", err)
		}
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		fmt.Fprintln(os.Stderr, "…  empty transcript, ignored")
		return
	}
	fmt.Fprintf(os.Stderr, "👤 %s\n", text)

	reply, err := a.AskText(ctx, text)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "✗  %v\n", err)
		}
		return
	}
	if textOnly {
		fmt.Println(reply)
	} else {
		fmt.Fprintf(os.Stderr, "🤖 %s\n", reply)
	}
}

func pttApplyRemap() error {
	return exec.Command("hidutil", "property", "--matching", sayoMatchJSON, "--set", sayoRemapJSON).Run()
}

// pttRestoreRemap drops the mic's z remap on exit but keeps x->Play/Pause,
// so an `aida serve` restart doesn't wipe the media key the LaunchAgent owns.
func pttRestoreRemap() {
	_ = exec.Command("hidutil", "property", "--matching", sayoMatchJSON, "--set", sayoRemapBaseJSON).Run()
}
