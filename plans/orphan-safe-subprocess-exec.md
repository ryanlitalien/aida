# Plan: orphan-safe subprocess exec

## Context

A `aida` query hung indefinitely because a `moredoor-chromeext` claude-project subagent, running with `--dangerously-skip-permissions`, spawned `node server.js` in the background. The subagent then exited, leaving node reparented to launchd - but node had inherited aida's stdout/stderr pipes. Go's `cmd.Run()` captures via `bytes.Buffer`, which blocks until *all* writers close the pipe, not just the direct child. Node never exits on its own, so `Wait()` parked forever. The 120s `claudeProjectTimeout` (`internal/sources/claude_project.go:19`) fired and killed the direct child, but did not kill grandchildren, so the hang persisted.

This is a general bug, not a moredoor-chromeext bug: any adapter that shells out and captures output via `bytes.Buffer` or `CombinedOutput` is exposed to the same wedge. Fix it once at the exec layer; don't whack-a-mole per source.

**Intended outcome:** no aida query can hang past its declared per-source timeout because a subprocess (or grandchild) held pipes open.

## Approach

Two-layer defense applied everywhere we shell out, centralized in one helper so adapters cannot reintroduce the bug:

1. **Process-group containment.** Every child gets its own PGID via `SysProcAttr.Setpgid=true`. On context cancel, SIGTERM the whole group (negative pgid), then SIGKILL on a short escalation timer. Kills grandchildren, not just the direct child.
2. **`cmd.WaitDelay` backstop.** A 3s grace window that only matters *after* ctx has cancelled or the child has already exited. Go then forcibly closes our pipe fds and returns from `Wait()` even if a grandchild is still holding them. It does **not** cap the command's runtime - a legitimately long `snow`/`claude` command is unaffected until its own timeout fires.

Precondition confirmed: `go.mod` declares Go 1.25.7 (WaitDelay requires 1.20+). `syscall.SysProcAttr{Setpgid: true}` precedent already exists in `internal/brain/sync.go:133`, so we're not introducing a new dependency.

## New file

**`internal/execx/run.go`** - the helper. Split unix-specific bits into `run_unix.go` (build tag `//go:build unix`) with a no-op windows stub, matching the project's existing portability posture (no current windows support, but don't poison it either).

Public surface:

```go
type RunOpts struct {
    Timeout     time.Duration // total per-command ceiling; 0 = inherit ctx only
    WaitDelay   time.Duration // default 3s
    Dir         string
    Env         []string
    MaxOutBytes int64         // bounded capture; default 4 MiB
}

type Result struct {
    Stdout   []byte
    Stderr   []byte
    Combined []byte        // populated when RunCombined used
    ExitCode int
    TimedOut bool
    Duration time.Duration
}

func Run(ctx context.Context, name string, args []string, opts RunOpts) (Result, error)
func RunShell(ctx context.Context, shellCmd string, opts RunOpts) (Result, error)     // wraps "sh -c"
func RunCombined(ctx context.Context, name string, args []string, opts RunOpts) (Result, error) // for agent_tools/skills
```

What it encodes internally:
- `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`
- `cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }`
- `cmd.WaitDelay = opts.WaitDelay` (default 3s)
- After WaitDelay fires, a follow-up `syscall.Kill(-pgid, SIGKILL)` via a goroutine armed on ctx cancel - belt-and-suspenders in case SIGTERM isn't honored.
- Stdout/stderr captured through `io.LimitWriter` over `bytes.Buffer` so a runaway subprocess can't OOM aida. `MaxOutBytes` exceeded → truncate + flag in Result.
- Distinguishes `TimedOut=true` (ctx deadline exceeded) from other exit failures so callers can report `status: "timeout"` vs `status: "error"`.
- Verbose log when group-kill actually reaps orphans: `ui.PrintVerbose("Exec cleanup", "killed N orphan(s) in pgid X for <name>")` - so we can tell when this defense is firing.

## Migrate existing call sites

All 10 call sites currently invoke `exec.CommandContext` directly. 8 capture via `bytes.Buffer`; 2 use `CombinedOutput` (same pipe-wait pitfall). Each migration is a mechanical collapse to a single `execx.Run*` call + preserving the adapter's existing error/status mapping.

Priority 1 (the proximate cause + highest blast radius):
- `internal/sources/claude_project.go:61` - spawns `claude`, which in this bug spawned `node`.

