# Engineering note: the voice pipeline

How a spoken sentence becomes a spoken answer, from the code
(`internal/jarvis/` and its subpackages). All Go, all local-first: ffmpeg for
capture, whisper.cpp for STT, Claude Haiku for the turn, Piper or ElevenLabs
for TTS. No Python in the runtime path.

## The stages

```
mic (ffmpeg, 16 kHz mono PCM)
  → amplitude VAD (30 ms chunks, RMS gate)
  → utterance buffer (pre-roll + trailing-silence cutoff)
  → whisper.cpp transcription
  → hallucination filter
  → wake-phrase routing (regex, per persona)
  → recall (one Voyage embed → thread memory + past feedback)
  → Haiku with in-process tool dispatch
  → grounding + silent-reply guards
  → TTS (streaming ElevenLabs, or Piper to WAV) → playback
  → audit log + turn promotion
```

### Capture and VAD (`internal/jarvis/audio`, `listener`)

ffmpeg streams the system mic as 16 kHz / mono / 16-bit PCM. The listener reads
30 ms chunks (480 samples) and computes int16 RMS per chunk; above the gate
(default 300) is speech. Segmentation rules, all tuned toward "easy to trigger"
because a missed wake-word is worse than a spurious transcription attempt:

- **Pre-roll**: an 800 ms rolling ring is prepended when speech starts, so a soft
  "Hey Jarvis" whose gate doesn't fire until the louder body of the sentence
  keeps its leading edge.
- **End of utterance**: 1200 ms of trailing silence, or a 12 s hard cap.
- **Minimum**: blips under 300 ms are dropped before STT.

With a mic preference list, a *following stream* re-evaluates available input
devices every few seconds and hot-swaps capture on dock/undock, so the mic never
silently goes deaf after a sleep/wake cycle.

### STT (`internal/jarvis/stt`)

Transcription shells out to whisper.cpp's `whisper-cli` (Metal-accelerated on
macOS). The default model is `small.en` - the pipeline started on `tiny.en` for
latency, but `small.en` is substantially better on technical vocabulary at a cost
of only ~100–300 ms, which the ack phrase (below) absorbs anyway. Two tricks:

- **Vocabulary prompt**: whisper's `--prompt` is seeded with the proper nouns
  that recur in spoken queries, so decoding favors the correct spelling over an
  acoustically similar guess ("pull request" instead of "polar quest").
- **Hallucination filter** (`stt.IsHallucination`): on quiet or noisy clips,
  whisper confidently emits `[BLANK_AUDIO]`, `(upbeat music)`, or bare words like
  "you" / "thank you". These are matched on the whole normalized transcript
  (never as substrings) and dropped before they reach the LLM or the audit log.

### Wake routing (`internal/jarvis/listener`)

One mic stream, multiple personas. Each persona owns a wake regex - punctuation-
tolerant, because whisper renders "ok jarvis" as any of "Okay, Jarvis,",
"OK JARVIS", "Good morning, Jarvis." - and the phrase that appears *earliest* in
the transcript wins. Name alternations absorb whisper's mishearings (an "aida"
persona also answers to "ada"/"ayda"), with a trailing `\b` so short branches
don't swallow longer words - "hey Idaho" must not wake Aida.

Statefulness is minimal but deliberate:

- **Bare wake arms the mic**: "Ok Jarvis" alone speaks a short rotating prompt
  ("Yes, sir?") and treats the *next* utterance as the query, no wake needed.
- **Conversational follow-up**: a reply containing a question mark keeps the mic
  armed for 15 s so the user can just answer. The check is "`?` anywhere", not
  "ends with `?`" - the assistant routinely appends a clause after the question,
  and a false positive only costs a bounded silent window, while a false negative
  drops the user's answer.
- **Push-to-talk barge-in**: a hardware button (HID, via the serve daemon)
  bypasses the wake word entirely; a press mid-reply cancels the in-flight turn's
  context, which kills TTS playback, and starts a clean capture.

