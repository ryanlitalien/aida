package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
	"github.com/ryanlitalien/aida/internal/jarvis/hid"
	"github.com/ryanlitalien/aida/internal/jarvis/listener"
	"github.com/ryanlitalien/aida/internal/jarvis/lmd"
	"github.com/ryanlitalien/aida/internal/jarvis/notify"
	"github.com/ryanlitalien/aida/internal/jarvis/server"
	"github.com/ryanlitalien/aida/internal/jarvis/stt"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/mcp"
	"github.com/spf13/cobra"
)

// JarvisHTTPPort is the persistent daemon port - Earth-1610 in Marvel
// multiverse parlance, alternate of Earth-616.
const JarvisHTTPPort = 1610

func newServeCmd() *cobra.Command {
	var httpOnly bool
	var stdioOnly bool
	var noJarvis bool
	var noListen bool
	var noPTT bool
	var port int
	var enableLMD bool
	var lmdPort int
	var menuPathFlag string
	var menuLANPort int
	var enableLoop bool
	loopO := defaultLoopOpts()
	sweepO := defaultHarvestSweepOpts()
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start Aida as an MCP tool server (stdio) and/or HTTP daemon (port 1610)",
		Long: `Aida serve has two modes:

  Stdio MCP (default when stdin is a pipe):
    Other agents (Claude Code, Cursor, etc.) spawn Aida and connect over
    stdin/stdout. The HTTP daemon does NOT run in this mode.

  HTTP daemon (default when stdin is a TTY, or with --http):
    Binds 127.0.0.1:1610 (Earth-1610) and exposes:
      GET  / - Jarvis status panel (web UI)
      GET  /jarvis/health - uptime + version
    Plus the Jarvis voice loop (mic in, wake word, STT, LLM, TTS) when the
    daemon is run interactively.

Disable the voice loop with --no-jarvis (useful for headless HTTP-only).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			profile, profileName := cfg.ActiveProfileConfig()

			// Auto-detect mode: if stdin is a TTY (interactive shell), run
			// HTTP daemon. If stdin is a pipe (Claude Code), run MCP stdio.
			// --stdio is the safety net for MCP clients that allocate a PTY for
			// the spawned `aida serve` (some do, for line editing/signal
			// handling reasons). Without it, isTTY(os.Stdin) would look
			// interactive and serveHTTPMode would launch the full HTTP daemon -
			// grabbing the mic, opening the SayoDevice HID monitor, rewriting
			// the key remap, and taking over the running daemon's port via
			// bindWithTakeover. --stdio always wins over --http so a
			// PTY-allocating MCP client can force stdio mode explicitly and
			// touch none of that.
			httpMode := serveHTTPMode(stdioOnly, httpOnly, isTTY(os.Stdin))

			if !httpMode {
				return runMCPStdio(cfg, profileName)
			}

			enableJarvis := resolveServeFlag(profile, "jarvis", !noJarvis, cmd.Flags().Changed("no-jarvis"))
			enableListen := resolveServeFlag(profile, "listen", !noListen, cmd.Flags().Changed("no-listen"))
			enablePTT := resolveServeFlag(profile, "ptt", !noPTT, cmd.Flags().Changed("no-ptt"))
			loopO.daemon = true // a serve-hosted dispatcher always runs continuously
			// Fail the `aida serve` invocation outright on a bad --loop config
			// (e.g. --loop-sandbox docker without --loop-worktree) rather than
			// letting the dispatcher die silently inside its goroutine while the
			// daemon reports healthy.
			if enableLoop {
				if err := loopO.validate(); err != nil {
					return fmt.Errorf("invalid --loop configuration: %w (use the --loop-* flags)", err)
				}
			}
			// Validated eagerly (like --loop above) even though the only
			// current failure mode is a negative duration -- so a bad
			// --harvest-sweep value fails `aida serve` outright instead of
			// the sweep goroutine silently never ticking.
			if err := sweepO.validate(); err != nil {
				return fmt.Errorf("invalid --harvest-sweep: %w", err)
			}
			// --menu-path wins over profile/config menu.path, which itself
			// defaults to ~/dev/health/nutrition/menu.yaml (see
			// config.Config.MenuPath). Expanded the same way as any other
			// configured path so a tilde on the CLI works too.
			menuPath := cfg.MenuPath()
			if menuPathFlag != "" {
				menuPath = config.ExpandPath(menuPathFlag)
			}
			return runHTTPDaemon(cfg, profileName, port, enableJarvis, enableListen, enablePTT, enableLoop, loopO, sweepO, enableLMD, lmdPort, menuPath, menuLANPort)
		},
	}
	cmd.Flags().BoolVar(&httpOnly, "http", false, "force HTTP daemon mode even when stdin is a pipe")
	cmd.Flags().BoolVar(&stdioOnly, "stdio", false, "force MCP stdio mode even when stdin is a TTY (never touches mic, HID, or the key remap)")
	cmd.Flags().BoolVar(&noJarvis, "no-jarvis", false, "disable Jarvis HTTP routes (overrides profile serve.jarvis)")
	cmd.Flags().BoolVar(&noListen, "no-listen", false, "disable the always-on mic listener (overrides profile serve.listen)")
	cmd.Flags().BoolVar(&noPTT, "no-ptt", false, "disable the SayoDevice push-to-talk button (Aida hot mic)")
	cmd.Flags().IntVar(&port, "port", JarvisHTTPPort, "HTTP daemon port")

	// --lmd brings up the L.M.D. (Life Model Decoy) HTTP listener for the
	// aida-android client: a second, Tailscale-bound http.Server distinct
	// from the loopback-only daemon above. Off by default (opt-in) since it
	// requires Jarvis and a working tailnet interface - see
	// docs/lmd-protocol.md for the wire contract.
	cmd.Flags().BoolVar(&enableLMD, "lmd", false, "enable the LMD listener for the Android client (requires Jarvis + Tailscale; see docs/lmd-protocol.md)")
	cmd.Flags().IntVar(&lmdPort, "lmd-port", lmd.Port, "LMD listener port (Earth-1218)")

	// --menu-path/--menu-lan-port configure the read-only weekly dinner-menu
	// page (see internal/cli/menu_web.go). The page is mounted on the main
	// loopback daemon at /menu (nav parity with the other pages) AND, always,
	// on a SECOND LAN-bound http.Server carrying ONLY the menu routes on its
	// own mux (mirrors startLMDServer's separate-listener pattern below), so
	// a kid's phone on the home wifi can reach the menu without anything
	// else on the daemon (/tasks, /mcp/call, ...) becoming LAN-reachable.
	cmd.Flags().StringVar(&menuPathFlag, "menu-path", "", "override the dinner-menu YAML path (else profile/config menu.path, else ~/dev/health/nutrition/menu.yaml)")
	cmd.Flags().IntVar(&menuLANPort, "menu-lan-port", 1611, "port for the always-on LAN menu listener (menu routes only)")

	// --loop hosts the autonomous dispatcher (aida loop --daemon) in-process as a
	// background goroutine, so "Jarvis, turn that call into tasks" flows
	// straight into worked PRs without a separate `aida loop` process. The
	// --loop-* knobs mirror the high-value `aida loop` flags; everything else
	// uses the loop defaults (run `aida loop` standalone for finer control).
	cmd.Flags().BoolVar(&enableLoop, "loop", false, "run the autonomous loop dispatcher in the background (in-process aida loop --daemon)")
	cmd.Flags().StringSliceVar(&loopO.tags, "loop-tag", nil, "only loop over tasks carrying ALL of these tags (repeatable)")
	cmd.Flags().BoolVar(&loopO.worktree, "loop-worktree", false, "run each looped task in an isolated worktree off origin/main")
	cmd.Flags().BoolVar(&loopO.pr, "loop-pr", false, "open a PR per passing looped task (implies --loop-worktree)")
	cmd.Flags().StringVar(&loopO.reviewer, "loop-reviewer", "", "GitHub handle to request review from on looped PRs")
	cmd.Flags().StringArrayVar(&loopO.checks, "loop-check", nil, "quality-gate command run after each looped attempt; must exit 0 (repeatable)")
	cmd.Flags().IntVar(&loopO.concurrency, "loop-concurrency", 1, "work up to N looped tasks in parallel (requires --loop-worktree)")
	cmd.Flags().StringVar(&loopO.sandboxTier, "loop-sandbox", "none", "confine looped agents: none | docker (requires --loop-worktree)")
	cmd.Flags().StringVar(&loopO.sandboxMemory, "loop-sandbox-memory", "", "sbx microVM memory limit, e.g. 8g (--loop-sandbox docker only)")
	cmd.Flags().StringVar(&loopO.provisionAida, "loop-provision-aida", "", "path to a prebuilt linux/arm64 aida to copy into the docker sandbox (needed when serve runs outside the aida checkout; build via `make build-linux-arm64`)")

	// --harvest-sweep runs the Codex/Gemini/Meetily memory-bridge harvest
	// core (aida brain harvest --tool codex|gemini|meetily) on a periodic
	// background timer, independent of the Stop/AfterAgent hooks aida
	// setup wires up. See harvest_sweep.go for why this exists: the
	// harvest quiet window can otherwise leave a session's final
	// hook-triggered run skipping it as "still running", with nothing
	// left to pick it back up once the user goes idle on that tool (and
	// it's Meetily's only automatic trigger at all, since it has no hook
	// to wire into `aida setup`). HTTP-daemon-mode only; a no-op under
	// stdio MCP.
	cmd.Flags().DurationVar(&sweepO.interval, "harvest-sweep", sweepO.interval, "how often to run the Codex/Gemini/Meetily harvest core in the background (0 disables)")
	return cmd
}

// resolveServeFlag picks the effective bool for a serve toggle in priority
// order: explicit CLI flag > profile.serve.<field> > legacy CLI default.
// `field` selects which serve config pointer to read ("jarvis" or "listen").
func resolveServeFlag(profile *config.Profile, field string, cliValue, cliSet bool) bool {
	if cliSet {
		return cliValue
	}
	if profile == nil || profile.Serve == nil {
		return cliValue
	}
	var v *bool
	switch field {
	case "jarvis":
		v = profile.Serve.Jarvis
	case "listen":
		v = profile.Serve.Listen
	}
	if v == nil {
		return cliValue
	}
	return *v
}

func runMCPStdio(cfg *config.Config, profileName string) error {
	daemonURL := daemonBaseURL()
	srv, err := mcp.NewServer(cfg, profileName, daemonURL)
	if err != nil {
		return fmt.Errorf("starting MCP server: %w", err)
	}
	defer srv.Close()
	if srv.Proxying() {
		fmt.Fprintf(os.Stderr, "Aida MCP server started (stdio → proxying tool calls to daemon at %s)\n", daemonURL)
	} else {
		fmt.Fprintln(os.Stderr, "Aida MCP server started (stdio, in-process)")
	}
	return srv.Serve()
}

// daemonBaseURL is where the stdio MCP proxy forwards tool calls: the local
// HTTP daemon on JarvisHTTPPort, overridable via AIDA_DAEMON_ADDR ("host:port").
func daemonBaseURL() string {
	if addr := os.Getenv("AIDA_DAEMON_ADDR"); addr != "" {
		return "http://" + addr
	}
	return fmt.Sprintf("http://127.0.0.1:%d", JarvisHTTPPort)
}

func runHTTPDaemon(cfg *config.Config, profileName string, port int, enableJarvis, enableListen, enablePTT, enableLoop bool, loopO loopOpts, sweepO harvestSweepOpts, enableLMD bool, lmdPort int, menuPath string, menuLANPort int) error {
	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		return err
	}
	defer b.Close()

	jobsStore, err := jobs.Open(profileName)
	if err != nil {
		return fmt.Errorf("open jobs store: %w", err)
	}
	defer jobsStore.Close()

	mux := http.NewServeMux()

	// Favicon/touch-icon/manifest routes, shared by every page this daemon
	// serves. Registered first and unguarded (see registerFaviconRoutes)
	// so nothing below can shadow /favicon.ico, /favicon.svg, etc.
	registerFaviconRoutes(mux)

	// Mount tasks UI under /tasks (HTML) and /api/* (REST). The Jarvis
	// status panel claims /, so tasks moves to a named path.
	registerTasksWebRoutesAt(mux, b, jobsStore, profileName, false, "/tasks")

	// Read-only weekly dinner-menu page, mounted on the main loopback daemon
	// for nav parity with the other pages. Reachable from a kid's phone only
	// while the always-on LAN-bound listener below carries the same routes.
	// isLAN=false here: this mux's nav links (Jarvis/Tasks/Runs/Dashboard/
	// Bifrost) are all real routes on the loopback daemon.
	registerMenuWebRoutes(mux, menuPath, false)

	// Habit-tracking calendar, mounted to the right of /menu in the nav.
	// Loopback-only -- unlike the menu page, this is never mounted on the
	// LAN listener below; it's personal, not for the kids' phones.
	registerHabitsWebRoutes(mux, cfg.HabitsDBPath(), cfg.FoodDiaryPath(), menuPath, cfg.WorkoutsDBPath())

	// MCP-over-HTTP: per-session stdio `aida serve` proxies forward their tool
	// calls to POST /mcp/call so this single daemon (current binary, one brain
	// handle) does the work, instead of each session running its own instance.
	// /healthz is an unconditional liveness route (unlike /jarvis/health, which
	// only exists when Jarvis is enabled).
	mcpDispatcher := mcp.NewDispatcher(b, cfg, profileName)
	mux.HandleFunc("/mcp/call", mcpDispatcher.HTTPHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})

	var assistant *jarvis.Assistant
	// aidaAssistant is the cosmetic Aida twin (see below); kept in an
	// outer-scoped variable, distinct from the block-local `aida` it's
	// assigned from, so startLMDServer can wire it in as the second LMD
	// persona further down - construction happens inside the `if
	// enableJarvis` block and the twin may be nil if it failed.
	var aidaAssistant *jarvis.Assistant
	var notifier *notify.Notifier
	// listenerPersonas is the wake-word routing table handed to the mic
	// listener: "ok jarvis" → Jarvis, "ok aida" → Aida (a cosmetic twin
	// sharing Jarvis's brain/tools, differing only in voice + name).
	var listenerPersonas []listener.Persona
	if enableJarvis {
		jcfg := jarvis.DefaultConfig()
		jcfg.AutoSync = cfg.Brain.AutoSync
		a, err := jarvis.New(jcfg, b, os.Getenv("ANTHROPIC_API_KEY"))
		if err != nil {
			return fmt.Errorf("jarvis init: %w (use --no-jarvis to skip)", err)
		}
		defer a.Close()
		assistant = a
		// voiceLabelFor renders a persona's actually-resolved voice for the
		// startup log: the ElevenLabs voice id when that synth is active, else
		// the Piper model. Only the primary honors $ELEVENLABS_VOICE_ID - a
		// twin always keeps its configured voice (matches newAssistant).
		voiceLabelFor := func(as *jarvis.Assistant, c jarvis.Config, honorEnv bool) string {
			if as.TTS() == nil || as.TTS().Name() != "elevenlabs" {
				return "piper:" + c.PiperModel
			}
			vid := c.ElevenLabsVoiceID
			if honorEnv {
				if v := os.Getenv("ELEVENLABS_VOICE_ID"); v != "" {
					vid = v
				}
			}
			return "elevenlabs:" + vid
		}

		listenerPersonas = []listener.Persona{{
			Name:      jcfg.Persona,
			Wake:      listener.WakeRegexForPrefix(jcfg.WakePrefix, jcfg.WakePattern),
			Assistant: a,
			Voice:     voiceLabelFor(a, jcfg, true),
			WakeWord:  jcfg.WakeWord,
		}}

		// Cosmetic twin: same brain, tools, and jobs queue as Jarvis; its
		// own ElevenLabs voice + display name + wake phrase. Borrows Jarvis's
		// MCP discovery + jobs store so we don't double-spawn stdio servers.
		// A twin-init failure is non-fatal - Jarvis alone still boots.
		aidaCfg := aidaPersonaConfig()
		aidaCfg.AutoSync = cfg.Brain.AutoSync
		// Upgrade Aida from cosmetic twin to chief-of-staff dispatcher: build
		// her roster + engine LLM client, reusing the base assistant's shared
		// jobs store and MCP discovery. Best-effort - a failure just leaves her
		// cosmetic. Jarvis is never handed a Dispatcher, so he is unaffected.
		if d, derr := buildAidaDispatcher(cfg, profileName, a); derr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  aida dispatcher: %v (continuing cosmetic)\n", derr)
		} else {
			aidaCfg.Dispatcher = d
		}
		if aida, aerr := a.NewTwin(aidaCfg, os.Getenv("ANTHROPIC_API_KEY")); aerr != nil {
			fmt.Fprintf(os.Stderr, "⚠️  aida twin init: %v (continuing with Jarvis only)\n", aerr)
		} else {
			defer aida.Close()
			aidaAssistant = aida
			listenerPersonas = append(listenerPersonas, listener.Persona{
				Name:      aidaCfg.Persona,
				Wake:      listener.WakeRegexForPrefix(aidaCfg.WakePrefix, aidaCfg.WakePattern),
				Assistant: aida,
				Voice:     voiceLabelFor(aida, aidaCfg, false),
				WakeWord:  aidaCfg.WakeWord,
			})
		}
		// Share the assistant's TTS handle so notifications speak in
		// the same voice as the rest of Jarvis without reloading the
		// .onnx model.
		notifier = notify.New(a.TTS())
		notifier.SetVolume(jcfg.Volume)
		// Immediate out-of-band channel: banner + chime the moment a
		// job transitions, so the user learns "daring-island is done"
		// without waking Jarvis; the spoken notice still follows on
		// next wake. Custom chime: drop any audio file at
		// ~/.aida/jarvis/notify-sound.* (default: macOS Hero).
		notifier.EnableDesktop()
		server.New(mux, a)
		for _, p := range listenerPersonas {
			fmt.Fprintf(os.Stderr, "🤖 %s online - model %s, voice %s\n",
				strings.ToLower(p.Name), jcfg.Model, p.Voice)
		}
	} else {
		// Jarvis off → no status panel claiming /, so land users on /tasks
		// (the only real UI route exposed by the daemon in this mode).
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			http.Redirect(w, r, "/tasks", http.StatusFound)
		})
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// /dashboard + /bifrost. Registered here rather than inside
	// registerTasksWebRoutesAt because that registrar is shared with
	// `aida tasks web --standalone`, which must not carry a
	// remote-execution surface or start a probe ticker. The mux is not
	// handed to the http.Server until below, so registering after ctx
	// exists lets the ticker share the daemon's signal context.
	dashDeps, fleetCache := newDashboardDeps(cfg, b, jobsStore, profileName, time.Now())
	registerDashboardRoutes(mux, dashDeps)
	// Keep the cache warm so the first page load is instant. Skipped when
	// the only device is the synthesized self entry -- there is nothing to
	// probe over the network and a ticker would be pure waste.
	if iv := cfg.Fleet.RefreshInterval(); iv > 0 && len(cfg.Devices) > 0 {
		go fleetCache.Run(ctx, iv)
	}

	// Single-instance takeover: bind the port up front, first stopping any
	// prior `aida serve` that still holds it. This happens BEFORE the mic is
	// acquired below, so re-running `aida serve` can never stack two listeners
	// fighting over the microphone - the old daemon is told to exit and we
	// inherit the port. (User asked for kill-and-take-over, not refuse-to-start.)
	ln, err := bindWithTakeover(addr)
	if err != nil {
		return err
	}

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		fmt.Fprintf(os.Stderr, "🌐 listening on http://%s  (Earth-1610)\n", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "http server: %v\n", err)
			cancel()
		}
	}()

	// LMD: a second, Tailscale-bound listener for the aida-android client -
	// entirely separate from the loopback-only srv above so the phone can
	// never reach /tasks or /mcp/call. Opt-in via --lmd; startLMDServer
	// itself declines (logging a clear warning) when Jarvis is off or no
	// tailnet interface is found, so a misconfigured --lmd can never take
	// down the rest of the daemon.
	var lmdSrv *http.Server
	if enableLMD {
		var lmdWhisperModel string
		if p, ok := cfg.Profiles[profileName]; ok && p.Serve != nil {
			lmdWhisperModel = p.Serve.LMDWhisperModel
		}
		lmdSrv = startLMDServer(assistant, aidaAssistant, lmdPort, lmdWhisperModel)
	}

	// Menu LAN listener: same separate-mux pattern as LMD above, but with
	// no Jarvis/Tailscale dependency -- it's a plain 0.0.0.0 bind carrying
	// ONLY the three menu routes (see startMenuLANServer), so opting a
	// phone-reachable menu page in never widens what's reachable from the
	// LAN to /tasks, /mcp/call, or anything else on the daemon. Always on:
	// the whole point of the page is the kids' phones.
	menuLANSrv := startMenuLANServer(menuPath, menuLANPort)

	// Background job watcher - polls the jobs store every 2s for
	// transitions to awaiting_input / done / failed and enqueues a
	// voice notification for each one not yet stamped with
	// NotifiedAt. NotifiedAt is the idempotency key: on daemon
	// restart, jobs that finished while the daemon was down (and
	// already got their notification stamp) won't re-speak.
	//
	// Only runs when both Jarvis and the listener are enabled -
	// without the listener there's no wake event to drain on, so a
	// notifier with no drain would just leak utterances.
	if enableJarvis && enableListen && notifier != nil {
		go watchJobsForNotifications(ctx, jobsStore, profileName, notifier)
	}

	// Autonomous loop dispatcher - runs `aida loop` in-process as a background
	// goroutine when --loop is set, so voice-ingested tasks get worked without
	// a separate process. It opens its own brain + jobs handles (SQLite WAL
	// tolerates the concurrent access) and stops cleanly when ctx is cancelled
	// on SIGINT/SIGTERM, before the HTTP server shuts down.
	var loopDone chan struct{}
	if enableLoop {
		loopDone = make(chan struct{})
		go func() {
			defer close(loopDone)
			fmt.Fprintf(os.Stderr, "🔁 starting loop dispatcher - tags=%v worktree=%v pr=%v sandbox=%s\n",
				loopO.tags, loopO.worktree, loopO.pr, loopO.sandboxTier)
			// Flag-combo errors are caught eagerly in RunE; a non-Canceled
			// error here (e.g. a docker-tier linux-aida build failure) means the
			// loop never started - surface it loudly rather than leaving the
			// daemon looking healthy with no work happening.
			if err := runLoopCtx(ctx, loopO); err != nil && !errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "⚠️  loop dispatcher failed - tasks will NOT be worked: %v\n", err)
			}
		}()
	}

	// Periodic harvest sweep - HTTP-daemon-mode only (never under stdio
	// MCP, since runHTTPDaemon is only ever called from that mode). See
	// harvest_sweep.go for the gap this closes: the harvest quiet window
	// can leave a session's last hook-triggered run skipping it as "still
	// running", with nothing left to catch it once the user goes idle on
	// that tool (and it's Meetily's only automatic trigger, period - it
	// has no Stop/AfterAgent-style hook to wire into `aida setup`). Off
	// via --harvest-sweep 0. runHarvestCore (brain_harvest.go) opens its
	// own brain handle per call and the built-in watermark makes a no-op
	// pass cheap, so ticking every few minutes is safe.
	var sweepDone chan struct{}
	if sweepO.enabled() {
		sweepDone = make(chan struct{})
		go func() {
			defer close(sweepDone)
			runHarvestSweepLoop(ctx, sweepO.interval, runHarvestCore)
		}()
	}

	if enableJarvis && enableListen && assistant != nil {
		au, err := audit.New("")
		if err != nil {
			return fmt.Errorf("audit log: %w", err)
		}
		// "ok jarvis (elevenlabs:…)" / "hey aida (elevenlabs:…)" - each
		// persona's own wake phrase plus resolved voice.
		wakeLabels := make([]string, 0, len(listenerPersonas))
		for _, p := range listenerPersonas {
			wakeLabels = append(wakeLabels, fmt.Sprintf("%s (%s)", p.WakeWord, p.Voice))
		}
		// Resolve the mic by NAME (avfoundation indices shift as devices come
		// and go). serve.mic_prefer is a priority-ordered list of name
		// substrings; the first connected match wins, else the system default.
		var micPrefer []string
		if p, ok := cfg.Profiles[profileName]; ok && p.Serve != nil {
			micPrefer = p.Serve.ResolveMicPrefer(audio.MachineName())
		}
		micDevice, micName := audio.SelectInputDevice(ctx, micPrefer)
		micLabel := "default mic"
		if micName != "" {
			micLabel = micName + " (" + micDevice + ")"
		}
		// With a mic_prefer list the listener FOLLOWS the best available device
		// (hot-swaps on dock/undock, recovers from sleep/wake stalls) instead
		// of pinning the boot-time pick.
		following := ""
		if len(micPrefer) > 0 {
			following = " (following)"
		}

		// Push-to-talk: the SayoDevice button drives Aida directly (no wake
		// word), sharing this listener's single mic. A press barges in on any
		// in-flight reply. Off with --no-ptt, or when Aida isn't present.
		var pttSignal chan bool
		var pttPersona *listener.Persona
		for i := range listenerPersonas {
			if listenerPersonas[i].Name == "Aida" {
				pttPersona = &listenerPersonas[i]
				break
			}
		}
		if enablePTT && pttPersona != nil {
			checkPTTAccess()
			if mon, herr := hid.Open(sayoVendorID, sayoProductID, false); herr != nil {
				fmt.Fprintf(os.Stderr, "⚠️  push-to-talk disabled: %v\n", herr)
			} else {
				defer mon.Close()
				if pttApplyRemap() == nil {
					defer pttRestoreRemap()
				}
				sig := make(chan bool, 8)
				pttSignal = sig
				go func() {
					for ev := range mon.Events() {
						if !isMicButton(ev.Usage) {
							continue
						}
						select {
						case sig <- ev.Down:
						case <-ctx.Done():
							return
						}
					}
				}()
				fmt.Fprintf(os.Stderr, "🎛️  push-to-talk: hold the SayoDevice button → %s\n", pttPersona.Name)
			}
		}

		go func() {
			fmt.Fprintf(os.Stderr, "🎤 listening for '%s' on %s%s (audit: %s)\n", strings.Join(wakeLabels, "' / '"), micLabel, following, au.Path())
			// Quiet by default - daemon mode is intended to run all
			// day so we only print real wake hits + replies + errors.
			// pttActive tracks whether the most recent EvSpeechStart/EvSpeechEnd
			// pair came from the PTT button (Persona set) rather than wake-word
			// VAD (Persona empty), so a following EvSkip can be attributed to
			// the same source - a button press should always show something,
			// but wake-word utterances shouldn't gain new noise.
			var pttActive bool
			err := listener.Run(ctx, listener.Options{
				Personas:   listenerPersonas,
				Audit:      au,
				Notifier:   notifier,
				PTTSignal:  pttSignal,
				PTTPersona: pttPersona,
				Device:     micDevice,
				MicPrefer:  micPrefer,
				OnMicSwitch: func(name string) {
					fmt.Fprintf(os.Stderr, "🎤 mic switched → %s\n", name)
				},
				OnEvent: func(ev listener.Event) {
					switch ev.Kind {
					case listener.EvWake:
						fmt.Fprintf(os.Stderr, "🎯 [%s] %q\n", ev.Persona, ev.Text)
					case listener.EvReply:
						fmt.Fprintf(os.Stderr, "🤖 [%s] %s\n", ev.Persona, ev.Text)
					case listener.EvError:
						fmt.Fprintf(os.Stderr, "listener: %v\n", ev.Err)
					case listener.EvSpeechStart:
						// Only the PTT path tags this event with a persona (see
						// listener.Run); wake-word VAD leaves it empty, so this
						// also gates the noise-prone EvSkip case below.
						pttActive = ev.Persona != ""
						if pttActive {
							fmt.Fprintf(os.Stderr, "🎙  [%s] button held - capturing\n", ev.Persona)
						}
					case listener.EvSpeechEnd:
						if ev.Persona != "" {
							fmt.Fprintf(os.Stderr, "🎙  [%s] button released\n", ev.Persona)
						}
					case listener.EvSkip:
						// Wake-word VAD emits this constantly (every too-short
						// blip, follow-up timeout, etc.) - only surface it when
						// it followed a PTT press, or serve's stderr would fill
						// with noise unrelated to the button.
						if pttActive {
							fmt.Fprintf(os.Stderr, "⏭  skipped: %s\n", ev.Text)
						}
					}
				},
			})
			if err != nil && err != context.Canceled {
				fmt.Fprintf(os.Stderr, "listener exited: %v\n", err)
			}
		}()
	}

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "shutting down…")
	// Give the loop dispatcher a bounded window to unwind before the process
	// drops: its in-flight processTask defers tear down the task's worktree and
	// sbx microVM, which would otherwise leak. The round loop returns promptly
	// on ctx cancel, so this normally returns in well under the timeout.
	if loopDone != nil {
		select {
		case <-loopDone:
		case <-time.After(15 * time.Second):
			fmt.Fprintln(os.Stderr, "loop dispatcher did not drain in 15s; proceeding with shutdown (worktree/sandbox cleanup may be incomplete)")
		}
	}
	// Same bounded-wait treatment as the loop dispatcher above: ctx
	// cancellation propagates into any in-flight harvestOnce call (its
	// distill/LLM calls take the same ctx), so this normally returns
	// almost immediately.
	if sweepDone != nil {
		select {
		case <-sweepDone:
		case <-time.After(15 * time.Second):
			fmt.Fprintln(os.Stderr, "harvest sweep did not stop in 15s; proceeding with shutdown")
		}
	}
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	// LMD shares the same bounded shutdown window as the main daemon -
	// both are plain http.Server instances with no long-lived connections
	// of the sort that would need more time to drain.
	if lmdSrv != nil {
		if err := lmdSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "lmd server shutdown: %v\n", err)
		}
	}
	// Same bounded shutdown window as LMD above.
	if menuLANSrv != nil {
		if err := menuLANSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "menu lan server shutdown: %v\n", err)
		}
	}
	return srv.Shutdown(shutdownCtx)
}