Priority 2 (generic adapters - any source using them gets the fix):
- `internal/sources/exec.go:58` (`ExecAdapter` - generic shell templates)
- `internal/sources/snowflake.go:65`
- `internal/sources/chrono.go:50`
- `internal/sources/notion.go:41`
- `internal/sources/grep.go:125` (also has local `runShell()` at lines 124-131 - delete it, use helper)
- `internal/sources/git.go:124`
- `internal/sources/sqlite.go:79`

Priority 3 (engine-level exec):
- `internal/engine/agent_tools.go:232` - claude agent loop, same hang risk as claude_project.
- `internal/engine/skills.go:94` - skill runner, `sh -c`.

Out of scope for this change (migrate separately if desired):
- `internal/brain/sync.go`, `internal/brain/tasks.go`, `internal/library/scan.go`, `internal/cli/library.go` - short git/gh metadata calls, real hang risk is low.
- `internal/cli/feedback.go:410`, `internal/cli/tasks.go:191/327` - editor invocations, need terminal inheritance.
- `internal/cli/investigate.go:567-569` - `open`/`xdg-open`, fire-and-forget.
- `cmd/golden-log/main.go` - test harness.

Per-source timeout constants (`claudeProjectTimeout = 120s` etc.) are preserved - they just get passed through `RunOpts.Timeout`. No timeout policy change.

## Tests

**`internal/execx/run_test.go`** - three cases covering the regressions we care about, each writing a small shell script to `t.TempDir()` (no existing subprocess-test pattern in the repo, so establish a clean one):

1. **Happy path** - `sh -c 'echo hi'`, expect `Stdout="hi\n"`, `TimedOut=false`, `ExitCode=0`.
2. **Direct-child timeout** - `sh -c 'sleep 60'` with `Timeout=500ms`. Expect `TimedOut=true` and `Duration < 1s`.
3. **Orphaned-grandchild timeout (regression test for this bug)** - `sh -c 'sleep 60 & disown; sleep 60'` with `Timeout=500ms`. Capture the grandchild's pid (echo `$!` from a slightly richer script; parse from Stdout). Assertions: `Run` returns in `< (Timeout + WaitDelay + 1s margin)`; grandchild is dead (`syscall.Kill(pid, 0)` returns ESRCH) after `Run` returns.

Skip (3) on non-unix via build tag - matches helper layout.

## Verification end-to-end

After migration:

1. `make test` - existing tests still pass; new `execx` tests pass.
2. `make vet` - clean.
3. `make install`, then rerun the failure case that prompted this:
   `aida "can you show me if I have any open PRs for the mongo db app and the moredoor chrome extension? Also check if there any non-committed files that need to be added"`
   - Expect completion within the 120s subagent timeout, not a hang.
   - If the subagent again spawns `node server.js`: `ps axo pid,pgid,command | grep node` from a separate shell after the query returns should show **no leftover node process** in aida's former pgid. Use `aida --verbose` to look for the `Exec cleanup` log confirming orphan reap.
4. Manual sanity on a normal query that doesn't trigger the bug (e.g. a Snowflake-routed question) - timing and behavior unchanged.

## Rollout

One PR on a new branch (e.g. `orphan-safe-exec`), two commits to keep the review readable:

- **Commit 1:** `internal/execx/` package + tests + migrate `claude_project.go` only. Proximate fix plus the concrete regression test - this commit on its own would resolve the reported hang.
- **Commit 2:** Migrate the remaining 9 call sites (priorities 2 and 3). Each file is <10 lines changed; safe to batch because commit 1 already exercised the helper.

## Judgment calls (decided, flagging in case you disagree)

- **WaitDelay default = 3s.** Long enough to flush buffered output, short enough that a legitimately-timed-out command returns promptly. Per-call override available via `RunOpts.WaitDelay` if some adapter needs different.
- **MaxOutBytes default = 4 MiB.** Existing adapters have no bound today - a pathological `grep -r` or verbose `claude` response could grow unbounded. 4 MiB is generous for real answers and small enough to not hurt.
- **Orphan-reap events logged at verbose tier, not warn.** Users running with `--verbose` will see "killed N orphan(s)"; everyone else won't. Loud enough to diagnose, quiet enough to not alarm.
- **`--dangerously-skip-permissions` stays.** Whether aida should sandbox subagents more tightly is a separate discussion - this change makes subagents safe to misbehave without hanging us, which is the necessary precondition for that discussion anyway.
