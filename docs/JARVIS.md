# JARVIS - Voice Assistant Layer

Always-on voice assistant for aida. Modeled on movie-Jarvis: native Go,
Mac-native audio, zero Python in the runtime path. Lives in
`internal/jarvis/`.

```
mic ── ffmpeg ──► amplitude VAD ──► whisper.cpp tiny.en (Metal/CoreML)
                                          │
                                          ▼
                                  wake-phrase regex
                                          │  (matches "ok jarvis", etc.)
                                          ▼
                              Claude Haiku + tool-use loop
                                  │  ┌────────────────────────┐
                                  │  │ in-process tools:      │
                                  ├──┤   tasks_list           │
                                  │  │   task_get             │
                                  │  │   current_time         │
                                  │  │   weather (wttr.in)    │
                                  │  │   aida_query         │
                                  │  └────────────────────────┘
                                          │
                                          ▼
                              Piper TTS (en_GB-jarvis-high.onnx)
                                          │
                                          ▼
                                       afplay → speakers
```

## One-time setup (macOS)

```bash
git clone https://github.com/ryanlitalien/aida.git
cd aida
make install                  # builds + installs `aida`
make jarvis-deps              # see "What jarvis-deps does" below
aida jarvis greet               # foreground; triggers macOS mic permission prompt
aida serve                      # always-on daemon (port 1610)
```

### What `jarvis-deps` does

Idempotent - safe to re-run on any machine.

1. Installs `ffmpeg`, `whisper-cpp`, `cmake` via Homebrew
2. Clones `rhasspy/piper`, builds the C++ binary from source (~3 min)
3. Vendors into `~/.aida/jarvis/bin/`:
   - `piper` - native Mach-O arm64 executable (~2.5 MB)
   - `libonnxruntime`, `libpiper_phonemize`, `libespeak-ng` dylibs
   - `espeak-ng-data/`
   - `install_name_tool -add_rpath @loader_path` is applied so the binary
     finds its dylibs without environment variables
4. Downloads voice + STT models into `~/.aida/jarvis/models/`:
   - `ggml-tiny.en.bin` (~75 MB)
   - `jarvis-high.onnx` (~114 MB, MIT-licensed `jgkawell/jarvis`)
   - `jarvis-high.onnx.json`
5. Smoke-test synthesis to confirm the pipeline works

Models, vendored binaries, and the audit log are intentionally **not**
git-tracked - they're per-machine artifacts the script reproduces from
upstream sources.

### Autostart at login (optional)

```bash
make jarvis-install-launchd       # installs ~/Library/LaunchAgents/com.ryanlitalien.aida.plist
make jarvis-status-launchd        # show plist + launchctl status
make jarvis-uninstall-launchd     # remove
```

The LaunchAgent runs `aida serve --http` at every login, restarts on crash
(not on clean exit), and writes logs to `~/.aida/jarvis/launchd.{out,err}.log`.

**TCC mic permission caveat:** the very first time anything reads the
mic, macOS shows a TCC consent dialog that **must come from a foreground
GUI launch** - launchd-spawned processes get silently denied. Run
`aida jarvis greet` from a terminal once after fresh setup; subsequent
launchd starts inherit the granted permission.

## CLI surface

| Command | Mode |
|---|---|
| `aida serve` | Canonical daemon: HTTP on `127.0.0.1:1610` + voice listener (interactive shell) |
| `aida serve --no-listen` | HTTP only (no mic) |
| `aida serve --no-jarvis` | Bare HTTP (no `/jarvis/*` routes) |
| `aida serve` (stdin pipe) | Auto-switches to stdio MCP - Claude Code spawns it this way |
| `aida jarvis ask "..."` | Typed query → spoken reply (no mic) |
| `aida jarvis listen [--seconds N]` | Push-to-talk: record → transcribe → reply |
| `aida jarvis daemon [--verbose]` | Listener-only, no HTTP |
| `aida jarvis greet` | Speak the configured greeting (TTS smoke test, mic prompt) |
| `aida tasks web` | Open `http://localhost:1610/tasks` in default browser |
| `aida tasks web --standalone` | Legacy: random-port ephemeral server (pre-daemon behavior) |

