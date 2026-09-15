// Package lmd is the server side of the L.M.D. (Life Model Decoy) wire
// protocol between `aida serve` and the aida-android client: a single
// bearer-token-authenticated HTTP surface exposing one full voice turn per
// request (audio in, audio + text out). See docs/lmd-protocol.md for the
// wire contract this package implements.
//
// It is mounted as a SECOND http.Server, separate from the loopback-only
// Earth-1610 daemon - bound to the host's Tailscale address on port 1218
// (Earth-1218) so the phone can reach it over the tailnet without exposing
// the unauthenticated tasks UI or /mcp/call tool dispatch.
//
// Handlers depend only on small interfaces (Transcriber, Asker, and
// tts.Synthesizer) rather than concrete *stt.Whisper / *jarvis.Assistant
// types, so the turn pipeline is fully unit-testable without a real whisper
// binary, Anthropic key, or ElevenLabs account.
package lmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/stt"
	"github.com/ryanlitalien/aida/internal/jarvis/tts"
)

// Port is the LMD listener's default port. 1218 is Earth-1218 in Marvel
// multiverse parlance - the "real world" the phone lives in, as distinct
// from the loopback-only Earth-1610 daemon (JarvisHTTPPort in cli/serve.go).
const Port = 1218

// maxBodyBytes caps a turn's WAV upload at 10MB - roughly 5 minutes of 16kHz
// mono 16-bit PCM, comfortably above the client's 30s recording cap, small
// enough that a runaway or malicious upload can't exhaust daemon memory.
const maxBodyBytes = 10 << 20

// turnTimeout bounds one full LMD turn: STT + recall + LLM tool-dispatch + TTS.
// It must clear a tool-using turn: the LLM loop can call aida_query, which runs on
// its own context.Background() deadline (aidaQueryTimeout, 240s) and in practice
// takes 60-100s+ for a web/tool-backed query. At 60s the turn context expired
// mid-tool, so the next Messages.New in the loop returned "context deadline
// exceeded" and a slow-but-valid query 500'd (see issue #130). 120s covers the
// observed range; it deliberately does NOT match aidaQueryTimeout's 240s ceiling,
// since a 4-minute voice turn is a worse outcome than a rare 500 - the real fix is
// cutting aida_query latency plus mid-turn progress, tracked in #130.
const turnTimeout = 120 * time.Second

// silenceRMSThreshold is the RMS amplitude (same int16 PCM scale as
// audio.RMS) below which an uploaded clip is rejected as silence - 422,
// before whisper is ever spawned. This is the fix for the bug where 3s of
// pure digital silence got transcribed by whisper as the bare word "you",
// slipped past the old bracket-only hallucination filter, and burned a full
// LLM + TTS round trip.
//
// Chosen conservatively and DELIBERATELY LOW. Digital silence (e.g.
// ffmpeg's anullsrc) measures RMS 0; typical phone-mic self-noise / room
// tone sits in the low tens on this scale. Real speech - even a soft, quiet
// question - reliably lands in the hundreds or higher (for comparison, the
// desk-mic listener's own amplitude-VAD gate, defaultRMSGate in
// internal/jarvis/listener, is 300, and that's already tuned to be the
// LOWEST level it considers "someone is definitely talking"). Sitting this
// gate roughly an order of magnitude below that means it only rejects audio
// that is unambiguous silence. If a genuinely quiet utterance somehow slips
// under this bar, stt.IsHallucination is the second line of defense; the
// reverse mistake - rejecting a real quiet question outright - must not
// happen, so this errs hard toward letting borderline audio through to
// whisper rather than gating it here.
const silenceRMSThreshold = 50.0

// Transcriber turns a 16kHz/mono/16-bit WAV file on disk into text.
// Satisfied by *stt.Whisper without modification.
type Transcriber interface {
	Transcribe(ctx context.Context, wavPath string) (string, error)
}

// Asker runs one full recall-augmented, tool-dispatching turn and returns
// the reply text without speaking it. sess is nil for a stateless turn;
// non-nil groups turns sharing an X-LMD-Session id into the same
// short-term conversational window. Production callers wire this to
// (*jarvis.Assistant).AskTextSilent via AskFunc; tests supply a fake.
type Asker interface {
	Ask(ctx context.Context, sess *jarvis.Session, userText string) (reply string, err error)
}

