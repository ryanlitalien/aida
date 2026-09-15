// Package jarvis is aida's voice-assistant layer. It wires:
//   - audio capture + VAD (later phase)
//   - wake-word detection (later phase)
//   - speech-to-text (later phase)
//   - LLM with in-process tool dispatch into internal/brain
//   - text-to-speech via Piper
//   - audio playback (afplay for now)
//
// The primary entry point is Assistant.AskText, which takes a transcribed (or
// directly-typed) user query, runs it through the LLM with tool use, and
// speaks the answer through the configured Synthesizer. Returns the spoken
// text so callers can also log/display it.
package jarvis

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
	"github.com/ryanlitalien/aida/internal/jarvis/llm"
	"github.com/ryanlitalien/aida/internal/jarvis/tools"
	"github.com/ryanlitalien/aida/internal/jarvis/tts"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/mcp"
)

type Assistant struct {
	cfg       Config
	brain     *brain.Brain
	llm       *llm.Client
	tts       tts.Synthesizer
	discovery *mcp.MCPDiscovery // nil if no MCP servers configured
	jobsStore *jobs.Store

	// Pre-synthesized acknowledgement plays before slow tools fire to mask
	// the latency. Empty if pre-synthesis failed (we tolerate it; ack is
	// non-essential to correctness).
	ackWAV string

	// Pre-synthesized "still working" line played at the 90s mark of a
	// long-running aida_query so the user hears a sign of life before
	// the 180s hard cap. Empty if pre-synth failed.
	midProgressWAV string

	// Pre-synthesized wake-only prompts. When the user says just the wake
	// phrase with no follow-up, the listener arms for the next utterance
	// and we play one of these to make the gap less awkward. Rotation is
	// random per turn so it doesn't feel canned. Empty if pre-synth failed.
	wakeOnlyWAVs []string

	// Last non-rating turn cached for the voice feedback tools. Updated
	// by the listener via SetLastTurn after a turn completes; the rating
	// tools read it via LastTurn to know which turn they're attaching
	// feedback to. Rating-only turns are skipped so "thumbs down" rates
	// the prior real turn, not itself.
	lastTurn   *audit.Record
	lastTurnMu sync.RWMutex

	// activity is the live "what is Jarvis doing right now" tracker that
	// backs the status page's activity card. Never nil after New.
	activity *Activity

	// sharedDeps is set on a cosmetic-twin assistant built via NewTwin. Its
	// discovery + jobsStore are borrowed from the primary, so Close must not
	// free them (the primary owns their lifecycle).
	sharedDeps bool
}

// wakeOnlyPhrases is the rotation pool played when a bare wake-phrase
// utterance arms the listener for a follow-up. Kept short - the user is
// about to talk again, this is just a "I'm here" beat.
var wakeOnlyPhrases = []string{
	"How can I help you, sir?",
	"Yes, sir?",
	"At your service.",
	"What can I do for you, sir?",
	"I'm listening, sir.",
}

// ackPhrase is what Jarvis says before a slow tool runs.
const ackPhrase = "One moment, sir."

// midProgressPhrase fires at the 90s mark of a aida_query subprocess so
// the user hears a sign of life instead of sitting in silence until the
// 180s timeout. Synthesized once at startup; same path-and-play model as
// ackWAV.
const midProgressPhrase = "I'm still working on your request, sir. Trying for another ninety seconds."

// slowTools fire the pre-tool acknowledgement. Fast in-process tools
// (tasks_list, current_time) skip it. minecraft_ask is here because
// the SSH'd claude -p round-trip routinely takes 5-10s; mcp_call_tool
// is here because remote MCP servers (Gmail search, Notion fetch) can
// take several seconds and Haiku gives the user nothing in the meantime.
var slowTools = map[string]bool{
	"aida_query":    true,
	"minecraft_ask": true,
	"mcp_call_tool": true,
	// job_start fires the ack so the user hears "One moment, sir."
	// while the subprocess spawns + the worktree-or-equivalent setup
	// runs. job_status / job_list are cheap and skip the ack.
	"job_start": true,
}

// playAudio is tts.Play behind a seam so tests can verify local-playback
// gating (onToolUse, progressAck) with an injected fake instead of
// actually shelling out to afplay. Production code never reassigns this;
// tests that do must restore the original via t.Cleanup so they don't
// bleed into other tests in the package.
var playAudio = tts.Play

// New constructs an Assistant. The caller owns brain.Brain (we don't close
// it). apiKey is the Anthropic key.
//
// MCP discovery runs once at startup: connect to every server in the
// merged ~/.claude/.mcp.json + ./.mcp.json (the "aida" server is
// skipped inside discovery.ConnectAll to prevent recursion), then
// snapshot the tool list. Failures are non-fatal - the daemon must
// boot cleanly even if every MCP server is unreachable, the rest of
// Jarvis still works.
//
// Opens a per-profile jobs store so the voice-side job_* tools have a
// queue to push work into. nil profile / store-open failure is
// tolerated - the tools simply aren't registered.
func New(cfg Config, b *brain.Brain, apiKey string) (*Assistant, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("jarvis: ANTHROPIC_API_KEY not set")
	}

	// Best-effort: pull ELEVENLABS_API_KEY (and friends) out of
	// ~/.aida/secrets.env so we never have to shell to `op` (and trip
	// the 1Password Touch ID prompt) at daemon startup.
	if err := LoadSecretsEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis: load secrets.env: %v (continuing)\n", err)
	}

	discovery := initMCPDiscovery()

	// Best-effort jobs store. If config or open fails the voice loop
	// still works, just without job_start / job_list / job_status.
	var jobsStore *jobs.Store
	if c, err := config.LoadConfig(); err == nil {
		if _, pname := c.ActiveProfileConfig(); pname != "" {
			if s, oerr := jobs.Open(pname); oerr == nil {
				jobsStore = s
			} else {
				fmt.Fprintf(os.Stderr, "jarvis: jobs store open failed: %v (job_* tools disabled)\n", oerr)
			}
		}
	}

	return newAssistant(cfg, b, apiKey, discovery, jobsStore, false)
}