## HTTP surface (port 1610 - Earth-1610)

| Path | Verb | Purpose |
|---|---|---|
| `/` | GET | Jarvis status panel (HTML) |
| `/jarvis/ask` | POST | **Disabled** - always returns `410 Gone`. Was an unauthenticated loopback route reachable via CSRF from any site the user visits; use `aida jarvis ask` (in-process) instead. |
| `/jarvis/health` | GET | `{"ok":true, "uptime":"...", "version":"jarvis-v1"}` |
| `/tasks` | GET | Tasks UI (HTML, also `/tasks/`) |
| `/api/tasks` | GET | List tasks JSON |
| `/api/tasks/{slug}/body` | GET | Task body markdown |
| `/api/tasks/{slug}/status` | POST | Set task status |
| `/api/tasks/{slug}/priority` | POST | Set priority |
| `/api/tasks/{slug}/draft` | POST | Queue an agent draft |
| `/api/runs/{id}/stream` | GET (SSE) | Stream agent run output |
| `/api/runs/{id}/output` | GET | Final agent run output |

## Wake phrases

Regex-matched, punctuation-tolerant. Whisper's text variations all fire:

- `ok jarvis` · `okay jarvis` · `hey jarvis`
- `good morning jarvis` · `good afternoon jarvis` · `good evening jarvis`

**Armed mode:** speaking just the wake phrase ("Ok Jarvis.") arms the next
utterance to be the query - so you can pause and think before asking.

Examples that fire:

```
"Hey Jarvis, what time is it?"
"Okay, Jarvis, tell me about task 162."
"Good morning Jarvis, what's on my schedule?"
"Ok jarvis." → (pause) → "what's the weather?"
```

## Voice tools (in-process, distinct from MCP tools)

These are NOT the MCP tools in `internal/mcp/server.go` - they live in
`internal/jarvis/tools/registry.go` and call `internal/brain` directly,
no MCP RPC overhead.

| Tool | Purpose | Latency |
|---|---|---|
| `tasks_list` | Priority-sorted task list with optional tag filter | ~200ms |
| `task_get` | Look up a single task by `#N` / slug / partial-slug | ~200ms |
| `tasks_add` | Create a task; echoes the new ID + title | ~300ms |
| `task_done` | Mark a task done; echoes the title for verification | ~300ms |
| `task_status` | Set any of the six statuses (open/in-progress/hold/deferred/done/closed) | ~300ms |
| `task_edit_tags` | Add/remove tags on a task | ~300ms |
| `day_summary` | Composite briefing: top tasks + time + weather | ~1.5s |
| `current_time` | Local time + date + timezone | instant |
| `weather` | Current conditions via wttr.in (free, no key); defaults to home | ~1.5s |
| `aida_query` | Delegate open-ended questions to `aida <query>` (web-search, codebase, partner data, …) | 5–15s |
| `minecraft_ask` | SSH'd `claude -p` against the basement Bedrock server | 5–10s |
| `mcp_find_tool` | Discover external MCP capabilities by query (Slack/Gmail/Notion/…) | ~300ms |
| `mcp_call_tool` | Invoke an MCP tool by `server` + `name` + `args`; echo-confirms the result | 1–5s |
| `job_start` | Spawn a background agent (`aida --agent --run-dir`); returns a run-id immediately | ~500ms |
| `job_status` | Get current state of one run (`#run-id` or task slug) | ~200ms |
| `job_list` | List active jobs filtered by state | ~200ms |
| `job_send_input` | Answer a paused agent's `ask_user` question | ~200ms |
| `job_cancel` | SIGTERM the agent and mark the job failed | ~300ms |

The system prompt steers Claude to prefer dedicated tools over
`aida_query` when the question fits one (weather, tasks, time). The
**"One moment, sir."** ack plays before slow tools (`aida_query`,
`minecraft_ask`, `mcp_call_tool`, `job_start`) to mask latency - it's
pre-synthesized at daemon startup so playback is instant.

### What you can say