// AskFunc adapts a plain function to Asker - lets cli.runHTTPDaemon wire in
// assistant.AskTextSilent (which also returns per-stage timing stats this
// package doesn't need) without lmd importing jarvis.AskResult's shape.
type AskFunc func(ctx context.Context, sess *jarvis.Session, userText string) (string, error)

// Ask implements Asker.
func (f AskFunc) Ask(ctx context.Context, sess *jarvis.Session, userText string) (string, error) {
	return f(ctx, sess, userText)
}

// PersonaDeps bundles the two pipeline stages that must switch together for
// one persona: Asker (the reply TEXT + tone) and Synth (the VOICE it's
// spoken in). They have to move as a pair - the persona difference between
// Aida and Jarvis lives entirely in which *jarvis.Assistant answers
// (assistant.AskTextSilent) and which TTS engine speaks the reply
// (assistant.TTS()). Routing only the Asker would still play the wrong
// voice, which is the exact bug this package exists to fix (see
// docs/lmd-protocol.md's Personas section).
type PersonaDeps struct {
	Asker Asker
	Synth tts.Synthesizer // Synthesize(ctx, text) → temp audio file path; Name() → "piper" | "elevenlabs"

	// AckPath is this persona's pre-synthesized "One moment, sir." clip -
	// wired from (*jarvis.Assistant).AckAudioPath. Empty when pre-synthesis
	// failed or was skipped at startup (best-effort, tolerated); handleAck
	// reports 404 rather than treating that as an error. A .mp3 path for
	// ElevenLabs, .wav for Piper - see handleAck's content-type detection.
	AckPath string
}

// Deps bundles the turn pipeline's dependencies plus the bearer token, so New
// (and tests) can wire in fakes without a real Assistant, whisper binary, or
// ElevenLabs key.
type Deps struct {
	Transcriber Transcriber

	// Personas maps a lowercase persona name ("aida", "jarvis") to that
	// persona's Asker + Synth pair - see docs/lmd-protocol.md's Personas
	// section. DefaultPersona names the entry used when a turn's
	// X-LMD-Persona header is absent, empty, unrecognized, or (case-folded)
	// doesn't match anything in this map. Falling back must never error -
	// an older client that never sends the header, or one naming a persona
	// the daemon didn't wire up (e.g. the Aida twin failed to construct),
	// has to keep working exactly as before.
	Personas       map[string]PersonaDeps
	DefaultPersona string

	Token   string // bearer token; see LoadOrGenerateToken
	Version string // reported verbatim in the health response
}

// Server is the LMD HTTP handler set.
type Server struct {
	deps Deps

	// sessions holds one in-memory jarvis.Session per X-LMD-Session id,
	// SHARED across personas (keyed by session id alone, not
	// session+persona) - deliberately mirroring the desk-mic listener's
	// existing behavior of one conversational session per loop regardless
	// of which wake word ("hey jarvis" vs "hey aida") answered a given turn
	// (see listener.Run's "One conversational session per listener loop").
	// Aida and Jarvis are cosmetic twins over the same brain/tools; a mid-
	// conversation persona switch (X-LMD-Persona changes between turns on
	// the same X-LMD-Session) still sees the prior turns, same as switching
	// wake words does at the desk mic. Kept for the lifetime of this
	// process only - deliberately NOT the listener's disk-persisted session
	// (jarvis.LoadSession/Save lives at a single fixed path with no id), so
	// a phone conversation can never clobber, or be clobbered by, the
	// desk-mic listener's saved thread.
	sessions   map[string]*jarvis.Session
	sessionsMu sync.Mutex
}

// New builds the LMD server and mounts its routes onto mux. Mirrors the
// shape of jarvis/server.New(mux, a): construct with real dependencies in
// production, fakes in tests.
func New(mux *http.ServeMux, deps Deps) *Server {
	s := &Server{deps: deps, sessions: make(map[string]*jarvis.Session)}
	mux.HandleFunc("/lmd/v1/health", s.handleHealth)
	mux.HandleFunc("/lmd/v1/turn", s.handleTurn)
	mux.HandleFunc("/lmd/v1/ack", s.handleAck)
	return s
}