// NewTwin builds a cosmetic-twin assistant - a second wake-word persona with
// its own voice, display name, and pre-synthesized phrase pool - that SHARES
// this assistant's brain, MCP discovery, and jobs store. Sharing avoids
// double-spawning stdio MCP servers and opening a redundant jobs handle; the
// twin's Close is a no-op for those borrowed handles (the primary owns them).
// Only the tools registry, LLM client, synthesizer, and phrase WAVs are rebuilt.
func (a *Assistant) NewTwin(cfg Config, apiKey string) (*Assistant, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("jarvis: ANTHROPIC_API_KEY not set")
	}
	return newAssistant(cfg, a.brain, apiKey, a.discovery, a.jobsStore, true)
}

// aidaDispatcherPrompt is appended to Aida's system prompt (only when she has a
// Dispatcher). It nudges her to delegate through the roster tools instead of
// answering big-domain or named-person requests herself.
const aidaDispatcherPrompt = `You are also a chief of staff with a roster of specialist agents. When the user
names one of your agents, or asks about a domain one of them owns, use the
ask_agent tool to delegate and relay what they report. Use roster_list when the
user asks who is on the team or what you can delegate. If a delegated task runs
in the background, tell the user you have handed it off and will report back.
For anything outside your roster, answer normally with your other tools.`

// newAssistant is the shared constructor behind New and NewTwin. discovery and
// jobsStore are injected (freshly built by New, borrowed by NewTwin); sharedDeps
// records whether Close should free them.
func newAssistant(cfg Config, b *brain.Brain, apiKey string, discovery *mcp.MCPDiscovery, jobsStore *jobs.Store, sharedDeps bool) (*Assistant, error) {
	// Pre-allocate the Assistant so the progress-ack closure passed to
	// tools.New can read its midProgressWAV field (populated below, after
	// the synthesizer is built). The closure runs at the 90s mark of a
	// long-running aida_query - by that point pre-synth has long
	// completed, so the wav path is ready.
	a := &Assistant{cfg: cfg, brain: b, discovery: discovery, jobsStore: jobsStore, activity: newActivity(), sharedDeps: sharedDeps}

	// a.progressAck is a method value (see its doc comment below) so it
	// can be passed straight into tools.New - reading a.midProgressWAV at
	// call time works the same as the closure this replaced, since a is
	// already allocated even though the field isn't populated until the
	// pre-synthesis step further down.
	registry := tools.New(b, cfg.Home, loadSourceDomains(), discovery, jobsStore, a.progressAck, a.LastTurn, cfg.AutoSync)
	// Aida-only: upgrade her registry with the roster dispatch tools (ask_agent,
	// roster_list). Jarvis passes a nil Dispatcher, so his registry is
	// byte-identical to before.
	if cfg.Dispatcher != nil {
		tools.RegisterRosterTools(registry, cfg.Dispatcher)
	}
	cli := llm.New(apiKey, cfg.Model, cfg.MaxTokens, cfg.Temp, cfg.Home, registry)
	cli.Persona = cfg.Persona
	cli.Tone = cfg.Tone

	// Load soul.yaml - the same "who is asking" context the engine injects
	// into its parser/router/synthesis prompts. Without it Jarvis knows the
	// user's home city but not their name, employer, or vocabulary (e.g.
	// disambiguating a company name Whisper transcribed ambiguously -
	// only the soul knows which company the user actually means). Best-effort:
	// a missing or broken soul just means no user-context block.
	if soul, err := config.LoadSoul(); err == nil {
		cli.UserContext = soul.ForPrompt()
	} else {
		fmt.Fprintf(os.Stderr, "jarvis: soul load: %v (continuing without user context)\n", err)
	}

	// Load hosts: from config.yaml - descriptive knowledge of the user's
	// machines (what each is for, how it's reached) so the assistant
	// doesn't burn a turn discovering blind that it lacks credentials for
	// a host it didn't know existed. Best-effort and additive, like soul
	// above: a missing config.yaml or an empty/absent hosts: block just
	// means no hosts block in the prompt, never an error worth surfacing
	// to the user.
	if hosts, err := config.LoadHosts(); err == nil {
		cli.HostsContext = hosts.ForPrompt()
	} else {
		fmt.Fprintf(os.Stderr, "jarvis: hosts load: %v (continuing without hosts context)\n", err)
	}

	// Aida-only chief-of-staff block. Appended after the soul assignment above
	// (which reassigns UserContext) so it is not clobbered. Jarvis, with a nil
	// Dispatcher, never sees it and his prompt stays byte-identical.
	if cfg.Dispatcher != nil {
		cli.UserContext = strings.TrimSpace(cli.UserContext + "\n\n" + aidaDispatcherPrompt)
	}

	piper := &tts.Piper{
		Bin:        cfg.PiperBin,
		Model:      cfg.ModelPath(),
		EspeakData: cfg.EspeakDataDir,
	}
	if _, err := os.Stat(piper.Model); err != nil {
		return nil, fmt.Errorf("jarvis: piper model missing at %s - run `make jarvis-deps`", piper.Model)
	}

	// Prefer ElevenLabs when both a voice id and an API key are available.
	// VoiceID can come from config or $ELEVENLABS_VOICE_ID; key resolution
	// tries the env var first, then `op read <ElevenLabsOpRef>`. The env
	// override is a global "revoice the default persona" knob, so a cosmetic
	// twin ignores it - otherwise both personas collapse onto one voice and
	// the twin is pointless.
	var synth tts.Synthesizer = piper
	voiceID := cfg.ElevenLabsVoiceID
	if v := os.Getenv("ELEVENLABS_VOICE_ID"); v != "" && !sharedDeps {
		voiceID = v
	}
	if voiceID != "" {
		if key := tts.ResolveElevenLabsKey(cfg.ElevenLabsOpRef); key != "" {
			synth = &tts.ElevenLabs{
				APIKey:     key,
				VoiceID:    voiceID,
				ModelID:    cfg.ElevenLabsModelID,
				Stability:  cfg.ElevenLabsStability,
				Similarity: cfg.ElevenLabsSimilarity,
				Style:      cfg.ElevenLabsStyle,
				Speed:      cfg.ElevenLabsSpeed,
				Volume:     cfg.Volume,
			}
		}
	}

	a.llm = cli
	a.tts = synth

	// Pre-synthesize the ack and the wake-only prompts once so playback
	// is instant. Failures are non-fatal - we just skip the unsynthesized
	// phrase.
	if !cfg.TextOnly {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if wav, err := synth.Synthesize(ctx, ackPhrase); err == nil {
			a.ackWAV = wav
		}
		if wav, err := synth.Synthesize(ctx, midProgressPhrase); err == nil {
			a.midProgressWAV = wav
		}
		for _, p := range wakeOnlyPhrases {
			if wav, err := synth.Synthesize(ctx, p); err == nil {
				a.wakeOnlyWAVs = append(a.wakeOnlyWAVs, wav)
			}
		}
	}

	// Wire the LLM client's tool-use callback to play the ack before
	// slow tools fire. Non-blocking on the work itself - Play returns
	// only when audio finishes, which is the desired sequencing
	// (user hears ack, then a brief pause while the tool runs, then
	// the real reply). See onToolUse's doc comment for the gating logic.
	cli.OnToolUse = a.onToolUse

	return a, nil
}