// startLMDServer brings up the LMD (Life Model Decoy) HTTP listener for the
// aida-android client: a second http.Server, bound to the host's Tailscale
// address rather than loopback, exposing only /lmd/v1/* (see
// docs/lmd-protocol.md). primary is the Jarvis assistant and is required -
// it reuses each assistant's LLM/TTS pipeline via AskTextSilent rather than
// building a separate one - so a nil primary (Jarvis disabled) logs a
// warning and returns nil instead of half-starting a listener with nothing
// behind it. twin is the cosmetic Aida assistant built alongside it in
// runHTTPDaemon; it may be nil if construction failed (that path is already
// tolerated - Jarvis alone still boots), in which case LMD serves the
// Jarvis persona only and its default persona degrades to "jarvis" so an
// unqualified request (or one asking for "aida") still resolves to
// something real rather than 500ing. See docs/lmd-protocol.md's Personas
// section for the routing contract this wires up. Likewise for a missing
// Tailscale interface or a bind failure: `aida serve` must still come up
// normally with just the loopback daemon in any of these cases.
//
// whisperModel overrides the Transcriber's whisper.cpp model path (profile
// serve.lmd_whisper_model); empty falls back to stt.TinyEnModelPath() - see
// that function's doc comment for why LMD defaults away from Whisper's own
// zero-value default (small.en), which assumes GPU-accelerated hardware LMD
// may not have.
func startLMDServer(primary, twin *jarvis.Assistant, port int, whisperModel string) *http.Server {
	if primary == nil {
		fmt.Fprintln(os.Stderr, "⚠️  --lmd requires Jarvis (drop --no-jarvis); LMD listener not started")
		return nil
	}

	ip, err := lmd.ResolveTailscaleIP()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  --lmd: resolve Tailscale IP: %v; LMD listener not started\n", err)
		return nil
	}
	if ip == "" {
		fmt.Fprintln(os.Stderr, "⚠️  --lmd: no Tailscale interface found; LMD listener not started")
		return nil
	}

	token, err := lmd.LoadOrGenerateToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  --lmd: load bearer token: %v; LMD listener not started\n", err)
		return nil
	}

	addr := fmt.Sprintf("%s:%d", ip, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  --lmd: bind %s: %v; LMD listener not started\n", addr, err)
		return nil
	}

	// askFuncFor adapts an *jarvis.Assistant's AskTextSilent (which also
	// returns per-stage timing stats the LMD wire response doesn't carry)
	// down to the plain (string, error) shape lmd.Asker expects. Each
	// persona gets its own closure over its OWN assistant so the reply
	// text and the voice it's synthesized in (pd.Synth below) both come
	// from that same assistant - routing only one half would reproduce the
	// bug this wiring exists to fix (Jarvis's reply spoken in Jarvis's
	// voice when the app expected Aida).
	askFuncFor := func(a *jarvis.Assistant) lmd.AskFunc {
		return func(ctx context.Context, sess *jarvis.Session, userText string) (string, error) {
			res, err := a.AskTextSilent(ctx, sess, userText)
			return res.Reply, err
		}
	}

	personas := map[string]lmd.PersonaDeps{
		"jarvis": {Asker: askFuncFor(primary), Synth: primary.TTS(), AckPath: primary.AckAudioPath()},
	}
	// docs/lmd-protocol.md: "The app defaults to aida" - but only when
	// there's an aida persona to default TO. Without the twin, defaulting
	// to "aida" would silently 500 every unqualified/older-client request
	// even though a perfectly good Jarvis persona is right there.
	defaultPersona := "jarvis"
	if twin != nil {
		personas["aida"] = lmd.PersonaDeps{Asker: askFuncFor(twin), Synth: twin.TTS(), AckPath: twin.AckAudioPath()}
		defaultPersona = "aida"
	} else {
		fmt.Fprintln(os.Stderr, "⚠️  --lmd: aida twin unavailable; serving the jarvis persona only (default persona degrades to jarvis)")
	}

	model := whisperModel
	if model == "" {
		model = stt.TinyEnModelPath()
	}

	// Best-effort audit logging - see lmd.Deps.Audit's doc comment. A
	// failure to open the audit log (e.g. a permissions problem under
	// ~/.aida/brain) must never block LMD from starting; it just runs
	// without per-turn audit records in that case.
	auLogger, auErr := audit.New("")
	if auErr != nil {
		fmt.Fprintf(os.Stderr, "⚠️  --lmd: open audit log: %v; LMD turns will not be audited\n", auErr)
		auLogger = nil
	}

	mux := http.NewServeMux()
	lmd.New(mux, lmd.Deps{
		Transcriber:    &stt.Whisper{Model: model},
		Personas:       personas,
		DefaultPersona: defaultPersona,
		Token:          token,
		Version:        buildVersion(),
		Audit:          auLogger,
	})

	lmdHTTP := &http.Server{Addr: addr, Handler: mux}
	go func() {
		names := make([]string, 0, len(personas))
		for name := range personas {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(os.Stderr, "📱 LMD listening on http://%s  (Earth-1218) - personas: %s (default: %s)\n",
			addr, strings.Join(names, ", "), defaultPersona)
		if err := lmdHTTP.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "lmd server: %v\n", err)
		}
	}()
	return lmdHTTP
}

