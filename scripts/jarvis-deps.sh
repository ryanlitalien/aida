#!/usr/bin/env bash
# scripts/jarvis-deps.sh - idempotent setup for the Jarvis voice layer.
#
# Run on a fresh macOS machine after cloning aida; safe to re-run any
# time. Installs system deps via Homebrew, ensures the Piper TTS CLI is
# available, and downloads model files to ~/.aida/jarvis/models/.
#
# Models / system deps are intentionally NOT git-tracked - too large, and
# different machines may need different builds. This script is the sync.

set -euo pipefail

MODELS_DIR="${AIDA_JARVIS_MODELS:-$HOME/.aida/jarvis/models}"
BIN_DIR="${AIDA_JARVIS_BIN:-$HOME/.aida/jarvis/bin}"
PIPER_REPO="https://github.com/rhasspy/piper.git"
PIPER_BUILD_DIR="${PIPER_BUILD_DIR:-/tmp/aida-piper-build}"
WHISPER_TINY_EN_URL="https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-tiny.en.bin"
WHISPER_SMALL_EN_URL="https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.en.bin"
PIPER_VOICE_BASE="https://huggingface.co/jgkawell/jarvis/resolve/main/en/en_GB/jarvis/high"

say() { printf '\033[36m▸\033[0m %s\n' "$*"; }
ok()  { printf '\033[32m✓\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m!\033[0m %s\n' "$*" >&2; }
die() { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

# ── 1. Sanity ─────────────────────────────────────────────────────────────────
[[ "$(uname -s)" == "Darwin" ]] || die "macOS only for now (uses avfoundation + afplay)."
command -v brew >/dev/null 2>&1 || die "Homebrew required. Install from https://brew.sh first."

# ── 2. Brew packages ──────────────────────────────────────────────────────────
# cmake is needed to build piper from source. ffmpeg + whisper-cpp are
# runtime deps for the voice pipeline.
need_brew=()
for pkg in ffmpeg whisper-cpp cmake; do
  if brew list --formula "$pkg" >/dev/null 2>&1; then
    ok "brew $pkg already installed"
  else
    need_brew+=("$pkg")
  fi
done
if [[ ${#need_brew[@]} -gt 0 ]]; then
  say "installing: ${need_brew[*]}"
  brew install "${need_brew[@]}"
fi

# ── 3. Piper TTS - built from source (no Python runtime dep) ──────────────────
# rhasspy/piper's 2023 release tarball was broken (missing dylibs) and the
# OHF-Voice fork is Python-only. We compile piper.cpp from source and
# vendor the binary + bundled dylibs + espeak-ng-data into BIN_DIR. The
# resulting daemon has zero Python in its runtime path.
mkdir -p "$BIN_DIR"

if [[ -x "$BIN_DIR/piper" ]] && [[ -d "$BIN_DIR/espeak-ng-data" ]]; then
  ok "piper binary already vendored at $BIN_DIR/piper"
else
  if [[ -d "$PIPER_BUILD_DIR/.git" ]]; then
    say "piper source already cloned at $PIPER_BUILD_DIR (re-using)"
  else
    say "cloning rhasspy/piper into $PIPER_BUILD_DIR"
    git clone --depth 1 --recurse-submodules "$PIPER_REPO" "$PIPER_BUILD_DIR"
  fi

  if [[ -x "$PIPER_BUILD_DIR/build/piper" ]]; then
    ok "piper already built"
  else
    say "configuring + building piper (cmake; ~3 min)"
    (cd "$PIPER_BUILD_DIR" && cmake -Bbuild -DCMAKE_INSTALL_PREFIX=install \
      -DCMAKE_POLICY_DEFAULT_CMP0135=NEW >/dev/null)
    (cd "$PIPER_BUILD_DIR" && cmake --build build --config Release >/dev/null 2>&1)
    [[ -x "$PIPER_BUILD_DIR/build/piper" ]] || die "piper build did not produce a binary"
  fi

  say "vendoring binary + dylibs + espeak-ng-data into $BIN_DIR"
  cp "$PIPER_BUILD_DIR/build/piper" "$BIN_DIR/piper"
  chmod +x "$BIN_DIR/piper"
  # Copy bundled dylibs (resolve symlinks → real files, then re-link)
  for lib in libonnxruntime libpiper_phonemize libespeak-ng; do
    for f in "$PIPER_BUILD_DIR/build/pi/lib/$lib"*.dylib; do
      [[ -f "$f" && ! -L "$f" ]] || continue
      cp "$f" "$BIN_DIR/$(basename "$f")"
    done
    # Recreate the symlinks (libfoo.dylib → libfoo.1.dylib) inside BIN_DIR
    (cd "$BIN_DIR" && for sym in "$PIPER_BUILD_DIR/build/pi/lib/$lib"*.dylib; do
      [[ -L "$sym" ]] || continue
      target="$(readlink "$sym")"
      ln -sf "$target" "$(basename "$sym")"
    done)
  done
  rm -rf "$BIN_DIR/espeak-ng-data"
  cp -R "$PIPER_BUILD_DIR/build/pi/share/espeak-ng-data" "$BIN_DIR/espeak-ng-data"

  # Add @loader_path to the binary's rpath list so it finds the bundled
  # dylibs without DYLD_LIBRARY_PATH every invocation. The original build
  # rpaths are absolute /tmp paths and meaningless after vendoring.
  if otool -l "$BIN_DIR/piper" | grep -q '@loader_path'; then
    :
  else
    install_name_tool -add_rpath @loader_path "$BIN_DIR/piper" 2>/dev/null || \
      warn "install_name_tool -add_rpath failed (will fall back to DYLD_LIBRARY_PATH)"
  fi

  ok "piper vendored: $BIN_DIR/piper ($(du -h "$BIN_DIR/piper" | cut -f1))"
fi

# Make sure piper is also on PATH (~/bin → BIN_DIR symlink, or install
# instructions). We do NOT modify PATH for the user - Jarvis invokes the
# vendored binary by absolute path via config.

# ── 4. Models ─────────────────────────────────────────────────────────────────
mkdir -p "$MODELS_DIR"

download_if_missing() {
  local url="$1" dst="$2"
  if [[ -f "$dst" ]]; then
    ok "$(basename "$dst") already present"
    return
  fi
  say "downloading $(basename "$dst")"
  curl -fsSL --retry 3 -o "$dst.tmp" "$url"
  mv "$dst.tmp" "$dst"
  ok "$(basename "$dst") ($(du -h "$dst" | cut -f1))"
}

download_if_missing "$WHISPER_TINY_EN_URL"               "$MODELS_DIR/ggml-tiny.en.bin"
download_if_missing "$WHISPER_SMALL_EN_URL"              "$MODELS_DIR/ggml-small.en.bin"
download_if_missing "$PIPER_VOICE_BASE/jarvis-high.onnx"        "$MODELS_DIR/jarvis-high.onnx"
download_if_missing "$PIPER_VOICE_BASE/jarvis-high.onnx.json"   "$MODELS_DIR/jarvis-high.onnx.json"

# ── 5. Smoke test ─────────────────────────────────────────────────────────────
say "verifying vendored piper synthesizes audio"
TEST_WAV="$(mktemp -t jarvis-deps-XXXX).wav"
if echo "Good morning, sir." | "$BIN_DIR/piper" \
    --model "$MODELS_DIR/jarvis-high.onnx" \
    --espeak_data "$BIN_DIR/espeak-ng-data" \
    --output_file "$TEST_WAV" 2>/dev/null; then
  ok "piper synthesized $(du -h "$TEST_WAV" | cut -f1) WAV - TTS pipeline working"
  rm -f "$TEST_WAV"
else
  warn "piper smoke test failed; check $BIN_DIR/piper manually"
fi

echo
ok "Jarvis deps installed."
echo "  Models: $MODELS_DIR"
echo "  Bins:   $BIN_DIR"
echo "  Next:   aida serve   (HTTP on 127.0.0.1:1610 + always-on listener)"
echo "          aida jarvis greet   (foreground - grants mic permission first time)"