Fast in-process (sub-second):
- *"Ok Jarvis, **add a task** to wire up the cron job."*
- *"Ok Jarvis, **mark task 162 done**."* → echoes the title.
- *"Ok Jarvis, **put task 175 on hold**."*
- *"Ok Jarvis, **add the `home` tag to task 200** and remove `work`."*
- *"Ok Jarvis, **what's my day look like?**"* → top tasks + time + weather.

MCP-backed mutations (Slack/Gmail/Calendar/Notion when configured as local stdio servers):
- *"Ok Jarvis, **search nytimes for stories about X**."*
- *"Ok Jarvis, **send a Slack message to Y saying ...**"* (works once a local Slack MCP server is configured; claude.ai web connectors remain out of reach without OAuth).

Background agents (the headline):
- *"Ok Jarvis, **continue working on PR 583**."* → spawns worktree, `gh pr checkout`, runs `aida --agent` detached. Replies *"Working on it, sir. Run id ..."*
- Walk away. When the agent hits a decision, it pauses (`awaiting_input`).
- Return, say *"Ok Jarvis."* → notifier drains pending updates: *"Sir, the agent on PR 583 is asking: should I rebase or merge?"* then the wake prompt.
- *"Ok Jarvis, **tell PR 583 to rebase onto main**."* → `job_send_input`.
- When done: next wake fires *"PR 583 is done, sir."* before the wake prompt.
- Other: *"how's PR 583 going?"*, *"what's running?"*, *"cancel run `<id>`"*.

⚠️ `aida_query` is safe **as a Jarvis tool** because Jarvis is the
top-level agent calling out to a fresh `aida` subprocess. Per the
`feedback_no_mcp_query_tool` memory, the same idea must NOT be exposed
as an MCP tool - sub-agents would recurse.

## Audit log

`~/.aida/brain/jarvis/audit.ndjson`. Lives under the brain git repo
so the auto-commit backs it up across machines (the legacy path
`~/.aida/jarvis/audit.ndjson` is migrated on first run). One JSON
line per real wake-triggered turn. Skipped utterances, VAD blips, and
whisper hallucinations (`[BLANK_AUDIO]`, `(upbeat music)`, etc.) are
NOT logged. The file is a flat NDJSON log, not vector-indexed - `aida`
semantic search does not retrieve from it.

```json
{
  "ts":"2026-05-10T17:55:43Z",
  "started_at":"2026-05-10T17:55:32Z",
  "transcript":"Hey Jarvis, what's the weather?",
  "query":"what's the weather?",
  "reply":"Partly cloudy and sixty-one degrees, sir...",
  "tool_calls":[{"name":"weather","took_ms":1500}],
  "llm_ms":1100,
  "tool_ms":1500,
  "tts_ms":900,
  "play_ms":5200,
  "took_ms":8700
}
```

Quick view:

```bash
jq -r '[.started_at, (.took_ms|tostring)+"ms", (.tool_calls // [] | map(.name+":"+(.took_ms|tostring)+"ms") | join(","))] | @tsv' \
  ~/.aida/brain/jarvis/audit.ndjson | tail -20
```

## Configuration

Defaults in code (`internal/jarvis/config.go`):

| Field | Default | Purpose |
|---|---|---|
| `WakeWord` | `"ok jarvis"` | Cosmetic; the regex hard-codes accepted variants |
| `Greeting` | `"Good morning, sir."` | Spoken on `aida jarvis greet` |
| `Home` | `"Example City, ST"` | Default location for `weather` and time-zone-relative queries |
| `Model` | `"claude-haiku-4-5"` | LLM via Anthropic SDK |
| `MaxTokens` | `512` | Cap reply length so TTS doesn't run forever |
| `Temp` | `0.7` | LLM temperature |
| `PiperBin` | `~/.aida/jarvis/bin/piper` | Vendored native binary |
| `PiperModel` | `jarvis-high.onnx` | Inside `ModelsDir` |
| `EspeakDataDir` | `~/.aida/jarvis/bin/espeak-ng-data` | espeak-ng phoneme data |
| `ModelsDir` | `~/.aida/jarvis/models` | All ONNX/whisper model files |

