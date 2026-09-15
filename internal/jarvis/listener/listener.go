// Package listener runs the always-on voice loop. It pulls 16kHz mono PCM
// from the system microphone via ffmpeg, applies a simple amplitude VAD to
// segment utterances, transcribes each utterance with whisper-cli, and when
// the transcript begins with the configured wake phrase ("ok jarvis") feeds
// the rest into the standard Jarvis Ask pipeline.
//
// This is the v1 listener: a single goroutine, no overlap, no streaming
// transcription. It's good enough to demo "ok jarvis, what are my tasks"
// while we evaluate the next phase (openWakeWord ONNX wake detection +
// continuous Silero VAD).
package listener

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/audit"
	"github.com/ryanlitalien/aida/internal/jarvis/notify"
	"github.com/ryanlitalien/aida/internal/jarvis/stt"
	"github.com/ryanlitalien/aida/internal/jarvis/tools"
)

// Tunables for the simple amplitude VAD. False-positive risk is annoyance,
// false-negative risk is missed wake-words. We err toward "easy to trigger".
const (
	sampleRate   = 16000
	chunkMs      = 30
	chunkSamples = sampleRate * chunkMs / 1000 // 480
	chunkBytes   = chunkSamples * 2            // 960

	defaultRMSGate = 300.0 // int16 amplitude RMS threshold; > → speech
	silenceMs      = 1200  // trailing silence to declare end-of-utterance
	minSpeechMs    = 300   // ignore blips shorter than this
	maxSpeechMs    = 12000 // bail out on very long segments
	preRollMs      = 800   // include this much pre-speech audio in the buffer
	//                         800ms is enough to recover a soft "Hey Jarvis"
	//                         when the gate doesn't fire until the louder
	//                         body of the sentence.
)

// wakeRegex returns a regex that matches any accepted wake phrase preceding
// "jarvis", with arbitrary punctuation between words and an optional trailing
// comma/period. Whisper's outputs are unpredictable - "Okay, Jarvis,",
// "Good morning, Jarvis.", "OK JARVIS" all need to fire.
//
// Accepted prefixes:
//
//	ok / okay / hey
//	good morning / good afternoon / good evening
func wakeRegex(canonical string) *regexp.Regexp {
	_ = canonical // canonical config field is currently cosmetic
	return WakeRegexFor("jarvis")
}

// WakeRegexFor builds a wake regex: one of the accepted prefixes
// (ok / okay / hey / good morning|afternoon|evening) followed by namePattern
// - a raw regex alternation for the assistant's name, e.g. "jarvis" or
// "aida|ada|ayda|ida" - tolerant of arbitrary punctuation and whisper's
// spacing quirks. Used to give each persona its own wake phrase off the one
// shared mic stream.
func WakeRegexFor(namePattern string) *regexp.Regexp {
	return WakeRegexForPrefix("", namePattern)
}

// defaultWakePrefixes is the full set of accepted wake prefixes (Jarvis's).
const defaultWakePrefixes = `ok|okay|hey|good\s+(?:morning|afternoon|evening)`

// WakeRegexForPrefix is WakeRegexFor with a caller-chosen prefix alternation -
// e.g. just "hey" for a persona that only answers "hey <name>". An empty
// prefixPattern uses the full default set. Trailing \b so short name branches
// ("ada", "ida") don't swallow the head of a longer word - "hey Idaho" must
// NOT wake Aida.
func WakeRegexForPrefix(prefixPattern, namePattern string) *regexp.Regexp {
	if prefixPattern == "" {
		prefixPattern = defaultWakePrefixes
	}
	return regexp.MustCompile(
		`(?i)\b(?:` + prefixPattern + `)[\s,.\-:;!?]+(?:` + namePattern + `)\b[\s,.\-:;!?]*`,
	)
}

// timeOfDayRegex pulls morning/afternoon/evening out of a wake-phrase match.
// Used to short-circuit a bare "good morning, jarvis" into an automatic
// greeting + briefing instead of arming for a follow-up.
var timeOfDayRegex = regexp.MustCompile(`(?i)\bgood\s+(morning|afternoon|evening)\b`)