// newMenuLANMux builds the mux startMenuLANServer binds to 0.0.0.0: ONLY the
// menu routes (registerMenuWebRoutes, isLAN=true -- see its doc comment) plus
// the shared favicon/touch-icon/manifest routes, so a kid's phone gets the
// icon and a home-screen bookmark of /menu looks right. Factored out of
// startMenuLANServer so tests can exercise exactly what's reachable on the
// LAN listener without binding a real socket.
func newMenuLANMux(menuPath string) *http.ServeMux {
	mux := http.NewServeMux()
	registerMenuWebRoutes(mux, menuPath, true)
	registerFaviconRoutes(mux)
	return mux
}

// startMenuLANServer brings up a second http.Server, bound to ALL interfaces
// (0.0.0.0, unlike the loopback-only main daemon), carrying ONLY the menu
// routes on a mux built fresh for this purpose (registerMenuWebRoutes is the
// same registrar used on the main daemon's mux - nothing else is ever added
// to this one). This is deliberately its own listener rather than just
// binding the main daemon's mux to 0.0.0.0: the main daemon carries /tasks,
// /mcp/call, and the rest of the remote-execution surface with no auth, so
// widening ITS bind address to reach a kid's phone would also expose all of
// that. A bind failure (port in use, etc.) logs a warning and returns nil -
// `aida serve` must still come up normally with just the loopback daemon.
func startMenuLANServer(menuPath string, port int) *http.Server {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠️  menu LAN listener: bind %s: %v; not started\n", addr, err)
		return nil
	}

	mux := newMenuLANMux(menuPath)

	// Best-effort hostname for the fridge-worthy URL in the startup log; a
	// resolution failure just falls back to the bare IP:port form printed
	// alongside it rather than blocking startup.
	host, herr := os.Hostname()
	if herr != nil || host == "" {
		host = "localhost"
	}

	// .local names do not always resolve (Bonjour is flaky on this LAN),
	// so print the first non-loopback IPv4 too; that one always works.
	lanIP := firstLANIPv4()

	menuHTTP := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if lanIP != "" {
			fmt.Fprintf(os.Stderr, "🍽️  menu LAN listener: http://%s:%d/menu  or  http://%s.local:%d/menu  (put this on the fridge)\n", lanIP, port, host, port)
		} else {
			fmt.Fprintf(os.Stderr, "🍽️  menu LAN listener: http://%s.local:%d/menu  (put this on the fridge)\n", host, port)
		}
		if err := menuHTTP.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "menu lan server: %v\n", err)
		}
	}()
	return menuHTTP
}

