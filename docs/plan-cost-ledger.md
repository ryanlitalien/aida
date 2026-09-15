# Plan: Persistent Cost Ledger

Status: **proposal** - not yet implemented. Branch: `feat/cost-ledger`.

## Summary

Make aida's LLM spend visible across sessions. Today every `aida` invocation builds a fresh `llm.CostTracker` in memory, prints a one-line summary at the end, and throws it away on exit. Nothing is persisted, the PR #29 orchestrator bypasses the tracker entirely, and cache-read/cache-write tokens - which materially change the real bill on Sonnet - are neither recorded nor priced.

This plan adds a single append-only JSONL file at `~/.aida/costs.jsonl`, routes both `internal/llm.Client` and `internal/engine/orchestrator/model.LLM` through one record path, teaches the pricing table about cache tokens, and adds `aida costs` for queries.

No Node. No Postgres. No daemon. One file, one package, one subcommand.

## Non-goals

- **Budget enforcement / throttling.** That's Paperclip's moat and needs persistent per-agent policies, incidents, a scheduler to block on. If wanted later, a read-only `aida costs --budget 50 --warn` that prints a warning past threshold is additive. Real enforcement is out of scope.
- **Multi-user / remote storage.** Single-user, local file. If user #2 shows up, revisit via Paperclip or a hosted backend.
- **UI / dashboard.** `aida costs` text output is enough. A local HTML view is a follow-up, not load-bearing.
- **Retroactive cost repair.** If pricing changes, the ledger shows what was recorded then. A separate tool could replay-reprice; out of scope here.
- **Streaming cost accrual.** Record on completion only. The orchestrator's `model.LLM.Complete` is one-shot; no streaming to instrument.

## Current state

- `internal/llm/cost.go` - `CostTracker`. In-process, session-scoped. `Record(stage, *Response)` computes USD from `LookupPricing(model)` × `InputTokens/OutputTokens`. No cache fields.
- `internal/llm/client.go` - every `Complete*` method records into `c.tracker` before returning. Good. `CostSummary()` prints a one-liner.
- `internal/llm/models.go` - `BuiltinPricing` covers haiku/sonnet/opus/gpt-4o/gemini. Two fields: `InputPerM`, `OutputPerM`. Cache not priced.
- `internal/engine/orchestrator/model/model.go` - separate `LLM` interface with a `Usage{InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens}` struct. Tracks cache tokens. **Bypasses `CostTracker` entirely** - the Anthropic and OpenAI adapters call the SDK directly and return `*Response`; nothing records.
- `~/.aida/` - already aida's config home; `brain/`, `lessons/`, `tasks.json` live alongside.

## Design

### Storage: JSONL

```
~/.aida/costs.jsonl
```

One record per LLM completion. Append-only. Line-oriented so `tail`, `grep`, `jq` all work without tooling. Rotation: when the file exceeds 10 MB, rename to `costs.<YYYY-MM>.jsonl` on next append and start fresh. No compaction, no indexing - JSONL at ~200 bytes/record holds ~50k calls before rotating, which is many months of single-user activity.

One record shape:

```jsonc
{
  "ts":              "2026-04-20T14:32:11.412Z",  // RFC3339 nano
  "invocation_id":   "01HX...",                   // ULID, one per aida invocation
  "profile":         "work",                      // from AIDA_PROFILE
  "provider":        "anthropic",                 // "anthropic" | "openai" | "ollama"
  "model":           "claude-haiku-4-5-20251001",
  "stage":           "synthesize",                // "parse"|"route"|"execute"|"synthesize"|"quality"|"agent-turn"|""
  "caller":          "engine",                    // "engine" | "orchestrator" | "investigate"
  "input_tokens":    1240,
  "output_tokens":   380,
  "cache_read":      820,
  "cache_write":     0,
  "cost_usd":        0.0041,
  "duration_ms":     612,
  "command":         "query"                      // cobra command name, for breakdowns
}
```

`invocation_id` groups the 3+ LLM calls a single `aida query` makes so `aida costs --invocation <id>` can show per-query cost. Generated once per process at startup and injected into both tracker paths.