// onToolUse is the LLM client's pre-tool-dispatch hook (Client.OnToolUse):
// it records the live tool for the status page's activity card for EVERY
// tool call, then plays the "One moment, sir." ack before a slowTools
// tool actually runs - fast in-process tools (tasks_list, current_time)
// skip the ack entirely.
//
// Skipped when ctx carries localAudioSuppressed - an LMD/Android turn
// (see AskTextSilent and localAudioSuppressed's doc comments): the LMD
// protocol is one-shot request/response, so this ack could never reach
// the phone anyway, and playing it on this process's own speakers instead
// would leak that remote turn into the room.
func (a *Assistant) onToolUse(ctx context.Context, ev llm.ToolUseEvent) {
	a.activity.SetCurrentTool(ev.Name)
	if !slowTools[ev.Name] || a.ackWAV == "" || localAudioSuppressed(ctx) {
		return
	}
	ackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = playAudio(ackCtx, a.ackWAV)
}

// progressAck plays the 90s "still working" beat for a long-running
// aida_query call (see midProgressPhrase and aidaQueryTool's own doc
// comment for why the 90s mark matters). ctx is the turn's context,
// threaded down through Registry.Run into aidaQueryTool's Run and passed
// to this method at the point it fires - see aidaQueryTool's doc comment
// for that plumbing.
//
// Skipped when ctx carries localAudioSuppressed, for the same reason
// onToolUse skips its ack - see that method's doc comment. Also a no-op
// if pre-synthesis of midProgressWAV failed (empty string).
func (a *Assistant) progressAck(ctx context.Context) {
	if a.midProgressWAV == "" || localAudioSuppressed(ctx) {
		return
	}
	playCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = playAudio(playCtx, a.midProgressWAV)
}

// Session holds short-term conversational memory for an active listener
// loop. Only the listener owns one - typed CLI invocations and HTTP
// requests stay stateless one-shots so they can't accidentally cross
// threads of conversation. Idle for IdleTimeout (default 30 minutes)
// and the next turn starts fresh - the window is wide enough to absorb
// the user pausing to look at the world, talking to someone else for
// a beat, or wandering off mid-build for several minutes before
// following up. The durable thread memory in the brain backstops gaps
// longer than this; the in-process window just cuts churn.
//
// MaxTurns caps history length so the prompt stays bounded even if the
// user keeps the listener active for hours.
//
// The struct is JSON-serialized to disk (see session_store.go) so a
// `aida serve` restart resumes the thread rather than starting blank.
type Session struct {
	Turns       []llm.TurnPair `json:"turns"`
	LastAt      time.Time      `json:"last_at"`
	MaxTurns    int            `json:"max_turns"`    // default 12 if zero
	IdleTimeout time.Duration  `json:"idle_timeout"` // default 30m if zero

	// mu guards Save's marshal against any future concurrent writer.
	// Tagged out of JSON; unexported fields are skipped anyway.
	mu sync.Mutex `json:"-"`
}

// Session window defaults - the ONLY place these numbers live. NewSession,
// Stale's zero-value fallback, AskTextInSession's trim, and LoadSession's
// restore all read them, so widening the window is a one-line change that
// can't leave a stale copy behind (the old 5m Stale fallback survived the
// 30m widening exactly that way).
const (
	defaultSessionMaxTurns    = 12
	defaultSessionIdleTimeout = 30 * time.Minute
)

// NewSession returns a Session with sensible defaults. Callers can
// override MaxTurns / IdleTimeout on the returned struct.
func NewSession() *Session {
	return &Session{MaxTurns: defaultSessionMaxTurns, IdleTimeout: defaultSessionIdleTimeout}
}

// Stale reports whether the session has been idle past its timeout
// and should be reset before the next turn.
func (s *Session) Stale() bool {
	if s == nil || s.LastAt.IsZero() {
		return false
	}
	timeout := s.IdleTimeout
	if timeout <= 0 {
		timeout = defaultSessionIdleTimeout
	}
	return time.Since(s.LastAt) > timeout
}

// Reset drops all prior turns. Called automatically when Stale() is
// true at the start of the next turn.
func (s *Session) Reset() {
	s.Turns = nil
	s.LastAt = time.Time{}
}

