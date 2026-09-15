package lmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jarvis/audio"
	"github.com/ryanlitalien/aida/internal/jarvis/tts"
)

// fakeTranscriber, fakeAsker, and fakeSynth let handleTurn be exercised
// end-to-end without a real whisper binary, Anthropic key, or ElevenLabs
// account - exactly the point of Transcriber/Asker/tts.Synthesizer being
// interfaces.

type fakeTranscriber struct {
	text  string
	err   error
	calls int
}

func (f *fakeTranscriber) Transcribe(_ context.Context, _ string) (string, error) {
	f.calls++
	return f.text, f.err
}

type fakeAsker struct {
	reply    string
	err      error
	calls    int
	sessions []*jarvis.Session // records the sess argument of every call
}

func (f *fakeAsker) Ask(_ context.Context, sess *jarvis.Session, _ string) (string, error) {
	f.calls++
	f.sessions = append(f.sessions, sess)
	return f.reply, f.err
}

// fakeSynth writes body to a temp file (dir/"reply"+ext) on every
// Synthesize call, mimicking Piper/ElevenLabs' "returns a path" contract
// closely enough to test handleTurn's read-file-then-base64 step and its
// cleanup defer.
type fakeSynth struct {
	dir   string
	ext   string
	body  []byte
	err   error
	calls int
	name  string
}

func (f *fakeSynth) Synthesize(_ context.Context, _ string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	path := filepath.Join(f.dir, "reply"+f.ext)
	if err := os.WriteFile(path, f.body, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (f *fakeSynth) Name() string { return f.name }

const testToken = "LMD-TESTT-OKEN00-00000-00000-00000"

// fakeWAV stands in for a real 16kHz/mono/16-bit WAV body - handleTurn
// never inspects its contents for anything other than the RMS silence gate,
// so this deliberately-unparseable-as-WAV byte slice exercises the
// write-to-temp-file path faithfully: isSilentWAV's PCMFromWAV call fails on
// it, and per its "can't measure this, don't block it" contract that's
// treated as NOT silent, so every test below that predates the RMS gate
// keeps working unmodified.
var fakeWAV = []byte("RIFF....WAVEfmt fake-pcm-bytes-not-real-audio")

// synthesizeWAV builds a REAL, parseable WAV file (via audio.WriteWAV, the
// same helper production code uses) out of pcm and returns its bytes. Used
// by the RMS-gate tests below, where the gate's behavior on a real WAV
// container - not fakeWAV's deliberately-garbage bytes - is exactly what's
// under test.
func synthesizeWAV(t *testing.T, pcm []byte) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synth.wav")
	if err := audio.WriteWAV(path, pcm, 16000); err != nil {
		t.Fatalf("WriteWAV: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synthesized wav: %v", err)
	}
	return b
}

// constAmplitudePCM builds n 16-bit little-endian samples alternating
// +amp/-amp - a crude but effective stand-in for "audio loud enough to be
// speech" or "audio quiet enough to be room tone", depending on amp.
func constAmplitudePCM(n int, amp int16) []byte {
	buf := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := amp
		if i%2 == 1 {
			v = -amp
		}
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(v))
	}
	return buf
}

// pureSilenceWAV is a real, parseable 3-second 16kHz mono WAV of all-zero
// PCM samples - exactly what ffmpeg's anullsrc produces, and exactly the
// clip that reproduced the original bug: whisper transcribed it as "you",
// which the OLD bracket-only hallucination filter let through to the LLM.
func pureSilenceWAV(t *testing.T) []byte {
	t.Helper()
	const samples = 16000 * 3 // 3s at 16kHz
	return synthesizeWAV(t, make([]byte, samples*2))
}

// speechLevelWAV is a real, parseable WAV loud enough to clear
// silenceRMSThreshold by a wide margin - used to prove the RMS gate does
// NOT block real speech-level audio, isolating "the RMS gate" from "the
// transcript hallucination filter" in tests that need to reach the fake
// transcriber legitimately (not merely because fakeWAV fails to parse).
func speechLevelWAV(t *testing.T) []byte {
	t.Helper()
	return synthesizeWAV(t, constAmplitudePCM(16000, 3000)) // 1s @ amplitude 3000
}