// timeOfDayAutoQuery returns the synthetic user-query string to feed askAndReply
// when a bare time-of-day wake fires (no follow-up text). Returns "" if the
// wake match isn't a "good morning/afternoon/evening" greeting. Morning adds
// the time + weather at the configured home location (the weather tool's
// default when no place is named); afternoon/evening get a plain greeting.
func timeOfDayAutoQuery(wakeMatch string) string {
	m := timeOfDayRegex.FindStringSubmatch(wakeMatch)
	if m == nil {
		return ""
	}
	switch strings.ToLower(m[1]) {
	case "morning":
		return "Good morning. Briefly greet me, then tell me the current local time and the current weather at home."
	case "afternoon":
		return "Good afternoon. Briefly greet me."
	case "evening":
		return "Good evening. Briefly greet me."
	}
	return ""
}

// Persona binds a wake-phrase regex to the assistant that answers it. The
// listener owns a single mic stream but can route to multiple personas: the
// wake phrase that appears earliest in a transcript wins, and its assistant
// micStream is the capture surface the read loop needs - satisfied by both
// *audio.PCMStream (fixed device) and *audio.FollowingStream (auto-following).
type micStream interface {
	Read(p []byte) (int, error)
	Flush() int
	Stop() error
}

// defaultFollowUpWindow is how long the mic stays armed for a wake-less
// answer after a reply that ends in a question (conversational mode).
const defaultFollowUpWindow = 15 * time.Second

// (with its own voice + name) handles the utterance.
type Persona struct {
	Name      string
	Wake      *regexp.Regexp
	Assistant *jarvis.Assistant
	Voice     string // human-readable voice label for the startup log, e.g. "elevenlabs:41bg…"
	WakeWord  string // representative wake phrase for the startup log, e.g. "hey aida"
}

type Options struct {
	// Personas is the wake-word routing table. When non-empty it takes
	// precedence over Assistant/WakeWord. When empty, a single Jarvis
	// persona is synthesized from Assistant + WakeWord (back-compat with
	// the push-to-talk `aida jarvis listen` path).
	Personas []Persona

	Assistant *jarvis.Assistant
	WakeWord  string        // default "ok jarvis"
	RMSGate   float64       // 0 → defaultRMSGate
	Verbose   bool          // print per-chunk RMS once per second
	Audit     *audit.Logger // optional; when set, real wake-triggered turns are appended
	Device    string        // avfoundation capture spec (e.g. ":2"); "" → ":0" (system default)
	OnEvent   func(Event)

	// MicPrefer, when non-empty, makes the listener FOLLOW the preferred
	// available input device: it re-evaluates the priority list every few
	// seconds and hot-swaps the capture on dock/undock (or after a sleep/wake
	// stall), so the mic never silently goes deaf. Empty → fixed Device.
	MicPrefer   []string
	OnMicSwitch func(name string) // fired when the active mic changes; for logging

	// Notifier is the proactive-voice-notification queue. When non-nil,
	// the listener calls Notifier.DrainOnNextWake at the start of every
	// wake-triggered turn (and bare-wake armed turn) so the user hears
	// pending updates from background agents before their question is
	// processed. Nil is tolerated - used by --no-jarvis daemons and
	// the push-to-talk CLI invocations.
	Notifier *notify.Notifier

	// PTTSignal, when non-nil, streams push-to-talk button transitions
	// (true = pressed, false = released). While pressed the listener captures
	// audio straight to PTTPersona with no wake word, and a fresh press barges
	// in on any in-flight reply. Nil disables push-to-talk. The serve daemon
	// feeds it from a hardware button, so the listener needs no HID/cgo import.
	PTTSignal  <-chan bool
	PTTPersona *Persona

	// FollowUpWindow enables conversational follow-up: after a reply that
	// ends in a question, the mic stays armed (no wake phrase needed) for
	// this long so the user can just answer. 0 uses defaultFollowUpWindow
	// (15s); a negative value disables follow-up entirely.
	FollowUpWindow time.Duration
}

type EventKind string

