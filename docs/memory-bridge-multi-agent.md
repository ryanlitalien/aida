# Multi-agent memory bridge: Codex + Gemini + Meetily -> Aida brain

Status: shipped. Companion to `docs/claude-memory-bridge-design.md` and `docs/claude-memory-bridge-capture.md` (the Claude Code side of the bridge). This doc covers the Codex, Gemini, and Meetily side: `aida brain harvest --tool codex|gemini|meetily`, `internal/brain/harvest*.go`, and (Codex/Gemini only) the `aida setup` hook wiring in `internal/cli/setup_harvest_hooks.go`.

## Why this is a harvester, not a mirror

The Claude Code bridge is a mirror: Claude Code already writes hand-authored memory files (`~/.claude/memory/*.md`, `~/.claude/projects/<slug>/memory/*.md`, `CLAUDE.md`), and `aida brain capture-hook` / `aida brain remember` just read those files, classify their scope from the path, and copy them into the brain as typed memory records.

Codex and Gemini don't write anything shaped like that. What's on disk is a raw session transcript, sometimes thousands of lines of tool calls, reasoning, and back-and-forth. There is no equivalent of "the user (or the assistant, on the user's behalf) deliberately wrote this down as worth remembering." So this side of the bridge has to do the deliberate-worth-remembering judgment itself: an LLM extraction pass reads each session and proposes 0 to 5 durable memories, the same way `aida brain consolidate` promotes event records into facts and instructions. Nothing worth keeping is a common, valid, and expected outcome for any given session; the prompt says so explicitly and the code never manufactures memories to hit a quota.

Meetily is the same shape of problem for a different reason: it's not a coding agent at all, so there's obviously no memory file to mirror -- what's on disk is an exported call recording (transcript + AI summary). The distillation prompt is different (a real conversation, not a coding-agent session; see "Meetily" below), but the harvester plumbing -- watermark, quiet window, `DistillFunc`, `WriteMemory` -- is identical.

## Architecture

```
Codex/Gemini session, or Meetily call folder, on disk
        |
        v
[cheap metadata scan]   <- session_index.jsonl / chat log listing /
        |                  call-folder directory listing + file mtimes
        v
[watermark filter]      <- brain/meta/harvest_watermark_<tool>.json
        |                  (quiet window, --since, already-harvested check)
        v
[transcript build]      <- read + parse the file(s) the session/call lives in
        |                  (only for sessions/calls that survived the filter)
        v
[LLM distillation]      <- DistillFunc, one CompleteJSON call per session/call
        |                  (0-5 memories: name, description, type, body)
        v
[WriteMemory]            -> brain/memory/{fact,event,instruction}/<id>.json
                             + memory_records row, profile = "codex" | "gemini" | "meetily"
```

Two sub-sources under Gemini skip the LLM step entirely and mirror pre-distilled text directly (Antigravity walkthroughs, `GEMINI.md`) -- see below.

Everything funnels through the same `brain.WriteMemory` every other memory record uses, so supersession-by-key, embedding, and the FTS corpus all work identically regardless of which tool a memory came from.

## Source formats

Verified on disk 2026-08. All of this can drift as Codex/Gemini ship new versions; the parsers degrade gracefully (log and skip) rather than crash when a shape doesn't match what's expected.

### Codex

- `~/.codex/session_index.jsonl` -- one JSON object per line: `{"id", "thread_name", "updated_at"}`. This is the cheap metadata the watermark filter runs against before any transcript is read.
- `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<id>.jsonl` -- the full transcript for one session id. The date-partition directory is keyed off the rollout's *creation* time, which need not match `session_index`'s `updated_at` (a session can be forked or resumed), so the harvester locates the file by walking the tree for a name ending in `-<id>.jsonl` rather than guessing a path from the id and date.
- The first line is `type == "session_meta"` with `payload.cwd` -- this is where project scope comes from.
- Subsequent lines are typed events. Only `type == "response_item"` with `payload.type == "message"` and `payload.role` in `{user, assistant}` carry conversation text, taken from `payload.content[].text` where the content type is `input_text` or `output_text`. Everything else (`reasoning`, `function_call`, `function_call_output`, `developer`/`system` messages, `turn_context`, `world_state`, ...) is scaffolding noise and is dropped before the transcript reaches the LLM.
- The transcript is capped to the first and last ~15,000 characters (roughly 30k total) to bound LLM cost on long sessions; the (usually least informative) middle is dropped.

### Gemini

Three independent sub-sources, all writing under the `gemini` profile and sharing one watermark file, namespaced by ID prefix so they never collide.

**(a) Gemini CLI session logs**, `~/.gemini/tmp/<projectHash>/chats/session-*.json`:

```json
{
  "sessionId": "...",
  "projectHash": "...",
  "startTime": "...",
  "lastUpdated": "...",
  "messages": [{"id": "...", "timestamp": "...", "type": "user" | "gemini", "content": "..."}]
}
```

LLM-distilled the same way Codex sessions are. `projectHash` is a hash of the working directory computed by the Gemini CLI; there is no reverse mapping from hash back to path on disk, so scope for these records is `project:gemini-<hash>` (the opaque hash), not a human-legible slug. This is a known limitation, not a bug: if Gemini ever exposes the hash's source path, the scope derivation can switch to the same `cwdToProjectSlug` convention Codex uses. The sibling `~/.gemini/tmp/<hash>/logs.json` is a flat, redundant index of the same messages and is not parsed.

**(b) Antigravity artifacts**, `~/.gemini/antigravity-cli/brain/<conversation-id>/*.md`:

These are already-distilled task/plan/walkthrough markdown Antigravity itself writes (e.g. `walkthrough.md`, `implementation_plan.md`). No LLM call: each file is mirrored verbatim as an `event` record. Non-markdown artifacts in the same directories (screenshots, crash logs, `.pb` protobufs) are skipped by construction -- only `*.md` is walked. Scope comes from `~/.gemini/antigravity-cli/conversation_summaries.db` (sqlite), the `workspace_uris` column, a JSON array of `file://` URIs; the first entry is converted via the same `cwdToProjectSlug` convention as Codex. A conversation id missing from the database (deleted, or never indexed) falls back to global scope rather than failing.

**(c) `~/.gemini/GEMINI.md`**, when present, mirrors exactly like Claude Code's global `CLAUDE.md`: one global `instruction` record, no LLM call, re-mirrored only when the file's mtime advances.

### Meetily

Meetily is Ryan's local call-recording/transcription app. A separate export sweep lands finished calls under `life-log/calls/<YYYY-MM-DD>-<HH-MM>-<slug>/`, one folder per call:

- `transcripts.json` -- `{"last_updated", "segments": [{"text", ...}, ...]}`. Required: a folder without this file isn't a candidate at all (call still recording, or errored before a transcript was written).
- `summary.json` -- `{"markdown": "...", "english_cache": {"markdown": "..."}}`. Optional (absent until Meetily finishes summarizing). Top-level `markdown` wins when present; `english_cache.markdown` is the fallback.
- `meeting.json` -- `{"id", "title", "created_at"}`. Optional; when absent, title and date fall back to values parsed from the folder name's `<YYYY-MM-DD>-<HH-MM>-<slug>` prefix.
- `metadata.json` -- present for essentially every call folder, finished or not; contributes to the mtime-based watermark key (see below) but its content isn't otherwise read.

GLOSSARY.md and any other non-directory entry directly under the calls root are ignored -- only directories are considered call candidates.

