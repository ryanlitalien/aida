# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commit policy

Always make **granular commits** - one logical change per commit - and **never squash** (not on merge, not via `gh pr merge --squash`, not when rebasing). Every commit must stay independently revertible so history is bisectable and any single change can be backed out without unpicking unrelated work. When merging a PR, use a merge commit (`gh pr merge --merge`) that preserves the branch's individual commits. If a single file carries hunks for two distinct changes, split them across commits (e.g. stage one hunk via a patch and `git apply --cached`) rather than bundling.

Releases follow the same discipline. `main` is PR-only - nothing lands there except through a merged PR, no direct pushes. Every PR must add a `CHANGELOG.md` entry under the next version heading (`## vX.Y.0`, the latest `v1+` tag's minor plus one, or `v1.0.0` if no `v1+` tag exists yet); the `changelog` job in `.github/workflows/ci.yml` enforces this on every PR and fails the check if the entry is missing. Every merge to `main` then auto-tags that same next minor version and publishes a GitHub release using that changelog section as its release notes, via the `release` job in the same workflow. Because both jobs compute the next version from the same rule (factored into `.github/scripts/next-version.sh`), changing the versioning convention means changing the `changelog` and `release` jobs together - drift between them would let a PR pass its changelog check under one version while the release job tags a different one.

## Project Overview

Aida (`aida`) is a CLI-first "agent of agents" orchestration layer written in Go. It parses natural language questions and routes them to the right tools/sources (a SQL data warehouse, an observability backend, Notion, GitHub, local codebases) automatically, executing in parallel where possible and synthesizing grounded answers with citations.

For the architecture and design rationale, see `docs/notes/` and `docs/diagrams/`, and `HISTORY.md` for the development timeline.

## Build & Development Commands

```bash
make build          # Build binary to bin/aida
make install        # Build, sign, and install to ~/go/bin/aida with ~/bin/aida symlinked to it (on PATH)
make test           # Run all tests: go test ./...
make fmt            # Format code: go fmt ./...
make vet            # Static analysis: go vet ./...
make clean          # Remove bin/ directory
make build-all      # Cross-compile for darwin-arm64, darwin-amd64, linux-amd64
```

**`make install` is required to test via the `aida` command, not `make build`.** `make install` runs `go install ./cmd/aida` to `$(go env GOPATH)/bin/aida`, signs it, then symlinks `~/bin/aida` (first on PATH) to that one binary. `make build`/`go build ./...` only produce `bin/aida` inside the repo, which is never on PATH. After any code change, if you (or the user) are about to run `aida <cmd>` to try it out, `make install` first - otherwise it silently runs the old binary and the change looks like it didn't happen (missing subcommand, old behavior, etc.), which reads as a regression when it's actually just a stale install. `aida serve` uses bind-with-takeover, so rerunning it after `make install` kills and replaces the old daemon automatically.

### Codesigning and Input Monitoring

`make install` signs the binary with a stable Developer ID identity (overridable via `CODESIGN_ID` / `CODESIGN_IDENTIFIER`), not an adhoc signature. This matters because an adhoc signature's designated requirement is a raw cdhash that changes on every rebuild, which silently voids the macOS Input Monitoring grant and kills a USB HID push-to-talk button (`internal/jarvis/hid/`). The failure is silent - `hid.Open` still succeeds and the "push-to-talk ready" banner still prints, but no keyboard-page HID events are ever delivered.

After the first signed install, re-add the binary once in System Settings > Privacy & Security > Input Monitoring. With a stable identity, that grant sticks across rebuilds from then on. There is now one real binary at `$(go env GOPATH)/bin/aida`, with `~/bin/aida` as a symlink to it.

Run a single test:
```bash
go test ./internal/engine/ -run TestClassifier
go test ./internal/config/ -run TestConfigRoundTrip
go test ./internal/engine/ -run TestGoldenRouting   # post-Phase-2 golden tests
go test ./internal/cli/ -run TestExtractDirective   # Phase 3.1 feedback parser
```

Useful subcommands beyond `aida <query>`:

```bash
aida <query> --explain       # print planner scoring table per candidate source
aida <query> --dry-run       # show plan + routing without executing
aida lint                    # validate library sources, flag misconfigs (non-zero on error)
aida lint --strict           # also fail on warnings
aida index --generate        # scan + classify + MERGE into library (preserves manual edits)
aida index --generate --force  # overwrite everything (old behavior)
```

## `aida serve` is dual-mode

`aida serve` auto-detects how it was invoked and runs one of two modes:

- **Stdio MCP** (when stdin is a pipe) - Claude Code, Cursor, etc. spawn `aida serve` and connect over stdin/stdout. Exposes the JSON-RPC server documented below.
- **HTTP daemon** (when stdin is a TTY) - binds `127.0.0.1:1610` (Earth-1610), serves the tasks web UI, the Jarvis voice/HTTP surface, and an always-on mic listener. This is what you run interactively to leave open all day.

Flags: `--http` forces HTTP mode; `--no-listen` disables the mic; `--no-jarvis` disables the `/jarvis/*` routes. The two modes never run together in one process; an MCP client always gets a fresh subprocess.

`aida serve --loop` additionally hosts the autonomous-loop dispatcher (`aida loop --daemon`, see "Autonomous loop surface" below) in-process as a background goroutine, so tasks ingested from voice get worked continuously without a separate `aida loop` process running alongside. HTTP-mode-only (a no-op under stdio MCP); the daemon forces `--daemon` on internally regardless of the flags below, and validates the `--loop-*` combination up front so a bad config (e.g. `--loop-sandbox docker` without `--loop-worktree`) fails `aida serve` outright instead of dying silently in the goroutine. On shutdown (SIGINT/SIGTERM) the dispatcher gets a bounded 15s window to unwind in-flight worktrees/sandboxes before the HTTP server stops.

| `aida serve` flag | Mirrors `aida loop` flag | Meaning |
|---|---|---|
| `--loop` | (n/a) | enable the dispatcher; off by default |
| `--loop-tag` (repeatable) | `--tag` | only loop over tasks carrying ALL of these tags |
| `--loop-worktree` | `--worktree` | isolate each looped task in its own git worktree off `origin/main` |
| `--loop-pr` | `--pr` | open a PR per passing looped task instead of completing it directly (implies `--loop-worktree`) |
| `--loop-reviewer` | `--reviewer` | GitHub handle to request review from on looped PRs |
| `--loop-check` (repeatable) | `--check` | quality-gate command run after each looped attempt; must exit 0 |
| `--loop-concurrency` | `--concurrency` | work up to N looped tasks in parallel (requires `--loop-worktree`) |
| `--loop-sandbox` | `--sandbox` | confine looped agents: `none` \| `docker` (requires `--loop-worktree`) |
| `--loop-sandbox-memory` | `--sandbox-memory` | docker sandbox memory limit, e.g. `8g` |
| `--loop-provision-aida` | `--provision-aida` | prebuilt linux/arm64 `aida` for the docker sandbox (needed when serve runs outside the aida checkout) |

These mirror only the high-value standalone-loop flags; everything else (`--max-iterations`, `--max-fix-iterations`, `--recall-k`, `--per-task-timeout`, `--base`, `--review-panel`, `--max-budget-usd`, `--max-consecutive-failures`, `--poll`) takes the same defaults documented in the flag table under "Autonomous loop surface" below; run `aida loop` standalone for finer control over those.

`aida serve --harvest-sweep <duration>` (default `15m`, `0` disables) hosts a background goroutine that periodically runs the Codex/Gemini/Meetily harvest core for all three tools, sequentially - see "Multi-agent memory bridge" below for what it closes. HTTP-daemon-mode only, validated up front like the `--loop-*` flags (a negative duration fails `aida serve` outright), and stops within the same bounded 15s shutdown window as the loop dispatcher.

`registerFaviconRoutes` (`internal/cli/favicon_web.go`) serves `/favicon.ico`, `/favicon.svg`, `/icon/favicon-{32,180,512}.png`, `/apple-touch-icon.png`, and `/site.webmanifest` on the loopback mux, so every page - Jarvis, tasks, runs, dashboard, bifrost - shows the A.I.D.A. round launcher icon; the source SVG lives at `internal/cli/assets/icon/favicon.svg`, derived from aida-android's `ic_launcher_round`.

### Models panel + `aida models`

`~/.aida/models.yaml` (`config.Config.ModelsPath`) is a hand-maintained AI provider/plan/nickname roster: which providers you can use, on which plan/account, and what a spoken or typed nickname resolves to. `internal/models` (`roster.go` + `usage.go` + one `probe_*.go` per probe kind) loads it, resolves nicknames (case-insensitive, punctuation-tolerant, also matching an exact model ID), and probes every provider's live usage in parallel - each bounded to its own timeout so a wedged network call can't hang `/dashboard`. Every `Bar.Percent` is the USED percentage of its window (never remaining); every probe distinguishes "no key" (not logged in / not configured) from "no data" (probed fine, nothing to show yet) from a real error, since collapsing those into one message hides which fix applies. `aida models` prints the compact table (`--json` for the full payload, `--fresh` to force re-probing); `GET /api/models` serves the same payload to the `/dashboard` Models panel behind a short response cache so a polling browser tab doesn't re-run every provider's probe on every poll. Tokens/keys are never written to Detail, logs, or JSON output - only ever used as a request header, in memory. See `internal/models/` for the per-provider probe implementations (Anthropic OAuth usage, a Codex app-server RPC, a LiteLLM proxy's spend endpoint, and so on).

## Jarvis voice layer (`internal/jarvis/`)

Inspired by movie-Jarvis: ffmpeg mic capture → amplitude VAD → whisper.cpp `tiny.en` (Metal) → Claude Haiku with **in-process** tool dispatch → Piper TTS (`en_GB-jarvis-high.onnx`, MIT, MITed via `jgkawell/jarvis`). All Go, all macOS-native, no Python in the runtime path.

### Wake phrases

Regex-matched, punctuation-tolerant. All of these fire (whisper's transcript variations included):
- `ok jarvis`, `okay jarvis`, `hey jarvis`
- `good morning jarvis`, `good afternoon jarvis`, `good evening jarvis`

Bare wake (just the phrase, no follow-up text) arms the next utterance to be the query.

### Voice tools (in-process, distinct from MCP tools)

| Tool | Purpose |
|------|---------|
| `tasks_list` | Priority-sorted task list with optional tag filter |
| `task_get` | Look up one task by `#N`, slug, or partial slug |
| `tasks_add` | Create a task; echoes new ID + title |
| `task_done` | Mark a task done; echoes title for verification |
| `task_status` | Set any of the six statuses |
| `task_edit_tags` | Add/remove tags on a task |
| `day_summary` | Composite morning briefing (tasks + time + weather) |
| `current_time` | Local time + date + timezone for time-relative queries |
| `weather` | Current conditions via wttr.in (free, no key); defaults to user's `Home` |
| `aida_query` | Delegate open-ended questions to `aida <query>` (web search, codebase grep, partner data, etc.) |
| `minecraft_ask` | SSH'd `claude -p` against a self-hosted Minecraft Bedrock server |
| `claude_memory_recall` | Recall the user's captured Claude Code memories: semantic, or `recent: true` for "last/latest memory" questions; targets the pinned `claude` profile so it works regardless of Jarvis's own home/work profile |
| `memory_save` | Persist a durable fact about the user to the `claude`-profile memory store (what `claude_memory_recall` reads); two-phase confirm - proposes a read-back, writes only after the user approves |
| `mcp_find_tool` | Discover external MCP capabilities (Slack/Gmail/Notion/…) by query |
| `mcp_call_tool` | Invoke a discovered MCP tool with echo-confirm reply |
| `job_start` | Spawn a background agent (`aida --agent --run-dir`); `kind: "pr_work"` does `gh pr checkout` into a worktree |
| `job_status` / `job_list` | Inspect running / awaiting_input / terminal jobs |
| `job_send_input` | Answer a paused agent's `ask_user` question |
| `job_cancel` | SIGTERM the agent and mark the job failed |
| `jarvis_thumbs_up` | Approve the most recent turn; persists to `jarvis_lessons` + (if turn used `aida_query`) fires `aida thumbs-up <run-id>` |
| `jarvis_thumbs_down` | Correct the most recent turn (reason required); same dual-write + engine passthrough |
| `jarvis_note` | Record a standing directive (e.g. "use Fahrenheit") for similar future turns |

The voice tool registry calls `internal/brain`, `internal/jobs`, and `internal/mcp` directly - no MCP RPC overhead. `feedback_no_mcp_query_tool` prohibits exposing `aida_query` as an MCP tool (sub-agent recursion); it's safe here because Jarvis is the top-level agent and shells out via subprocess.

### Example voice commands

| Category | Example |
|---|---|
| Add a task | *"Ok Jarvis, add a task to wire up the cron job"* |
| Mark done | *"Ok Jarvis, mark task 162 done"* (echo-confirms with title) |
| Change status | *"Ok Jarvis, put task 175 on hold"* |
| Edit tags | *"Ok Jarvis, add the `home` tag to task 200"* |
| Day briefing | *"Ok Jarvis, what's my day look like"* |
| MCP read | *"Ok Jarvis, search nytimes for stories about X"* |
| MCP send | *"Ok Jarvis, send a Slack message to Y saying ..."* (requires a local stdio Slack MCP server) |
| Background agent | *"Ok Jarvis, continue working on PR 583"* - returns run-id; notifier pings you on next wake when input is needed or done |
| Answer paused agent | *"Ok Jarvis, tell PR 583 to rebase onto main"* |
| Check status | *"Ok Jarvis, how's PR 583 going?"* / *"what's running?"* |
| Rate a turn | *"Ok Jarvis, thumbs down, you should have used the weather tool"* - writes to `jarvis_lessons` and (if applicable) propagates to engine via `aida thumbs-down <run-id>` |
| Standing directive | *"Ok Jarvis, note that I prefer Fahrenheit"* - recall surfaces this on similar future turns |

### Voice feedback loop

Rating tools (`jarvis_thumbs_up/down`, `jarvis_note`) attach feedback to the most recent non-rating turn - pulled from an in-memory `lastTurn` cache on `Assistant` that the listener updates after each turn. Rating-only turns are transparent (don't supersede the turn they're rating). The `jarvis_lessons` SQLite table in `brain.db` stores `query` + `query_embedding` + rating + reason + reply; recall fires at the start of every turn, embedding the user's new query via Voyage and pulling the top-3 most-similar past lessons (cosine > 0.25) into the system prompt as a "Past feedback" block - three sub-sections for thumbs-down, thumbs-up, notes.

**Engine passthrough.** When the rated turn used `aida_query`, the voice tool also fires `aida thumbs-<verb> <run-id> --because <reason>` so the engine `lessons` table benefits from the same correction (router boost on next similar `aida` query). Run id is captured via a `[aida-run-id:<id>]` sentinel that the engine emits on stderr after `runs.Save` and the `aida_query` tool parses out of its captured output - eliminates the race with concurrent terminal `aida` runs.

### Background-agent loop

Long-running work - drafting a budget, continuing a PR, investigating an incident - goes through the per-profile jobs queue in `internal/jobs/` (SQLite-backed; manifests in `~/.aida/jobs/<profile>/runs/<run_id>/`). Voice tools `job_start` / `job_status` / `job_list` / `job_send_input` / `job_cancel` expose it to Jarvis. The engine-side `ask_user` agent tool (`internal/cli/agent.go`) pauses a job into the `awaiting_input` state and polls `<run-dir>/input.txt` for a reply. A 2s polling goroutine in `serve.go` (`watchJobsForNotifications`) detects state transitions, enqueues a voice utterance via `internal/jarvis/notify`, and the listener drains the queue at the start of every wake-triggered turn - **speak-on-next-wake**, no Whisper feedback risk. `Manifest.NotifiedAt` is the idempotency key across daemon restarts. `pr_work` kind creates a worktree at `~/.aida/worktrees/<profile>/pr-<num>-<runID>/`, runs `gh pr checkout`, and cleans up the worktree on terminal state.

### MCP client surface

`internal/mcp/discovery.go` is the *client* (not the server). At Jarvis startup `initMCPDiscovery` reads `~/.claude.json` `mcpServers`, `~/.claude/.mcp.json`, and the project `.mcp.json` (in that precedence), connects to each via stdio (`command` shape) or HTTP/SSE (`url` shape), and caches the tool list. The `aida` server is intentionally skipped to prevent recursion. Failures degrade gracefully - a 401 on a URL server logs and continues. Claude.ai web connectors (Slack/Gmail/Calendar/Drive/Notion/Airtable in `claude mcp list`) are NOT reachable: their tokens live in Claude Code's keychain, not in any config file.

### CLI surface

```
aida serve                     # canonical: HTTP :1610 + voice listener
aida serve --no-listen         # HTTP only
aida jarvis ask "..."          # typed → spoken (no mic)
aida jarvis listen             # push-to-talk: 6s record → STT → reply
aida jarvis daemon [--verbose] # listener-only, no HTTP
aida jarvis greet              # TTS smoke test
aida jarvis thumbs-up --last N # rate the last N voice turns +1 (jarvis_lessons)
aida tasks web                 # opens http://localhost:1610/tasks in browser
aida tasks web --standalone    # legacy ephemeral random-port server
```

### Audit log

Real wake-triggered turns append to `~/.aida/brain/jarvis/audit.ndjson` (under the brain git repo, so the auto-commit backs it up across machines), one JSON line each:

```json
{"ts":"...","started_at":"...","transcript":"Hey Jarvis, what's the weather?",
 "query":"what's the weather?","reply":"...","tool_calls":[{"name":"weather","took_ms":1500}],
 "llm_ms":1100,"tool_ms":1500,"tts_ms":900,"play_ms":5200,"took_ms":8700}
```

Skipped utterances, VAD blips, and whisper hallucinations (e.g. `[BLANK_AUDIO]`, `(upbeat music)`) are filtered out - the log captures Jarvis's actual behavior, not raw mic input. Errors writing the log are non-fatal.

LMD (Android client, `internal/jarvis/lmd`) turns are logged to the same file too, tagged `"source": "lmd"` and carrying an additional `stt_ms` field, so the two pipelines are distinguishable in one place.

### Latency expectations

- Local fast tools (`tasks_list`, `task_get`, `current_time`, `weather`): **2–4s** total round-trip
- `aida_query` (full engine + web search): **8–15s** total - the "One moment, sir." ack plays first to mask the wait
- TTS playback (`play_ms`) usually dominates total wall-clock for replies longer than two sentences

## MCP server surface (`aida serve`)

Stdio JSON-RPC server exposing brain and task operations to other agents:

| Tool | Purpose |
|------|---------|
| `brain_search` | Semantic search over lessons + entity pages |
| `brain_recall` | Scope-aware recall over captured coding-agent memories (semantic + recency); `scope` filter, `recent` flag, `profile` defaults to `claude` (also `codex`, `gemini`, `meetily` -- see "Multi-agent memory bridge" below -- or `all`). Separate from `brain_search`, which covers lessons/entities |
| `brain_get_page` | Read an entity page by type + slug |
| `brain_stats` | Lesson / entity / task counts, DB size |
| `tasks_list` | List tasks (filter by tag, status, include_done; defaults to `open + in-progress`) |
| `tasks_add` | Create a task |
| `tasks_done` | Mark a task done - accepts slug, partial slug, or `#N` stable ID |
| `tasks_reopen` | Flip a terminal task back to open |
| `tasks_edit` | Mutate title/body/tags/status without opening `$EDITOR` (accepts all six statuses) |
| `tasks_show` | Return the task's markdown body (frontmatter stripped) |
| `tasks_get` | Return structured `TaskRecord` JSON (metadata, not body) |

All slug-accepting tools resolve `#N` task IDs, exact slugs, or partial-slug substrings via `Brain.ResolveTaskRef`. Handlers live in `internal/mcp/server.go`; mutation methods live in `internal/brain/tasks.go` (`UpdateTask`, `ReopenTask`). No `tasks_delete` - see feedback memory "no task deletion".

## Task CLI surface (`aida tasks`)

| Subcommand | Purpose |
|------------|---------|
| `aida tasks` | List tasks (defaults to `open + in-progress`; `--all` for every status; `--status hold,deferred` to filter) |
| `aida tasks add [title]` | Create a task; `--more` opens `$EDITOR` for context paste; `--tag` repeatable |
| `aida tasks done <ref>` | Mark a task `done` (your work) |
| `aida tasks status <ref> <value>` | Set any of the six statuses |
| `aida tasks reopen <ref>` | Flip a terminal task back to `open` |
| `aida tasks edit <ref>` | Open `$EDITOR` on the task file |
| `aida tasks show <ref>` | Print the task body |
| `aida tasks web` | Launch a tiny local web UI (loopback only) for browsing/filtering tasks and changing status/priority |

`<ref>` accepts slug, partial slug, or `'#N'` stable ID (quote `'#N'` so the shell doesn't treat `#` as a comment).

Task ids are assigned after a pull, not before. Every add path (`aida tasks add`, the natural-language create intent, the MCP `tasks_add` tool, the Jarvis `tasks_add` voice tool, `aida tasks ingest`, `aida loop plan`) pulls the brain repo (bounded, `brain.PullAndWait`, ~10s) when `auto_sync` is on, and `Brain.AddTask` re-seeds `task_seq` from whatever `task_id` values are on disk right before allocating one, so a task file another machine already pushed doesn't get its id reused. A machine that is offline at add time can still collide with one added elsewhere in the same window - the pull just times out and the add proceeds anyway rather than blocking. When that happens, the fix is to renumber the newer of the two conflicting task files (bump its `task_id` in frontmatter past the current max on disk) and let the next pull re-seed the counter past it.

### Six task statuses

| Status        | Default visible | Terminal | Meaning                                          |
|---------------|-----------------|----------|--------------------------------------------------|
| `open`        | yes             | no       | Default for new tasks                            |
| `in-progress` | yes             | no       | Actively being worked                            |
| `hold`        | no              | no       | Waiting on someone/something external            |
| `deferred`    | no              | no       | Pushed out, not blocked on anything specific     |
| `done`        | no              | **yes**  | I completed it (counts as my work)               |
| `closed`      | no              | **yes**  | No longer relevant (coworker did it, won't do)   |

The legacy `completed` bool is now "is terminal" - true for both `done` and `closed`. The GitHub Issues mirror passes `--reason "not planned"` for `closed`, default reason for `done`. Frontmatter without an explicit `status` key is treated as `open`. Schema and validation live in `internal/brain/tasks.go`; CLI handlers in `internal/cli/tasks.go`.

## Autonomous loop surface (`aida loop`)

> Status: merged to main (PR #81, merged 2026-06-23). Design: `docs/autonomous-loop-design.md`. Implementation: `internal/cli/loop.go`.

A Ralph-style autonomous driver: a deterministic outer loop that repeatedly spawns fresh-context `aida --agent` instances, each completing one highest-priority open task (up to `--concurrency` in parallel per round), until the set is drained or `--max-iterations` is hit; in `--daemon` mode it instead waits for new tasks rather than exiting. Tasks (filtered by `--tag`) replace `prd.json`; brain recall replaces `progress.txt`. Each iteration: pick task → seed fresh agent with brain recall → implement → run the `--check` gate(s), retrying up to `--max-fix-iterations` times on failure → on pass mark done + distill a `## Learnings` section back into the brain (so the next similar task recalls it). `--worktree` isolates each task in its own git worktree (needed for `--concurrency` > 1 and for `--sandbox docker`); `--pr` commits, runs an optional adversarial review panel (`--review-panel`), then opens a PR and tags a reviewer instead of completing the task directly (the loop never merges).

| Subcommand | Purpose |
|------------|---------|
| `aida loop` | Run the loop over the task set |
| `aida loop plan <goal>` | Decompose a plain-English goal into tagged, priority-ordered tasks (reuses the `aida tasks ingest` LLM extraction path) |

| `aida loop` flag | Default | Meaning |
|----------------|---------|---------|
| `--tag` (repeatable) | (none) | only loop over tasks carrying ALL of these tags |
| `--max-iterations N` | 10 | safety bound (like `ralph.sh`); counts rounds, not tasks |
| `--check "<cmd>"` (repeatable) | (none) | quality gate run after each attempt; must exit 0 (e.g. `"make test"`). No gate = pass with warning |
| `--max-fix-iterations N` | 3 | max fix attempts per task before parking it on hold |
| `--commit` | false | commit the working tree after a passing iteration (mutually exclusive with `--pr`) |
| `--worktree` | false | run each task in an isolated git worktree branched off `origin/main` |
| `--pr` | false | open a PR per passing task and tag a reviewer; implies `--worktree`; the loop never merges |
| `--reviewer <handle>` | (none) | GitHub handle to request review from on the PR (`--pr`) |
| `--base <branch>` | main | base branch for PRs (`--pr`) |
| `--review-panel N` | 0 | run N adversarial reviewer agents (correctness/security/test-coverage) that must majority-approve before a PR opens |
| `--concurrency N` | 1 | work up to N tasks in parallel, each in its own worktree (requires `--worktree`) |
| `--max-budget-usd` | 0 | stop once accumulated agent cost exceeds this (0 = use config `agent.max_budget_usd`, else unlimited) |
| `--max-consecutive-failures N` | 0 | circuit breaker: stop after N consecutive tasks parked on hold (0 = unlimited) |
| `--daemon` | false | run continuously; wait for new tasks instead of exiting once the set drains |
| `--poll` | 5s | in `--daemon` mode, how long to sleep when no tasks are ready |
| `--sandbox <tier>` | none | confine spawned agents: `none` or `docker` (requires `--worktree`) |
| `--sandbox-memory` | (none) | docker sandbox memory limit, e.g. `8g` (docker tier only) |
| `--provision-aida <path>` | (none) | prebuilt linux/arm64 `aida` to copy into the docker sandbox (default: auto-build) |
| `--recall-k N` | 3 | similar brain lessons seeded into each fresh agent |
| `--per-task-timeout` | 7m30s | per-iteration agent timeout |
| `--dry-run` (global) | (none) | print the task(s) it would pick; no spawn, no mutation |

`aida loop plan` flags: `--tag` (default `loop`), `--max-tasks N` (0 = no cap), `--dry-run`.

Typical use, drive the whole thing from a goal:
```bash
aida loop plan "add priority to tasks: schema, badge, edit selector, filter" --tag prio
aida loop --tag prio --check "make test"
```
Safe by default: `--commit` and `--pr` off, failures park the task on `hold` (no silent re-spin). Worktree isolation and budget/circuit-breaker stops have landed (`--worktree`, `--max-budget-usd`, `--max-consecutive-failures`); a loop-specific decomposition prompt (dependency ordering, one-iteration sizing, verifiable acceptance criteria) is still TODO (see the design doc).

## Burn-down capacity (`aida burndown`)

> Package: `internal/burndown/`. Design: a burn-down design doc that lives only on another branch (piece #1, "Capacity view"), not part of this repo.

`aida burndown capacity` is a read-only view over the AI model roster's live usage: for every rate-limit window (and every LiteLLM/budget row), it reports how much is left, the effective floor Ryan's own reserve requires right now, and the headroom above that floor an unattended burn-down loop could safely spend. That's all that exists today. There is no picker, no scheduler, no `aida serve --burndown`, no runners, no ledger, and nothing here dispatches an agent or mutates a task.

The floor rule (`internal/burndown/capacity.go`'s `CapacityFor`): start from a static floor configured per window and time of day (daytime, overnight, or the last 24h before a weekly reset), forcing it to 0 ("drain-to-zero") when the window resets before the end of the configured overnight period, since anything left would just expire unused. Once a window carries a real burn-pace verdict (`internal/models/pace.go`'s `PaceFor`, i.e. not `PaceEarly` and not a sub-24h window), that static floor is replaced by a pace-derived one instead: `floor = clamp(max(0, Pace.Projected - used%) * safety_multiplier, hard_floor, left%)`, since the live pace is a better estimate of what Ryan's own usage needs than a fixed percentage. A window's `hard_floor`, when configured, always wins, even over drain-to-zero (Fable's 7-day bar never drops below 50%, any day, even on a night it resets). A budget row's `floor_usd` (`CapacityForSpend`) is the same kind of hard floor: pace can raise it but never lower it below floor_usd's percent-of-budget equivalent, since the LiteLLM reserve it protects exists for bursty, unscheduled Jarvis turns and browser jobs that never register in a pace reading.

Floors live in `~/.aida/burndown.yaml` (`config.Config.BurndownPath`, mirroring `ModelsPath`'s shape exactly), copied from `examples/burndown.yaml` and hand-edited from there. Each floors entry names a `provider:` that must match a `~/.aida/models.yaml` provider's `label:` field exactly (e.g. "Anthropic / Claude"); a live bar whose provider or window isn't named in the floors config is never an error, it just reports "no floor configured" with the full remaining percent as headroom. `aida burndown capacity` probes usage the same way `aida models` does (`--fresh` forces a re-probe) and takes `--json` for the full per-provider payload or `--at <RFC3339>` to compute capacity as of a different time, so "what will the overnight wave see" is answerable without waiting for it. Antigravity's CLI is `agy` (`internal/models/probe_agy.go`), and its "5-hour Claude/GPT" / "7-day Claude/GPT" bars are Claude and GPT usage routed through the Google account, not Gemini quota, so `Report` excludes them from the capacity view entirely.

## Aida dispatcher + agent roster (`aida ask`, `aida roster`)

> Packages: `internal/roster/` (registry + backends), `internal/dispatch/` (the orchestrator).

**Aida** is the front-door orchestrator over a profile-scoped, backend-agnostic roster of named agent call-signs, addressable by voice (the Jarvis wake loop) and typed query. You talk to Aida on any subject; she routes to the right agent, fans out across several, spawns background jobs, and aggregates one answer. The roster lives at `~/.aida/roster.yaml` (see `examples/roster.yaml`).

Each roster entry is a call-sign mapped to a `kind` backend:

| Kind | Backend | Transport |
|------|---------|-----------|
| `subagent` | a Claude Code subagent in a repo | `claude --print` in the entry's dir, targeting a named `.claude/agents/` persona (the `ClaudeProjectAdapter` recipe) |
| `source` | an aida library source | shells `aida --source <name> "<task>"` (the `--source` pin bypasses the LLM router) |
| `mcp` | a discovered MCP tool | `discovery.CallTool`; tool + args chosen heuristically, no LLM |
| `job` | background `aida --agent` | enqueued on the jobs store; returns a handle, notifier speaks completion on next wake |
| `aida` | reserved orchestrator row | never a dispatch target; metadata only |

**Team freshness:** a single `discover:` entry expands live into one virtual entry per persona in a `.claude/agents` directory (call-signs from an org chart, mtime-cached), so a growing team never goes stale. A hand-authored entry with the same name overrides its discovered twin. There is deliberately no `aida roster import` command.

**LLM at the edges:** 0 LLM calls when a call-sign is named in the task (deterministic word-boundary scan), 1 select call over the profile's roster otherwise, 1 synthesize call only on a multi-agent fan-out. An empty selection falls back to `Dispatcher.fallback`: when `~/.aida/AGENTS.md` exists, `aida ask aida` answers in Aida's own persona via the same `claude --print` subagent transport a roster entry uses (`roster.RunClaudeIn`), which is cheaper and more her than the general engine's three-LLM-call hedge; only when no charter directory is configured does fallback drop to a plain `aida <task>` subprocess. `dispatch.Decide` is a pure sync-vs-background policy the voice layer consumes (subagent/fan-out from voice go async via a job).

**One-hop depth guard:** every process `roster.RunClaudeIn` spawns (a roster subagent, or the charter fallback above) carries `AIDA_DISPATCH_DEPTH` one higher than it inherited, via `roster.BumpDispatchDepthEnv`; `aida ask` refuses to run once it reads that env var at 2 or more, so an agent can call `aida ask` once but that agent's own agent cannot call `aida ask` again.

**Aida vs Jarvis:** additive divergence, not a fork. `aida serve` builds a `dispatch.Dispatcher` and sets it on the Aida twin's `jarvis.Config.Dispatcher`; `newAssistant` then registers the `ask_agent` + `roster_list` voice tools and a chief-of-staff prompt block only for her. Jarvis gets a nil dispatcher and stays byte-identical (his prompt golden test is unchanged).

| Surface | Command / tool |
|---|---|
| CLI | `aida ask <who> "<task>"` (named entry), `aida ask aida "<task>"` (full dispatch), `aida roster list [--all]`, `aida roster lint` |
| Voice (Aida only) | `ask_agent {agent, task}`, `roster_list` |
| MCP server | `roster_list` deferred: `internal/mcp` cannot import `internal/roster` (which imports `internal/mcp` for its mcp backend) without a cycle, so a read-only list over MCP needs a roster/mcp decoupling first. No dispatch tool over MCP regardless, per `feedback_no_mcp_query_tool`. |

## Architecture: Six-Step Engine Pipeline

The core design principle is **LLM at edges, deterministic middle**. Each query makes exactly 3 LLM calls:

```
User Query
    │
    ▼
[1. PARSE]        ← LLM Call #1: natural language → Intent struct (parser.go)
    │
    ▼
[2. CLASSIFY]     ← Deterministic: ARI pattern matching + strategy selection (classifier.go)
    │
    ▼
[3. RESOLVE]      ← Deterministic: entity typing, flattens resolved identifiers (resolver.go)
    │
    ▼
[4. PLAN]         ← Deterministic: capability matching, source ranking, execution plan (planner.go)
    │
    ▼
[5. EXECUTE]      ← LLM Call #2: generate source-specific queries, parallel shell exec (executor.go)
    │
    ▼
[6. SYNTHESIZE]   ← LLM Call #3: aggregate results into answer with citations (synthesizer.go)
```

## Key Packages

- **`cmd/aida/`** - Entry point, root Cobra command
- **`internal/engine/`** - The six-step pipeline (parse → classify → resolve → plan → execute → synthesize)
- **`internal/llm/`** - Anthropic SDK wrapper, prompt templates, JSON schemas for structured output
- **`internal/sources/`** - Adapter interface + implementations (sqlite, git, Notion, grep, exec, CSV)
- **`internal/config/`** - YAML config management for `~/.aida/{config,sources}.yaml`
- **`internal/index/`** - File scanning and LLM-based source entry generation
- **`internal/cli/`** - Cobra command handlers (query, init, index, profile, sources)
- **`internal/ui/`** - Terminal UI (spinners, formatted output, lipgloss styles)

## Configuration System

All config lives in `~/.aida/`:

| File | Purpose |
|------|---------|
| `config.yaml` | Global settings: LLM model, active profile, API key env var reference |
| `op.env` | 1Password tokens (service accounts, interim session); loaded into the process env like `.env`, never rendered by the secrets sync, never committed |
| `library.yaml` | Registry of library roots (each root has its own sources/layers/routes) |
| `library/sources/*.yaml` | Per-source configs (one file per source) |
| `library/layers/sources/*.md` | Per-source LLM context docs |
| `library/routes.yaml` | `match_cwd` / `match_entity` routes that activate layers + sources |
| `brain/` | Brain git repo: lessons, entity pages, knowledge, SQLite index |
| `sources.yaml`, `index.yaml` | Legacy flat config (fallback if library is empty) |

### Source YAML shape

```yaml
path: /absolute/path           # required for codebase, optional for tool/docs
type: codebase | docs | tool | data-source | app | claude-project | git
context: CLAUDE.md             # context doc read into LLM when this source is picked
description: one-liner
capabilities: [sql-query, booking-lookup, ...]
entities: [wakanda, vibranium-checkout, ...]   # topic tokens driving routing
profiles: [work]               # optional - limit to named profiles (else all)
search:                        # Phase 1.1
  mode: grep                   # flip a `type: docs` source to GrepAdapter
  include: ["*.md", "*.txt"]
  exclude: ["gdrive/**/*.pdf"]
exec:
  query: 'tool-cmd {query}'    # NEVER '{query}' alone - library rejects raw passthroughs
```

## Key Design Decisions

- **Config-driven routing** - The LLM does not decide which sources to query. `sources.yaml` capabilities/`entities:` tokens and `routes.yaml` `match_entity` routes drive routing deterministically.
- **Parallel execution** - `errgroup` with a semaphore (limit 5 concurrent) for multi-source fan-out.
- **Profile auto-detection** - Ships with a single undetected `home` profile by default; additional profiles (e.g. a `work` profile scoped to a work-only CLI) are opt-in via `profiles:` config and `Profile.Detect` (`HasTool`/`MissingTool`).
- **Notion via Claude CLI** - Shells out to `claude --print` to reuse Notion MCP integration rather than direct API.
- **Offline fallback** - Can use Ollama (e.g., `qwen3.5:35b`) when no API key is available.

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/spf13/cobra` | CLI framework |
| `github.com/anthropics/anthropic-sdk-go` | LLM calls (Claude Haiku) |
| `gopkg.in/yaml.v3` | Config file parsing |
| `golang.org/x/sync/errgroup` | Parallel execution with error propagation |
| `github.com/charmbracelet/lipgloss` | Terminal styling |
| `github.com/muesli/termenv` | Terminal color detection |

## Classifier Patterns

The classifier in `internal/engine/classifier.go` uses regex-based ARI pattern matching to identify entity types from user queries (16-char ARI patterns, user IDs, charge IDs, URLs). It then selects a strategy: `lookup`, `query`, `investigate`, `record`, `execute`, or `search`.

## Routing contract

1. Parser extracts raw entities (LLM Call #1).
2. Classifier types each entity.
3. Resolver flattens each typed entity's metadata (e.g. `external_order_id`) for the planner and prompt builders; it does not attach any additional identity - entity-to-source affinity is expressed entirely by sources' `entities:` token lists and `routes.yaml` `match_entity` routes (+100), not resolved here.
4. Planner's `rankSources` scores sources by exact/substring name match against raw entities, topic match between a source's `entities:` list and the question's keywords/raw entities, and capability/keyword-description hits. `routes.yaml` `match_entity` routes add a further +100 when a route applies.
5. `flattenCandidates` (cli/query.go) passes the top-N non-zero sources from the full ranking to the LLM router - not just phase-picked sources.
6. LLM router (4th LLM call) applies deterministic `+50 / -50` from the top-3 most-similar thumbs-down lessons' `FeedbackIntendedSources` / `FeedbackExcludedSources` BEFORE generating the prompt.

## Feedback loop (Phase 3)

- `aida thumbs-down --because "..."` runs `extractDirective` (cli/feedback.go): a polarity-tracking tokenizer that handles negation phrases ("no need for", "do not use", "instead of") and sentence boundaries. Returns `intendedSources []string` and `excludedSources []string` - both persisted to brain.db and the JSON lesson file.
- The router reads those fields on the next similar question and applies the boost/demote deterministically (not just as prose hints in the LLM prompt).

## Brain rebuild behavior (Phase 4)

`IsStale` in `internal/brain/db.go` returns true when 3+ lesson/entity/knowledge files are newer than `brain.db`. On stale, `cli/query.go` spawns a detached `aida brain index` subprocess (Setpgid) so the current query proceeds in < 1s instead of blocking ~3 minutes for a full re-embed. Subsequent queries pick up the rebuilt `brain.db` naturally.

## Multi-agent memory bridge (Codex + Gemini + Meetily)

`aida brain harvest --tool codex|gemini|meetily` is the Codex/Gemini/Meetily analogue of the Claude Code memory bridge (`aida brain capture-hook` / `aida brain remember`, above). Codex and Gemini don't write memory files aida can mirror the way Claude Code does - what's on disk is a raw session transcript - so this side is a distillation harvester: an LLM extraction pass (`internal/brain/harvest.go`'s `DistillFunc`, matching `(*llm.Client).CompleteJSON`'s signature so tests can inject a canned responder) reads each new/changed session and proposes 0-5 durable fact/instruction/event records, written under a fixed `codex`, `gemini`, or `meetily` memory profile via the same `WriteMemory` path every other memory record goes through.

Sources: `internal/brain/harvest_codex.go` reads `~/.codex/session_index.jsonl` + the matching `~/.codex/sessions/**/rollout-*.jsonl`, keeping only user/assistant message text. `internal/brain/harvest_gemini.go` covers three sub-sources - Gemini CLI session logs (`~/.gemini/tmp/<hash>/chats/session-*.json`, LLM-distilled), Antigravity walkthrough/plan artifacts (`~/.gemini/antigravity-cli/brain/<conversation-id>/*.md`, mirrored verbatim via `conversation_summaries.db`'s `workspace_uris` for scope), and `~/.gemini/GEMINI.md` (mirrored like `CLAUDE.md`). `internal/brain/harvest_meetily.go` distills exported call recordings from `life-log/calls/<YYYY-MM-DD>-<HH-MM>-<slug>/` (default calls root `~/dev/life-log/calls`, overridable via `AIDA_MEETILY_CALLS_ROOT`) - each folder's `transcripts.json` (required) plus optional `summary.json`/`meeting.json` for the AI summary and title. Since these are real conversations, not coding-agent sessions, the distill prompt is written for that (`meetilyDistillSystemPrompt`), and when `<calls-root>/GLOSSARY.md` exists its content is folded in as a proper-noun correction glossary (transcripts are STT output that garbles names - e.g. "Owens" → Ryan L'Italien).

Incremental via a per-tool watermark file under `brain/meta/`; a session (or call) updated in the last 10 minutes is treated as still running and skipped until quiet - Meetily keys this by call-folder name + newest mtime among its files rather than a session id + `updated_at` field, since it has no such field on disk. `aida setup` wires detached harvest hooks into `~/.codex/hooks.json` (Stop event) and `~/.gemini/settings.json` (AfterAgent event), additively merged so hooks another tool already manages are preserved; Meetily has no equivalent hook (it isn't a coding agent with an event system), so the periodic sweep below is its only automatic trigger. Recall with `aida brain recall --profile codex|gemini|meetily` or `brain_recall` (MCP) with `profile: "codex"|"gemini"|"meetily"`. Full design: `docs/memory-bridge-multi-agent.md`.

Meetily also runs a classifier pass (aida task #367) in the SAME distill call: it additionally returns `tags` (chosen only from a vocabulary built at harvest time from task tags/entity slugs/library source entities - never hardcoded, see `buildMeetilyTagVocabulary` in `internal/brain/harvest_meetily_vocab.go`), `participants`, `action_items`, `key_points` (the call's badge summary), and an `is_new_category` flag + `proposed_tag` when no vocabulary tag fits. This rides two general hooks on `HarvestSession` (`SchemaOverride`, `TagsFromResponse`, both nil/no-op for Codex/Gemini) so no second LLM call is needed. The classification is written to `call.json` next to `summary.json`, and stamps `"meetily-call"` + the chosen tags onto the written records in place of the generic `"meetily-code"` tag; `is_new_category` also tags `needs-review` and opens an aida task (`Review new call category: <tag> (<title>, <date>)`, tagged `calls`,`needs-review`) via `Brain.AddTask` - a `TODO(meetily-notify)` marks where Jarvis notification should hook in once serve wiring for this is touched, not wired yet. Recall supports a tag filter too: `aida brain recall --tag <tag>` / MCP `brain_recall`'s `tag` argument (e.g. `--profile meetily --tag cta` for one project's calls).

Hook-triggered harvest alone has a gap: a session's LAST hook can fire while the session is still inside the 10-minute quiet window, so that run skips it as "still running" - and if the user then goes idle on that tool, nothing fires again to pick it back up. `aida serve --harvest-sweep <duration>` (default `15m`; see the `aida serve` flag table above) closes this by running the same harvest core (`internal/cli/brain_harvest.go`'s `runHarvestCore`, factored out for exactly this reuse) on a periodic background timer for all three tools, sequentially, independent of hooks. The watermark and quiet window already make a no-op sweep cheap (zero LLM calls, near-zero I/O), so ticking every few minutes in the background is safe; the sweep only logs when a pass actually writes or drops something.

## Library load-time validator

`internal/library/sources.go` rejects sources whose `exec.query` is a bare `{query}` passthrough (Phase 1.4). Without a real command prefix, `sh -c "<LLM output>"` shell-injects whenever the LLM emits prose. Rejected sources are dropped from the registry with a `ui.PrintVerbose` warning; `aida lint` surfaces them explicitly.