### Package: `internal/llm/ledger`

New package, isolated from `llm.Client` so the orchestrator can import it without pulling in legacy wrapper code:

```
internal/llm/ledger/
  ledger.go      // Ledger: Open, Append, Close
  record.go      // Record struct + JSON tags
  rotate.go      // size-based rotation
  ledger_test.go
```

Key type:

```go
type Record struct { /* fields above */ }

type Ledger interface {
    Append(r Record) error
    Close() error
}

func Open(path string) (Ledger, error)   // file-backed, line-buffered, fsync-on-close
func OpenDiscard() Ledger                // no-op for tests / --no-costs
```

Pointed from env: `AIDA_COSTS_PATH` overrides `~/.aida/costs.jsonl`. Setting it to `/dev/null` or empty string disables persistence. A `--no-costs` flag plumbed on the root command also selects `OpenDiscard()`.

### Pricing: cache-aware

Extend `ModelPricing` in `internal/llm/models.go`:

```go
type ModelPricing struct {
    InputPerM     float64
    OutputPerM    float64
    CacheReadPerM  float64  // typically 0.1× input
    CacheWritePerM float64  // typically 1.25× input
}
```

Update the haiku/sonnet/opus entries with the published cache multipliers. `LookupPricing` signature unchanged. Cost formula becomes:

```
cost = input/1M*In + output/1M*Out + cacheRead/1M*CRead + cacheWrite/1M*CWrite
```

For providers that don't report cache tokens, the extra terms are zero - identical to today.

### Wiring

**Legacy path** (`internal/llm/client.go`): `CostTracker.Record` already runs on every completion. Add a second line: `ledger.Append(recordFromCallCost(...))`. One record per `CallCost`. `invocation_id` stored on `Client` at construction. No changes to call sites.

**Orchestrator path** (`internal/engine/orchestrator/model/anthropic/anthropic.go` and `.../openai/openai.go`): the `Complete` method returns `*model.Response`. Wrap the adapter in a `Recording` decorator in `internal/engine/orchestrator/model/recording.go`:

```go
func Recording(inner model.LLM, led ledger.Ledger, caller string) model.LLM
```

`Runner.New` takes an optional `Ledger`; when set, it wraps the supplied `LLM` before use. Keeps the core adapters pure. Stage is set from `Runner` context (`agent-turn`); exact agent name is available on `Response` via a thin extension or on the runner's own per-turn scope - detail deferred to implementation. Record is emitted after every successful `Complete`.

The decorator must run even on error so partial-usage completions are captured. Provider APIs usually return usage even when the completion itself errors mid-flight; honor that.

### CLI: `aida costs`

New file: `internal/cli/costs.go`.

```
aida costs                          # current month summary: total, by-model, by-stage
aida costs --days 7                 # rolling window
aida costs --from 2026-04-01        # absolute range
aida costs --profile work           # filter
aida costs --model claude-haiku-4-5 # filter
aida costs --invocation 01HX...     # per-query breakdown
aida costs --group-by day           # day|model|profile|stage|caller
aida costs --raw                    # stream JSONL unchanged (composable with jq)
aida costs --tail                   # follow mode for long-running investigate
aida costs --json                   # machine-readable summary
```

Reads `~/.aida/costs.jsonl` + any rotated siblings. Streaming reader; does not load the full file. For `--tail`, inotify/kqueue is overkill - poll `Stat()` every 1s.

Default output:

```
April 2026 · profile=work
  Total:        $4.82 across 312 calls
  By model:
    claude-haiku-4-5-20251001      $1.14   (214 calls)
    claude-sonnet-4-6               $3.68    (98 calls)
  By caller:
    engine          $2.91  (236 calls)
    orchestrator    $1.70   (62 calls)
    investigate     $0.21   (14 calls)
  Cache hit rate:   41% of input tokens served from cache
```

### Concurrency

Aida already runs the 6 sources in parallel under `errgroup`. The orchestrator's `ParallelAgent` adds another fan-out axis. The ledger must be safe under concurrent `Append`:

- `Ledger.Append` takes a mutex, writes the line, flushes.
- One `Ledger` instance per process, shared. Not one per goroutine.
- No fsync per append - fsync on `Close()` only. Crash loses the last few records; acceptable for a single-user ledger.

### Observability of the ledger itself

If `Open` fails (disk full, perms), log a single warning and fall back to `OpenDiscard`. Never block a query on ledger failure. A `aida costs --doctor` flag diagnoses the file (exists? writable? readable? parses?).

## Phasing

Six commits, each independently green. Same shape as PR #29.

| Phase | Commit scope | Verified by |
|---|---|---|
| 1 | `internal/llm/ledger` package - `Record`, `Ledger`, `Open`, `OpenDiscard`, `Append`, `Close`, rotation. No integration yet. | `go test ./internal/llm/ledger/` |
| 2 | Cache-aware `ModelPricing` + `LookupPricing` update. Legacy `CostTracker.Record` accepts `Usage` and records cache fields on `CallCost`. Summary string updated. | Existing `llm` tests updated; new test covers cache math |
| 3 | Wire `ledger.Ledger` into `llm.Client`. Generate `invocation_id` at `NewClient`. Every `Record` also calls `ledger.Append`. `AIDA_COSTS_PATH` / `--no-costs` plumbed. | End-to-end: run a query, assert records on disk |
| 4 | `Recording` decorator for `model.LLM`. `Runner` takes optional `Ledger`. Orchestrator path now records. | Unit test with in-memory ledger asserts N turns → N records |
| 5 | `aida costs` subcommand. All flags above. Streaming read. | Fixture ledger + golden snapshot per flag |
| 6 | Docs update: `CLAUDE.md` section on costs, `aida costs --help` examples. MEMORY.md pointer. | N/A - doc only |

## Test plan

- Unit: ledger append/rotate, pricing math with cache tokens, record marshaling, CLI flag parsing.
- Integration: one `aida query` writes exactly N records where N = number of LLM calls that ran; each record carries the same `invocation_id`.
- Orchestrator: a 3-turn agent run produces 3 orchestrator records plus any sub-agent records, all correlated.
- Concurrency: 100 goroutines × 100 appends = 10000 well-formed lines, no interleaving.
- Failure: `AIDA_COSTS_PATH=/nonexistent/nope` - query still succeeds, single warning logged.
- Rotation: fixture at 10 MB + 1 byte rotates cleanly; both files readable by `aida costs`.

## What this doesn't solve

Honest limits worth calling out:

1. **Ollama is free but records `cost_usd: 0`.** Tokens still logged for call-count stats.
2. **Cached tokens are provider-reported.** If the provider under-reports, aida under-bills. Matches actual invoice.
3. **Record writes are post-hoc.** A process killed mid-completion records nothing for that call. The in-flight completion was already billed by the provider. Small leak; acceptable.
4. **No per-call prompt/response content.** Deliberate - privacy and ledger-size both push toward metadata only. If debugging needs content, `brain/` or a separate debug log is the right place.
5. **No reconciliation against Anthropic's billing dashboard.** Aida's numbers are estimates from published prices × self-reported tokens. Real invoice is authoritative.

## Open questions

- Should `invocation_id` be a ULID (sortable, time-prefixed) or a short random string? ULID recommended so `sort costs.jsonl` is chronological without parsing timestamps.
- Does the orchestrator need a `session_id` separate from `invocation_id`? Probably yes once sub-agent graphs get deep - defer to phase 4.
- Should `aida tasks done` record an optional cost annotation from the session that resolved it? Nice-to-have; not in scope.

## Follow-ups (not this PR)

- `aida costs --watch` as a live Vim-friendly status strip for long `investigate` runs.
- Budget *warning* (`AIDA_BUDGET_MONTHLY=50`) that prints once past threshold. Still no enforcement.
- Per-profile monthly roll-up posted to `brain/` so cost trends become memory-searchable.
- If a user #2 ever appears: swap `ledger.Ledger` for a Postgres-backed implementation behind the same interface. Paperclip-style, but only then.