LLM-distilled like Codex/Gemini sessions, under its own `meetily` profile, but the distillation prompt (`meetilyDistillSystemPrompt`) is written for a real conversation rather than a coding-agent session, and folds in one extra piece of context Codex/Gemini don't need: when `<calls-root>/GLOSSARY.md` exists, its full content is appended to the prompt as a correction glossary. Call transcripts are raw STT output over Ryan's actual conversations and routinely garble proper nouns (a former employer's name mis-heard as an unrelated common phrase, "Owens" for Ryan himself); the glossary instructs the LLM to correct these before writing any memory, so distilled records use the real names rather than propagating STT noise into the brain. Ryan already maintains this glossary for other call-summarization uses (see `life-log/calls/GLOSSARY.md`).

The calls root defaults to `~/dev/life-log/calls`, overridable via the `AIDA_MEETILY_CALLS_ROOT` environment variable (the same lightweight override pattern as `AIDA_TTS_VOLUME`, `AIDA_LMD_TOKEN`, etc. -- no config-schema change needed).

Meetily has no hook mechanism to wire into `aida setup` the way Codex's Stop event and Gemini's AfterAgent event do -- it isn't a coding agent with an event hook system. Its only automatic trigger is the periodic sweep, `aida serve --harvest-sweep` (see "Hook wiring" below); otherwise it only runs when invoked manually via `aida brain harvest --tool meetily`.

**Classifier pass (aida task #367).** The SAME distill call that extracts memories also returns a lightweight classification: `tags` (chosen only from a vocabulary built at harvest time, never a hardcoded project list -- see `buildMeetilyTagVocabulary` in `internal/brain/harvest_meetily_vocab.go`), `participants`, `action_items`, `key_points` (the call's summary badge), and an `is_new_category` flag with a `proposed_tag` for a call that fits no vocabulary tag. This rides two general-purpose hooks added to `HarvestSession`/`harvestDistilledSessions` for exactly this purpose (`SchemaOverride`, `TagsFromResponse` -- see `internal/brain/harvest.go`), so no second LLM call is needed and Codex/Gemini are unaffected (both fields are nil for them, same as before).

The tag vocabulary itself has three sources, all reflecting what Ryan is *actually* working on rather than a fixed list: task tags across every profile/status (mechanical bookkeeping tags -- `profile:*`, `due:*`, `source-hash:*`, `tool:*`, `owner:*`, `from-*`, `today`, `tomorrow`, `jarvis-error` -- excluded), brain entity page slugs (`DB.ListEntitySlugs`), and library source `entities:` lists (`internal/library`, imported directly from `internal/brain` -- confirmed no cycle, since `library` depends only on `config`/`ui`/`sources`/`execx`, none of which import `brain`). `filterTagsAgainstVocabulary` enforces "chosen ONLY from the vocabulary" as a hard guarantee regardless of what the LLM actually returns.

The classification is written to `call.json` next to `summary.json` in the call's export folder (idempotent -- a plain overwrite), and stamped onto the written memory records' tags: `"meetily-call"` plus the chosen tags, replacing the generic `"<tool>-code"` tag every other harvest source gets from the shared write path. When `is_new_category`, records and `call.json` are additionally tagged `"needs-review"`, and an aida task opens via `Brain.AddTask` -- `"Review new call category: <proposed_tag> (<call title>, <date>)"`, tagged `calls`, `needs-review` -- so Ryan decides whether the proposed tag becomes a real vocabulary entry. A `TODO(meetily-notify)` comment marks where Jarvis voice notification (`internal/jarvis/notify`) should hook in once serve wiring for this pass is touched; it is deliberately not wired in this pass.

Recall can filter by tag: `aida brain recall --tag cta` or MCP `brain_recall`'s `tag` argument now recall only records carrying that tag (case-insensitive), across any profile -- most useful for meetily (`--profile meetily --tag cta` recalls only one project's calls) but not restricted to it.

## Watermark semantics

Each tool has one watermark file, `brain/meta/harvest_watermark_<tool>.json`:

```json
{
  "tool": "codex",
  "sessions": {
    "<session-or-item-id>": "<RFC3339Nano UpdatedAt it was last harvested at>"
  }
}
```

This lives under `brain/meta/`, the same directory `aida brain consolidate`'s trigger state lives in, so it's part of the brain git repo and survives/syncs across machines like everything else there.

A candidate (session or direct-mirror item) is harvested when:

1. It is not "still running": its `UpdatedAt` is older than 10 minutes ago (`harvestQuietWindow`). A session updated more recently is assumed to still be in progress and is skipped until a later run finds it quiet.
2. `--since`, if given, doesn't exclude it: `--since` is a manual lower bound on `UpdatedAt`, used for backfilling a specific window. It doesn't bypass the watermark -- a session already harvested at or after its current `UpdatedAt` is still skipped even if it falls inside `--since`.
3. Its `UpdatedAt` is newer than what the watermark has on record for its id (or it has no watermark entry at all).

Candidates that survive are processed oldest-updated-first and capped to `--max-sessions` (default 20) per run, so a very large backlog makes steady, bounded-cost progress across repeated runs rather than one enormous, expensive first invocation.

`--dry-run` still makes the real LLM call(s), so the preview reflects real cost, but writes nothing to the brain and does not advance the watermark -- a dry run never counts as "harvested," and a real re-run afterward sees exactly the same candidates.

Watermark timestamps are stored with sub-second precision (RFC3339Nano), not truncated to the second. This matters for the direct-mirror sources, whose `UpdatedAt` is a file's on-disk mtime: a watermark truncated to whole seconds would read as "older" than the file's own mtime forever, and every run would re-mirror it.

Meetily fits the same watermark file/shape but with a different id source: Codex keys by session id and Gemini CLI sessions by `"gemini-cli:<sessionId>"`, both carrying their own `updated_at`/`lastUpdated` field from the source JSON. Meetily calls have no such field, so the id is the call folder's name and `UpdatedAt` is the newest mtime among the folder's four files (`transcripts.json`, `summary.json`, `meeting.json`, `metadata.json`) -- whichever are present. This is why the quiet window and watermark-advance semantics above ("its `UpdatedAt` is older than 10 minutes ago", "newer than what the watermark has on record") apply identically to Meetily: a call folder whose newest file changed in the last 10 minutes reads as still in progress, exactly like a Codex/Gemini session that's still being written.

## Hook wiring (`aida setup`)

`aida setup` installs two small detached scripts, mirroring `capture-memory.sh`'s pattern exactly (drain stdin, `nohup ... &` in a background subshell, always exit 0):

- `~/.aida/hooks/harvest-codex.sh` runs `aida brain harvest --tool codex`, wired into `~/.codex/hooks.json`'s `Stop` event.
- `~/.aida/hooks/harvest-gemini.sh` runs `aida brain harvest --tool gemini`, wired into `~/.gemini/settings.json`'s `AfterAgent` event.

Unlike the Claude Code capture hook, which only detects whether it's wired and prints a snippet to merge by hand (because `~/.claude/settings.json` is large and externally managed), these two files are written directly. Both `hooks.json` and `settings.json` are structured JSON with a top-level `"hooks"` object mapping event name to an array of hook-group objects, which is mergeable safely: `mergeAdditiveHook` (`internal/cli/setup_harvest_hooks.go`) parses just enough to append one new hook-group under the target event, while every other top-level key (Gemini's `security`, `general`, ...) and every other event's hook-group array (including hooks this command didn't install, e.g. supacode's `SessionStart`/`UserPromptSubmit`/`BeforeAgent`/`AfterTool` entries) round-trips untouched.

