#!/usr/bin/env bash
# Deploy the latest aida + LaunchAgent to the photon Jarvis satellite and
# reload the listener. One command to "update photon".
#   usage: ~/dev/aida/scripts/jarvis/deploy.sh
set -euo pipefail

# Status: the rename deploy (old assistant binaries -> aida) to photon COMPLETED 2026-07-20.
# photon runs ~/.local/bin/aida with config in ~/.aida; the old
# binaries are gone.
#
# Cast is NOT in mainline. It merged as 03333e4 (2026-07-06) and was
# reverted 20 minutes later by d297b06, so there is no internal/jarvis/cast
# package and no --cast-device flag. The satellite plist must NOT pass
# --cast-device; aida crash-loops on the unknown flag. Casting is disabled
# and replies play locally.
# Note: GitHub still shows PR #79 as "merged" - that is misleading. The
# merge commit is in history but its content was reverted.

REPO="$HOME/dev/aida"
HOST=photon
LABEL=com.ryan.jarvis-satellite
PLIST="$REPO/scripts/jarvis/$LABEL.plist"

echo "▶ building aida (arm64)…"
( cd "$REPO" && make build >/dev/null )

# Sign with a real Developer ID, matching the repo Makefile. An ad-hoc
# signature's designated requirement is a raw cdhash that changes on every
# build, so macOS TCC treats each deploy as a new app and silently drops the
# satellite's mic grant. A Developer ID signature is stable, so the grant
# survives rebuilds.
CODESIGN_ID="Developer ID Application: Ryan L'Italien (92Y3P4P8RZ)"
CODESIGN_IDENTIFIER="com.ryanlitalien.aida"
if security find-identity -v -p codesigning | grep -qF "$CODESIGN_ID"; then
  echo "▶ signing aida ($CODESIGN_IDENTIFIER, Developer ID)…"
  codesign -s "$CODESIGN_ID" --force --identifier "$CODESIGN_IDENTIFIER" "$REPO/bin/aida"
else
  echo "⚠ codesigning identity not found: $CODESIGN_ID"
  echo "⚠ shipping unsigned - photon's mic (TCC) grant will likely need re-granting."
fi

echo "▶ pushing aida + LaunchAgent…"
scp -q "$REPO/bin/aida" "$HOST:.local/bin/aida"
# The plist ships with __HOME__ placeholders (no personal paths in the repo);
# render it against the remote user's real home before installing.
REMOTE_HOME="$(ssh "$HOST" 'printf %s "$HOME"')"
RENDERED="$(mktemp)"
sed "s|__HOME__|$REMOTE_HOME|g" "$PLIST" > "$RENDERED"
scp -q "$RENDERED" "$HOST:Library/LaunchAgents/$LABEL.plist"
rm -f "$RENDERED"

echo "▶ reloading listener…"
# launchctl bootout is async, so bootstrap immediately after races (EIO).
# Explicitly pkill any lingering daemon/caffeinate procs before bootstrapping
# so the new instance starts cleanly every time.
ssh "$HOST" "uid=\$(id -u)
  launchctl bootout gui/\$uid/$LABEL 2>/dev/null || true
  pkill -f 'aida jarvis daemon' 2>/dev/null || true
  pkill -f 'caffeinate -i .*/.local/bin/aida' 2>/dev/null || true
  launchctl bootstrap gui/\$uid ~/Library/LaunchAgents/$LABEL.plist
  launchctl list | grep jarvis || echo '(not listed)'"

echo "✓ deployed. tail logs:  ssh $HOST tail -f ~/Library/Logs/jarvis-satellite.err.log"