const (
	EvListening   EventKind = "listening"
	EvSpeechStart EventKind = "speech_start"
	EvSpeechEnd   EventKind = "speech_end"
	EvTranscript  EventKind = "transcript"
	EvWake        EventKind = "wake"
	EvSkip        EventKind = "skip"
	EvReply       EventKind = "reply"
	EvError       EventKind = "error"
)

type Event struct {
	Kind    EventKind
	Text    string
	Err     error
	Persona string // which persona handled this wake/reply ("Jarvis"/"Aida"); empty for non-routed events
}

// Run blocks until ctx is cancelled, processing the mic stream in place. It
// returns ctx.Err() on graceful shutdown.
func Run(ctx context.Context, opts Options) error {
	// Build the persona routing table. Explicit Personas win; otherwise fall
	// back to a single Jarvis persona from Assistant + WakeWord.
	personas := opts.Personas
	if len(personas) == 0 {
		if opts.Assistant == nil {
			return fmt.Errorf("listener: nil assistant")
		}
		personas = []Persona{{
			Name:      "Jarvis",
			Wake:      wakeRegex(opts.WakeWord),
			Assistant: opts.Assistant,
		}}
	}
	for i := range personas {
		if personas[i].Assistant == nil {
			return fmt.Errorf("listener: persona %q has nil assistant", personas[i].Name)
		}
		if personas[i].Wake == nil {
			return fmt.Errorf("listener: persona %q has nil wake regex", personas[i].Name)
		}
	}
	// Push-to-talk is active only when the daemon wired up both a button signal
	// and a persona to route it to. Everything PTT-related below is gated on
	// this, so the wake-word-only path stays exactly as it was.
	usePTT := opts.PTTSignal != nil && opts.PTTPersona != nil
	if usePTT && opts.PTTPersona.Assistant == nil {
		return fmt.Errorf("listener: PTTPersona %q has nil assistant", opts.PTTPersona.Name)
	}
	gate := opts.RMSGate
	if gate <= 0 {
		gate = defaultRMSGate
	}
	emit := opts.OnEvent
	if emit == nil {
		emit = func(Event) {}
	}

	// With a preference list, follow the best available mic (hot-swap on
	// dock/undock, recover from sleep/wake stalls). Otherwise capture a fixed
	// device - the CLI push-to-talk / daemon paths that pass an explicit Device.
	var stream micStream
	if len(opts.MicPrefer) > 0 {
		fs, err := audio.StartFollowingStream(ctx, opts.MicPrefer, opts.OnMicSwitch)
		if err != nil {
			return fmt.Errorf("listener: %w", err)
		}
		stream = fs
	} else {
		device := opts.Device
		if device == "" {
			device = ":0"
		}
		ps, err := audio.StartPCMStream(ctx, device)
		if err != nil {
			return fmt.Errorf("listener: %w", err)
		}
		stream = ps
	}
	defer stream.Stop()

	emit(Event{Kind: EvListening})

	// Pre-roll ring (so we don't lop off the user's first 300ms).
	preRollChunks := preRollMs / chunkMs
	preRoll := make([][]byte, 0, preRollChunks)

	chunk := make([]byte, chunkBytes)
	var (
		speechActive  bool
		buffer        []byte
		silenceAcc    int
		debugRMSAcc   float64
		debugRMSCount int
		debugRMSPeak  float64
	)
	// armed is non-nil when the previous utterance was a bare wake for that
	// persona; the next utterance (no wake phrase) becomes its follow-up query.
	var armed *Persona
	// armedUntil bounds a *conversational* follow-up arm (reply ended in a
	// question). Zero means no deadline - bare-wake arming stays unbounded.
	var armedUntil time.Time
	followUpWindow := opts.FollowUpWindow
	if followUpWindow == 0 {
		followUpWindow = defaultFollowUpWindow
	}
	// One conversational session per listener loop. Loaded from disk so a
	// daemon restart resumes the in-flight thread; idle past IdleTimeout and
	// the next turn auto-resets, so a "Hey Jarvis" long after the last
	// exchange doesn't drag stale context along.
	session := jarvis.LoadSession()

	w := &stt.Whisper{}

	// Per-turn cancel so a push-to-talk press can barge in on an in-flight
	// reply: cancelling the turn's child ctx kills TTS playback (afplay runs
	// under it). pttWant mirrors the button state. All of this is dormant
	// unless usePTT, so the wake-word-only daemon path is unaffected.
	var (
		turnMu     sync.Mutex
		turnCancel context.CancelFunc
		pttWant    atomic.Bool
	)
	setTurnCancel := func(c context.CancelFunc) { turnMu.Lock(); turnCancel = c; turnMu.Unlock() }
	clearTurnCancel := func() { turnMu.Lock(); turnCancel = nil; turnMu.Unlock() }
	cancelTurn := func() {
		turnMu.Lock()
		if turnCancel != nil {
			turnCancel()
			turnCancel = nil
		}
		turnMu.Unlock()
	}
	// runTurn runs a reply under a cancelable child ctx so a button press can
	// interrupt it. With PTT off it runs under ctx directly, unchanged.
	runTurn := func(fn func(context.Context)) {
		if !usePTT {
			fn(ctx)
			return
		}
		tctx, c := context.WithCancel(ctx)
		setTurnCancel(c)
		fn(tctx)
		clearTurnCancel()
		c()
	}
	if usePTT {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case down, ok := <-opts.PTTSignal:
					if !ok {
						return
					}
					pttWant.Store(down)
					if down {
						cancelTurn() // barge in on any in-flight reply
					}
				}
			}
		}()
	}
	var (
		pttCapturing bool
		pttBuf       []byte
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := stream.Read(chunk); err != nil {
			// Suppress error events when the context is already
			// cancelled - that's a graceful Ctrl-C shutdown, not a
			// genuine mic failure.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			emit(Event{Kind: EvError, Err: fmt.Errorf("read mic: %w", err)})
			return err
		}

		rms := audio.RMS(chunk)
		if opts.Verbose {
			debugRMSAcc += rms
			debugRMSCount++
			if rms > debugRMSPeak {
				debugRMSPeak = rms
			}
			if debugRMSCount >= (1000 / chunkMs) { // ~1s
				avg := debugRMSAcc / float64(debugRMSCount)
				fmt.Fprintf(os.Stderr, "  rms avg=%.0f peak=%.0f gate=%.0f\n", avg, debugRMSPeak, gate)
				debugRMSAcc, debugRMSCount, debugRMSPeak = 0, 0, 0
			}
		}

		if usePTT {
			want := pttWant.Load()
			if want && !pttCapturing {
				// Button pressed: abandon any in-progress VAD utterance, drop
				// pipe backlog (stale audio, and self-audio on a barge-in),
				// and start a clean capture.
				stream.Flush()
				pttBuf = pttBuf[:0]
				pttCapturing = true
				speechActive = false
				emit(Event{Kind: EvSpeechStart, Persona: opts.PTTPersona.Name})
				continue // discard this pre-press chunk
			}
			if pttCapturing {
				pttBuf = append(pttBuf, chunk...)
				if !want {
					// Released: transcribe and route straight to the PTT
					// persona, no wake word required.
					pttCapturing = false
					emit(Event{Kind: EvSpeechEnd, Persona: opts.PTTPersona.Name})
					if ms := len(pttBuf) / (sampleRate / 1000 * 2); ms >= minSpeechMs {
						clip := append([]byte(nil), pttBuf...)
						var followUp, spoke bool
						runTurn(func(tctx context.Context) {
							followUp, spoke = handlePTTUtterance(tctx, clip, w, opts.PTTPersona, session, emit, opts.Audit, opts.Notifier)
						})
						if spoke {
							if dropped := stream.Flush(); dropped > 0 {
								emit(Event{Kind: EvSkip, Text: fmt.Sprintf("flushed %d bytes of self-audio", dropped)})
							}
							preRoll = preRoll[:0]
						}
						// A PTT reply ending in a question keeps the mic open for a
						// wake-less answer, same as the VAD path. This only arms the
						// state; after release the loop falls through to wake-word VAD
						// (see the "want == false and not capturing" comment below),
						// which is what actually notices the follow-up speech and
						// routes it back to handleUtterance. Deliberate behavior
						// change: a PTT turn that does NOT end in a question now
						// clears any stale arm left over from an earlier VAD
						// exchange, rather than leaving it - the PTT turn
						// supersedes the older conversation.
						var turnArmed *Persona
						if followUp {
							turnArmed = opts.PTTPersona
						}
						armed, armedUntil = resolveArm(turnArmed, armedUntil, followUp, followUpWindow, time.Now())
					} else {
						emit(Event{Kind: EvSkip, Text: "too short"})
					}
				}
				continue
			}
			// want == false and not capturing: fall through to wake-word VAD.
		}

		if !speechActive {
			// Conversational follow-up window elapsed with no speech: disarm
			// and fall back to wake-gated listening. Guarded on rms<=gate so a
			// user who starts answering right at the boundary isn't cut off.
			if armed != nil && !armedUntil.IsZero() && rms <= gate && time.Now().After(armedUntil) {
				emit(Event{Kind: EvSkip, Text: "follow-up window elapsed"})
				armed = nil
				armedUntil = time.Time{}
			}

			// Maintain a rolling pre-roll so we don't miss the leading
			// edge of speech.
			cp := make([]byte, len(chunk))
			copy(cp, chunk)
			preRoll = append(preRoll, cp)
			if len(preRoll) > preRollChunks {
				preRoll = preRoll[len(preRoll)-preRollChunks:]
			}

			if rms > gate {
				speechActive = true
				silenceAcc = 0
				buffer = buffer[:0]
				for _, c := range preRoll {
					buffer = append(buffer, c...)
				}
				buffer = append(buffer, chunk...)
				emit(Event{Kind: EvSpeechStart})
			}
			continue
		}

		// Speech is active - keep appending.
		buffer = append(buffer, chunk...)
		if rms > gate {
			silenceAcc = 0
		} else {
			silenceAcc += chunkMs
		}

		bufferMs := len(buffer) / (sampleRate / 1000 * 2)
		if silenceAcc >= silenceMs || bufferMs >= maxSpeechMs {
			speechActive = false
			emit(Event{Kind: EvSpeechEnd})

			if bufferMs < minSpeechMs {
				emit(Event{Kind: EvSkip, Text: "too short"})
				continue
			}
			var spoke, followUp bool
			clip := append([]byte(nil), buffer...)
			runTurn(func(tctx context.Context) {
				armed, followUp, spoke = handleUtterance(tctx, clip, w, personas, session, armed, emit, opts.Audit, opts.Notifier)
			})
			if spoke {
				// Jarvis just played audio while this loop was blocked, so
				// the mic captured his own voice into the pipe backlog.
				// Drop it (and the stale pre-roll) before resuming VAD so
				// he doesn't transcribe - or, when armed, answer - himself.
				if dropped := stream.Flush(); dropped > 0 {
					emit(Event{Kind: EvSkip, Text: fmt.Sprintf("flushed %d bytes of self-audio", dropped)})
				}
				preRoll = preRoll[:0]
			}
			// Conversational follow-up: the reply ended in a question, so
			// keep the persona armed (handleUtterance already set it) and
			// start the bounded window. If follow-up is disabled, drop the
			// arm so a question doesn't hold the mic open.
			armed, armedUntil = resolveArm(armed, armedUntil, followUp, followUpWindow, time.Now())
		}
	}
}