func newTurnRequest(body []byte, bearerToken string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/lmd/v1/turn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "audio/wav")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return req
}

func newTestServer(deps Deps) *Server {
	return New(http.NewServeMux(), deps)
}

// onePersonaDeps builds a Deps with a single "aida" persona wired in as both
// the only entry and the default - the common shape for every test above
// that predates persona routing and doesn't care about it (persona-routing
// itself gets its own tests further down). Keeping the persona name fixed at
// "aida" rather than some test-only placeholder means these pre-existing
// tests exercise the exact same default-persona path a real absent-header
// request would take.
func onePersonaDeps(tr Transcriber, ask Asker, synth tts.Synthesizer, token string) Deps {
	return Deps{
		Transcriber:    tr,
		Personas:       map[string]PersonaDeps{"aida": {Asker: ask, Synth: synth}},
		DefaultPersona: "aida",
		Token:          token,
	}
}

func TestHandleTurn_MissingToken(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, "") // no Authorization header at all
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if tr.calls != 0 || ask.calls != 0 || synth.calls != 0 {
		t.Errorf("no pipeline stage should run on auth failure: transcribe=%d ask=%d synth=%d", tr.calls, ask.calls, synth.calls)
	}
}

func TestHandleTurn_BadToken(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, "LMD-WRONG-TOKEN0-00000-00000-00000")
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if tr.calls != 0 || ask.calls != 0 || synth.calls != 0 {
		t.Error("no pipeline stage should run on auth failure")
	}
}

// TestHandleTurn_GoodTokenLookAlikeVariant covers the normalization contract
// end-to-end: a token typed with lowercase, hyphens rearranged, and an O/I
// look-alike still authenticates against the canonical stored token.
func TestHandleTurn_GoodTokenLookAlikeVariant(t *testing.T) {
	stored := "LMD-3K7H0-9QXZP-1RTVW-2H9JK-5M3N1" // contains real 0s and 1s
	typedByHand := "lmd-3k7ho-9qxzp-irtvw-2h9jk-5m3nl"

	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".wav", body: []byte("audio"), name: "piper"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, stored))

	req := newTurnRequest(fakeWAV, typedByHand)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTurn_EmptyTranscript(t *testing.T) {
	tr := &fakeTranscriber{text: "   "} // whitespace-only, counts as empty
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if ask.calls != 0 || synth.calls != 0 {
		t.Errorf("an empty transcript must never reach the LLM or TTS: ask=%d synth=%d", ask.calls, synth.calls)
	}
}

func TestHandleTurn_HallucinatedTranscript(t *testing.T) {
	for _, hallucination := range []string{"[BLANK_AUDIO]", "(upbeat music)", "[silence]"} {
		t.Run(hallucination, func(t *testing.T) {
			tr := &fakeTranscriber{text: hallucination}
			ask := &fakeAsker{reply: "hi"}
			synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
			s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

			req := newTurnRequest(fakeWAV, testToken)
			rec := httptest.NewRecorder()
			s.handleTurn(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
			}
			if ask.calls != 0 || synth.calls != 0 {
				t.Errorf("a hallucinated transcript must never reach the LLM or TTS: ask=%d synth=%d", ask.calls, synth.calls)
			}
		})
	}
}

// TestHandleTurn_BareWordHallucinatedTranscript covers the new bug fix:
// whisper's well-known bare-word transcriptions of silence/noise (not just
// the old bracket/paren tags). Body is a real speech-level WAV (clears the
// RMS gate on its own merits) so this test isolates the transcript filter
// from the RMS gate - the point is that the FILTER catches these, not that
// the gate happened to.
func TestHandleTurn_BareWordHallucinatedTranscript(t *testing.T) {
	wav := speechLevelWAV(t)
	for _, hallucination := range []string{
		"you", "Thank you", "thank you.", "thanks for watching",
		"thanks for watching!", "bye", "bye.", "so", "um", "uh", "okay",
		"oh", "please subscribe", ".", "♪", "♫",
	} {
		t.Run(hallucination, func(t *testing.T) {
			tr := &fakeTranscriber{text: hallucination}
			ask := &fakeAsker{reply: "hi"}
			synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
			s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

			req := newTurnRequest(wav, testToken)
			rec := httptest.NewRecorder()
			s.handleTurn(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
			}
			if ask.calls != 0 || synth.calls != 0 {
				t.Errorf("a bare-word hallucination must never reach the LLM or TTS: ask=%d synth=%d", ask.calls, synth.calls)
			}
		})
	}
}

