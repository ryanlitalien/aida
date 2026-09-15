# Agent guide - JARVIS voice assistant

You are working on a Go monorepo whose voice-assistant layer (`internal/jarvis/`)
ships an always-on daemon called Jarvis. This file is the agent-focused
summary; for the full reference see [`docs/JARVIS.md`](../docs/JARVIS.md).

## What you need to know up front

- **`aida serve` is dual-mode.** Stdin TTY → HTTP daemon on port 1610 (Earth-1610)
  with the voice listener. Stdin pipe → stdio MCP server. Same binary, no
  flags needed; auto-detected by `os.Stdin.Stat() & ModeCharDevice`. The
  two modes never run together.
- **Voice tools are NOT the MCP tools.** They live in
  `internal/jarvis/tools/registry.go` and call `internal/brain` directly.
  Don't confuse them with the MCP tool surface in `internal/mcp/server.go`.
- **`aida_query` is OK as a Jarvis tool** because Jarvis is the
  top-level agent shelling out via subprocess. It is NOT OK as an MCP
  tool - sub-agents would recurse. See memory `feedback_no_mcp_query_tool`.
- **Audit log:** `~/.aida/brain/jarvis/audit.ndjson`. NDJSON, one line
  per real wake-triggered turn. Skipped utterances are filtered out.
  Lives under the brain repo so the auto-commit picks it up; not
  vector-indexed, so `aida` semantic search does not retrieve from it.
- **No Python at runtime.** Piper is built from source in
  `make jarvis-deps` and vendored as a native arm64 binary into
  `~/.aida/jarvis/bin/`.

## Wake phrases

Hard-coded regex in `internal/jarvis/listener/listener.go` `wakeRegex()`.
Punctuation-tolerant. Add variants there, not in config:

- `ok jarvis` · `okay jarvis` · `hey jarvis`
- `good morning jarvis` · `good afternoon jarvis` · `good evening jarvis`

## Surface map

| Layer | File | What |
|---|---|---|
| Entry struct | `internal/jarvis/jarvis.go` | `Assistant` - DI for LLM, TTS, brain |
| Config | `internal/jarvis/config.go` | All defaults (Home, Piper bin path, etc.) |
| Listener | `internal/jarvis/listener/listener.go` | Mic loop + VAD + wake match |
| Audio capture | `internal/jarvis/audio/{stream,record,wav}.go` | ffmpeg PCM stream + WAV writer |
| STT | `internal/jarvis/stt/whisper.go` | whisper-cli subprocess |
| LLM | `internal/jarvis/llm/client.go` | Anthropic SDK + tool-use loop + `OnToolUse` |
| Tool registry | `internal/jarvis/tools/registry.go` | tasks_{list,get,add,done,status,edit_tags} / day_summary / current_time / weather / aida_query / minecraft_ask / mcp_{find,call}_tool / job_{start,status,list,send_input,cancel} |
| MCP client | `internal/mcp/discovery.go` | `MCPDiscovery.ConnectAll/DiscoverTools/CallTool` - stdio + HTTP transports; reads `~/.claude.json`, `~/.claude/.mcp.json`, `./.mcp.json` |
| Jobs queue | `internal/jobs/` | SQLite-backed per-profile queue + `awaiting_input` state + Pause/Resume + NotifiedAt idempotency |
| Notifier | `internal/jarvis/notify/notify.go` | Queued utterances, drained at start of every wake-triggered turn |
| Background-agent watcher | `internal/cli/serve.go` `watchJobsForNotifications` | 2s poll of jobs store → enqueue voice notifications + worktree cleanup |
| TTS | `internal/jarvis/tts/{piper,play,synth}.go` | Vendored piper subprocess + afplay |
| HTTP routes | `internal/jarvis/server/server.go` | `/`, `/jarvis/ask` (disabled, 410), `/jarvis/health` |
| Audit log | `internal/jarvis/audit/audit.go` | NDJSON appender |
| Cobra wiring | `internal/cli/jarvis.go`, `internal/cli/serve.go` | All `aida jarvis ...` subcommands + dual-mode serve |
| Tasks UI mount | `internal/cli/tasks_web.go` | `registerTasksWebRoutesAt(mux, ..., "/tasks")` |
| Setup script | `scripts/jarvis-deps.sh` | Brew + cmake build + model downloads |
| LaunchAgent | `scripts/launchd/com.ryanlitalien.aida.plist` + `scripts/jarvis-install-launchd.sh` | Autostart at login |