// LastTurn returns a copy of the most recent non-rating turn's audit
// record, or nil if no such turn is cached. Used by the voice feedback
// tools to attach a rating to the prior real turn - rating-only turns
// (jarvis_thumbs_up/down, jarvis_note) deliberately don't update this
// so "thumbs down" always lands on the turn the user is actually
// reacting to, not on the rating turn itself.
func (a *Assistant) LastTurn() *audit.Record {
	if a == nil {
		return nil
	}
	a.lastTurnMu.RLock()
	defer a.lastTurnMu.RUnlock()
	if a.lastTurn == nil {
		return nil
	}
	cp := *a.lastTurn
	return &cp
}

// SetLastTurn caches the just-completed audit record for the voice
// feedback tools. The listener calls this once a turn finishes, only
// when the turn included at least one non-rating tool call (rating
// turns are transparent - they don't supersede the turn they're
// rating). Safe to pass nil; that resets the cache.
func (a *Assistant) SetLastTurn(r *audit.Record) {
	if a == nil {
		return
	}
	a.lastTurnMu.Lock()
	defer a.lastTurnMu.Unlock()
	if r == nil {
		a.lastTurn = nil
		return
	}
	cp := *r
	a.lastTurn = &cp
}

// ActivitySnapshot returns a point-in-time copy of the live/recent activity
// for the status page. Safe on a nil Assistant.
func (a *Assistant) ActivitySnapshot() ActivitySnapshot {
	if a == nil {
		return ActivitySnapshot{Recent: []TurnActivity{}}
	}
	return a.activity.Snapshot()
}

// RecordActivity appends a completed turn to the recent-activity ring. Called
// by the listener after every wake-triggered turn (including errored ones, so
// failures are visible). Distinct from PromoteTurn, which writes durable
// memory only for substantive turns.
func (a *Assistant) RecordActivity(rec audit.Record) {
	if a == nil {
		return
	}
	a.activity.PushTurn(rec)
}

// TTS returns the assistant's text-to-speech synthesizer. Exposed so
// the daemon's notify.Notifier can synthesize utterances using the
// same voice as the rest of Jarvis without needing to construct a
// second Piper handle (which would double-load the .onnx model).
func (a *Assistant) TTS() tts.Synthesizer {
	if a == nil {
		return nil
	}
	return a.tts
}

// JobsStore returns the assistant's jobs store (nil if none was opened). Used
// to build the Aida dispatcher's Deps on the same shared handle.
func (a *Assistant) JobsStore() *jobs.Store {
	if a == nil {
		return nil
	}
	return a.jobsStore
}

// Discovery returns the assistant's MCP discovery client (nil if no MCP
// servers are configured). Used to build the Aida dispatcher's Deps.
func (a *Assistant) Discovery() *mcp.MCPDiscovery {
	if a == nil {
		return nil
	}
	return a.discovery
}

// AckAudioPath returns the path to this assistant's pre-synthesized "One
// moment, sir." acknowledgement clip (see ackWAV's doc comment) - a .mp3
// when ElevenLabs is the configured engine, a .wav for Piper. Returns "" if
// pre-synthesis failed or was skipped (cfg.TextOnly), which is a normal,
// tolerated condition, not an error. Exported so internal/jarvis/lmd, which
// cannot reach the unexported ackWAV field directly, can serve it over
// GET /lmd/v1/ack (docs/lmd-protocol.md).
func (a *Assistant) AckAudioPath() string {
	if a == nil {
		return ""
	}
	return a.ackWAV
}

// PlayWakeOnlyPrompt speaks a randomly-chosen "I'm listening" phrase
// for when the user said only the wake phrase and the listener is
// arming for their follow-up. Returns immediately if no prompts were
// pre-synthesized (e.g. TextOnly config) - non-fatal.
func (a *Assistant) PlayWakeOnlyPrompt(ctx context.Context) {
	if len(a.wakeOnlyWAVs) == 0 {
		return
	}
	wav := a.wakeOnlyWAVs[rand.Intn(len(a.wakeOnlyWAVs))]
	_ = tts.PlayAt(ctx, wav, a.cfg.Volume)
}

// AskResult bundles the spoken/printed reply with per-stage timing for the
// audit log. Zero values mean "stage didn't run" or "skipped".
type AskResult struct {
	Reply  string
	Stats  llm.AskStats
	TTSMs  int64
	PlayMs int64

	// GroundingRewritten is true when the grounding guard replaced the
	// model's reply (it claimed a background job on a turn that used no
	// job_* tool at all). OriginalReply holds the pre-rewrite text for
	// offline review.
	GroundingRewritten bool
	OriginalReply      string

	// EmptyReplyFallback is true when the model returned no text at all
	// despite a tool succeeding this turn, and askAndSpeak substituted a
	// spoken fallback (either the tool's own short result or a generic
	// acknowledgement) instead of letting the turn go silent. See
	// fallbackForSilentReply.
	EmptyReplyFallback bool
}

// AskText runs a single user-text turn through the LLM (with tool use) and
// speaks the response. Returns the spoken text. If cfg.TextOnly is true the
// TTS step is skipped (useful for tests / no-mic environments).
func (a *Assistant) AskText(ctx context.Context, userText string) (string, error) {
	res, err := a.AskTextWithStats(ctx, userText)
	return res.Reply, err
}

// AskTextWithStats is AskText that also returns per-stage timing. Use this
// when you want to log the round-trip into the audit log.
func (a *Assistant) AskTextWithStats(ctx context.Context, userText string) (AskResult, error) {
	return a.askAndSpeak(ctx, nil, userText, true)
}

// AskTextInSession is AskTextWithStats with short-term conversational
// memory. Listener-only - typed CLI and HTTP callers stay stateless via
// AskTextWithStats. The session is reset before the turn if it has been
// idle past its timeout, then the new (user, assistant) pair is appended
// on success and trimmed to the configured MaxTurns window.
func (a *Assistant) AskTextInSession(ctx context.Context, sess *Session, userText string) (AskResult, error) {
	if sess == nil {
		return a.AskTextWithStats(ctx, userText)
	}
	if sess.Stale() {
		sess.Reset()
	}
	res, err := a.askAndSpeak(ctx, sess.Turns, userText, true)
	if err != nil || res.Reply == "" {
		return res, err
	}
	appendSessionTurn(sess, userText, res.Reply)
	// Persist so a daemon restart resumes this thread. Best-effort -
	// a failed save must never break the turn.
	if err := sess.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis: session save: %v\n", err)
	}
	return res, nil
}

