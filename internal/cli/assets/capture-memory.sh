#!/usr/bin/env bash
# Aida memory-bridge capture hook (Claude Code PostToolUse: Write|Edit).
# Mirrors a Claude Code memory write into Aida's brain. Fire-and-forget:
# it must never block or fail the tool call that triggered it.
#
# Installed by `aida setup`. All path/scope/parse logic lives in Go
# (`aida brain capture-hook`); this script only reads the hook JSON and
# hands it off, fully detached.
set +e
LOG="$HOME/.aida/logs/claude-capture.log"
mkdir -p "$HOME/.aida/logs" 2>/dev/null

# Read the PostToolUse JSON synchronously (it is tiny, so this is instant),
# then run the capture fully detached: a background subshell so the child
# is reparented and survives this hook's process group exiting, nohup for
# SIGHUP immunity, and the JSON handed in via a here-string.
payload=$(cat)
( nohup aida brain capture-hook >>"$LOG" 2>&1 <<<"$payload" & ) >/dev/null 2>&1
exit 0
