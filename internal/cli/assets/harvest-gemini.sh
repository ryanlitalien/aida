#!/usr/bin/env bash
# Aida memory-bridge harvest hook (Gemini AfterAgent).
# Distills new/changed Gemini CLI sessions, Antigravity artifacts, and
# GEMINI.md into Aida's brain. Fire-and-forget: it must never block or
# fail the Gemini turn that triggered it.
#
# Installed by `aida setup`. All parsing/watermark/distillation logic
# lives in Go (`aida brain harvest --tool gemini`); this script only
# drains the hook's stdin payload and fires the harvest off, fully
# detached.
set +e
LOG="$HOME/.aida/logs/gemini-harvest.log"
mkdir -p "$HOME/.aida/logs" 2>/dev/null

cat >/dev/null
( nohup aida brain harvest --tool gemini >>"$LOG" 2>&1 & ) >/dev/null 2>&1
exit 0
