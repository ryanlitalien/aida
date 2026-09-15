package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
	"github.com/ryanlitalien/aida/internal/jarvis/listener"
	"github.com/ryanlitalien/aida/internal/jarvis/stt"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newJarvisCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jarvis",
		Short: "Voice-assistant subcommands (text-mode demo + listener daemon)",
	}
	cmd.AddCommand(newJarvisAskCmd())
	cmd.AddCommand(newJarvisGreetCmd())
	cmd.AddCommand(newJarvisListenCmd())
	cmd.AddCommand(newJarvisPTTCmd())
	cmd.AddCommand(newJarvisDaemonCmd())
	cmd.AddCommand(newJarvisMicsCmd())
	cmd.AddCommand(newJarvisThumbsUpCmd())
	return cmd
}

// newJarvisMicsCmd lists avfoundation audio inputs and shows which one the
// listener would pick given the active profile's serve.mic_prefer - a
// debugging aid for the unstable-index problem (find a device's exact name to
// put in mic_prefer, and confirm selection without launching the daemon).
func newJarvisMicsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mics",
		Short: "List input mics and show which one the listener would select",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			devices, err := audio.ListInputDevices(ctx)
			if err != nil {
				return fmt.Errorf("list input devices (is ffmpeg installed?): %w", err)
			}

			host := audio.MachineName()
			var micPrefer []string
			cfg, cfgErr := config.LoadConfig()
			if cfgErr == nil {
				if p, profName := cfg.ActiveProfileConfig(); p != nil && p.Serve != nil {
					micPrefer = p.Serve.ResolveMicPrefer(host)
					fmt.Printf("active profile: %s   machine: %s\n", profName, host)
				}
			}
			selected, selName := audio.SelectInputDevice(ctx, micPrefer)

			fmt.Println("\nAudio input devices (avfoundation):")
			for _, d := range devices {
				marker := "  "
				if fmt.Sprintf(":%d", d.Index) == selected {
					marker = "→ "
				}
				fmt.Printf("%s:%d  %s\n", marker, d.Index, d.Name)
			}
			if len(micPrefer) == 0 {
				fmt.Printf("\nmic_prefer: (unset) - listener uses the system default (:0)\n")
			} else {
				fmt.Printf("\nmic_prefer: %s\n", strings.Join(micPrefer, " > "))
				if selName != "" {
					fmt.Printf("selected:   %s (%s)\n", selName, selected)
				} else {
					fmt.Printf("selected:   none matched - falling back to system default (%s)\n", selected)
				}
			}
			return nil
		},
	}
}

// newJarvisDaemonCmd runs the always-on listener loop. Mic stays open;
// utterances containing the wake phrase ("ok jarvis ...") trigger the
// pipeline. Ctrl-C to quit.
func newJarvisDaemonCmd() *cobra.Command {
	var verbose bool
	var rmsGate float64
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the always-on listener: 'ok jarvis, ...' triggers the pipeline",
		Long: `Always-on voice loop on the default microphone.

Tuning:
  --verbose  prints rolling RMS so you can watch what the VAD sees vs the
             gate. If your speech RMS never crosses the gate, lower it.
  --rms-gate explicit VAD threshold. Default 300. If the listener fires on
             every breath, raise it. If it never fires, lower it.

Wake-phrase variants accepted: "ok jarvis", "okay jarvis", "hey jarvis".
You can also say just "ok jarvis" then pause, then your question.`,
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

			jcfg := jarvis.DefaultConfig()
			jcfg.AutoSync = cfg.Brain.AutoSync
			a, err := jarvis.New(jcfg, b, os.Getenv("ANTHROPIC_API_KEY"))
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			au, err := audit.New("")
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "🎤 listening for '%s' (variants: okay jarvis, hey jarvis) - Ctrl-C to quit\n", jcfg.WakeWord)
			fmt.Fprintf(os.Stderr, "   audit log: %s\n", au.Path())
			err = listener.Run(ctx, listener.Options{
				Assistant: a,
				WakeWord:  jcfg.WakeWord,
				RMSGate:   rmsGate,
				Verbose:   verbose,
				Audit:     au,
				OnEvent:   makeListenerPrinter(verbose),
			})
			if err == context.Canceled {
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print every utterance + per-second RMS (default: only wake hits + replies)")
	cmd.Flags().Float64Var(&rmsGate, "rms-gate", 0, "VAD RMS threshold (default 300; lower=more sensitive)")
	return cmd
}