// handleUtterance returns (armed, spoke). armed is the persona whose bare
// wake just fired (nil = not armed); when non-nil on entry, a wake-less
// utterance is treated as that persona's follow-up query (so "ok jarvis" →
// silence → "what's my schedule" works as one logical ask). spoke is true
// whenever this turn played any audio (a reply, a wake-only prompt, or a
// drained notification) - the caller uses it to flush the mic backlog so the
// assistant doesn't hear itself. session carries short-term conversational
// memory across wake-triggered turns; pass nil for stateless behavior.
//
// personas is the wake-word routing table: the phrase that appears earliest
// in the transcript wins, and its assistant answers. notifier, when non-nil,
// has its queue drained at the start of every wake-triggered turn (after a
// wake regex hits, before STT-derived query content reaches the LLM) - this
// is where the proactive "Sir, the PR 583 job is asking…" utterances speak.
func handleUtterance(
	ctx context.Context, pcm []byte, w *stt.Whisper, personas []Persona,
	session *jarvis.Session,
	armed *Persona, emit func(Event), au *audit.Logger,
	notifier *notify.Notifier,
) (newArmed *Persona, followUp bool, spoke bool) {
	wavPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("jarvis-utt-%d.wav", time.Now().UnixNano()))
	if err := audio.WriteWAV(wavPath, pcm, sampleRate); err != nil {
		emit(Event{Kind: EvError, Err: err})
		return nil, false, false
	}
	defer os.Remove(wavPath)

	transcript, err := w.Transcribe(ctx, wavPath)
	if err != nil {
		// A cancelled ctx here is a barge-in (button pressed mid-turn), not a
		// real STT failure, so stay quiet.
		if ctx.Err() == nil {
			emit(Event{Kind: EvError, Err: err})
		}
		return nil, false, false
	}
	emit(Event{Kind: EvTranscript, Text: transcript})

	lower := strings.ToLower(transcript)

	// Whisper hallucinates "[BLANK_AUDIO]", "[silence]", "(upbeat music)",
	// "(electronic music)", bare words like "you"/"thank you", and other
	// non-speech transcriptions on quiet or noisy clips. Drop them silently.
	if strings.TrimSpace(lower) == "" || stt.IsHallucination(lower) {
		return armed, false, false // preserve armed state; ignore noise
	}

	// Route to the persona whose wake phrase appears earliest in the
	// transcript (so "ok aida" and "ok jarvis" each reach their own voice).
	var (
		hit    *Persona
		hitLoc []int
	)
	for i := range personas {
		loc := personas[i].Wake.FindStringIndex(transcript)
		if loc == nil {
			continue
		}
		if hit == nil || loc[0] < hitLoc[0] {
			hit = &personas[i]
			hitLoc = loc
		}
	}

	// Armed mode: previous utterance was a bare wake for `armed`. Take this
	// whole transcript as that persona's query, even without a wake prefix.
	if hit == nil {
		if armed != nil {
			tail := strings.TrimSpace(transcript)
			emit(Event{Kind: EvWake, Text: tail, Persona: armed.Name})
			// User is engaged - drain any queued proactive notifications
			// before processing their actual question. The drain blocks
			// only while utterances are pending; queue empty → instant.
			drainNotifier(ctx, notifier)
			reply := askAndReply(ctx, armed, session, transcript, tail, emit, au)
			// If the follow-up reply is itself a question, keep the
			// conversation open; otherwise the exchange is complete.
			if expectsAnswer(reply) {
				return armed, true, true
			}
			return nil, false, true
		}
		emit(Event{Kind: EvSkip, Text: "no wake phrase"})
		return nil, false, false
	}

	tail := strings.TrimSpace(transcript[hitLoc[1]:])
	tail = strings.TrimLeft(tail, " ,.;:-")
	emit(Event{Kind: EvWake, Text: tail, Persona: hit.Name})

	if tail == "" {
		// Bare time-of-day greeting ("good morning, jarvis") auto-triggers
		// a greeting + (for morning) a time/weather briefing instead of
		// arming. The synthetic query is fed through the normal LLM path
		// so the reply is natural and tools fire as usual.
		if auto := timeOfDayAutoQuery(transcript[hitLoc[0]:hitLoc[1]]); auto != "" {
			emit(Event{Kind: EvWake, Text: auto, Persona: hit.Name})
			drainNotifier(ctx, notifier)
			askAndReply(ctx, hit, session, transcript, auto, emit, au)
			return nil, false, true
		}
		emit(Event{Kind: EvSkip, Text: "wake-only; awaiting follow-up"})
		// User has engaged with the wake word - speak any queued
		// notifications before the wake-only prompt so the spoken
		// order is (1) what they need to know, (2) "I'm listening".
		drainNotifier(ctx, notifier)
		// Speak a short prompt ("How can I help you, sir?" / "Yes, sir?"
		// / etc., rotated) so the gap before the follow-up isn't silent.
		// Bounded context so a stuck playback can't block arming.
		promptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		hit.Assistant.PlayWakeOnlyPrompt(promptCtx)
		cancel()
		if au != nil {
			au.Append(audit.Record{
				Transcript: transcript,
				Query:      "",
				WakeOnly:   true,
			})
		}
		return hit, false, true // arm THIS persona for the next utterance (unbounded)
	}

	// Wake + immediate query: drain notifications first so a heads-up
	// like "Sir, PR 583 is done" plays before the assistant answers the
	// freshly-asked question.
	drainNotifier(ctx, notifier)
	reply := askAndReply(ctx, hit, session, transcript, tail, emit, au)
	if expectsAnswer(reply) {
		return hit, true, true
	}
	return nil, false, true
}