YAML loading from `~/.aida/jarvis.yaml` is wired into the struct shape
but not enabled yet - defaults ship sensibly.

### Mic selection (`serve.mic_prefer`)

The listener picks its input device by NAME, not avfoundation index
(indices shift as devices connect/disconnect). Configured per profile in
`~/.aida/config.yaml` under `serve:`:

- `mic_prefer`: priority-ordered list of device-name substrings
  (case-insensitive). The first connected match wins; no match falls
  back to the system default mic (`:0`).
- `mic_prefer_by_host`: per-machine override map. Keys are
  case-insensitive substrings of the machine's
  `scutil --get LocalHostName`; the first matching key's list wins, else
  `mic_prefer` applies. Needed because one profile syncs across several
  computers with different audio setups.

**Renaming a machine breaks its `mic_prefer_by_host` key.** Keys match
against the live hostname, so after a rename the old key silently stops
matching and the listener falls back to `mic_prefer`. Update the key in
`~/.aida/config.yaml` (aida-config repo) and restart `aida serve`. This
happened on 2026-07-19 when the clamshell MacBook Pro was renamed
LavaChicken -> edith; the config key was re-keyed to `edith` the same
day.

## Latency expectations

End-to-end voice round-trip = STT + LLM + tools + TTS synth + TTS playback.

| Query type | Typical total |
|---|---|
| `current_time` (in-process) | 2–4 s |
| `tasks_list` / `task_get` (in-process) | 2–4 s |
| `weather` (wttr.in HTTP) | 4–6 s |
| `aida_query` (full aida engine + web-search) | 8–15 s |

TTS playback (`play_ms`) often dominates total wall-clock for replies
longer than two sentences - that's just the time it takes to read the
words aloud at natural cadence.

## Troubleshooting

**`read mic: unexpected EOF`** on `aida serve` startup: TCC mic permission
denied. Run `aida jarvis greet` from a foreground terminal, accept the
prompt in System Settings → Privacy & Security → Microphone.

**Wake word not firing**: `aida jarvis daemon --verbose` prints the rolling
RMS so you can see what the VAD threshold is doing. Default gate is 300
(int16 amplitude). If your speech peaks well below 300, lower it with
`--rms-gate 150`. If whisper transcribes your wake but the regex misses,
paste the transcript line and we'll add a variant.

**`aida tasks web` shows "site can't be reached"**: `aida serve` isn't
running. Start it in another terminal.

**Piper says "audio file open failed" or similar**: re-run
`make jarvis-deps` - the build/vendor flow is idempotent and will fix a
broken layout.

**LaunchAgent loaded but no audio**: TCC mic permission must come from
a foreground process first. Run `aida jarvis greet` once interactively.

## Architecture notes

- **macOS-only** today. Audio I/O uses `ffmpeg avfoundation` for capture
  and `afplay` for playback. Linux/Windows would need swapping the audio
  layer.
- **No barge-in** in v1: while Jarvis is speaking, the mic capture loop
  still runs, but the playback path doesn't watch for cancellation
  mid-clip. Saying "stop" mid-reply doesn't interrupt. Adding this
  requires switching from `afplay` (one-shot) to `malgo` (callback-based).
- **No streaming TTS** in v1: each reply synthesizes to WAV, then plays.
  Streaming would let the first sentence start playing while the rest is
  still being generated.
- **MCP server is unaffected.** When Claude Code spawns `aida serve` over
  stdin/stdout, the binary auto-detects pipe mode and runs the existing
  stdio MCP server with the documented brain/tasks tools - no HTTP, no
  voice loop. The two modes never coexist in one process.

## Attribution

`internal/jarvis/ATTRIBUTION.md` lists upstream inspirations and
licenses:

- **GLaDOS** (`dnhkng/GLaDOS`, MIT) - conversation-loop architecture
- **rhasspy/piper** (MIT) - TTS engine source we build from
- **jgkawell/jarvis** (MIT) - Piper voice model finetuned for Jarvis
- **whisper.cpp** (MIT) - Apple-Silicon-accelerated STT
- **wttr.in** - free weather endpoint (no key required)