// AskTextSilent is the LMD (Android turn) entry point: it runs the SAME
// recall-augmented, tool-dispatching path as AskTextInSession - including
// short-term session memory when sess is non-nil - but returns the reply
// text WITHOUT calling tts.Play. This exists because `aida serve` may be
// running the desk-mic listener in the same process; the LMD HTTP handler
// (internal/jarvis/lmd) synthesizes the reply into its own temp audio file
// and ships the bytes back to the phone, so playing it through this
// process's speakers as well would leak the phone's turn into the room.
//
// Contrast with AskTextMute: that one skips recall AND history (a fast
// tool-dispatch-only fallback). AskTextSilent keeps the full pipeline and
// only removes the final synth+play step - reuses askAndSpeak's internals
// via its speak=false parameter rather than duplicating the body.
//
// sess is caller-owned and, unlike AskTextInSession, is never persisted to
// sessionStatePath(): the LMD handler keeps one in-memory Session per
// X-LMD-Session id, entirely separate from the listener's single
// disk-backed session, so a phone conversation can never clobber (or be
// clobbered by) the desk listener's saved thread. Pass nil for a stateless
// one-shot turn.
func (a *Assistant) AskTextSilent(ctx context.Context, sess *Session, userText string) (AskResult, error) {
	var history []llm.TurnPair
	if sess != nil {
		if sess.Stale() {
			sess.Reset()
		}
		history = sess.Turns
	}
	res, err := a.askAndSpeak(ctx, history, userText, false)
	if sess != nil && err == nil && res.Reply != "" {
		appendSessionTurn(sess, userText, res.Reply)
	}
	return res, err
}

// appendSessionTurn records the (user, assistant) pair on sess and trims the
// window to sess.MaxTurns (defaultSessionMaxTurns if unset) - the shared
// bookkeeping behind both AskTextInSession and AskTextSilent so the trim
// logic lives in exactly one place.
func appendSessionTurn(sess *Session, userText, reply string) {
	sess.Turns = append(sess.Turns, llm.TurnPair{User: userText, Assistant: reply})
	max := sess.MaxTurns
	if max <= 0 {
		max = defaultSessionMaxTurns
	}
	if len(sess.Turns) > max {
		sess.Turns = sess.Turns[len(sess.Turns)-max:]
	}
	sess.LastAt = time.Now()
}

// askAndSpeak is the shared body of AskText / AskTextWithStats /
// AskTextInSession / AskTextSilent. history may be nil for one-shot calls.
// speak gates the final TTS synth+play step: true for every voice-loop and
// typed-CLI caller, false for AskTextSilent (the LMD path), which wants the
// reply text only. Embeds the query once and injects two recall blocks into
// the LLM's system prompt: prior thread context ("## Earlier in this
// conversation") and past feedback on similar requests ("## Past
// feedback ..."). Recall failures (no Voyage key, empty brain, network) are
// non-fatal - the turn proceeds with whatever (possibly empty) block we got.
func (a *Assistant) askAndSpeak(ctx context.Context, history []llm.TurnPair, userText string, speak bool) (AskResult, error) {
	// Mark the turn BEFORE the LLM/tool-dispatch pipeline starts, so every
	// downstream consumer of this ctx - tool dispatch's OnToolUse hook and
	// aidaQueryTool's progressAck alike - sees the same decision for the
	// whole turn. speak=false is AskTextSilent's LMD/Android path; every
	// other caller passes speak=true and this is a no-op. See
	// localAudioSuppressed's doc comment for why suppression, not a
	// global config flag, is the right mechanism here.
	if !speak {
		ctx = withLocalAudioSuppressed(ctx)
	}
	recallProse := a.recall(ctx, userText)
	reply, stats, err := a.llm.AskWithHistory(ctx, history, recallProse, userText)
	// The LLM has returned its final text, so no tool is executing anymore.
	a.activity.ClearCurrentTool()
	res := AskResult{Reply: reply, Stats: stats}
	if err != nil {
		return res, err
	}
	// Grounding guard: if the reply claims a background job/run but no
	// job_* tool actually fired this turn, the model confabulated it
	// (the "Run id dome-50-layer-5" failure - a turn with ZERO tool calls).
	// Any job tool grounds the claim: status/list turns legitimately say
	// "background job" while reporting real state. Replace the spoken text
	// with a truthful fallback BEFORE synthesis, and record the rewrite so
	// the audit log and durable promotion never carry the lie.
	if claimsBackgroundWork(reply) && !usedJobTool(stats.Calls) {
		fmt.Fprintf(os.Stderr, "jarvis: grounding guard rewrote a phantom job claim: %q\n", reply)
		res.GroundingRewritten = true
		res.OriginalReply = reply
		reply = groundingFallback
		res.Reply = reply
	}
	// Silent-turn guard: a tool can succeed while the model still hands
	// back zero text. This happened for real: "Hey, aida. Thumbs up."
	// ran jarvis_thumbs_up cleanly (223ms, no error) and got no reply;
	// fifteen seconds later "I'm gonna call those my agents of SHIELD"
	// ran jarvis_note the same way. Both turns wrote their lesson row and
	// both said nothing, because a tool's return value is a tool_result
	// block the model independently decides whether to echo, not
	// something that lands in the spoken reply on its own. Runs after the
	// grounding guard on purpose: that guard only ever touches a
	// non-empty reply, so the two never fight over the same turn, and by
	// running second this one only ever fills a reply that is still
	// truly empty.
	if newReply, usedFallback := resolveReply(reply, stats.Calls); usedFallback {
		reply = newReply
		res.Reply = reply
		res.EmptyReplyFallback = true
	}
	if !speak || a.cfg.TextOnly || reply == "" {
		return res, nil
	}
	// Streaming engines (ElevenLabs /stream) play audio as it arrives, so the
	// first words start ~immediately instead of after the whole clip is
	// downloaded. TTSMs captures time-to-first-sound; PlayMs the remainder.
	if streamer, ok := a.tts.(tts.StreamingSynthesizer); ok {
		st, err := streamer.SpeakStream(ctx, reply)
		if err != nil {
			return res, fmt.Errorf("tts stream: %w", err)
		}
		res.TTSMs = st.TTFA.Milliseconds()
		res.PlayMs = (st.Total - st.TTFA).Milliseconds()
		if os.Getenv("JARVIS_STREAM_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "jarvis: stream TTFA=%dms total=%dms\n",
				st.TTFA.Milliseconds(), st.Total.Milliseconds())
		}
		return res, nil
	}

	ttsStart := time.Now()
	wav, err := a.tts.Synthesize(ctx, reply)
	res.TTSMs = time.Since(ttsStart).Milliseconds()
	if err != nil {
		return res, fmt.Errorf("tts: %w", err)
	}
	defer os.Remove(wav)
	playStart := time.Now()
	if err := tts.PlayAt(ctx, wav, a.cfg.Volume); err != nil {
		return res, fmt.Errorf("playback: %w", err)
	}
	res.PlayMs = time.Since(playStart).Milliseconds()
	return res, nil
}