// handlePTTUtterance transcribes a push-to-talk clip and routes it straight to
// the button's persona with no wake word (holding the button IS the intent).
// Returns whether the reply itself expects a follow-up answer (so the caller
// can arm the mic for a wake-less reply, matching the VAD path), and whether
// it played any audio, so the caller can flush self-audio.
func handlePTTUtterance(
	ctx context.Context, pcm []byte, w *stt.Whisper, p *Persona,
	session *jarvis.Session, emit func(Event), au *audit.Logger,
	notifier *notify.Notifier,
) (followUp bool, spoke bool) {
	wavPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("jarvis-ptt-%d.wav", time.Now().UnixNano()))
	if err := audio.WriteWAV(wavPath, pcm, sampleRate); err != nil {
		emit(Event{Kind: EvError, Err: err})
		return false, false
	}
	defer os.Remove(wavPath)

	transcript, err := w.Transcribe(ctx, wavPath)
	if err != nil {
		if ctx.Err() == nil { // cancelled = barge-in, not a failure
			emit(Event{Kind: EvError, Err: err})
		}
		return false, false
	}
	emit(Event{Kind: EvTranscript, Text: transcript})

	lower := strings.ToLower(strings.TrimSpace(transcript))
	if lower == "" || stt.IsHallucination(lower) {
		emit(Event{Kind: EvSkip, Text: "empty/noise"})
		return false, false
	}

	// No wake regex to strip: the transcript is the whole query. Drain proactive
	// notifications first, same as a wake turn, then answer.
	emit(Event{Kind: EvWake, Text: transcript, Persona: p.Name})
	drainNotifier(ctx, notifier)
	reply := askAndReply(ctx, p, session, transcript, transcript, emit, au)
	return expectsAnswer(reply), true
}

