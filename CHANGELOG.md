# Changelog

This project versions from `v1.0.0`. Every PR that merges to `main` adds its entry under a heading for the version it will become once merged, computed as the latest `v1+` tag's minor plus one (`v1.0.0` if no `v1+` tag exists yet). CI's `changelog` job enforces this: a PR fails unless `CHANGELOG.md` changed and contains a `## vX.Y.0` heading matching that computed next version, followed by at least one `- ` bullet.

If another PR merges first and claims the version heading you were targeting, rebase onto `main` and bump your heading to the new next version before merging.

On every merge to `main`, the `release` job re-computes the same next version, tags it, and publishes a GitHub release using that version's changelog section as the release notes.

## v1.13.0

- The models roster (`~/.aida/models.yaml`, `internal/models/roster.go`) gains an optional per-provider `retired:` list of model ids. A retired id still counts as known for every unlisted-model diff a probe runs against a local tool cache (Codex's `models_cache.json` via `unlistedCodexModels`, agy's `agy models` via `agyModelsUnlisted`, including `-high`/`-medium`/`-low` effort-suffix stripping), so dropping a superseded version from the active roster doesn't make `aida models` start flagging it as "unlisted locally, not in roster" every time a warm local cache still lists it. Retired ids are otherwise invisible everywhere else: `Provider.Models` never contains them, so `Resolve` never attaches a nickname to one, and they never appear in `aida models`, `aida models --json`'s `models` arrays, or the dashboard's Models panel. `examples/models.yaml` documents the shape.
- The `/dashboard` Models panel now draws every usage bar as a bullet bar instead of a thin accent-colored progress bar: a flat track with a shaded 90%+ zone at the far end, a thin fill in a hue fixed per provider (roster order, never cycled), six labeled day ticks (`d1`..`d6`) on any 7-day window, and a pace marker at how far through the window the clock is, so fill left of the marker reads as under pace at a glance; the marker now draws on every bar that has a real reset time and window length, not just ones with a computed pace verdict, so 5-hour bars and idle 7-day bars (e.g. a 0%-used Gemini week) show the clock position too. The meta column is a bold used% (red at 90%+) followed by the reset as a short local absolute time ("resets Fri 2:00pm", or "idle" for a zero reset time); the ratio/IDLE verdict and its detail moved into the track's tooltip. LiteLLM spend rows use the same track with spend/budget as the fill and "$x / $y" in the meta. A 0-100% scale row and a one-line key sit under the provider cards. Both themes carry their own series and track inks.
- The Anthropic OAuth usage probe (`internal/models/probe_claude.go`) now parses the usage payload's `extra_usage`, `spend`, and `seven_day_breakdown` fields and surfaces two new lines under each Claude provider, in `aida models`, `--json`, and the dashboard's Models panel: a `usage credits` line reporting whether Anthropic's purchasable usage credits (there is no free reset, unlike Codex's `reset_credits`) are on with a dollar amount and monthly cap, off by choice, or never enabled, plus a `(can buy)` suffix and balance/spend-limit detail where applicable; and a `7-day by surface` line breaking the account's 7-day usage down by Claude Code / Chats / Cowork / Other. Both are omitted entirely when the payload doesn't carry the corresponding field.

## v1.12.0

- README gained a "The shape around it" section: a table naming the five kinds of private repo that sit around the public core (config, memory, knowledge, fleet, clients), what each holds, why each stays private, and which aida surface reaches it. The point is the separation rather than the author's particular repos, so a reader can see how to keep their own machines, data and credentials out of anything they publish while still running the same binary.

## v1.11.0

- The autonomous loop no longer hardcodes `origin` as the remote a project lives on. At the start of a `--worktree` / `--pr` run it now asks git which remote the `--base` branch tracks (`worktree.ResolveUpstream`, reading `branch.<base>.remote` and `branch.<base>.merge`) and resolves that once into the loop options, so worktree creation (`internal/cli/loop.go`), the `--pr` push (`internal/cli/pr.go`), and the review-panel diff all use the same remote and remote-tracking ref (e.g. `public` / `public/main`) and cannot drift. Previously `--base` never reached worktree creation at all (even `--base release` branched off `origin/main`), the PR push went to a literal `origin` and then `gh pr create` failed on a branch GitHub had never seen, and the review panel diffed against `origin/<base>` regardless of what the base branch tracks. A base branch with no upstream, or a repo with no remotes, falls back to `origin` / `origin/<base>`, so any single-remote project behaves exactly as before; a base tracking a local branch (`.` remote) cuts from that branch and refuses to push.
- `worktree.Create` now pre-fetches whichever configured remote the base ref names (checked against `git remote`) rather than only when the ref starts with `origin/`, so a worktree cut off `public/main` or `upstream/release` starts from the fresh remote tip too. The fetch stays best-effort and an offline failure still falls back to the local ref.
- `aida loop --pr` now fails fast, before any push, in a repo whose base branch tracks a public GitHub remote while some other remote is a non-GitHub mirror (a review-on-the-forge-first layout). The error names both remotes and their URLs and says `--pr` is not supported there yet; use `--commit` or push the `auto/` branch to the mirror by hand. Opening the PR on the mirror is a separate feature.
- Running `aida loop --worktree` outside a git repository now fails the run up front instead of parking every task on hold one at a time.

## v1.10.0

- Bumped `go.opentelemetry.io/otel/sdk` from 1.43.0 to 1.45.0, which carries `otel`, `otel/trace`, and `otel/metric` to 1.45.0 alongside it, plus the indirect `github.com/go-logr/logr` 1.4.4 and `golang.org/x/sys` 0.47.0 that those modules require. No API surface aida uses changed, and the full test suite passes unmodified.

## v1.9.0