// recall embeds the user's query ONCE and builds the combined recall block
// for the system prompt: prior thread context first ("## Earlier in this
// conversation"), then past feedback on similar requests ("## Past
// feedback ..."). Either or both may be empty. Returns "" on any failure
// (no brain, no Voyage key, network error, empty match set) - recall is
// purely additive, the turn must proceed regardless. Sharing one EmbedQuery
// across both lookups halves the per-turn Voyage spend/latency.
func (a *Assistant) recall(ctx context.Context, query string) string {
	if a == nil || a.brain == nil || a.brain.Embeddings == nil || !a.brain.Embeddings.Available() {
		return ""
	}
	if query == "" {
		return ""
	}
	// 2s cap: this embed runs serially BEFORE the LLM call, so its worst
	// case is dead air added to every voice turn. Voyage answers in a few
	// hundred ms normally; when it's slower than 2s, skipping recall (a
	// purely-additive feature) beats making the user wait for it.
	embCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	emb, err := a.brain.Embeddings.EmbedQuery(embCtx, query)
	if err != nil || len(emb) == 0 {
		return ""
	}

	var threadProse, lessonProse string
	if recalled, err := a.brain.FindSimilarThreadMemory(ctx, emb, 3); err == nil && len(recalled) > 0 {
		threadProse = brain.FormatThreadRecallProse(recalled)
	}
	if recalled, err := a.brain.FindSimilarJarvisLessons(ctx, emb, 3); err == nil && len(recalled) > 0 {
		lessonProse = brain.FormatJarvisRecallProse(recalled)
	}
	return strings.TrimSpace(strings.TrimSpace(threadProse) + "\n\n" + strings.TrimSpace(lessonProse))
}

// PromoteTurn writes a compact summary of a completed voice turn into the
// durable thread memory so later turns can recall it - even after an idle
// reset or a daemon restart. Best-effort and asynchronous: it runs in its
// own goroutine with a bounded context.Background() (the turn's own ctx is
// often near-cancelled after TTS) so embed latency never delays the next
// utterance. Non-substantive turns (errored, empty, wake-only, rating-only)
// are skipped. Safe on a nil receiver.
func (a *Assistant) PromoteTurn(rec audit.Record) {
	if a == nil || a.brain == nil || !substantiveForPromotion(rec) {
		return
	}
	summary := buildTurnSummary(rec)
	if summary == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := a.brain.WriteThreadMemory(ctx, summary); err != nil {
			fmt.Fprintf(os.Stderr, "jarvis: promote turn: %v\n", err)
		}
	}()
}

// substantiveForPromotion reports whether a turn is worth persisting as
// durable thread memory: it succeeded, produced a spoken reply, wasn't a
// bare wake, and wasn't a pure rating turn (thumbs/note only - those rate
// the prior turn, they aren't their own thread event).
func substantiveForPromotion(rec audit.Record) bool {
	if rec.Error != "" || strings.TrimSpace(rec.Reply) == "" || rec.WakeOnly {
		return false
	}
	if len(rec.ToolCalls) > 0 {
		allRating := true
		for _, c := range rec.ToolCalls {
			if !tools.IsRatingTool(c.Name) {
				allRating = false
				break
			}
		}
		if allRating {
			return false
		}
	}
	// A zero-tool turn whose reply is a question is Jarvis asking for
	// clarification ("which company do you mean, sir?") - there's no fact
	// in it worth carrying forward, and once stored it gets recalled on the
	// next similar query where it anchors the model into asking again
	// instead of answering. Skip it.
	if len(rec.ToolCalls) == 0 && strings.HasSuffix(strings.TrimSpace(rec.Reply), "?") {
		return false
	}
	return true
}

// buildTurnSummary renders a turn's audit Record into a compact,
// self-contained summary for durable storage. It deliberately keeps the
// reply nearly whole so the build parameters it carries (material,
// coordinate, next step) survive into a later limited-context recall - the
// "capture WHAT, not just WHERE" lesson. Tool names are included so the
// record states the truth ("used minecraft_ask"), countering confabulation.
func buildTurnSummary(rec audit.Record) string {
	when := rec.StartedAt
	if when == "" {
		when = rec.Timestamp
	}
	if len(when) >= 10 {
		when = when[:10] // date only, for brevity
	}
	q := truncate(strings.TrimSpace(rec.Query), 200)
	reply := truncate(strings.TrimSpace(rec.Reply), 240)

	var b strings.Builder
	if when != "" {
		fmt.Fprintf(&b, "[%s] ", when)
	}
	fmt.Fprintf(&b, "User asked: %q.", q)
	if names := nonRatingToolNames(rec.ToolCalls); names != "" {
		fmt.Fprintf(&b, " Jarvis used %s and replied: %q", names, reply)
	} else {
		fmt.Fprintf(&b, " Jarvis replied: %q", reply)
	}
	return b.String()
}

