# ElevenLabs TTS model A/B

Measurements behind the per-persona TTS model split: **Aida runs
`eleven_turbo_v2_5`, Jarvis stays on `eleven_multilingual_v2`.**

Run 2026-07-21 against the live ElevenLabs streaming endpoint, resolving
[issue #120](https://github.com/ryanlitalien/aida/issues/120). 26 requests,
1,436 characters of quota.

| File | What it is |
|---|---|
| `report.html` | The comparison page. Open it in a browser - it embeds the clip players. |
| `run-ab.sh` | The harness. Re-runnable; writes back into this directory. |
| `latency.csv` | Phase B: 5 TTFB samples per option, short reply. |
| `clips.csv` | Phase A: one full-length render per (voice, model). |
| `clips/*.mp3` | The audio, as the API returned it (MP3 is the native format). |

The `clips/*.mp3` files themselves were removed from the public tree (ElevenLabs redistribution terms weren't worth the review); `run-ab.sh` regenerates them locally against your own API key.

## What was measured

Time-to-first-audio (TTFB), not total synthesis time - TTFB is what the user
experiences as dead air. Both personas' real production `voice_settings` were
used, mirroring `internal/jarvis/config.go` (Jarvis) and
`internal/cli/jarvis_ptt.go` (Aida).

| Option | Mean | Worst of 5 | Std dev | Long reply | Credit/char |
|---|---|---|---|---|---|
| `eleven_multilingual_v2` | 827ms | 877ms | 37ms | ~2020ms | 1.0 |
| `eleven_flash_v2_5` | 379ms | 596ms | 162ms | ~416ms | 0.5 |
| **`eleven_turbo_v2_5`** | **309ms** | **337ms** | **31ms** | **~432ms** | **0.5** |
| `eleven_flash_v2_5` + `osl=3` | 316ms | 491ms | 105ms | - | 0.5 |

## Three findings worth keeping

**1. Turbo beat Flash here, contrary to the vendor's own guidance.**
ElevenLabs' docs recommend Flash over Turbo "in all use cases". On this network
path Flash was slower on average (379ms vs 309ms) and far jitterier - standard
deviation 162ms against Turbo's 31ms, with one run spiking to 596ms. Perceived
latency tracks the worst case, not the mean. Re-measure rather than trusting
published latency figures; the vendor's ~75ms figure is model inference only,
while roughly 375ms of any real TTFB here is TCP/TLS setup that no model choice
removes.

**2. `multilingual_v2`'s TTFB scales with reply length; Flash and Turbo's do
not.** A 49-character reply costs it 827ms, a 567-character paragraph costs it
~2020ms. Turbo sits at ~310-430ms across both. So the win is largest exactly
where the delay was worst: briefings and `aida_query` results.

**3. The model is the engine, not the voice.** Voice IDs are unchanged across
every clip, so the risk of switching was never a *different* voice - only a
flatter one, since ElevenLabs concedes emotional nuance is Flash and Turbo's
weak point.

## Why the personas differ

That last point is why the decision splits:

- **Jarvis stays on `multilingual_v2`.** His voice is an IVC replica built from
  MCU lines. Turbo and Flash re-render it into an audibly different person, so
  the faster/cheaper models are not available to him at any price.
- **Aida moves to `turbo_v2_5`.** She was generated inside ElevenLabs via Voice
  Design, and Turbo reproduces her essentially unchanged - despite her persona
  riding on low stability (0.40) for inflection, which was the predicted risk.

Judged by ear from `clips/`; the numbers cannot settle it. `TestPersonasUseDifferentTTSModels`
in `internal/cli/jarvis_ptt_test.go` pins the split so a future config-unifying
refactor fails loudly instead of silently re-voicing Jarvis.

## Caveats

- **n=5 per option.** Enough to separate 827ms from 309ms, and enough to expose
  Flash's variance. **Not** enough to call Turbo's 309ms different from
  flash+`osl`'s 316ms - those two are a tie on this evidence.
- **One machine, one session, one network path.** Re-run before generalising.
- **`eleven_v3` was excluded, not tested.** It bills at the same 1 credit/char
  as `multilingual_v2` and ElevenLabs states it is not built for real-time use,
  so it cannot win on either axis. Ruled out on cost and latency, not quality.
- **`output_format` left at default.** Dropping the bitrate would shave more off
  TTFB but trades audio quality, cutting against keeping the voices intact.
- **`optimize_streaming_latency` is still unsent in production.** It measured as
  a wash against plain Turbo, so it was not adopted - but it remains an untouched
  lever in `internal/jarvis/tts/elevenlabs.go`, which sends no query params at all.

## Re-running

```bash
docs/tts-model-ab/run-ab.sh          # refreshes clips + CSVs in place
REPS=10 docs/tts-model-ab/run-ab.sh  # more samples per option
```

Costs real credits (~1.4k characters per run). The key resolves from
`$ELEVENLABS_API_KEY`, then `~/.aida/secrets.env`, then 1Password; it is never
printed. The script prints remaining quota before and after.