Idempotency is a substring check: if a hook-group already present under the target event contains the harvest script's path, `aida setup` treats it as already wired and makes no changes. Re-running `aida setup` therefore never duplicates the entry, never reorders existing hooks, and never drops anything.

A missing `~/.codex` or `~/.gemini` directory is a no-op, not an error -- either tool may simply not be installed on a given machine.

Meetily has no equivalent hook here -- it isn't a coding agent and has no Stop/AfterAgent-style event system to wire into. `aida serve --harvest-sweep` (see `internal/cli/harvest_sweep.go`) is its only automatic trigger; otherwise `aida brain harvest --tool meetily` has to be run by hand.

## Recall

No new recall code was needed: `Brain.RecallMemories` already took a `profile` parameter, and `profile == "all"` already disabled the profile filter. What changed is discoverability:

- `aida brain recall --profile codex` / `--profile gemini` / `--profile meetily` / `--profile all`
- MCP `brain_recall` tool, `profile: "codex" | "gemini" | "meetily" | "all"` argument
- `aida brain harvest`'s own printed report after each run

## Backfilling

The watermark makes every run incremental by default, so a fresh install starts from nothing. To backfill an existing session history:

```bash
aida brain harvest --tool codex --since 2026-01-01T00:00:00Z --max-sessions 50
aida brain harvest --tool gemini --since 2026-01-01T00:00:00Z --max-sessions 50
aida brain harvest --tool meetily --since 2026-01-01T00:00:00Z --max-sessions 50
```

Repeat (raising `--max-sessions` or just re-running) until a run reports 0 selected -- the watermark ensures nothing already harvested gets reprocessed, so it's safe to run as many times as needed.

## Privacy note

Raw transcripts never leave the local machine and are never written into the brain git repo. Only the LLM-distilled output -- a handful of short, deliberately curated fact/instruction/event records per session -- enters the brain, exactly like every other memory record. The LLM call itself sees the (capped, tool-noise-stripped) transcript text as part of its prompt, same as any other `aida` LLM call; no new data leaves the machine that wasn't already leaving it for other `aida brain` operations that call the same configured model.

The Antigravity and `GEMINI.md` mirror paths make no LLM call at all -- those records are copied verbatim from files Gemini/Antigravity already wrote to disk.

## Known limitations

- Gemini CLI session scope is an opaque `projectHash`, not a readable path, because no reverse mapping exists on disk (see "Gemini" above). Recall by `--scope project:gemini-<hash>` still works; it's just not a friendly name.
- Antigravity artifacts whose conversation id isn't present in `conversation_summaries.db` (deleted, or the db was pruned) fall back to global scope rather than being dropped.
- The Codex rollout locator walks `~/.codex/sessions/**` by filename suffix rather than indexing it once and caching the index; on a very large multi-year session history this could get slow. `--max-sessions` bounds how many rollout files get read per run regardless.
- Meetily records are always `scope: global` -- calls aren't tied to a project working directory the way coding-agent sessions are, so there's no equivalent of Codex's `cwd`/Gemini's `projectHash` to derive a project scope from.
- Meetily's proper-noun correction only fires when `<calls-root>/GLOSSARY.md` exists; without it the prompt still asks the LLM to correct obviously garbled names on its own judgment, but with no glossary to check against, some STT noise (an unfamiliar name misheard) can end up in a distilled record uncorrected.
