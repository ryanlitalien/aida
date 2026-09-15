# L.M.D. protocol (aida-android ↔ aida serve)

Wire contract between the **A.I.D.A.** Android client (*Life Model Decoy*) and the
`aida serve` daemon. Spike-grade: one round trip per turn, no streaming.

## Transport

A **second HTTP listener**, separate from the existing `:1610` daemon.

| | `:1610` (existing) | `:1218` (new) |
|---|---|---|
| Bind | `127.0.0.1` | Tailscale IP only (e.g. `100.64.0.1`) |
| Auth | none | bearer token, required |
| Surface | tasks UI, `/api/*`, `/mcp/call`, `/jarvis/*` | `/lmd/v1/*` only |

Port 1218 is Earth-1218 - the "real world" in Marvel cosmology, which is where
the phone lives. `:1610` (Earth-1610) stays loopback-only so the unauthenticated
tasks UI and `/mcp/call` tool dispatch are never reachable from the tailnet.

**Bind to the resolved Tailscale address, never `0.0.0.0`.** If no Tailscale
interface is found, log and skip starting the listener rather than falling back
to a wildcard bind.

### Token

Read in order: `AIDA_LMD_TOKEN` env → `~/.aida/lmd/token`. If neither exists,
generate one, write it to `~/.aida/lmd/token` mode `0600`, and log it at
startup so it can be typed into the app.