// drainNotifier is a thin wrapper that tolerates a nil notifier so
// every wake-path call site can omit the nil check. Bounded ctx
// timeout so a stuck synthesizer can't pin the listener - the queue
// resumes draining on the next wake.
func drainNotifier(ctx context.Context, n *notify.Notifier) {
	if n == nil || n.Pending() == 0 {
		return
	}
	drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n.DrainOnNextWake(drainCtx)
}

func askAndReply(
	ctx context.Context, p *Persona, session *jarvis.Session,
	transcript, query string, emit func(Event), au *audit.Logger,
) string {
	a := p.Assistant
	startedAt := time.Now()
	res, err := a.AskTextInSession(ctx, session, query)
	took := time.Since(startedAt)

	calls := make([]audit.ToolCall, len(res.Stats.Calls))
	for i, c := range res.Stats.Calls {
		calls[i] = audit.ToolCall{Name: c.Name, TookMs: c.TookMs, Error: c.Error, EngineRunID: c.EngineRunID}
	}
	for i, c := range calls {
		if c.Error == "" || toolCallRecovered(calls, i) {
			continue
		}
		// Capture failed tool calls as aida tasks so the user can review
		// them later (the spoken reply rarely conveys "this was a timeout
		// against the workouts source"). Skipped when a later call in the
		// same turn retried the same tool and succeeded - the turn already
		// healed itself, so there is nothing to review.
		a.SaveToolErrorTask(query, c.Name, c.Error)
	}

	rec := audit.Record{
		StartedAt:          startedAt.UTC().Format(time.RFC3339),
		Transcript:         transcript,
		Query:              query,
		ToolCalls:          calls,
		LLMMs:              res.Stats.LLMMs,
		ToolMs:             res.Stats.ToolMs,
		TTSMs:              res.TTSMs,
		PlayMs:             res.PlayMs,
		TookMs:             took.Milliseconds(),
		GroundingRewritten: res.GroundingRewritten,
		OriginalReply:      res.OriginalReply,
		EmptyReplyFallback: res.EmptyReplyFallback,
	}

	if err != nil {
		rec.Error = err.Error()
		emit(Event{Kind: EvError, Err: err, Persona: p.Name})
	} else {
		rec.Reply = res.Reply
		emit(Event{Kind: EvReply, Text: res.Reply, Persona: p.Name})
	}

	if au != nil {
		au.Append(rec)
	}

	// Cache this turn for the voice feedback tools, but only if it
	// included at least one non-rating tool call. Rating turns are
	// transparent - they don't supersede the turn they're rating, so
	// "thumbs down" always lands on the prior real turn.
	if err == nil && hasNonRatingToolCall(rec.ToolCalls) {
		a.SetLastTurn(&rec)
	}

	// Record the turn in the live-activity ring for the status page (all
	// turns, including errored ones, so failures are visible).
	a.RecordActivity(rec)

	// Promote the turn into durable thread memory so a later turn - even
	// after an idle reset or daemon restart - can recall this thread.
	// PromoteTurn self-filters non-substantive turns and runs async.
	a.PromoteTurn(rec)

	return rec.Reply
}