### The turn (`internal/jarvis/jarvis.go`)

Before the LLM call, `recall` embeds the query **once** (Voyage, 2 s cap - recall
is purely additive, so when the embed is slow the turn proceeds without it) and
shares that embedding across two lookups: durable thread memory ("Earlier in this
conversation") and past feedback lessons ("Past feedback on similar requests").
Both land in the system prompt as prose.

Tools dispatch in-process - the registry calls the brain, jobs store, and MCP
client directly, no RPC. Slow tools (`aida_query`, `mcp_call_tool`, `job_start`)
fire a pre-synthesized "One moment, sir." acknowledgment before running, masking
the 8–15 s an engine query takes; a second canned phrase fires at the 90 s mark
of a long subprocess so silence never reads as a hang.

Two guards run on the reply *before* synthesis, because a wrong sentence spoken
aloud is worse than a wrong sentence printed:

- **Grounding guard**: if the reply claims a background job was started but no
  `job_*` tool actually fired this turn, the model confabulated it. The spoken
  text is replaced with a truthful fallback, and the rewrite is recorded so the
  audit log never carries the lie.
- **Silent-reply guard**: a tool can succeed while the model returns zero text
  (a tool result is a block the model independently decides whether to echo).
  An empty reply after successful tool calls becomes a short spoken confirmation
  instead of dead air.

### TTS and playback (`internal/jarvis/tts`)

Two engines behind one interface: Piper (local, offline-capable, an MIT-licensed
voice) as the baseline, ElevenLabs as the quality upgrade. ElevenLabs implements
a second, optional interface - `StreamingSynthesizer` - that plays audio as it
arrives, dropping time-to-first-sound from "whole clip generated and downloaded"
to "first chunk". The audit record splits the timing accordingly: `tts_ms` is
time-to-first-audio, `play_ms` the remainder.

### The echo problem

The always-on mic hears the assistant's own voice. After any turn that played
audio, the listener flushes the mic stream's pipe backlog and clears the pre-roll
ring before resuming VAD - otherwise the assistant transcribes, and in armed
mode *answers*, itself. This shipped as a fix after the failure occurred in
production (see `docs/notes/failure-stories.md`).

## The latency budget

From the NDJSON audit log (`~/.aida/brain/jarvis/audit.ndjson`), which records
per-stage timings for every real wake-triggered turn:

| Stage | Typical | Notes |
|---|---|---|
| VAD end-of-utterance wait | 1.2 s | trailing-silence threshold, paid on every turn |
| whisper.cpp (small.en, Metal) | 0.3–1 s | scales with utterance length |
| recall embed | 0.1–0.3 s | hard-capped at 2 s, then skipped |
| LLM turn (`llm_ms`) | ~1.1 s | Haiku; more with tool round-trips |
| local fast tool (`tool_ms`) | 0.1–1.5 s | tasks/time/weather |
| `aida_query` tool | 8–15 s | full engine pipeline; masked by the ack |
| TTS first sound (`tts_ms`) | ~0.9 s | streaming: time-to-first-audio |
| playback (`play_ms`) | 2–6 s | dominates any reply over two sentences |

End-to-end: **2–4 s** for a fast-tool turn, **8–15 s** when the engine is
involved. The design lesson: past a point, perceived latency is governed by
*acknowledgment*, not speed - the "One moment, sir." ack and the 90 s progress
phrase bought more perceived responsiveness than any optimization of the actual
pipeline did.

## What gets remembered

Every real turn appends one JSON line to the audit log (transcript, query,
reply, per-tool timings, per-stage ms). Filtered noise - VAD blips, hallucinated
transcripts - never lands there: the log captures the assistant's actual
behavior, not raw mic input. Substantive turns are additionally promoted
asynchronously into durable thread memory (embedded, recallable across daemon
restarts), and the last non-rating turn is cached in memory so the voice
feedback tools ("thumbs down - you should have used the weather tool") know
which turn they're rating (see `docs/notes/feedback-mechanics.md`).