**Format:** `LMD-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX` - 25 characters of
[Crockford base32](https://www.crockford.com/base32.html) in five
hyphen-groups, drawn from `crypto/rand`. That is ~125 bits of entropy, which is
well past brute-forceable, while staying something you can read off a terminal
and thumb into a phone without errors.

Crockford's alphabet excludes `I`, `L`, `O`, and `U` precisely because they get
misread as `1`, `1`, `0`, and each other. Normalize on the way in: uppercase,
strip hyphens, and map `I`/`l` → `1` and `O` → `0` before comparing, so a
hand-typed token that got those wrong still authenticates. The `LMD-` prefix is
a human affordance for spotting the string in a scrollback, not a checksum - 
strip it before comparison and do not require it.

Compare with `crypto/subtle.ConstantTimeCompare` on the normalized value.
Missing/bad token → `401`.

Transport note: this is plain HTTP, but every byte rides inside Tailscale's
WireGuard tunnel, so the token is not crossing the network in the clear. It is
still a replayable bearer credential - anything already on the tailnet can
reuse it. Acceptable here because the tailnet is a single-owner device set.

### `aida lmd token`

Prints the current token. `--rotate` generates and persists a new one, which
invalidates the old one on the next request - the phone then needs the new
value pasted into settings.

## `GET /lmd/v1/health`

**Unauthenticated** - deliberately. The client must be able to distinguish
"can't reach the host" (→ show the Tailscale screen) from "reachable but my
token is wrong" (→ show a credentials error). Leaks only that aida is running.

```json
{
  "ok": true, "service": "aida-lmd", "version": "…", "engine": "elevenlabs",
  "personas": ["aida", "jarvis"], "default_persona": "aida"
}
```

`personas` lets the client populate its voice selector from whatever the daemon
actually has running, rather than hardcoding a list that drifts.

## Personas

`aida serve` runs two assistants - a primary and a cosmetic twin built via
`NewTwin` - sharing one brain, MCP discovery, and jobs store. They differ only
in persona, voice, and phrase pool:

| Persona | Voice ID | `ElevenLabsSpeed` | Character |
|---|---|---|---|
| `aida` (default) | `Lopz0RdZlxPXALj2qGr3` | 1.0 | elite/arch, inflects |
| `jarvis` | `mfGn240ErL2HMopgBro1` | 0.85 | measured butler cadence |

**The app defaults to `aida`** - it is the A.I.D.A. client, and Jarvis's 0.85
speed reads as sluggish when you are expecting Aida. The original LMD wiring
pointed at the primary (Jarvis) assistant, which is why the first build sounded
both wrong and slow; those were one bug, not two.

An unknown or absent persona falls back to the default rather than erroring - 
a client on an older build must keep working.

## `POST /lmd/v1/turn`

One complete voice turn: audio in, audio + text out.

**Request**

```
Content-Type: audio/wav
Authorization: Bearer <token>
X-LMD-Session: <uuid>        // optional; groups turns for short-term memory
X-LMD-Persona: aida|jarvis   // optional; defaults to aida
```

Body is a 16 kHz / mono / 16-bit PCM WAV. That is exactly what Android's
`AudioRecord` produces and exactly what `stt.Whisper.Transcribe` requires, so
no transcoding happens on either side.

Cap the body at **10 MB** (~5 min of 16k mono) via `http.MaxBytesReader`.

The Transcriber defaults to whisper.cpp's tiny.en model (`stt.TinyEnModelPath`)
rather than `stt.Whisper`'s own zero-value default (small.en) - the LMD daemon
may run on weak/headless hardware with no GPU, where small.en's better
accuracy isn't worth 5-10x the transcription time. Override via profile
`serve.lmd_whisper_model` in `~/.aida/config.yaml` (e.g. to point at
`ggml-small.en.bin` on capable hardware).

Transcribe also auto-sizes whisper.cpp's `-ac`/`--audio-ctx` flag from each
clip's own duration (`stt.Whisper.AudioCtx`, default `0`): whisper.cpp always
encodes a full 30-second window on a CPU-only host regardless of how short
the clip is, so a 3s LMD turn otherwise costs as much to transcribe as a
30s one. Auto mode reads the WAV's duration from its header and shrinks the
context accordingly, falling back to full context (the flag omitted) when
the duration can't be determined or the clip is 28s or longer. Set
`AudioCtx` to `-1` to always use full context (e.g. on GPU-accelerated
hardware where the encode is already fast) or to a positive fixed value to
bypass auto-sizing.

**Response** `200`

```json
{
  "transcript":    "what's on my plate today",
  "reply":         "Three open tasks, sir. …",
  "audio":         "<base64>",
  "audio_format":  "mp3",
  "took_ms":       4120,
  "stt_ms":        820,
  "llm_ms":        2600,
  "tts_ms":        700
}
```

`audio_format` is `mp3` for ElevenLabs, `wav` for Piper - the client must not
assume one. Base64-in-JSON costs ~33% overhead (a 10s reply lands around
200–600 KB) which is irrelevant over a tailnet and keeps the client to a single
`kotlinx.serialization` decode. Revisit if streaming TTFA ever matters.

`stt_ms`/`llm_ms`/`tts_ms` break `took_ms` down by pipeline stage - measured
around `Transcriber.Transcribe`, `Asker.Ask`, and `Synth.Synthesize`
respectively (they may not sum to exactly `took_ms`, which also covers the
WAV read/write and silence-gate steps around them). Added alongside the
pre-existing `took_ms` field, so a client that only reads `took_ms` keeps
working unchanged. The daemon also prints one `📱 LMD turn: ...` log line per
completed turn with the same breakdown, and appends a record to
`~/.aida/brain/jarvis/audit.ndjson` tagged `"source": "lmd"` (see CLAUDE.md's
Audit log section) - both non-fatal if they fail.

**Errors** - `{"error": "…"}` with:

| Code | Meaning | Client behavior |
|---|---|---|
| 401 | bad/missing token | credentials error, do not retry |
| 413 | body over cap | drop, show "too long" |
| 422 | STT produced nothing usable | keep previous text, brief "didn't catch that" |
| 500 | LLM/TTS failure | keep previous text, show error toast |

### Silence rule (RMS gate)

Before whisper is even spawned, the uploaded clip's PCM is decoded and its
RMS amplitude checked against a conservative silence threshold
(`silenceRMSThreshold` in `internal/jarvis/lmd`). Digital silence and typical
room tone/self-noise fall well under it; real speech, even quiet speech,
does not. Below it → **422 without transcribing at all**. This is the fix
for the original bug: 3s of pure digital silence got whisper-transcribed as
the bare word "you", which the old filter didn't catch, and the turn burned
a full LLM + TTS round trip on a stray/stuck button press.

### Empty-transcript rule

If the transcript is blank or matches the whisper-hallucination filters - 
bracketed/parenthesized tags (`[BLANK_AUDIO]`, `(upbeat music)`, …) or
well-known bare-word outputs (`you`, `thank you`, `so`, `um`, …, matched on
the whole normalized transcript, never as a substring - see
`internal/jarvis/stt.IsHallucination`) - return **422 without invoking the
LLM**. A held button that captured only room tone must not cost an API call.

## `GET /lmd/v1/ack?persona=aida`

Returns the persona's pre-synthesized acknowledgement clip ("One moment, sir.")
as raw audio - `Content-Type: audio/mpeg` or `audio/wav` to match the engine.
Authenticated, same bearer token as `/turn`. 404 if the persona has no ack
(pre-synthesis is best-effort and tolerated to fail at startup).

### Why the client plays this, not the server

The daemon plays an ack locally before any slow tool runs, to mask latency. For
a phone turn that is a **bug** - the phone's conversation comes out of the
laptop's speakers in whatever room it happens to be in. Local playback is
therefore suppressed for remote turns (see "Playback must be suppressed per
call" below, which now covers the ack sites too, not just the final reply).

But suppression alone leaves the phone silent for the 8-15s an `aida_query`
takes, which reads as a hang. Since the protocol is one-shot, the server cannot
push "I just started a slow tool" mid-turn. So the client fetches this clip once
per persona, caches it, and plays it **locally after 5s of waiting**.

5s is chosen so it never fires on a normal turn (~4s end to end) - it only
speaks up right about when you would start wondering whether it broke, and it
never talks over a fast reply. The clip is the same audio in the same voice the
desk mic would have played, so the ack follows the originating device rather
than being hardcoded to the daemon's speakers.

## Server-side turn pipeline

```
WAV bytes → temp .wav → <RMS silence gate>
          → stt.Whisper.Transcribe
          → <hallucination / empty guard>
          → Assistant.<recall+tools, playback suppressed> → reply text
          → Assistant.TTS().Synthesize(reply) → temp audio file
          → base64 → JSON
```

Both temp files are removed with `defer`, including on the error paths.

**Playback must be suppressed per call, not globally.** The same process runs
the desk-mic listener; mutating `cfg.TextOnly` to silence the phone would
silence the mic loop too. Add a per-call variant that runs the full
recall-augmented, tool-dispatching path and *returns* the reply instead of
calling `tts.Play`, then synthesize separately in the handler.

`AskTextMute` already dispatches tools (`llm.Ask` → `AskWithHistory(ctx, nil,
"", …)`), but drops the recall block and history - good enough as a fallback,
not as the primary path.

## Known gaps

Spike-grade caveats a future reader should know before trusting this
document as a description of production behavior. Tracked as `aida` tasks
under the `aida-android` tag; recorded here because they are context you
need while reading the spec, not just work items.

**Session state is not safe under concurrent same-session turns.** The
`sessions` map is mutex-guarded, but the `*Session` it hands back is not
held under that lock for the duration of a turn. Two simultaneous requests
sharing one `X-LMD-Session` would race on `Turns`. Unreachable with one
phone and one button - the session id is per app launch - but an Android
Auto surface would be a second concurrent client against the same daemon,
which is exactly when this stops being theoretical.

**The 5s ack threshold is a guess.** It was chosen against a measured ~4s
normal turn and 8-15s `aida_query`, but has not been validated against
real use. A turn landing at 5.5s gets an ack it did not need, and the user
hears the ack immediately followed by the reply. The honest fix is not a
better constant - it is server-pushed progress (SSE or WebSocket), which
lets the daemon say "I just started a slow tool" instead of making the
client guess. That is a real protocol change, deliberately deferred.

**A TTS failure discards a complete reply.** Synthesis happens last, so an
ElevenLabs outage or exhausted quota returns 500 and throws away a
transcript, an LLM response, and any tool work already done. Falling back
to the local Piper engine would preserve the turn in a lesser voice. This
has already bitten once in real use.

**Upstream TTS error text is returned verbatim** in the `error` field,
including provider request ids and quota figures. Useful while spiking,
too chatty for anything exposed more widely than a single-owner tailnet.

## Client state machine

```
        ┌──────────────┐  health fails   ┌─────────────────┐
        │  CONNECTED   │ ──────────────► │  DISCONNECTED   │
        │  PTT armed   │ ◄────────────── │ "Turn on        │
        └──────┬───────┘  health ok      │  Tailscale"     │
               │                         └─────────────────┘
   press ──► RECORDING ──► release ──► SENDING ──► (200) update text + play
                                              └──► (4xx/5xx) keep text
```

- Health poll every **5s** while foregrounded; the app is unusable off-tailnet
  by design, so there is no offline queue.
- **Text updates only on a `200`.** Any failure leaves the previous reply on
  screen - a failed turn must never blank out what she last said.
- Recording is hard-capped at **30s**; hitting the cap sends what it has rather
  than discarding, so a stuck button degrades into a normal turn.