// resolveArm computes the follow-up arming state after a turn. armed is the
// persona the turn left armed (nil = none), armedUntil its current deadline.
// Returns the new (armed, armedUntil).
//
// A reply that expects an answer arms a bounded window so the user can just
// answer with no wake phrase. A non-negative-but-zero window means "use the
// caller's default", already resolved by the caller; window <= 0 here means
// follow-up is disabled, so a question disarms rather than holding the mic.
// A bare-wake arm (armed set, followUp false) is deliberately unbounded and
// keeps whatever deadline it had.
func resolveArm(armed *Persona, armedUntil time.Time, followUp bool, window time.Duration, now time.Time) (*Persona, time.Time) {
	if followUp {
		if window > 0 {
			return armed, now.Add(window)
		}
		return nil, time.Time{}
	}
	if armed == nil {
		return nil, time.Time{}
	}
	return armed, armedUntil
}

// expectsAnswer reports whether a spoken reply poses a question to the user -
// the signal to keep the mic armed for a conversational follow-up. It matches
// a question mark ANYWHERE in the reply, not just at the end: Jarvis routinely
// appends a short clause after the question ("...for what, sir? I need the
// quantity."), so an ends-with test misses real prompts. A false positive only
// costs a bounded, silent follow-up window that times out unheard; a false
// negative silently drops the user's answer, which is the worse failure.
func expectsAnswer(reply string) bool {
	return strings.Contains(reply, "?")
}

