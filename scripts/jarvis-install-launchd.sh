#!/usr/bin/env bash
# scripts/jarvis-install-launchd.sh - install/uninstall a LaunchAgent that
# runs `aida serve` at every login. Idempotent: re-running re-installs.
#
# Usage:
#   ./scripts/jarvis-install-launchd.sh install
#   ./scripts/jarvis-install-launchd.sh uninstall
#   ./scripts/jarvis-install-launchd.sh status

set -euo pipefail

LABEL="com.ryanlitalien.aida"
TEMPLATE="$(cd "$(dirname "$0")" && pwd)/launchd/${LABEL}.plist"
TARGET="$HOME/Library/LaunchAgents/${LABEL}.plist"

say()  { printf '\033[36m▸\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

cmd="${1:-status}"

case "$cmd" in
install)
  [[ -f "$TEMPLATE" ]] || die "template not found at $TEMPLATE"
  AIDA_PATH="$(command -v aida 2>/dev/null || true)"
  [[ -n "$AIDA_PATH" ]] || die "aida not on PATH - run 'make install' first"

  mkdir -p "$(dirname "$TARGET")" "$HOME/.aida/jarvis"

  say "rendering $LABEL.plist with aida=$AIDA_PATH"
  sed -e "s|@AIDA_PATH@|$AIDA_PATH|g" -e "s|@HOME@|$HOME|g" "$TEMPLATE" > "$TARGET"

  # If already loaded, bootout first (idempotent re-install).
  if launchctl list "$LABEL" >/dev/null 2>&1; then
    say "unloading existing instance"
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  fi

  say "loading"
  launchctl bootstrap "gui/$(id -u)" "$TARGET"
  launchctl enable "gui/$(id -u)/$LABEL"
  ok "installed and started"
  echo "   plist:  $TARGET"
  echo "   logs:   $HOME/.aida/jarvis/launchd.{out,err}.log"
  echo "   verify: launchctl list $LABEL"
  ;;

uninstall)
  if launchctl list "$LABEL" >/dev/null 2>&1; then
    say "unloading"
    launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
  fi
  if [[ -f "$TARGET" ]]; then
    rm "$TARGET"
    ok "removed $TARGET"
  else
    ok "no plist installed"
  fi
  ;;

status)
  if [[ -f "$TARGET" ]]; then
    ok "plist installed at $TARGET"
  else
    warn "no plist installed"
  fi
  if launchctl list "$LABEL" 2>/dev/null; then
    :
  else
    warn "not loaded in launchd"
  fi
  ;;

*)
  die "usage: $0 {install|uninstall|status}"
  ;;
esac