// bindWithTakeover binds a TCP listener on addr. If the port is already held
// by a prior `aida serve` daemon, it stops that daemon (SIGTERM, escalating to
// SIGKILL) and inherits the port, so re-launching the daemon never leaves two
// instances fighting over the mic. If the holder is *not* an aida daemon, it
// refuses to kill it and returns the original bind error - we only ever
// reclaim our own.
func bindWithTakeover(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil // port free - common case, no prior daemon
	}

	pid := listenerPID(addr)
	if pid <= 0 || pid == os.Getpid() {
		return nil, fmt.Errorf("bind %s: %w", addr, err) // can't identify holder; surface it
	}
	if !isAidaDaemon(pid) {
		return nil, fmt.Errorf("bind %s: %w (held by pid %d, which is not an aida daemon - refusing to kill it)", addr, err, pid)
	}

	fmt.Fprintf(os.Stderr, "↺ stopping existing aida daemon (pid %d) to take over %s\n", pid, addr)
	_ = syscall.Kill(pid, syscall.SIGTERM)

	// Poll for the port to free as the old daemon unwinds (it tears down the
	// mic capture and any loop worktrees on SIGTERM, so give it a few seconds).
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if ln, err := net.Listen("tcp", addr); err == nil {
			// The port is free again, but that only means pid closed its
			// listening socket - not that pid itself has exited. Confirm it
			// actually died (forcing it with SIGKILL if not) before we hand
			// back the listener, so the old process never lingers holding the
			// HID device and its deferred remap-restore.
			reapDaemon(pid)
			return ln, nil
		}
	}

	// Overstayed its welcome - force it and try once more.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(300 * time.Millisecond)
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind %s after stopping pid %d: %w", addr, pid, err)
	}
	// Same confirmation on this fallback path: the SIGKILL above should be
	// unstoppable, but reapDaemon costs nothing to call again and closes the
	// gap if pid was somehow still unwinding when net.Listen raced it.
	reapDaemon(pid)
	return ln, nil
}

