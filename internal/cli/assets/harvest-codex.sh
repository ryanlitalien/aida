#!/usr/bin/env bash
# Aida memory-bridge harvest hook (Codex Stop).
# Distills new/changed Codex sessions into Aida's brain. Fire-and-forget:
# it must never block or fail the Codex turn that triggered it.
#
# Installed by `aida setup`. All parsing/watermark/distillation logic
# lives in Go (`aida brain harvest --tool codex`); this script only
# drains the hook's stdin payload and fires the harvest off, fully
# detached.
set +e
LOG="$HOME/.aida/logs/codex-harvest.log"
mkdir -p "$HOME/.aida/logs" 2>/dev/null

# Drain the hook's stdin payload -- harvest reads session state from
# disk, not from this payload -- then run fully detached: a background
# subshell so the child is reparented and survives this hook's process
# group exiting, nohup for SIGHUP immunity.
cat >/dev/null
( nohup aida brain harvest --tool codex >>"$LOG" 2>&1 & ) >/dev/null 2>&1
exit 0