// makeListenerPrinter returns an OnEvent callback that prints to stderr.
// In quiet mode (default), only wake hits, replies, and genuine errors are
// printed - perfect for working in a terminal where the mic might pick up
// background conversation all day. Pass verbose=true to also print speech
// boundaries, transcripts, and skips.
func makeListenerPrinter(verbose bool) func(listener.Event) {
	return func(ev listener.Event) {
		switch ev.Kind {
		case listener.EvSpeechStart:
			if verbose {
				fmt.Fprintln(os.Stderr, "▶️  speech")
			}
		case listener.EvSpeechEnd:
			if verbose {
				fmt.Fprintln(os.Stderr, "⏹  end")
			}
		case listener.EvTranscript:
			if verbose {
				fmt.Fprintf(os.Stderr, "📝 %q\n", ev.Text)
			}
		case listener.EvSkip:
			if verbose {
				fmt.Fprintf(os.Stderr, "·  skip (%s)\n", ev.Text)
			}
		case listener.EvWake:
			fmt.Fprintf(os.Stderr, "🎯 %q\n", ev.Text)
		case listener.EvReply:
			fmt.Fprintf(os.Stderr, "🤖 %s\n", ev.Text)
		case listener.EvError:
			fmt.Fprintf(os.Stderr, "✗  %v\n", ev.Err)
		}
	}
}

// newJarvisListenCmd records a clip from the default mic, transcribes it via
// whisper-cli, and feeds the transcript through the standard Jarvis pipeline.
// This is the no-wake-word, push-to-talk path - start the command, speak,
// wait for the recording window to close.
func newJarvisListenCmd() *cobra.Command {
	var seconds int
	var textOnly bool
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Record from mic, transcribe, then run through Jarvis (push-to-talk)",
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

			jcfg := jarvis.DefaultConfig()
			jcfg.TextOnly = textOnly
			jcfg.AutoSync = cfg.Brain.AutoSync

			a, err := jarvis.New(jcfg, b, os.Getenv("ANTHROPIC_API_KEY"))
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			fmt.Fprintf(os.Stderr, "🎤 listening for %d seconds - speak now…\n", seconds)
			wav, err := audio.Record(ctx, audio.RecordOptions{Seconds: seconds})
			if err != nil {
				return err
			}
			defer os.Remove(wav)

			fmt.Fprintln(os.Stderr, "📝 transcribing…")
			w := &stt.Whisper{}
			text, err := w.Transcribe(ctx, wav)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "👤 %s\n", text)

			if text == "" {
				return fmt.Errorf("empty transcript - try speaking louder or longer")
			}

			reply, err := a.AskText(ctx, text)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "🤖 ")
			fmt.Println(reply)
			return nil
		},
	}
	cmd.Flags().IntVar(&seconds, "seconds", 6, "recording window in seconds")
	cmd.Flags().BoolVar(&textOnly, "text", false, "print response instead of speaking it")
	return cmd
}

func newJarvisAskCmd() *cobra.Command {
	var textOnly bool
	cmd := &cobra.Command{
		Use:   "ask [query...]",
		Short: "Send a single typed query through the Jarvis pipeline (LLM + tools + TTS)",
		Long: `Demo entry point that bypasses the wake-word + STT layers and feeds a
typed query straight into the LLM/tools/TTS chain.

Example:
  aida jarvis ask "what are my three highest priority butterstack tasks"

With --text the answer is printed instead of spoken (useful for tests).`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")

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

			jcfg := jarvis.DefaultConfig()
			jcfg.TextOnly = textOnly
			jcfg.AutoSync = cfg.Brain.AutoSync

			apiKey := os.Getenv("ANTHROPIC_API_KEY")
			a, err := jarvis.New(jcfg, b, apiKey)
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			ui.PrintVerbose("jarvis", fmt.Sprintf("← %q", query))
			reply, err := a.AskText(ctx, query)
			if err != nil {
				return err
			}
			fmt.Println(reply)
			return nil
		},
	}
	cmd.Flags().BoolVar(&textOnly, "text", false, "print response instead of speaking it")
	return cmd
}

func newJarvisGreetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "greet",
		Short: "Speak the configured greeting line ('Good morning, sir.') - quick TTS smoke test",
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

			a, err := jarvis.New(jarvis.DefaultConfig(), b, os.Getenv("ANTHROPIC_API_KEY"))
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(),
				syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return a.SpeakGreeting(ctx)
		},
	}
}