// processAlive reports whether pid still exists. Signal 0 does the
// permission/existence check without delivering a signal, so it's safe to
// call on any pid we merely suspect might still be running.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// reapDaemon makes sure a SIGTERM'd daemon actually exited, not just that it
// let go of the port. bindWithTakeover's poll loop only checks whether
// net.Listen succeeds again - but releasing the port is not the same as
// exiting. A process unbinds its listening socket as an early step of
// shutting down, then keeps running through the rest of its teardown (or, in
// the bug this guards against, never finishes tearing down at all). Either
// way, bindWithTakeover would otherwise return a fresh listener the instant
// the port frees and leave that old pid running in the background forever -
// still holding the SayoDevice HID monitor open, still carrying a deferred
// pttRestoreRemap() that wipes the push-to-talk key remap whenever it
// eventually dies. That is exactly how four such daemons accumulated in one
// day (started 20:47, 20:50, 22:55, 10:20), each one's eventual
// remap-restore misdiagnosed as a hardware fault.
//
// reapDaemon polls every 100ms for up to 3 seconds for pid to actually exit.
// If it is still alive after that, it force-kills it with SIGKILL so a
// daemon that ignores SIGTERM (or gets stuck mid-teardown) can never linger
// past the takeover.
func reapDaemon(pid int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		fmt.Fprintf(os.Stderr, "↺ pid %d released the port but is still running; forcing it\n", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// listenerPID returns the pid of the process LISTENing on addr's port via
// lsof, or 0 if none/unknown. macOS/BSD lsof; the whole daemon is already
// macOS-specific (afplay, avfoundation).
func listenerPID(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	out, err := exec.Command("lsof", "-nP", "-t", "-iTCP:"+portStr, "-sTCP:LISTEN").Output()
	if err != nil {
		return 0
	}
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			return pid // first listener wins
		}
	}
	return 0
}

