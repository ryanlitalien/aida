#!/usr/bin/env bash
#
# run-ab2.sh — ElevenLabs TTS A/B for aida issue #120, second-generation harness.
#
# Extends ~/Desktop/aida-voice-tests-2026-07-20/run-ab.sh with two changes that
# the decision actually needs:
#
#   1. SEPARATES the voice-quality question from the latency question.
#      Phase A renders the full arch-butler phrase once per (voice, model) so
#      the clips can be listened to side by side. Phase B measures TTFB with a
#      short, REALISTIC reply phrase repeated N times, because a single sample
#      per model is dominated by TCP/TLS jitter (~375ms of a measured 478ms,
#      per the issue #120 investigation) and would not survive scrutiny.
#
#   2. DROPS eleven_v3. It bills at the same 1 credit/char as the current
#      baseline and ElevenLabs' docs say it "is not made for real-time
#      applications", so it cannot win on either the cheaper or the faster
#      axis. Testing it would spend ~1120 credits to confirm that.
#
#   3. ADDS optimize_streaming_latency as a variant, the orthogonal lever
#      noted in the issue: internal/jarvis/tts/elevenlabs.go currently sends
#      NO query params on the stream endpoint at all.
#
# Voice settings mirror production: internal/jarvis/config.go DefaultConfig()
# for Jarvis, internal/cli/jarvis_ptt.go aidaPersonaConfig() for Aida.
#
set -uo pipefail
# Deliberately NOT `set -e`: one failing pair must not abort the matrix.

# Defaults to this script's own directory, so a re-run refreshes the committed
# clips and CSVs in place and `git diff` shows what moved since last time.
# Override with OUT_DIR=... to scratch somewhere else instead.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="${OUT_DIR:-$SCRIPT_DIR}"
CLIPS_DIR="$OUT_DIR/clips"
LAT_CSV="$OUT_DIR/latency.csv"
CLIPS_CSV="$OUT_DIR/clips.csv"
REPS="${REPS:-5}"

mkdir -p "$CLIPS_DIR"

# --- key resolution: env, then ~/.aida/secrets.env, then 1Password. ---------
# Never echoed, never traced. No `set -x`, no curl -v anywhere in this file.
resolve_api_key() {
  if [[ -n "${ELEVENLABS_API_KEY:-}" ]]; then printf '%s' "$ELEVENLABS_API_KEY"; return 0; fi
  local k
  if [[ -r "$HOME/.aida/secrets.env" ]]; then
    k="$(grep -m1 '^ELEVENLABS_API_KEY=' "$HOME/.aida/secrets.env" | cut -d= -f2- | tr -d '"'"'"' ')"
    if [[ -n "${k:-}" ]]; then printf '%s' "$k"; return 0; fi
  fi
  if command -v op >/dev/null 2>&1; then
    k="$(op read 'op://Vault/ElevenLabs API - jarvis/credential' 2>/dev/null)"
    if [[ -n "${k:-}" ]]; then printf '%s' "$k"; return 0; fi
  fi
  echo "ERROR: no ElevenLabs API key from \$ELEVENLABS_API_KEY, ~/.aida/secrets.env, or 'op'." >&2
  return 1
}
API_KEY="$(resolve_api_key)" || exit 1

quota() {
  curl -sS --max-time 15 -H "xi-api-key: ${API_KEY}" \
    https://api.elevenlabs.io/v1/user/subscription 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d.get("character_limit",0)-d.get("character_count",0))' 2>/dev/null
}

QUOTA_BEFORE="$(quota)"
echo "Quota remaining before run: ${QUOTA_BEFORE:-unknown} characters"
echo ""

# --- voices, with real production voice_settings ----------------------------
set_voice_vars() {
  case "$1" in
    jarvis)  # internal/jarvis/config.go DefaultConfig()
      VOICE_ID="mfGn240ErL2HMopgBro1"; STABILITY="1.0"; SIMILARITY="1.0"; STYLE="0.05"; SPEED="0.85" ;;
    aida)    # internal/cli/jarvis_ptt.go aidaPersonaConfig()
      VOICE_ID="Lopz0RdZlxPXALj2qGr3"; STABILITY="0.40"; SIMILARITY="1.0"; STYLE="0.40"; SPEED="1.0" ;;
    *) echo "ERROR: unknown voice '$1'" >&2; return 1 ;;
  esac
}