## When to add what

| Need | Where |
|---|---|
| New voice-only behavior | `internal/jarvis/tools/registry.go` (in-process tool) - register in `New(...)` |
| New external-system mutation | Add a stdio MCP server in `~/.claude.json mcpServers`; Jarvis picks it up via `mcp_find_tool`/`mcp_call_tool` |
| New background-agent kind | `internal/jarvis/tools/jobs.go` `jobStartTool` `kind` switch + the agent prompt |
| New MCP behavior for Claude Code / Cursor | `internal/mcp/server.go` (separate surface - this is the MCP *server*, not the client) |
| New wake-phrase variant | `wakeRegex()` in `internal/jarvis/listener/listener.go` |
| New HTTP endpoint | `internal/jarvis/server/server.go` (Jarvis-scoped) or wire onto `mux` in `internal/cli/serve.go` (top-level) |
| New audit field | `internal/jarvis/audit/audit.go` `Record` struct + populate in `internal/jarvis/listener/listener.go` `askAndReply` |
| New manifest field | `internal/jobs/manifest.go` Manifest struct (omitempty) + db.go schema + idempotent `ALTER TABLE` migration |

## Hard rules

- Always run `make install` after Go changes (memory: `feedback_build_after_changes`).
- Test with real `aida` invocations; not just `go test` (memory: `feedback_test_with_hm`).
- For commit/PR messages: NEVER pass body inline; write to a temp file
  and use `git commit -F` / `gh pr edit --body-file` (CLAUDE.md global instruction).
- Don't expose `aida_query` as an MCP tool. Ever.
- macOS-only assumption. If you're touching audio I/O and contemplating
  Linux/Windows, surface that as a question first - current code uses
  `avfoundation` and `afplay` directly.

## Quick smoke tests after changes

```bash
make install
aida jarvis ask --text "what time is it"            # LLM + tool dispatch
aida jarvis greet                                    # TTS path + mic-permission grant
aida serve                                           # full daemon
aida jarvis ask --text "what are my tasks"           # /jarvis/ask is disabled (410); use this instead
tail -1 ~/.aida/brain/jarvis/audit.ndjson | jq  # confirm audit fields are populated
aida jobs list --state awaiting_input                # T3 paused-agent surface
```

## What the user can say

| Category | Example |
|---|---|
| Add a task | *"Ok Jarvis, add a task to wire up the cron job"* |
| Mark done | *"Ok Jarvis, mark task 162 done"* (echoes title for verification) |
| Change status | *"Ok Jarvis, put task 175 on hold"* |
| Edit tags | *"Ok Jarvis, add the `home` tag to task 200"* |
| Day briefing | *"Ok Jarvis, what's my day look like"* |
| MCP search | *"Ok Jarvis, search nytimes for stories about X"* |
| MCP send (when local Slack/Gmail MCP is configured) | *"Ok Jarvis, send a Slack message to Y saying ..."* |
| Background agent | *"Ok Jarvis, continue working on PR 583"* - replies with run-id, agent runs detached, notifier pings you on next wake when input is needed or job completes |
| Answer a paused agent | *"Ok Jarvis, tell PR 583 to rebase onto main"* |
| Check status | *"Ok Jarvis, what's running?"* / *"how's PR 583 going?"* |

Echo-confirm pattern: Jarvis reads back what was sent / changed so the user can verify aloud. No two-turn confirmation gate.

## Full reference

See [`docs/JARVIS.md`](../docs/JARVIS.md) - covers the full architecture
diagram, every CLI command, every HTTP route, latency expectations,
troubleshooting, and the attribution chain.