// TestHandleTurn_RealSentenceContainingHallucinationWordReachesLLM is the
// critical regression guard for the bare-word filter: a real question that
// merely CONTAINS one of the filtered words as a substring ("you", "so",
// "thank you", ...) must reach the LLM like any other query, not get
// silently dropped.
func TestHandleTurn_RealSentenceContainingHallucinationWordReachesLLM(t *testing.T) {
	wav := speechLevelWAV(t)
	sentences := []string{
		"what did you say about the deploy",
		"so what's next",
		"tell him thank you for me",
		"is that okay with you",
	}
	for _, sentence := range sentences {
		t.Run(sentence, func(t *testing.T) {
			tr := &fakeTranscriber{text: sentence}
			ask := &fakeAsker{reply: "some real reply"}
			synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
			s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

			req := newTurnRequest(wav, testToken)
			rec := httptest.NewRecorder()
			s.handleTurn(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if ask.calls != 1 || synth.calls != 1 {
				t.Errorf("a real sentence must reach the LLM and TTS exactly once: ask=%d synth=%d", ask.calls, synth.calls)
			}
		})
	}
}

// TestHandleTurn_PureSilence is the exact repro from the bug report: 3s of
// pure digital silence (what ffmpeg's anullsrc produces) must be rejected
// by the RMS gate BEFORE whisper is ever spawned - not just before the LLM.
// Old behavior: whisper transcribed this as "you", the bracket-only filter
// missed it, and the turn burned a full LLM + TTS round trip.
func TestHandleTurn_PureSilence(t *testing.T) {
	tr := &fakeTranscriber{text: "you"} // what whisper actually hallucinated on the real repro
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(pureSilenceWAV(t), testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if tr.calls != 0 {
		t.Errorf("pure silence must be rejected by the RMS gate BEFORE whisper runs: transcribe calls = %d, want 0", tr.calls)
	}
	if ask.calls != 0 || synth.calls != 0 {
		t.Errorf("pure silence must never reach the LLM or TTS: ask=%d synth=%d", ask.calls, synth.calls)
	}
}

// TestHandleTurn_RMSGateTable is a table-driven pass over the RMS gate at
// the handler level: silence and low-level noise get rejected pre-whisper;
// speech-level amplitude passes through untouched.
func TestHandleTurn_RMSGateTable(t *testing.T) {
	cases := []struct {
		name      string
		pcm       []byte
		wantGated bool // true → 422, zero transcribe calls
	}{
		{"digital silence", make([]byte, 16000*2), true},
		{"very low-level noise", constAmplitudePCM(16000, 8), true},
		{"speech-level amplitude", constAmplitudePCM(16000, 3000), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := &fakeTranscriber{text: "what's on my plate today"}
			ask := &fakeAsker{reply: "hi"}
			synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
			s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

			req := newTurnRequest(synthesizeWAV(t, c.pcm), testToken)
			rec := httptest.NewRecorder()
			s.handleTurn(rec, req)

			if c.wantGated {
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
				}
				if tr.calls != 0 || ask.calls != 0 || synth.calls != 0 {
					t.Errorf("gated audio must never reach whisper, the LLM, or TTS: transcribe=%d ask=%d synth=%d", tr.calls, ask.calls, synth.calls)
				}
			} else {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
				}
				if tr.calls != 1 || ask.calls != 1 || synth.calls != 1 {
					t.Errorf("speech-level audio must reach every stage exactly once: transcribe=%d ask=%d synth=%d", tr.calls, ask.calls, synth.calls)
				}
			}
		})
	}
}

func TestHandleTurn_OversizedBody(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("audio"), name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	oversized := make([]byte, maxBodyBytes+1)
	req := newTurnRequest(oversized, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body=%s", rec.Code, rec.Body.String())
	}
	if tr.calls != 0 || ask.calls != 0 || synth.calls != 0 {
		t.Error("an oversized body must never reach transcription, the LLM, or TTS")
	}
}