// isAidaDaemon reports whether pid is an `aida serve` process - the executable
// basename is "aida" and "serve" appears in its args. Guards bindWithTakeover so
// we never SIGKILL an unrelated process that happens to hold the port.
//
// WARNING: this matches ANY `aida serve` process, including stdio MCP
// instances spawned as children of editor sessions (Claude Code, Cursor,
// etc.) - `aida serve` is dual-mode, and isAidaDaemon has no way to tell
// which mode a given pid is running in. Never repurpose this check to sweep
// or kill `aida serve` processes broadly; a broad sweep would kill live
// MCP servers that legitimately share the name. It must only ever be used,
// as it is here, to confirm the identity of a pid that lsof already named as
// the actual TCP port holder (see listenerPID) immediately before signalling
// that single confirmed pid.
func isAidaDaemon(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 || filepath.Base(fields[0]) != "aida" {
		return false
	}
	for _, a := range fields[1:] {
		if a == "serve" {
			return true
		}
	}
	return false
}

// watchJobsForNotifications scans the jobs store every 2s for jobs in
// awaiting_input or terminal (done/failed) states that haven't been
// notified yet (NotifiedAt == ""). For each, it formats a one-line
// voice utterance, enqueues it on the notifier, then stamps the
// manifest's NotifiedAt so the next pass (and any future daemon
// restart) won't re-speak it.
//
// Two-second cadence matches the ask_user tool's input-poll cadence
// so a paused agent and its corresponding "Sir, …" utterance land in
// the queue within the same tick. Survives ctx cancellation cleanly -
// the next daemon start will resume noticing un-notified transitions.
func watchJobsForNotifications(ctx context.Context, store *jobs.Store, profile string, n *notify.Notifier) {
	if n == nil || store == nil {
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		scanJobsForNotifications(store, profile, n)
	}
}