type healthResponse struct {
	OK             bool     `json:"ok"`
	Service        string   `json:"service"`
	Version        string   `json:"version"`
	Engine         string   `json:"engine"`
	Personas       []string `json:"personas"`
	DefaultPersona string   `json:"default_persona"`
}

// handleHealth is deliberately UNAUTHENTICATED (see docs/lmd-protocol.md):
// the client must be able to tell "can't reach the host" apart from
// "reachable but my token is wrong", and this route leaks nothing beyond
// "aida is running". `personas` reports whichever personas are ACTUALLY
// wired up in s.deps.Personas - not a hardcoded list - so the client's
// voice selector reflects reality (e.g. Jarvis-only when the Aida twin
// failed to construct) rather than drifting from it.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.deps.Personas))
	for name := range s.deps.Personas {
		names = append(names, name)
	}
	sort.Strings(names)

	// Every persona is expected to share one TTS engine (the choice of
	// ElevenLabs vs Piper is process-wide, driven by whether an ElevenLabs
	// key is configured - not a per-persona setting), so any wired
	// persona's Synth reports the same engine name. Prefer the default
	// persona's explicitly, then fall back to the sorted list, so the
	// result is deterministic rather than depending on map iteration order.
	engine := ""
	if pd, ok := s.deps.Personas[s.deps.DefaultPersona]; ok && pd.Synth != nil {
		engine = pd.Synth.Name()
	} else {
		for _, name := range names {
			if pd := s.deps.Personas[name]; pd.Synth != nil {
				engine = pd.Synth.Name()
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(healthResponse{
		OK:             true,
		Service:        "aida-lmd",
		Version:        s.deps.Version,
		Engine:         engine,
		Personas:       names,
		DefaultPersona: s.deps.DefaultPersona,
	})
}

type turnResponse struct {
	Transcript  string `json:"transcript"`
	Reply       string `json:"reply"`
	Audio       string `json:"audio"`        // base64
	AudioFormat string `json:"audio_format"` // "mp3" (ElevenLabs) | "wav" (Piper)
	TookMs      int64  `json:"took_ms"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// handleTurn implements the server-side turn pipeline documented in
// docs/lmd-protocol.md:
//
//	WAV bytes → temp .wav → <RMS silence gate>
//	          → Transcriber.Transcribe
//	          → <hallucination / empty guard>
//	          → Asker.Ask (recall + tools, playback suppressed) → reply text
//	          → Synth.Synthesize(reply) → temp audio file
//	          → base64 → JSON
//
// Both temp files are removed via defer on every path, including errors.
func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !s.authenticate(r) {
		writeError(w, http.StatusUnauthorized, "bad or missing token")
		return
	}

	// Resolve which persona answers this turn BEFORE doing any of the
	// expensive work below - see resolvePersona's doc comment for the
	// fallback contract (X-LMD-Persona absent/empty/unrecognized → default,
	// never an error).
	pd := s.resolvePersona(r)
	if pd.Asker == nil || pd.Synth == nil {
		writeError(w, http.StatusInternalServerError, "no persona configured on this LMD server")
		return
	}

	start := time.Now()

	// MaxBytesReader trips on the FIRST read past the cap, so an oversized
	// body never gets fully written to disk - it fails mid-copy inside
	// writeTempWAV below.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	wavPath, err := writeTempWAV(r.Body)
	if err != nil {
		if isMaxBytesError(err) {
			writeError(w, http.StatusRequestEntityTooLarge, "body exceeds 10MB cap")
			return
		}
		writeError(w, http.StatusInternalServerError, "read request body: "+err.Error())
		return
	}
	defer os.Remove(wavPath)

	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()

	// RMS energy gate: reject unambiguous silence BEFORE whisper is even
	// spawned. A held button on the phone has no VAD upstream of it (unlike
	// the desk-mic listener), so this is the first line of defense against
	// a stray/stuck press costing an LLM + TTS call. See
	// silenceRMSThreshold's doc comment for the threshold and why it errs
	// toward letting borderline audio through rather than rejecting it.
	wavBytes, err := os.ReadFile(wavPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read wav for silence check: "+err.Error())
		return
	}
	if isSilentWAV(wavBytes) {
		writeError(w, http.StatusUnprocessableEntity, "no usable speech detected")
		return
	}

	transcript, err := s.deps.Transcriber.Transcribe(ctx, wavPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "transcribe: "+err.Error())
		return
	}

	// Empty-transcript rule: a held button that captured only room tone
	// (or whose energy slipped past the RMS gate above) must not cost an
	// LLM call. Mirrors the whisper-hallucination filter the desk-mic
	// listener applies before routing to a persona.
	if strings.TrimSpace(transcript) == "" || stt.IsHallucination(transcript) {
		writeError(w, http.StatusUnprocessableEntity, "no usable speech detected")
		return
	}

	// sessionFor is keyed by session id ALONE, shared across personas - see
	// the sessions field's doc comment for why a mid-conversation persona
	// switch still sees the prior turns.
	sess := s.sessionFor(r.Header.Get("X-LMD-Session"))
	reply, err := pd.Asker.Ask(ctx, sess, transcript)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ask: "+err.Error())
		return
	}

	audioPath, err := pd.Synth.Synthesize(ctx, reply)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "synthesize: "+err.Error())
		return
	}
	defer os.Remove(audioPath)

	audioBytes, err := os.ReadFile(audioPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read synthesized audio: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(turnResponse{
		Transcript:  transcript,
		Reply:       reply,
		Audio:       base64.StdEncoding.EncodeToString(audioBytes),
		AudioFormat: strings.TrimPrefix(filepath.Ext(audioPath), "."),
		TookMs:      time.Since(start).Milliseconds(),
	})
}

// handleAck implements `GET /lmd/v1/ack?persona=<persona>` (see
// docs/lmd-protocol.md). It returns the resolved persona's pre-synthesized
// "One moment, sir." clip as raw audio so the Android client can fetch it
// once, cache it to disk, and play it locally after 5s of silence on a
// turn - masking the latency of an in-flight aida_query without the
// server needing to push anything mid-turn (the protocol is one-shot).
//
// Authenticated with the same bearer token as /turn (authenticate is
// shared, not duplicated). Persona resolution reuses resolvePersonaQuery,
// which is built on the exact same normalize-and-lookup pair as /turn's
// resolvePersona, so the two cannot drift on what "absent/empty/unknown
// persona" means.
func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !s.authenticate(r) {
		writeError(w, http.StatusUnauthorized, "bad or missing token")
		return
	}

	pd := s.resolvePersonaQuery(r)
	if pd.AckPath == "" {
		// Pre-synthesis at startup is best-effort (no ElevenLabs key, a
		// network blip) and legitimately fails - a missing clip is a
		// normal condition here, not something worth logging loudly.
		writeError(w, http.StatusNotFound, "no ack clip configured for this persona")
		return
	}

	audioBytes, err := os.ReadFile(pd.AckPath)
	if err != nil {
		// Unlike an empty AckPath (never synthesized), this means the path
		// WAS set at startup but the file is gone now - genuinely
		// unexpected, so it's a 500 rather than a 404.
		writeError(w, http.StatusInternalServerError, "read ack audio: "+err.Error())
		return
	}

	// Determine format from the extension rather than sniffing bytes - the
	// codebase produces these paths itself (ElevenLabs → .mp3, Piper →
	// .wav), so the extension is trustworthy. The app reads this header to
	// pick its cached file's extension; it must never be hardcoded.
	contentType := "audio/mpeg"
	if strings.ToLower(filepath.Ext(pd.AckPath)) == ".wav" {
		contentType = "audio/wav"
	}
	w.Header().Set("Content-Type", contentType)
	// The clip is synthesized once at startup and never regenerated for the
	// life of this process, so the client can cache it indefinitely once
	// fetched - exactly what it does (docs/lmd-protocol.md: "fetches once
	// per persona and caches to disk").
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Write(audioBytes)
}

// sessionFor returns the in-memory Session for id, creating one on first
// use. Empty id (no X-LMD-Session header) means a stateless one-shot turn -
// returns nil, which Assistant.AskTextSilent treats the same as
// AskTextWithStats.
func (s *Server) sessionFor(id string) *jarvis.Session {
	if id == "" {
		return nil
	}
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		sess = jarvis.NewSession()
		s.sessions[id] = sess
	}
	return sess
}

// normalizePersonaName case-folds and trims a raw persona name so the two
// entry points that resolve a persona - /turn's X-LMD-Persona header and
// /ack's ?persona= query parameter - treat "aida", "AIDA", and " Aida "
// identically. Both resolvePersona and resolvePersonaQuery route their raw
// value through this before handing it to personaDepsFor, so the two
// surfaces are built on the exact same normalization rather than each
// growing their own copy that could quietly drift apart.
func normalizePersonaName(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// personaDepsFor looks up an already-normalized persona name (see
// normalizePersonaName) against s.deps.Personas, falling back to
// s.deps.DefaultPersona on no match. An absent, empty, or unrecognized name
// - including one that names a persona this server just doesn't have wired
// up (e.g. the Aida twin failed to construct at startup) - falls back
// rather than erroring, so an older client that never sends persona info at
// all keeps working exactly as before. This is the ONLY place either
// resolvePersona (header, /turn) or resolvePersonaQuery (query param, /ack)
// perform the lookup, so their fallback behavior cannot drift apart.
func (s *Server) personaDepsFor(name string) PersonaDeps {
	if pd, ok := s.deps.Personas[name]; ok {
		return pd
	}
	return s.deps.Personas[s.deps.DefaultPersona]
}

// resolvePersona maps a turn's X-LMD-Persona header to the PersonaDeps that
// should answer it (docs/lmd-protocol.md: `X-LMD-Persona: aida|jarvis`). See
// personaDepsFor for the shared fallback contract.
func (s *Server) resolvePersona(r *http.Request) PersonaDeps {
	return s.personaDepsFor(normalizePersonaName(r.Header.Get("X-LMD-Persona")))
}

// resolvePersonaQuery maps /lmd/v1/ack's `?persona=` query parameter to the
// PersonaDeps whose AckPath should answer it (docs/lmd-protocol.md:
// `GET /lmd/v1/ack?persona=aida`). Sourced from a query parameter rather
// than a header - the only difference from resolvePersona - but built on
// the same normalizePersonaName + personaDepsFor pair, so the fallback
// contract is identical by construction, not by convention.
func (s *Server) resolvePersonaQuery(r *http.Request) PersonaDeps {
	return s.personaDepsFor(normalizePersonaName(r.URL.Query().Get("persona")))
}

// authenticate checks the "Authorization: Bearer <token>" header against
// the server's configured token via constant-time comparison. Both sides
// are run through normalizeToken first - see its doc comment - so a
// hand-typed token that hit one of Crockford base32's look-alike traps
// (I/L for 1, O for 0) still authenticates. Missing header, wrong scheme,
// and a genuinely mismatched token are all indistinguishable failures on
// the wire (401) - see writeError callers.
func (s *Server) authenticate(r *http.Request) bool {
	if s.deps.Token == "" {
		return false // misconfigured server; never treat "no token set" as "anything goes"
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	presented := strings.TrimPrefix(h, prefix)
	return constantTimeEqual(normalizeToken(presented), normalizeToken(s.deps.Token))
}

// writeError renders the protocol's uniform {"error": "..."} body at the
// given status.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

// writeTempWAV copies body into a new temp .wav file and returns its path.
// On any error the partially-written file (if created) is removed before
// returning, so callers never need to clean up a failed write themselves.
func writeTempWAV(body io.Reader) (string, error) {
	path := filepath.Join(os.TempDir(), fmt.Sprintf("lmd-turn-%d.wav", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// isMaxBytesError reports whether err came from an http.MaxBytesReader
// tripping its cap.
func isMaxBytesError(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// isSilentWAV reports whether wavBytes' PCM payload sits at or below
// silenceRMSThreshold. A malformed/unparseable WAV is treated as NOT
// silent - better to hand a bad file to whisper (which will error or
// mistranscribe harmlessly) than to reject audio this couldn't even
// measure.
func isSilentWAV(wavBytes []byte) bool {
	pcm, err := audio.PCMFromWAV(wavBytes)
	if err != nil {
		return false
	}
	return audio.RMS(pcm) < silenceRMSThreshold
}