// TestHandleTurn_HappyPath covers the full pipeline and the wire shape from
// docs/lmd-protocol.md: transcript + reply + base64 audio + the right
// audio_format, and that each stage is called exactly once, and that both
// temp files (WAV in, audio out) are cleaned up.
func TestHandleTurn_HappyPath(t *testing.T) {
	tr := &fakeTranscriber{text: "what's on my plate today"}
	ask := &fakeAsker{reply: "Three open tasks, sir."}
	audioBytes := []byte("totally-real-mp3-bytes")
	synthDir := t.TempDir()
	synth := &fakeSynth{dir: synthDir, ext: ".mp3", body: audioBytes, name: "elevenlabs"}
	d := onePersonaDeps(tr, ask, synth, testToken)
	d.Version = "test-1.0"
	s := newTestServer(d)

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp turnResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Transcript != "what's on my plate today" {
		t.Errorf("transcript = %q", resp.Transcript)
	}
	if resp.Reply != "Three open tasks, sir." {
		t.Errorf("reply = %q", resp.Reply)
	}
	if resp.AudioFormat != "mp3" {
		t.Errorf("audio_format = %q, want mp3", resp.AudioFormat)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Audio)
	if err != nil {
		t.Fatalf("audio field is not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, audioBytes) {
		t.Errorf("decoded audio = %q, want %q", decoded, audioBytes)
	}
	if resp.TookMs < 0 {
		t.Errorf("took_ms = %d, want >= 0", resp.TookMs)
	}
	if tr.calls != 1 || ask.calls != 1 || synth.calls != 1 {
		t.Errorf("expected exactly one call per stage: transcribe=%d ask=%d synth=%d", tr.calls, ask.calls, synth.calls)
	}

	if _, err := os.Stat(filepath.Join(synthDir, "reply.mp3")); !os.IsNotExist(err) {
		t.Error("synthesized audio temp file should have been removed after the response was written")
	}
}

// TestHandleTurn_PiperWAVFormat covers the other half of "audio_format must
// not be assumed" - Piper's .wav path.
func TestHandleTurn_PiperWAVFormat(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".wav", body: []byte("wav-bytes"), name: "piper"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	var resp turnResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AudioFormat != "wav" {
		t.Errorf("audio_format = %q, want wav", resp.AudioFormat)
	}
}

func TestHandleTurn_TranscribeError(t *testing.T) {
	tr := &fakeTranscriber{err: errTest("whisper-cli exploded")}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if ask.calls != 0 || synth.calls != 0 {
		t.Error("ask/synthesize must not run after a transcribe error")
	}
}

func TestHandleTurn_AskError(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{err: errTest("anthropic 529")}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", name: "elevenlabs"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
	if synth.calls != 0 {
		t.Error("synthesize must not run after an ask error")
	}
}

func TestHandleTurn_SynthesizeError(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{err: errTest("elevenlabs 401")}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleTurn_GetMethodNotAllowed(t *testing.T) {
	s := newTestServer(Deps{Token: testToken})
	req := httptest.NewRequest(http.MethodGet, "/lmd/v1/turn", nil)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestHandleTurn_SessionHeaderGroupsCalls verifies the X-LMD-Session
// behavior: two turns sharing the same header value get the same
// in-memory *jarvis.Session passed to Asker.Ask, groupong them into one
// short-term conversational window; a turn with no header gets nil
// (stateless), matching AskTextWithStats's one-shot behavior.
func TestHandleTurn_SessionHeaderGroupsCalls(t *testing.T) {
	tr := &fakeTranscriber{text: "hello"}
	ask := &fakeAsker{reply: "hi"}
	synth := &fakeSynth{dir: t.TempDir(), ext: ".wav", body: []byte("x"), name: "piper"}
	s := newTestServer(onePersonaDeps(tr, ask, synth, testToken))

	for i := 0; i < 2; i++ {
		req := newTurnRequest(fakeWAV, testToken)
		req.Header.Set("X-LMD-Session", "phone-session-abc")
		rec := httptest.NewRecorder()
		s.handleTurn(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if len(ask.sessions) != 2 {
		t.Fatalf("expected 2 recorded sessions, got %d", len(ask.sessions))
	}
	if ask.sessions[0] == nil || ask.sessions[1] == nil {
		t.Fatal("a request carrying X-LMD-Session should get a non-nil Session")
	}
	if ask.sessions[0] != ask.sessions[1] {
		t.Error("two turns sharing the same X-LMD-Session id should reuse the same Session pointer")
	}

	// A follow-up request with no session header must be stateless.
	req := newTurnRequest(fakeWAV, testToken)
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stateless call: status = %d", rec.Code)
	}
	if ask.sessions[len(ask.sessions)-1] != nil {
		t.Error("a request with no X-LMD-Session header should pass a nil Session")
	}
}

// TestHandleHealth_Unauthenticated also covers "Health lists exactly the
// wired personas": with both aida and jarvis wired, the response must list
// both, sorted, plus the configured default.
func TestHandleHealth_Unauthenticated(t *testing.T) {
	s := newTestServer(Deps{
		Personas: map[string]PersonaDeps{
			"aida":   {Asker: &fakeAsker{}, Synth: &fakeSynth{name: "elevenlabs"}},
			"jarvis": {Asker: &fakeAsker{}, Synth: &fakeSynth{name: "elevenlabs"}},
		},
		DefaultPersona: "aida",
		Token:          testToken,
		Version:        "v-test",
	})

	// Deliberately no Authorization header - health must work anyway.
	req := httptest.NewRequest(http.MethodGet, "/lmd/v1/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.OK || resp.Service != "aida-lmd" || resp.Version != "v-test" || resp.Engine != "elevenlabs" {
		t.Errorf("unexpected health response: %+v", resp)
	}
	if len(resp.Personas) != 2 || resp.Personas[0] != "aida" || resp.Personas[1] != "jarvis" {
		t.Errorf("personas = %v, want exactly [aida jarvis] (sorted)", resp.Personas)
	}
	if resp.DefaultPersona != "aida" {
		t.Errorf("default_persona = %q, want aida", resp.DefaultPersona)
	}
}

// TestHandleHealth_OnlyPrimaryAvailable is the degradation path: when the
// Aida twin failed to construct (or was never configured), health must
// report ONLY the persona(s) actually wired up - never a hardcoded
// ["aida","jarvis"] - and default_persona must point at something real
// instead of naming a persona nobody answers for.
func TestHandleHealth_OnlyPrimaryAvailable(t *testing.T) {
	s := newTestServer(Deps{
		Personas: map[string]PersonaDeps{
			"jarvis": {Asker: &fakeAsker{}, Synth: &fakeSynth{name: "elevenlabs"}},
		},
		DefaultPersona: "jarvis",
		Token:          testToken,
	})

	req := httptest.NewRequest(http.MethodGet, "/lmd/v1/health", nil)
	rec := httptest.NewRecorder()
	s.handleHealth(rec, req)

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Personas) != 1 || resp.Personas[0] != "jarvis" {
		t.Errorf("personas = %v, want exactly [jarvis]", resp.Personas)
	}
	if resp.DefaultPersona != "jarvis" {
		t.Errorf("default_persona = %q, want jarvis", resp.DefaultPersona)
	}
}

// TestHandleTurn_PersonaHeaderRouting covers docs/lmd-protocol.md's Personas
// routing contract: an absent, empty, unrecognized, or oddly-cased-but-
// unmatched X-LMD-Persona header all fall back to the default persona
// ("aida") without erroring - a 4xx/5xx here would break every older client
// that never sends the header at all.
func TestHandleTurn_PersonaHeaderRouting(t *testing.T) {
	for _, headerValue := range []string{"", "bogus", "AIDA", "aIdA"} {
		t.Run("header="+headerValue, func(t *testing.T) {
			aidaAsk := &fakeAsker{reply: "aida reply"}
			aidaSynth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("aida-audio"), name: "elevenlabs"}
			jarvisAsk := &fakeAsker{reply: "jarvis reply"}
			jarvisSynth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("jarvis-audio"), name: "elevenlabs"}
			s := newTestServer(Deps{
				Transcriber: &fakeTranscriber{text: "hello"},
				Personas: map[string]PersonaDeps{
					"aida":   {Asker: aidaAsk, Synth: aidaSynth},
					"jarvis": {Asker: jarvisAsk, Synth: jarvisSynth},
				},
				DefaultPersona: "aida",
				Token:          testToken,
			})

			req := newTurnRequest(fakeWAV, testToken)
			if headerValue != "" {
				req.Header.Set("X-LMD-Persona", headerValue)
			}
			rec := httptest.NewRecorder()
			s.handleTurn(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if aidaAsk.calls != 1 || aidaSynth.calls != 1 {
				t.Errorf("expected the default (aida) persona to answer: ask=%d synth=%d", aidaAsk.calls, aidaSynth.calls)
			}
			if jarvisAsk.calls != 0 || jarvisSynth.calls != 0 {
				t.Errorf("jarvis persona must not be touched: ask=%d synth=%d", jarvisAsk.calls, jarvisSynth.calls)
			}
		})
	}
}

// TestHandleTurn_ExplicitJarvisPersonaRoutesAskAndSynth is the direct
// regression guard for the reported bug: routing only the Asker (reply
// TEXT) while leaving Synth (the VOICE) pointed at the wrong persona would
// still play the wrong voice, reproducing the exact bug this wiring fixes.
// Asserts BOTH the asker and the synthesizer that ran are jarvis's, using
// distinct fakes per persona so a swap or a half-routed fix gets caught.
func TestHandleTurn_ExplicitJarvisPersonaRoutesAskAndSynth(t *testing.T) {
	aidaAsk := &fakeAsker{reply: "aida reply"}
	aidaSynth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("aida-audio"), name: "elevenlabs"}
	jarvisAsk := &fakeAsker{reply: "jarvis reply"}
	jarvisSynth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("jarvis-audio"), name: "elevenlabs"}
	s := newTestServer(Deps{
		Transcriber: &fakeTranscriber{text: "hello"},
		Personas: map[string]PersonaDeps{
			"aida":   {Asker: aidaAsk, Synth: aidaSynth},
			"jarvis": {Asker: jarvisAsk, Synth: jarvisSynth},
		},
		DefaultPersona: "aida",
		Token:          testToken,
	})

	req := newTurnRequest(fakeWAV, testToken)
	req.Header.Set("X-LMD-Persona", "jarvis")
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp turnResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Reply != "jarvis reply" {
		t.Errorf("reply = %q, want the jarvis asker's reply", resp.Reply)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Audio)
	if err != nil {
		t.Fatalf("audio field is not valid base64: %v", err)
	}
	if !bytes.Equal(decoded, []byte("jarvis-audio")) {
		t.Errorf("audio = %q, want the jarvis synthesizer's output", decoded)
	}
	if jarvisAsk.calls != 1 || jarvisSynth.calls != 1 {
		t.Errorf("jarvis persona should answer exactly once: ask=%d synth=%d", jarvisAsk.calls, jarvisSynth.calls)
	}
	if aidaAsk.calls != 0 || aidaSynth.calls != 0 {
		t.Errorf("aida persona must not be touched: ask=%d synth=%d", aidaAsk.calls, aidaSynth.calls)
	}
}

