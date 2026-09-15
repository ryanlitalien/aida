# Engineering note: the L.M.D. protocol (phone ↔ daemon)

The wire contract between the A.I.D.A. Android client and the `aida serve`
daemon, and the security reasoning behind it. The normative spec is
`docs/lmd-protocol.md` (this note is the narrative companion); the
implementation lives in `internal/jarvis/lmd/` and `startLMDServer` in
`internal/cli/serve.go`.

## Two listeners, two trust levels

The daemon already ran an HTTP surface on `127.0.0.1:1610` - tasks web UI,
`/api/*`, MCP tool dispatch - all **unauthenticated**, which is fine precisely
because loopback is the auth: nothing off-box can reach it. Letting a phone in
means crossing that line, and the wrong way to cross it is widening the
existing bind.

So the phone gets a *second* listener with its own mux:

| | `:1610` | `:1218` |
|---|---|---|
| Bind | `127.0.0.1` | the machine's Tailscale IP only |
| Auth | none (loopback is the auth) | bearer token, required |
| Surface | tasks UI, `/api/*`, `/mcp/call`, `/jarvis/*` | `/lmd/v1/*` only |

Rules that make the split real rather than decorative:

- **Bind to the resolved Tailscale address, never `0.0.0.0`.** The resolver
  (`internal/jarvis/lmd/tailscale.go`) enumerates network interfaces for an
  address in the CGNAT range Tailscale uses (`100.64.0.0/10`, RFC 6598) rather
  than shelling out to the `tailscale` CLI. No Tailscale interface found →
  log and *skip starting the listener*, never fall back to a wildcard bind.
- **A separate `http.NewServeMux()`** carrying only the three LMD routes. The
  unauthenticated tasks UI and `/mcp/call` are structurally unreachable from
  the tailnet, not just unlinked.

(The port naming - Earth-1610 for the daemon, Earth-1218 for "the real world,"
where the phone lives - is flavor. The bind discipline is the substance.)

## The token: designed to be thumbed into a phone

Format: `LMD-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX` - 25 characters of Crockford
base32 from `crypto/rand`, ~125 bits of entropy. Design constraints, in order:

1. **Unbrute-forceable** - 125 bits is far past the line.
2. **Transcribable** - it gets read off a terminal and typed into a phone
   once. Crockford's alphabet excludes `I`, `L`, `O`, `U` because they misread
   as `1`, `1`, `0`, and each other; the server additionally *normalizes* on
   the way in (uppercase, strip hyphens, map `I`/`l`→`1`, `O`→`0`) so a
   hand-typed token that got those wrong still authenticates.
3. **Spottable** - the `LMD-` prefix is a human affordance for finding the
   string in scrollback, not a checksum; it's stripped before comparison and
   not required.

Comparison is `crypto/subtle.ConstantTimeCompare` on the normalized value;
missing or bad → `401`. Storage is `~/.aida/lmd/token`, mode `0600`, generated
on first start and printed once; `aida lmd token --rotate` invalidates the old
value on the next request.

Threat-model honesty: this is plain HTTP carrying a replayable bearer
credential. It's acceptable because every byte rides inside Tailscale's
WireGuard tunnel and the tailnet is a single-owner device set - anything
already on the tailnet is already trusted hardware. The same design would be
wrong on a shared network.

## The endpoints

**`GET /lmd/v1/health`** - deliberately unauthenticated. The client must be
able to distinguish "can't reach the host" (show the Tailscale screen) from
"reachable but my token is wrong" (show a credentials error); it leaks only
that aida is running. The response also lists the daemon's live personas so
the app's voice selector never hardcodes a list that drifts.

**`POST /lmd/v1/turn`** - one complete voice turn: `audio/wav` in, JSON out
(`transcript`, `reply`, base64 `audio`, `audio_format`, `took_ms`). The body
is 16 kHz / mono / 16-bit PCM WAV - exactly what Android's `AudioRecord`
produces and exactly what the whisper wrapper requires, so *no transcoding
happens on either side*. Capped at 10 MB via `http.MaxBytesReader`.
`audio_format` is `mp3` for ElevenLabs, `wav` for Piper - the client must not
assume. Base64-in-JSON costs ~33% overhead, which is irrelevant over a tailnet
and keeps the client to a single serialization decode.

**`GET /lmd/v1/ack?persona=…`** - the persona's pre-synthesized "One moment,
sir." clip, fetched once and cached by the client.

## Cheap guards before expensive work

Two gates run before any money or time is spent, both born from real failures:

- **RMS silence gate**: the uploaded clip's PCM is decoded and its RMS checked
  against a conservative silence threshold *before whisper is even spawned*.
  Below it → `422` without transcribing. The original bug: 3 s of pure digital
  silence from a stray button press got whisper-transcribed as the bare word
  "you", slipped past the old filter, and burned a full LLM + TTS round trip.
- **Hallucination / empty-transcript gate**: a blank transcript, or one
  matching the shared whisper-hallucination filters (`[BLANK_AUDIO]`,
  `(upbeat music)`, bare "you"/"thank you"/"um" matched on the whole
  normalized transcript) → `422` without invoking the LLM.

The error contract is written from the client's chair: `401` don't retry,
`413` show "too long", `422` brief "didn't catch that", `500` keep the
previous text and toast. **Text updates only on a 200** - a failed turn must
never blank out what the assistant last said.

## Whose speakers?

The desk daemon masks slow tools by playing an ack *locally* - which for a
phone turn is a bug: the phone's conversation would come out of the laptop's
speakers in whatever room it happens to be in. So playback is suppressed
**per call**, not via a global flag (the same process still runs the desk-mic
listener, which must keep speaking).

Suppression alone leaves the phone silent for the 8–15 s an engine query
takes, and the one-shot protocol has no way to push "I just started a slow
tool" mid-turn. The compromise: the client fetches the ack clip once per
persona and plays it **locally after 5 s of waiting** - chosen against a
measured ~4 s normal turn so it never fires on a fast reply, and only speaks
up right when a human would start wondering if it broke. The ack follows the
originating device instead of being hardcoded to the daemon's speakers.

## Known gaps (spike-grade honesty)

Recorded in the spec so a future reader doesn't trust it past its weight:

- Session state races under concurrent same-session turns (unreachable with
  one phone and one button; real the day a second client shares a session).
- The 5 s ack threshold is a guess; the honest fix is server-pushed progress
  (SSE/WebSocket), deliberately deferred as a real protocol change.
- A TTS failure discards a complete reply - synthesis runs last, so a provider
  outage throws away transcript, LLM response, and tool work. Falling back to
  the local Piper engine would preserve the turn in a lesser voice; this has
  bitten in real use.
- Upstream TTS error text is returned verbatim - fine on a single-owner
  tailnet, too chatty for anything wider.

## The client state machine

Simple by design: health poll every 5 s while foregrounded (the app is
unusable off-tailnet *by design*, so there is no offline queue); press →
record → release → send; recording hard-capped at 30 s, and hitting the cap
sends what it has rather than discarding - a stuck button degrades into a
normal turn.