// toolCallRecovered reports whether some later call in the same turn used
// the same tool and succeeded, meaning the model retried after the failure
// at index i and the turn ultimately healed itself before replying. Filing
// a permanent jarvis-error ticket for a failure the user never actually
// experienced is exactly the kind of noise that inflated the backlog - a
// triage found tasks filed for turns that auto-recovered a few tool calls
// later with no user-visible impact.
func toolCallRecovered(calls []audit.ToolCall, i int) bool {
	for j := i + 1; j < len(calls); j++ {
		if calls[j].Name == calls[i].Name && calls[j].Error == "" {
			return true
		}
	}
	return false
}

// hasNonRatingToolCall returns true when at least one tool call in the
// turn isn't a voice feedback tool. Empty turn-tool-list (pure conversational
// reply with no tools) also counts as a "real" turn - the user might want to
// rate Jarvis's chitchat too.
func hasNonRatingToolCall(calls []audit.ToolCall) bool {
	if len(calls) == 0 {
		return true
	}
	for _, c := range calls {
		if !tools.IsRatingTool(c.Name) {
			return true
		}
	}
	return false
}

// isHallucination and chunkRMS used to live here, duplicated (with a slightly
// weaker word list) in internal/jarvis/lmd. They're now stt.IsHallucination
// and audio.RMS respectively - shared by both callers, see those packages'
// doc comments for why each was the natural, cycle-free home.