// nonRatingToolNames joins the names of the turn's non-rating tool calls.
func nonRatingToolNames(calls []audit.ToolCall) string {
	var names []string
	for _, c := range calls {
		if tools.IsRatingTool(c.Name) {
			continue
		}
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// groundingFallback replaces a reply that claimed background work which was
// never actually started. It's phrased as a question so even a rare false
// positive degrades gracefully - it prompts the user to start the job for real.
const groundingFallback = "I've noted that, sir, but I haven't actually started a background job for it. Shall I begin one?"

// backgroundClaimPhrases are high-precision markers that a reply is claiming
// a background job/run. Deliberately narrow - generic "working on it" is
// omitted because it legitimately accompanies synchronous tools
// (minecraft_ask, aida_query). These phrases essentially only appear when
// the model is announcing a background job or reading back a run/handle.
var backgroundClaimPhrases = []string{
	"run id",
	"in the background",
	"background job",
	"background agent",
	"i'll call this one",
	"ill call this one",
	"the agent will",
	"started the job",
	"started a job",
	"kicked off the job",
}

// claimsBackgroundWork reports whether reply announces a background job/run.
func claimsBackgroundWork(reply string) bool {
	l := strings.ToLower(reply)
	for _, p := range backgroundClaimPhrases {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// usedJobTool reports whether any job_* tool fired this turn. job_start
// grounds a "started the job" claim, and job_status / job_list /
// job_send_input / job_cancel ground status replies that legitimately
// mention background jobs - only a turn that touched NO job tooling can
// be confabulating one. Rating tools are jarvis_*, so no prefix clash.
func usedJobTool(calls []llm.ToolCallStat) bool {
	for _, c := range calls {
		if strings.HasPrefix(c.Name, "job_") {
			return true
		}
	}
	return false
}

// silentReplyFallbackMaxLen bounds how much of a successful tool's raw
// result resolveReply will speak verbatim when the model itself returned
// no text. Tool results are written for the MODEL to read, not the user
// to hear: some are short, single-line acknowledgements meant to be
// echoed back ("Noted, sir.", "Added task #183: review the index
// pipeline"), but others are paragraphs (aida_query), multi-line lists
// (tasks_list, job_list), dossiers (day_summary), or structured data that
// was never meant for a human ear at all (mcp_find_tool returns a raw
// JSON array of candidate tools). Speaking one of those verbatim would be
// worse than the silence this fix is closing, so speakableAsIs only
// accepts something short, single-line, and not JSON-shaped; anything
// else falls back to a generic acknowledgement instead.
const silentReplyFallbackMaxLen = 160

// silentReplyGenericFallback is what Jarvis says when a tool ran
// successfully, the model returned no text, and the tool's own result
// isn't safe to hand straight to TTS (see silentReplyFallbackMaxLen).
const silentReplyGenericFallback = "Done, sir."

// speakableAsIs reports whether s is short, single-line, plain-text prose
// that's safe to pass straight to TTS: not a multi-line list or dossier,
// and not JSON (a raw tool payload like mcp_find_tool's result starts
// with '[' or '{').
func speakableAsIs(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > silentReplyFallbackMaxLen {
		return false
	}
	if strings.ContainsAny(s, "\n\r") {
		return false
	}
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		return false
	}
	return true
}

// fallbackForSilentReply returns something to speak when the model's own
// reply came back empty despite at least one tool succeeding this turn.
// It uses the most recent successful call's result (the one closest to
// "what just happened"), speaking it verbatim when speakableAsIs allows
// it and falling back to a generic acknowledgement otherwise. Returns ""
// when no tool succeeded at all, so a genuinely tool-free empty reply
// (unrelated to this fix) is left untouched by the caller.
func fallbackForSilentReply(calls []llm.ToolCallStat) string {
	succeeded := false
	var lastResult string
	for _, c := range calls {
		if c.Error != "" {
			continue
		}
		succeeded = true
		lastResult = c.Result
	}
	if !succeeded {
		return ""
	}
	if speakableAsIs(lastResult) {
		return lastResult
	}
	return silentReplyGenericFallback
}

// resolveReply decides the final spoken text for a turn once the model
// and the grounding guard have both had their say. A non-empty reply is
// returned unchanged. An empty reply is filled in from
// fallbackForSilentReply when at least one tool succeeded; usedFallback
// reports whether that substitution happened, so the caller can record it
// for offline review (AskResult.EmptyReplyFallback).
func resolveReply(reply string, calls []llm.ToolCallStat) (finalReply string, usedFallback bool) {
	if reply != "" {
		return reply, false
	}
	fb := fallbackForSilentReply(calls)
	if fb == "" {
		return "", false
	}
	return fb, true
}

// AskTextMute is AskText without the TTS playback step - used by HTTP clients
// who only want the text response.
func (a *Assistant) AskTextMute(ctx context.Context, userText string) (string, error) {
	reply, _, err := a.llm.Ask(ctx, userText)
	return reply, err
}

// SpeakGreeting synthesizes and plays the configured greeting line. Useful as
// a startup sound bite ("Good morning, sir.").
func (a *Assistant) SpeakGreeting(ctx context.Context) error {
	if a.cfg.Greeting == "" || a.cfg.TextOnly {
		return nil
	}
	wav, err := a.tts.Synthesize(ctx, a.cfg.Greeting)
	if err != nil {
		return err
	}
	defer os.Remove(wav)
	return tts.PlayAt(ctx, wav, a.cfg.Volume)
}

// SaveToolErrorTask creates an aida task capturing a failed tool call so the
// user can review later. Returns the task slug (or empty on failure - non-
// fatal, the voice loop should not break because the task tracker is
// momentarily unhappy). Tagged with "jarvis-error" plus the tool name so
// `aida tasks --tag jarvis-error` lists them in one go.
//
// Deduplicates against already-open tasks first: a triage of the backlog
// found ten near-identical timeout tickets and seven near-identical SSH
// exit-status tickets for what were really two root causes, because every
// failure filed its own permanent ticket with no check against what was
// already open. If a non-terminal task already tracks this tool + error
// signature (see normalizeErrorSignature), the recurrence is appended to
// that task's body instead of filing a new one.
func (a *Assistant) SaveToolErrorTask(query, tool, errMsg string) string {
	if a == nil || a.brain == nil {
		return ""
	}

	sigTag := errorSignatureTag(errMsg)
	dedupTags := []string{"jarvis-error", "tool:" + tool, sigTag}
	if existing, err := a.brain.ListTasksByStatus(brain.NonTerminalStatuses(), dedupTags, 0, 1); err == nil && len(existing) > 0 {
		task := existing[0]
		if body, err := a.brain.TaskBody(task.Slug); err == nil {
			recurrence := fmt.Sprintf("\nRecurred at %s (query: %q)\n", time.Now().UTC().Format(time.RFC3339), truncate(query, 60))
			newBody := body + recurrence
			a.brain.UpdateTask(task.Slug, brain.TaskPatch{Body: &newBody})
		}
		return task.Slug
	}

	title := fmt.Sprintf("jarvis: %s failed - %q", tool, truncate(query, 60))
	body := fmt.Sprintf("Tool: %s\nQuery: %s\nError: %s\nLogged: %s\n",
		tool, query, errMsg, time.Now().UTC().Format(time.RFC3339))
	rec, err := a.brain.AddTask(title, dedupTags, body)
	if err != nil || rec == nil {
		return ""
	}
	return rec.Slug
}

// truncate cuts s to at most n bytes, backing the cut off to a rune
// boundary - replies carry °/ - /… and a mid-rune slice would feed invalid
// UTF-8 into prompts and JSON.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// Close releases per-Assistant resources: MCP server subprocesses and
// the jobs store handle. Safe to call on a nil Assistant or one that
// didn't bring up either. Should run from the same defer chain in
// serve.go that closes brain.
func (a *Assistant) Close() {
	if a == nil {
		return
	}
	if a.sharedDeps {
		// Twin borrows the primary's discovery + jobs store; the primary
		// owns their lifecycle. Drop our references without closing.
		a.discovery = nil
		a.jobsStore = nil
		return
	}
	if a.discovery != nil {
		a.discovery.Close()
		a.discovery = nil
	}
	if a.jobsStore != nil {
		_ = a.jobsStore.Close()
		a.jobsStore = nil
	}
}

// initMCPDiscovery brings up the MCP client pool. Returns nil if no
// servers are configured or every connect attempt failed - the rest of
// Jarvis treats a nil discovery as "MCP disabled" and skips registering
// the dispatcher tools. All errors are logged to stderr and swallowed
// so a bad MCP server config can never block daemon startup.
func initMCPDiscovery() *mcp.MCPDiscovery {
	configs, err := mcp.LoadMCPConfigs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "jarvis: MCP config load failed: %v (continuing without MCP)\n", err)
		return nil
	}
	if len(configs) == 0 {
		return nil
	}
	d := mcp.NewMCPDiscovery(false)
	// 15s mirrors the agent.go reference path. Plenty of time for every
	// configured server's first handshake even on a cold launch.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := d.ConnectAll(ctx, configs); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis: MCP ConnectAll: %v (continuing with partial discovery)\n", err)
	}
	if _, err := d.DiscoverTools(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "jarvis: MCP DiscoverTools: %v (continuing with empty tool list)\n", err)
	}
	tools := d.Tools()
	fmt.Fprintf(os.Stderr, "🧰 MCP discovery: %d tools across %d servers\n", len(tools), countServers(tools))
	printConnectBanner(d.Statuses())
	return d
}