// scanJobsForNotifications is the inner per-tick body of
// watchJobsForNotifications, split out for testability.
func scanJobsForNotifications(store *jobs.Store, profile string, n *notify.Notifier) {
	rows, err := store.List(jobs.ListOpts{Limit: 200})
	if err != nil {
		// Best-effort - one tick failing is not worth surfacing
		// loudly. The next tick will retry.
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, j := range rows {
		if j.NotifiedAt != "" {
			continue
		}
		var msg string
		switch j.State {
		case jobs.StateAwaitingInput:
			msg = formatAwaitingNotice(&j)
		case jobs.StateAwaitingApproval:
			msg = formatApprovalNotice(&j)
		case jobs.StateDone, jobs.StateIncomplete:
			msg = formatDoneNotice(&j)
		case jobs.StateFailed:
			msg = formatFailedNotice(&j)
		default:
			continue
		}
		if msg == "" {
			continue
		}
		n.Enqueue(msg)
		// Stamp first, then ignore stamp errors - if the stamp
		// fails we may re-speak on the next tick, which is better
		// than crashing or silently dropping the next legitimate
		// notification.
		if err := store.MarkNotified(j.RunID, now); err != nil {
			fmt.Fprintf(os.Stderr, "notify: MarkNotified(%s) failed: %v\n", j.RunID, err)
		}
		// Cleanup hook for pr_work worktrees on terminal transitions.
		// Done in the same pass as the notification stamp so a single
		// 2s tick handles both. Best-effort - failures here log but
		// don't unwind the notification.
		if jobs.IsTerminalState(j.State) && j.WorktreePath != "" {
			go cleanupWorktreePath(j.WorktreePath)
		}
		_ = profile // reserved for future per-profile notification routing
	}
}

// cleanupWorktreePath is the daemon-side wrapper around the worktree
// cleanup hook. Pushed to its own goroutine so a slow `git worktree
// remove` (dirty index, network filesystem) can't block the next
// notify tick.
func cleanupWorktreePath(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	// Find parent repo via the worktree's .git common-dir, same shape
	// as the tools-side helper but inlined here to keep serve.go free
	// of voice-tool imports.
	out, err := exec.Command("git", "-C", path, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		_ = os.RemoveAll(path)
		return
	}
	commonDir := strings.TrimSpace(string(out))
	parentGit := commonDir
	if filepath.Base(commonDir) != ".git" {
		parentGit = filepath.Dir(commonDir)
	}
	parentRoot := filepath.Dir(parentGit)
	rm := exec.Command("git", "-C", parentRoot, "worktree", "remove", "--force", path)
	rm.Stdout = nil
	rm.Stderr = nil
	if err := rm.Run(); err != nil {
		_ = os.RemoveAll(path)
	}
}

// formatAwaitingNotice composes the one-line "sir, …" string the
// notifier will speak when a job pauses for user input. Kept terse
// so the user gets the prompt verbatim with minimal preamble.
func formatAwaitingNotice(j *jobs.Job) string {
	prompt := j.AwaitingPrompt
	if prompt == "" {
		prompt = "needs your input"
	}
	return fmt.Sprintf("Sir, the %s is asking: %s", spokenJobRef(j), prompt)
}

// formatDoneNotice composes the spoken notice for a job that reached
// done OR incomplete (scanJobsForNotifications routes both here). It
// used to say a flat "is done" regardless of whether the run actually
// verified anything, the exact vacuum a model filled with an invented
// success claim. jobs.DescribeOutcome supplies the real evidence
// (state, termination reason, verified outcome or its absence, and a
// usable summary), or the UnverifiedResultNotice wording when there's
// nothing to back a claim up, so the spoken line can't over-promise
// either.
func formatDoneNotice(j *jobs.Job) string {
	return fmt.Sprintf("Sir, the %s %s", spokenJobRef(j), jobs.DescribeOutcome(j))
}

// formatApprovalNotice composes the spoken prompt when a job pauses awaiting
// human approval for an irreversible action (merge/send/writeback). Surfaces
// the payload + the PR link when present so "approve PR 583" is unambiguous.
func formatApprovalNotice(j *jobs.Job) string {
	what := j.ApprovalPayload
	if what == "" {
		what = "an action"
		if j.ApprovalAction != "" {
			what = "a " + j.ApprovalAction
		}
	}
	return fmt.Sprintf("Sir, the %s is ready and needs your approval to %s. Say approve or reject.",
		spokenJobRef(j), what)
}

func formatFailedNotice(j *jobs.Job) string {
	reason := j.Error
	if reason == "" {
		reason = "an unknown error"
	}
	return fmt.Sprintf("Sir, the %s failed: %s.", spokenJobRef(j), reason)
}

// spokenJobRef renders a job's spoken identity for notifications:
// "pr-583 job, amber-otter," - descriptive label plus the derived
// handle as an appositive (the commas buy natural TTS pauses). The
// handle matters: the system prompt tells the model to pass "the
// handle from the most recent notification" to job_send_input, and
// resolveJobRef matches handles but NOT kinds, so a notice that only
// said "the pr_work job" gave the user a name that resolved nothing.
func spokenJobRef(j *jobs.Job) string {
	return fmt.Sprintf("%s job, %s,", jobLabel(j), jobs.DeriveHandle(j.RunID))
}

// jobLabel picks the most human-readable label for a job: the task
// slug if present, otherwise the kind (e.g. "pr_work") or "agent" as
// a last resort.
func jobLabel(j *jobs.Job) string {
	if j.TaskSlug != "" {
		return j.TaskSlug
	}
	if j.Kind != "" {
		return j.Kind
	}
	return "agent"
}

// serveHTTPMode decides whether `aida serve` should run as the HTTP daemon
// (true) or the MCP stdio server (false). Extracted as a pure function so the
// mode-selection logic is table-testable without spawning a real process or
// faking stdin's TTY-ness. --stdio always wins: it is the explicit override
// for an MCP client that allocated a PTY for the child, so a TTY-looking
// stdin must never be allowed to imply the HTTP daemon in that case.
func serveHTTPMode(stdioOnly, httpOnly, stdinIsTTY bool) bool {
	return !stdioOnly && (httpOnly || stdinIsTTY)
}

// isTTY returns true when fd is connected to a terminal. We use it to
// auto-detect whether `aida serve` was launched interactively (HTTP daemon
// mode) or spawned by an MCP client like Claude Code (stdio mode).
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// firstLANIPv4 returns the first non-loopback, non-link-local IPv4 address
// on an interface that is up, or "" when there is none. Used only for the
// menu LAN listener's startup log; nothing binds to it.
func firstLANIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}