- Scrubbed the personal and private nouns that test fixtures, testdata, doc examples, prompt examples, and code comments had been using as stand-ins: a real campground business is now the fictional Pine Hollow Campground (slug `pine-hollow`), a real company and its GitHub org/repo are now the Acme Widgets family (`acme-widgets`, `acme_widgets`, `acmewidgets`), and a real first name is now Ralph. Fixtures that exercise slug splitting, token compaction, and artifact-id matching keep the same two-word hyphenated shape so those tests still cover what they did. No behavior change: the `aida fleet start` `--cwd` default, the whisper vocabulary prompt, and the live golden query suite are untouched because they must match the machine they run on.
- Curated knowledge pages under `brain/knowledge/domains/` are now part of the searchable corpus. `aida brain index` (and the staleness-triggered background rebuild) used to only count those files for its summary line without reading, embedding, or inserting a single one, so `brain_search` could never return them. Every `*.md` page in that directory, both generated source profiles and hand-written prose pages with no `Keywords:` line, now lands in a new `knowledge_pages` table plus a `knowledge:<slug>` row in the shared FTS corpus (doc type `knowledge:domain`), indexed incrementally by mtime with best-effort embeddings, pruned when a file disappears, and re-embedded by `aida brain reembed` alongside the other tables. The `domains` figure in the index summary now reports what was actually indexed. The planner's keyword-routing read of the same files is unchanged.
- The MCP `brain_search` tool and `aida brain search` now return matching knowledge pages (a new `## Knowledge Pages` section with the page body), and the agent's multi-channel `SearchMulti` gains a `knowledge` vector channel next to `wiki`, with keyword hits arriving through the shared FTS corpus.
- The brain staleness check now watches `knowledge/domains/` rather than all of `knowledge/`, so editing `knowledge/routing/` or `knowledge/patterns/` (which the index never reads) no longer triggers a full multi-minute re-embed, while a batch of new domain pages still does, and that rebuild now picks them up.

## v1.8.0

- Removed the last references to a specific third-party Claude Code hook manager from the repo. The additive-merge behavior is unchanged - `aida setup` still merges its harvest hooks into `~/.codex/hooks.json` and `~/.gemini/settings.json` so anything another tool already wrote survives byte-for-byte, and still refuses to write `~/.claude/settings.json` at all - but the comments, docs, and test fixtures now describe that tool generically instead of by name, and the merge tests simulate a fictional "othertool" rather than a real product.

## v1.7.0

- Documented the memory stack's L4 tier, which the docs had reduced to one table row reading "Planned": the README's memory section now shows a path per tier, corrects the L4 row (the consolidation pass shipped as `aida brain consolidate`), and explains why the wiki is a separate repo rather than another directory under `~/.aida/brain/` - it carries raw archive corpora and human-reviewed prose, and material graduates into it only once it stops changing - along with the OKF v0.1 format, the `wiki.path` config key, and the `aida wiki index` / `aida wiki lint` commands, which the Commands block had never listed.
- INSTALL.md's backup section now covers the wiki as a third optional directory and says plainly that a private git remote is the lowest-friction route rather than a requirement: an rsync to a local server or NAS, or a Time Machine target, backs up a directory of plain files just as well. The privacy warning extends to the wiki, which holds the same class of material as the brain.
- `aida wiki --help` now names the `wiki.path` config key instead of presenting its default as the path, and the wiki plan doc is scrubbed of owner-specific references and renamed to `docs/plan-wiki-integration.md`. Help strings and comments only; no behavior change.

## v1.6.0

- Roster `subagent` entries can now pin the Claude model their `claude --print` transport runs on via a new `subagent.model` field, passed through verbatim as `--model <value>` (an alias like `sonnet`/`opus`/`haiku`/`fable`, or a full model id), so cheap reporting personas can run on a lighter model instead of inheriting Claude Code's configured default; leaving it unset keeps the old inherit-the-default behavior.

## v1.5.0

- Removed the open-sourcing plan, launch plan, and fresh-start cut runbook from the public tree (they documented the publication process itself, not the architecture), and scrubbed remaining hostnames and personal notes from the system-map report, the memory-bridge design doc, and the burndown example.

## v1.4.0

- Fixed profile `serve.lmd_whisper_model` to properly expand tilde (`~`) in paths before passing to whisper-cli, allowing paths like `~/.aida/jarvis/models/ggml-small.en.bin` to work as documented.

## v1.3.0

- `stt.Whisper.Transcribe` now auto-sizes whisper.cpp's `-ac`/`--audio-ctx` flag from each clip's own duration instead of always encoding the full 30-second window, cutting transcription time on CPU-only hosts (measured 8.0s to 3.3s for a 3s clip on a 2-core Pentium); override via the new `AudioCtx` field (`-1` for full context, a positive value for a fixed override).

## v1.2.0

- LMD (Android client) turns now default to whisper.cpp's tiny.en model instead of small.en, overridable via profile `serve.lmd_whisper_model`, since the LMD daemon may run on weak/headless hardware where small.en's transcription time is prohibitive.
- LMD turns are now instrumented per-stage (`stt_ms`/`llm_ms`/`tts_ms` in the `/lmd/v1/turn` response, a `📱 LMD turn: ...` log line, and an audit record tagged `source: "lmd"`), matching the desk-mic listener's observability.

## v1.1.0

- Plan doc status reflects the 2026-09-15 fresh-start cut.
- Dropped the @claude and auto-review workflows from the public repo; they need a secret this repo does not carry and would let any commenter start an agent run.

## v1.0.0

- Initial public release, cut from the private development repo.
- Main is now PR-only with a changelog gate: every PR must add an entry under the next version heading.
- Every merge to main automatically bumps the minor version and publishes a GitHub release from that changelog section.