// printConnectBanner renders one line per configured MCP server so the
// user can see at a glance which servers connected and which failed
// (typically: URL servers that need OAuth, or missing stdio binaries).
func printConnectBanner(statuses []mcp.ConnectStatus) {
	if len(statuses) == 0 {
		return
	}
	for _, s := range statuses {
		if s.Connected {
			fmt.Fprintf(os.Stderr, "  ✓ %s (%s): %d tools\n", s.Server, s.Transport, s.NumTools)
			continue
		}
		// Auth-gated remote servers (e.g. attio) will always 401 from here -
		// their tokens live in Claude Code's keychain. Render as an expected
		// skip, not an alarming error.
		if s.AuthSkipped {
			fmt.Fprintf(os.Stderr, "  ⊘ %s (%s): needs OAuth - use it in Claude Code; skipped here\n", s.Server, s.Transport)
			continue
		}
		reason := "unknown error"
		if s.Err != nil {
			reason = s.Err.Error()
		}
		fmt.Fprintf(os.Stderr, "  ✗ %s (%s): %s\n", s.Server, s.Transport, reason)
	}
}

// countServers returns the number of distinct MCP servers represented
// in the discovered tool slice. Used only for the startup banner.
func countServers(tools []mcp.DiscoveredTool) int {
	seen := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		seen[t.Server] = struct{}{}
	}
	return len(seen)
}

// loadSourceDomains returns the names of available aida sources for
// injection into the aida_query tool description. Lets the LLM router
// see what data domains are reachable (workouts, partners, finances, ...)
// instead of guessing from prose. Failures are non-fatal - we return an
// empty list and the tool falls back to its generic description.
func loadSourceDomains() []string {
	reg, err := library.LoadRegistry(config.Dir())
	if err != nil || reg == nil {
		return nil
	}
	return reg.AvailableSources()
}