json_escape() {
  local s=$1
  s=${s//\\/\\\\}; s=${s//\"/\\\"}; s=${s//$'\n'/\\n}; s=${s//$'\r'/}; s=${s//$'\t'/\\t}
  printf '%s' "$s"
}

build_body() {
  local model="$1" text="$2" esc
  esc="$(json_escape "$text")"
  printf '{"text": "%s", "model_id": "%s", "voice_settings": {"stability": %s, "similarity_boost": %s, "style": %s, "speed": %s}}' \
    "$esc" "$model" "$STABILITY" "$SIMILARITY" "$STYLE" "$SPEED"
}

# one request; echoes "ttfb_ms total_ms bytes http_code"
tts_request() {
  local model="$1" text="$2" outfile="$3" qs="${4:-}"
  local url="https://api.elevenlabs.io/v1/text-to-speech/${VOICE_ID}/stream"
  [[ -n "$qs" ]] && url="${url}?${qs}"
  local out
  out="$(curl -sS --max-time 60 -X POST "$url" \
    -H "xi-api-key: ${API_KEY}" \
    -H "Content-Type: application/json" \
    -H "Accept: audio/mpeg" \
    -d "$(build_body "$model" "$text")" \
    -o "$outfile" \
    -w '%{time_starttransfer} %{time_total} %{size_download} %{http_code}')" || return 1
  local ttfb total bytes code
  read -r ttfb total bytes code <<< "$out"
  printf '%.0f %.0f %s %s' \
    "$(awk -v t="$ttfb" 'BEGIN{print t*1000}')" \
    "$(awk -v t="$total" 'BEGIN{print t*1000}')" \
    "$bytes" "$code"
}

MODELS=(eleven_multilingual_v2 eleven_flash_v2_5 eleven_turbo_v2_5)

# ===========================================================================
# PHASE A — full-length clips, for judging whether the voice still sounds
# like itself. This is the "same voices" question, and it is decided by ear,
# not by a number.
# ===========================================================================
PHRASE_LONG="Good evening, sir. I have reviewed your calendar, your inbox, and your general life choices this week, and all three require intervention. The nine o'clock meeting has quietly moved to eight, on the theory that punctuality remains fashionable in at least one household. I have also muted several colleagues who believe reply-all is a personality trait rather than a mistake. Coffee is ready, and the day has, against considerable odds, been kept from collapsing before breakfast. Shall I proceed with the briefing, or would you prefer to face this morning unassisted?"

echo "=== PHASE A: full clips (${#PHRASE_LONG} chars) ==="
echo "voice,model,http_code,ttfb_ms,total_ms,bytes,file" > "$CLIPS_CSV"
for voice in jarvis aida; do
  set_voice_vars "$voice"
  for model in "${MODELS[@]}"; do
    f="$CLIPS_DIR/${voice}-${model}.mp3"
    echo "==> ${voice} / ${model}"
    if r="$(tts_request "$model" "$PHRASE_LONG" "$f")"; then
      read -r ttfb total bytes code <<< "$r"
      if [[ "$code" == "200" ]]; then
        echo "    TTFB ${ttfb}ms | total ${total}ms | ${bytes} bytes"
        echo "${voice},${model},${code},${ttfb},${total},${bytes},${f}" >> "$CLIPS_CSV"
      else
        echo "    HTTP ${code} — error body left at ${f}" >&2
        echo "${voice},${model},${code},,,,FAILED" >> "$CLIPS_CSV"
      fi
    else
      echo "    curl failed" >&2
      echo "${voice},${model},curl_error,,,,FAILED" >> "$CLIPS_CSV"
    fi
  done
done

# ===========================================================================
# PHASE B — TTFB reps on a short, realistic reply. Short phrase keeps credit
# spend near zero while still measuring the thing that matters: time to the
# FIRST byte of audio, which is what the user experiences as dead air.
# The 4th config probes optimize_streaming_latency, which production does
# not currently send at all.
# ===========================================================================
PHRASE_SHORT="The nine o'clock meeting has moved to eight, sir."

echo ""
echo "=== PHASE B: TTFB x ${REPS} reps (${#PHRASE_SHORT} chars each) ==="
echo "config,model,query_params,rep,http_code,ttfb_ms,total_ms,bytes" > "$LAT_CSV"
set_voice_vars jarvis

run_lat() {
  local label="$1" model="$2" qs="$3" i r ttfb total bytes code
  for i in $(seq 1 "$REPS"); do
    if r="$(tts_request "$model" "$PHRASE_SHORT" /dev/null "$qs")"; then
      read -r ttfb total bytes code <<< "$r"
      echo "    ${label} rep${i}: ${ttfb}ms (http ${code})"
      echo "${label},${model},${qs},${i},${code},${ttfb},${total},${bytes}" >> "$LAT_CSV"
    else
      echo "${label},${model},${qs},${i},curl_error,,," >> "$LAT_CSV"
    fi
  done
}

run_lat "multilingual_v2 (current)" eleven_multilingual_v2 ""
run_lat "flash_v2_5"                eleven_flash_v2_5      ""
run_lat "turbo_v2_5"                eleven_turbo_v2_5      ""
run_lat "flash_v2_5 + osl=3"        eleven_flash_v2_5      "optimize_streaming_latency=3"

QUOTA_AFTER="$(quota)"
echo ""
echo "Quota remaining after run: ${QUOTA_AFTER:-unknown}"
if [[ -n "${QUOTA_BEFORE:-}" && -n "${QUOTA_AFTER:-}" ]]; then
  echo "Credits spent this run: $(( QUOTA_BEFORE - QUOTA_AFTER ))"
fi
echo ""
echo "Clips:   $CLIPS_DIR"
echo "Latency: $LAT_CSV"