// TestHandleTurn_UnknownPersonaFallsBackWhenOnlyJarvisWired is the
// only-primary-available degradation path at the turn-handling level: with
// the Aida twin unavailable (so only jarvis is wired), a request naming
// X-LMD-Persona: aida must still fall back to the default (jarvis) and
// succeed, rather than 500ing because the named persona isn't present.
func TestHandleTurn_UnknownPersonaFallsBackWhenOnlyJarvisWired(t *testing.T) {
	jarvisAsk := &fakeAsker{reply: "jarvis reply"}
	jarvisSynth := &fakeSynth{dir: t.TempDir(), ext: ".mp3", body: []byte("jarvis-audio"), name: "elevenlabs"}
	s := newTestServer(Deps{
		Transcriber: &fakeTranscriber{text: "hello"},
		Personas: map[string]PersonaDeps{
			"jarvis": {Asker: jarvisAsk, Synth: jarvisSynth},
		},
		DefaultPersona: "jarvis",
		Token:          testToken,
	})

	req := newTurnRequest(fakeWAV, testToken)
	req.Header.Set("X-LMD-Persona", "aida") // not wired up on this server
	rec := httptest.NewRecorder()
	s.handleTurn(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if jarvisAsk.calls != 1 || jarvisSynth.calls != 1 {
		t.Errorf("should have fallen back to the only wired persona: ask=%d synth=%d", jarvisAsk.calls, jarvisSynth.calls)
	}
}

// errTest is a minimal error type so tests don't need to import "errors"
// just to build a sentinel.
type errTest string

func (e errTest) Error() string { return string(e) }

// newAckRequest builds a GET /lmd/v1/ack request with the given ?persona=
// value (omitted entirely when empty, mirroring a client that never sends
// the parameter) and bearer token.
func newAckRequest(persona, bearerToken string) *http.Request {
	url := "/lmd/v1/ack"
	if persona != "" {
		url += "?persona=" + persona
	}
	req := httptest.NewRequest(http.MethodGet, url, nil)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return req
}

func TestHandleAck_MissingToken(t *testing.T) {
	s := newTestServer(Deps{
		Personas:       map[string]PersonaDeps{"aida": {AckPath: writeTempAudio(t, ".mp3", []byte("ack-audio"))}},
		DefaultPersona: "aida",
		Token:          testToken,
	})

	req := newAckRequest("aida", "") // no Authorization header at all
	rec := httptest.NewRecorder()
	s.handleAck(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() > 0 && rec.Header().Get("Content-Type") != "application/json" {
		t.Error("an unauthenticated request must never receive audio bytes")
	}
}

func TestHandleAck_BadToken(t *testing.T) {
	s := newTestServer(Deps{
		Personas:       map[string]PersonaDeps{"aida": {AckPath: writeTempAudio(t, ".mp3", []byte("ack-audio"))}},
		DefaultPersona: "aida",
		Token:          testToken,
	})

	req := newAckRequest("aida", "LMD-WRONG-TOKEN0-00000-00000-00000")
	rec := httptest.NewRecorder()
	s.handleAck(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

// writeTempAudio writes body to a new file under t.TempDir() with the given
// extension and returns its path - stands in for the path
// (*jarvis.Assistant).AckAudioPath would return.
func writeTempAudio(t *testing.T, ext string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ack"+ext)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	return path
}

// TestHandleAck_KnownPersonaTable covers the 200 path for both audio
// formats a real deployment can produce - ElevenLabs' .mp3 and Piper's
// .wav - asserting the Content-Type header matches the file extension (not
// hardcoded) and the body bytes are exactly what was on disk.
func TestHandleAck_KnownPersonaTable(t *testing.T) {
	cases := []struct {
		name   string
		ext    string
		wantCT string
		body   []byte
	}{
		{"elevenlabs mp3", ".mp3", "audio/mpeg", []byte("totally-real-mp3-bytes")},
		{"piper wav", ".wav", "audio/wav", []byte("totally-real-wav-bytes")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeTempAudio(t, c.ext, c.body)
			s := newTestServer(Deps{
				Personas:       map[string]PersonaDeps{"aida": {AckPath: path}},
				DefaultPersona: "aida",
				Token:          testToken,
			})

			req := newAckRequest("aida", testToken)
			rec := httptest.NewRecorder()
			s.handleAck(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != c.wantCT {
				t.Errorf("Content-Type = %q, want %q", ct, c.wantCT)
			}
			if !bytes.Equal(rec.Body.Bytes(), c.body) {
				t.Errorf("body = %q, want %q", rec.Body.Bytes(), c.body)
			}
			if cc := rec.Header().Get("Cache-Control"); cc == "" {
				t.Error("Cache-Control should be set - the clip is stable for the daemon's lifetime")
			}
		})
	}
}

// TestHandleAck_NoAckConfigured covers pre-synthesis legitimately failing
// at startup (no ElevenLabs key, network blip): AckPath is empty, so the
// endpoint must 404 rather than error.
func TestHandleAck_NoAckConfigured(t *testing.T) {
	s := newTestServer(Deps{
		Personas:       map[string]PersonaDeps{"aida": {AckPath: ""}},
		DefaultPersona: "aida",
		Token:          testToken,
	})

	req := newAckRequest("aida", testToken)
	rec := httptest.NewRecorder()
	s.handleAck(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleAck_PersonaQueryFallback covers docs/lmd-protocol.md's fallback
// contract from the query-param side: an absent, empty, unknown, or
// mixed-case ?persona= value must all resolve to the default persona,
// consistent with /turn's header-based fallback (see
// TestPersonaResolution_HeaderAndQueryAgree for the direct cross-check).
func TestHandleAck_PersonaQueryFallback(t *testing.T) {
	aidaPath := writeTempAudio(t, ".mp3", []byte("aida-ack"))
	jarvisPath := writeTempAudio(t, ".mp3", []byte("jarvis-ack"))
	deps := Deps{
		Personas: map[string]PersonaDeps{
			"aida":   {AckPath: aidaPath},
			"jarvis": {AckPath: jarvisPath},
		},
		DefaultPersona: "aida",
		Token:          testToken,
	}

	for _, persona := range []string{"", "bogus", "AIDA", "aIdA"} {
		t.Run("persona="+persona, func(t *testing.T) {
			s := newTestServer(deps)
			req := newAckRequest(persona, testToken)
			rec := httptest.NewRecorder()
			s.handleAck(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if !bytes.Equal(rec.Body.Bytes(), []byte("aida-ack")) {
				t.Errorf("body = %q, want the default (aida) persona's clip", rec.Body.Bytes())
			}
		})
	}
}

func TestHandleAck_GetMethodOnly(t *testing.T) {
	s := newTestServer(Deps{Token: testToken})
	req := httptest.NewRequest(http.MethodPost, "/lmd/v1/ack", nil)
	rec := httptest.NewRecorder()
	s.handleAck(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestPersonaResolution_HeaderAndQueryAgree is the direct regression guard
// for the shared-factoring requirement: /turn's header-based resolvePersona
// and /ack's query-based resolvePersonaQuery must resolve identically for
// every input, since both are built on the same normalizePersonaName +
// personaDepsFor pair. If a future change duplicated the lookup instead of
// sharing it, this is what would catch the two drifting apart.
func TestPersonaResolution_HeaderAndQueryAgree(t *testing.T) {
	s := newTestServer(Deps{
		Personas: map[string]PersonaDeps{
			"aida":   {AckPath: "aida-path"},
			"jarvis": {AckPath: "jarvis-path"},
		},
		DefaultPersona: "jarvis",
	})

	for _, name := range []string{"", "aida", "AIDA", " Jarvis ", "bogus", "jArViS"} {
		t.Run("name="+name, func(t *testing.T) {
			headerReq := httptest.NewRequest(http.MethodPost, "/lmd/v1/turn", nil)
			headerReq.Header.Set("X-LMD-Persona", name)
			fromHeader := s.resolvePersona(headerReq)

			queryURL := "/lmd/v1/ack"
			if name != "" {
				queryURL += "?persona=" + strings.ReplaceAll(name, " ", "%20")
			}
			queryReq := httptest.NewRequest(http.MethodGet, queryURL, nil)
			fromQuery := s.resolvePersonaQuery(queryReq)

			if fromHeader.AckPath != fromQuery.AckPath {
				t.Errorf("header resolution = %q, query resolution = %q - must agree for input %q",
					fromHeader.AckPath, fromQuery.AckPath, name)
			}
		})
	}

	// The zero-value case (no header sent at all vs. no query param at
	// all) must also agree - this is the exact "absent" case a client that
	// doesn't send persona info hits.
	headerReq := httptest.NewRequest(http.MethodPost, "/lmd/v1/turn", nil)
	fromHeader := s.resolvePersona(headerReq)
	queryReq := httptest.NewRequest(http.MethodGet, "/lmd/v1/ack", nil)
	fromQuery := s.resolvePersonaQuery(queryReq)
	if fromHeader.AckPath != fromQuery.AckPath {
		t.Errorf("absent header = %q, absent query = %q - must agree", fromHeader.AckPath, fromQuery.AckPath)
	}
}
