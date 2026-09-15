# /dashboard and /bifrost on the 1610 daemon

## Context

Two HTML artifacts were produced in earlier sessions: a household network survey
(`192.168.40.0/24`, nine computers, tailnet vs LAN planes) and an aida system map + gap
analysis (six-step pipeline, runtime topology, subsystem scorecard). Both are static
snapshots that went stale the moment they were written, and neither lives in the repo.

The ask is to fold them into one **live** page served by the always-on `aida serve`
daemon: one place to see how a query flows through the system, what infra exists, which
agents are available, and what jobs are running on each. Plus a second page, `/bifrost`,
to drive a herdr session on minty from the browser instead of dropping to
`ssh -t minty 'sudo -iu heimdall herdr'`.

Both pages are explicitly **tests** - the UI is throwaway, to be reworked later. What has
to be right is the data plumbing and the safety of the remote-execution path.

This closes the *visibility* half of the fleet-awareness gap. Issue **#102** covers the
*memory* half (teaching aida's brain about the fleet). Complementary, not duplicates.

### Two facts that shape the whole design

**1. The daemon is loopback-only.** `aida serve` binds `127.0.0.1:1610`
(`internal/cli/serve.go:289`). This box cannot reach photon's or minty's daemon over the
tailnet, and photon does not run `aida serve` at all -
`scripts/jarvis/com.ryan.jarvis-satellite.plist:20-29` runs `aida jarvis daemon`, which
starts no HTTP server. So the fleet panel probes **out** over SSH rather than polling
**in** over HTTP. Nothing new is exposed on the tailnet.

**2. The pipeline is already instrumented.** I checked: `runs.TracingData`
(`internal/runs/run.go`) records per-span `DurationMs`, `CostUSD`, `InTokens`, `OutTokens`,
`Model`, `Status`, plus run totals, and `Run.Phases[].Sources[]` records which sources
fired with per-source duration and status. There are **746 runs** in `~/.aida/runs/` right
now; the newest carries 29 LLM calls, $0.11, and spans `parse → route → execute×N →
verify → synthesize → quality`. So the flow panel can be a real weighted graph built from
observed runs, not a decorative diagram with fake numbers.

Note a correction worth carrying into implementation: span names are bare `execute`, not
`execute:snowflake`. Per-source attribution comes from `Phases[].Sources[]`, which must be
joined against the spans.

---

## Decisions (confirmed with you)

| Decision | Choice |
|---|---|
| Fleet probe | `tailscale status --json` for reachability + SSH for aida detail. TTL-cached. |
| Bifrost depth | Status board + pane reader + send-text console. No PTY, no xterm.js. |
| Arrow.js | Vendored and `go:embed`ed, served from the daemon. |

---

## Part 1 - Save the two artifacts

New folder `docs/reports/` (docs/ is otherwise flat markdown; these are rendered HTML
snapshots and deserve their own drawer):

- `docs/reports/household-survey-2026-07-19.html` (artifact `dc440c36`)
- `docs/reports/aida-system-map-2026-07-20.html` (artifact `af18d646`)
- `docs/reports/README.md` - index, dates, and what each is a snapshot *of*

Both are already downloaded to the session tool-results directory. Strip two injected
blocks that are claude.ai plumbing, not content:

1. The `<!-- frame-runtime -->…<!-- /frame-runtime -->` script on line 1 of both.
2. In the system map only, the inlined mermaid bundle
   (`<!--claude-mermaid-runtime-begin…-->` … `<!--claude-mermaid-runtime-end-->`,
   lines 614-4093) - **3.2 MB of that file's 3.35 MB**.

Stripping leaves ~75 KB and ~40 KB. The `<pre class="mermaid">` sources survive, so the
diagrams stay readable and re-renderable; say so in the README.

---

## Part 2 - Backend

### Packages

```
internal/remotex/     runner.go   Runner iface, ExecRunner, FakeRunner
                      script.go   BuildRemoteScript, ValidateHost, ValidateTarget, SSHArgs
internal/fleet/       device.go   Device, DeviceStatus, Snapshot
                      prober.go   tailscale + ssh probes
                      cache.go    TTL cache, singleflight, background ticker
internal/bifrost/     client.go   herdr over ssh
                      types.go    herdr JSON shapes (passthrough)
internal/cli/         dashboard_web.go / .html
                      bifrost_web.go / .html
                      vendor/arrow*.mjs
```

Separate packages rather than more files in `internal/cli` for one concrete reason: faking
SSH needs an injectable `Runner`. In `internal/cli` that becomes a package-level var
mutated by tests, which goes flaky under `-race`. As a struct field on `fleet.Prober` /
`bifrost.Client`, every test constructs its own. `internal/remotex` is shared so the
escaping code exists exactly once - two copies would drift and one would become the hole.

### Prerequisite: `execx` stdin support

`execx.RunOpts` (`internal/execx/run.go:40`) has no `Stdin`. Add `Stdin []byte` wired to
`cmd.Stdin` in `runCmd`. Two lines, backward compatible (nil behaves as today), and
load-bearing for the injection defense below. Its own commit.

### Command-execution safety

This is the highest-risk part of the feature and the part I'd want reviewed hardest.

**Argv separation gives you zero protection over SSH.** `ssh host a b c` does not deliver
three arguments - ssh joins its remote argv with spaces into one string and hands it to the
remote login shell. So this is exactly as dangerous as `sh -c`:

```go
// UNSAFE - userText is re-parsed by minty's shell
execx.Run(ctx, "ssh", []string{"minty", "sudo", "-iu", "heimdall",
    "herdr", "agent", "send", target, userText}, opts)
```

**And there is a second shell layer.** `sudo -i` does not exec the command directly; it
runs the target user's *login shell* with `-c` and the joined string. sudo does some
metacharacter escaping in `-i` mode, but its interaction with quoting you added yourself
is not documented well enough to be load-bearing. Nesting quotes through two shell parsers
is how injection bugs get written.

**So: never let user-controlled bytes cross a shell boundary as text.** Deliver the remote
command as a script on ssh's **stdin**, carrying every dynamic value as base64 decoded into
a shell variable on the far side:

```sh
set -eu
a0=$(printf %s 'aGVyZHI=' | base64 -d)
a1=$(printf %s 'YWdlbnQ=' | base64 -d)
a2=$(printf %s 'c2VuZA==' | base64 -d)
a3=$(printf %s 'bXktYWdlbnQ=' | base64 -d)
a4=$(printf %s 'cm0gLXJmIC87IGVjaG8gJCh3aG9hbWkp' | base64 -d)
exec "$a0" "$a1" "$a2" "$a3" "$a4"
```

Why this holds:
1. The only interpolated text is base64, re-validated against `^[A-Za-z0-9+/]*={0,2}$`
   before emitting - belt and braces against a future refactor widening the encoding.
2. The *result* of a `$(...)` substitution is never re-scanned for metacharacters; the
   double quotes at `exec` suppress field splitting and globbing.
3. `exec "$a0" … "$a4"` passes each as exactly one argv element, so `rm -rf /` arrives as
   a literal string - which is what "send this text to the agent" means.
4. Locally: `ssh -T -o BatchMode=yes … -- <host> sudo -iu <user> /bin/sh -s` with the
   script on stdin. Both shell layers see only the fixed string `sudo -iu heimdall
   /bin/sh -s`. No user data reaches either. `-T` matters - a pty would mangle the script.

Supporting guards:
- `SSHArgs` always emits `--` before the host, so a config typo like
  `name: -oProxyCommand=…` can never be read as a local ssh flag.
- `ValidateHost` = `^[A-Za-z0-9][A-Za-z0-9._@-]{0,253}$` (leading `-` excluded by the
  first class). `ValidateTarget` = `^[A-Za-z0-9][A-Za-z0-9._:%-]{0,127}$`, applied *after*
  `r.PathValue` URL-decoding. Rejection is the primary control; escaping is the fallback.
- `-o BatchMode=yes` on every invocation, so a host without a key fails fast instead of
  hanging on a password prompt every cycle forever.
- Cap `send` text at 8 KiB, reject `\x00`, rate-limit to ~5/s.

**Cut freeform `herdr agent start`.** "Browser POSTs an argv, we execute it as another
user on another machine" is a remote shell with a JSON wrapper, and it turns any CSRF
weakness into RCE on minty. Instead, starting a session goes through **config-declared
named presets** - the API accepts only `{"preset": "<name>"}`, never user argv:

```yaml
bifrost:
  presets:
    - name: work-session
      cwd: /home/heimdall/work
      argv: [claude]
```

That still satisfies "connect into bifrost/herdr to run a session on minty", alongside the
phone-session button.

### CSRF

Loopback is **not** a boundary against a browser. Any site the user visits can
`fetch('http://127.0.0.1:1610/api/bifrost/agents/phone/send', {method:'POST', …})`.
Same-origin policy stops it *reading* the reply; it does not stop the request executing.
For an endpoint that types into a live Claude session on another machine, fire-and-forget
is plenty of damage.

`guardLocal` middleware on all dashboard/bifrost routes, four independent checks:
1. **Host pinned** to `127.0.0.1` / `localhost` / `::1` at port 1610 - blocks DNS
   rebinding, which passes every origin check.
2. `Sec-Fetch-Site` must be absent, `same-origin`, or `none`.
3. `Origin`, when present, must be ours.
4. Mutating verbs require `Content-Type: application/json` - this forces a CORS preflight
   we never answer, blocking the simple-request form-POST bypass.

No token/cookie scheme: it needs bootstrap state for no gain on a daemon with no auth
concept.

### Pre-existing exposure: disable `POST /jarvis/ask`

Not introduced here, but it lives on the same daemon. `POST /jarvis/ask`
(`internal/jarvis/server/server.go:47`) lets any website the user visits make the daemon
run an LLM turn with full tool dispatch. Loopback binding does not stop it.

I audited the callers. There is exactly one programmatic caller: the ask form on the Jarvis
status panel at `/` (`server.go:169`), a same-origin fetch. `aida jarvis ask` does **not**
use HTTP - it's in-process (no `http.Post` or loopback URL in `internal/cli/jarvis.go`).
Nothing else in `~/dev`, `~/bin`, `~/.aida`, or Shortcuts calls it; `.claude/agent.md:89`
documents a manual `curl` for debugging.

Given that, **disable the endpoint rather than guarding it.** Short-circuit `handleAsk` to
return immediately with `410 Gone` and a short body, before any JSON decode or LLM work:

```go
// TODO(decommission): /jarvis/ask is disabled pending removal. It ran a full
// LLM turn with tool dispatch on an unauthenticated loopback route, reachable
// by CSRF from any site the user visits. Its only caller was the ask form on
// the status panel, removed alongside this. Delete handleAsk, askRequest,
// askResponse, and this route once nothing has 410'd for a release or two.
// Use `aida jarvis ask` (in-process) instead.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "disabled; use `aida jarvis ask`", http.StatusGone)
	return
}
```

`410 Gone` over `404` deliberately: it says *decommissioned*, not *typo*, which is the
useful signal for a future caller.

**Remove the ask form from `indexHTML` in the same commit** (`server.go:163-175` - the
`askForm` submit handler, its input, and the routes line advertising the endpoint).
Disabling the route while leaving the form is a button that silently fails; that's worse
than either option alone.

Also update `.claude/agent.md:89` (the curl example), `docs/JARVIS.md:101` (the route
table), and `internal/cli/serve.go:57` (the `aida serve` help text), all of which currently
advertise the endpoint as live.

Leave `POST /api/runs/{id}/cancel|input` alone for now - those have real callers in
`runs_web.html` and need `guardLocal`, not removal. That stays a separate follow-up PR.

### Config

```yaml
devices:
  - {name: edith,  role: primary,      self: true}
  - {name: photon, role: satellite,    probe: ssh}
  - {name: minty,  role: bifrost-host, probe: ssh, herdr: true}
  - {name: beast,  role: gpu,          probe: tailscale-only}
fleet: {ttl: 30s, probe_timeout: 6s, refresh_interval: 45s, max_concurrent_probes: 8}
```

`DeviceConfig`: `Name`, `Role`, `Probe` (`ssh`|`tailscale-only`|`local`|`none`), `Self`,
`Herdr`, `SSHHost`, `TailscaleName`, `AidaPort`, `Notes`. `FleetConfig` holds the tunables
so `devices:` stays a clean list.

**No `devices:` block must still render a working page** - `ResolveDevices(machineName)`
synthesizes a single `self`/`local` device from `audio.MachineName()`
(`internal/jarvis/audio/devices.go:19`, which prefers `scutil --get LocalHostName`). Pass
the name in rather than calling it inside `config`, to keep `internal/config` free of a
`internal/jarvis` dependency. `/bifrost` with no `herdr: true` device renders an explainer,
not a 404.

`ValidateDevices()` returns *all* problems (unique names, at most one `self`, at most one
`herdr`, valid probe enum, port range, `ssh_host` shape). Called **non-fatally** from
`runHTTPDaemon` - warn and drop bad entries; a typo in `devices:` must never stop the voice
loop booting. Fatal only in `aida lint`.

### Routes

Register via a **new `registerDashboardRoutes(mux, deps)` called only from `serve.go`** -
not from `registerTasksWebRoutesAt`, which is shared with `aida tasks web --standalone`
(`internal/cli/tasks_web.go:88`). A 30-second ephemeral tasks browser should not start a
probe ticker or carry a remote-execution surface, and it has no `*config.Config` in scope.

| Method + Path | Returns |
|---|---|
| `GET /dashboard`, `/dashboard/` | embedded HTML (mirrors the `/runs` double-mount at `tasks_web.go:166`) |
| `GET /bifrost`, `/bifrost/` | embedded HTML |
| `GET /static/arrow*.mjs` | vendored Arrow.js |
| `GET /api/fleet` | `Snapshot` - never blocks |
| `POST /api/fleet/refresh` | kicks async refresh, returns immediately |
| `GET /api/dashboard/summary` | one-request first paint |
| `GET /api/dashboard/flow` | weighted node/edge graph from runs |
| `GET /api/bifrost/status` \| `/agents` \| `/agents/{target}/read` | herdr reads |
| `POST /api/bifrost/agents/{target}/send` | herdr `agent send` |
| `POST /api/bifrost/sessions` | `{preset}` only |
| `POST /api/bifrost/phone` | `phone-session.sh` → claude.ai/code URL |

`GET /api/brain/stats` goes in `registerTasksWebRoutesAt` instead - it needs only
`*brain.Brain`, which is already in scope, and it's a read-only counter dump so leaking
into standalone is harmless. It's genuinely missing today: `brain.Stats`
(`internal/brain/brain.go:550`) is only reachable via the `brain_stats` MCP tool, which
returns *formatted text*. Omit `BrainPath` from the response - a local filesystem path the
page has no use for.

herdr JSON passes through **verbatim** (`Agent{Agent, AgentStatus, Cwd, Name, PaneID,
TerminalTitle, …}`) rather than being re-shaped, so a herdr version adding fields costs
nothing. Bifrost handlers return **200 with `{"error": …}`** for *remote* failures (herdr
down, ssh refused) and 4xx only for *request* failures - one red banner is far easier to
render than five distinct 5xx causes.

### Probe caching

- Background ticker (45s) started from `runHTTPDaemon` on the existing `signal.NotifyContext`
  ctx, alongside `watchJobsForNotifications` (`serve.go:317`). Not started when the device
  list is just the synthesized self device.
- `GET /api/fleet` **never blocks on the network.** Fresh → serve it. Stale → serve it with
  `Stale: true` and kick an async refresh. Cold → return cards with `reach: unknown`,
  `refreshing: true`, and kick. If `Snapshot` could block, one sleeping laptop would make
  every 3s poll take the full 6s timeout and the whole page would feel dead.
- `singleflight` on the refresh key. The real hazard is not a data race but *probe
  pile-up*: two tabs polling at 3s against a 6s timeout would otherwise open unbounded SSH
  connections to a host already timing out. Still need an `RWMutex` for the snapshot field -
  different problem. `golang.org/x/sync` is already an indirect dep, so this is a go.mod
  promotion, not a new download.
- Per-host `context.WithTimeout` (6s) + a `chan struct{}` semaphore capped at 8, so one
  slow host can't extend another's budget. Each result slot is written by exactly one
  goroutine with `wg.Wait()` as the happens-before edge - no mutex on the slice.
- One `tailscale status` call for the whole fleet, not one per host. Missing/failing
  tailscale sets `TailscaleOK: false` and leaves devices at `reach: unknown` - never an
  error to the caller.
- `ProbeOne` must not panic; a parse failure becomes `Error: "…"`, never a crash that takes
  down `aida serve`.

### The flow panel

`GET /api/dashboard/flow?window=24h&limit=200` walks the newest N runs and emits a weighted
node/edge graph: spans give phase timing and cost, `Phases[].Sources[]` gives which sources
actually fired with per-source duration and status. Plus real totals -
`total_cost_usd`, tokens in/out, `llm_calls`.

Nodes carry `Measured bool`. Structural-only topology (which MCP servers are configured,
that Piper sits after the LLM, that the mic feeds Whisper) emits nothing countable, so those
render dimmed and **without numbers**. No faked weights.

Cache behind a 30s TTL and cap at 200 runs - 746 files re-read on a 3s poll would be a real
problem. Set `Truncated` so the UI says "last 200 runs", not "all activity".

**Explicitly not doing:** adding counters to engine hot paths to feed this panel. An
`internal/metrics` package is a real architectural change that should be justified on its
own merits, not smuggled in as a dashboard dependency. Disk-derived gets ~90% of the value
at zero risk.

---

## Part 3 - Frontend

Vendor `@arrow-js/core@1.0.6`. Note the npm dist is **two** ESM files (`index.mjs` 48 KB +
`chunks/internal-*.mjs` 19 KB) - `go:embed` both preserving the relative import path, or
run `esbuild --bundle` once at vendor time to produce a single file. Either is fine; decide
at implementation. No build step in CI regardless.

Both pages follow `runs_web.html` conventions (CSS custom properties, sticky `header`+`nav`).
Add `/dashboard` and `/bifrost` to the nav in both existing pages (`runs_web.html:105`,
`tasks_web.html:290`).

**`/dashboard`** - fleet grid (reachable, tailnet IP, OS, role, aida version, jobs), jobs
strip, brain stats, jarvis activity, flow panel. 10s poll on `/api/fleet`, 3s on jobs.

**`/bifrost`** - herdr status line; agent cards colored by `idle`/`working`/`blocked`;
click to open a pane (`agent read`, polled); send box; preset launcher; phone-session
button. 3s poll while a pane is open.

**Do not** move the dashboard to `/` - it collides with `server.New`'s root handler
(`internal/jarvis/server/server.go:20`) and panics at startup when Jarvis is on.

---

## Part 4 - Tests

The ones that matter:

- **`remotex`: `TestBuildRemoteScriptRoundTrip`** - for each adversarial payload
  (`'; rm -rf / ;'`, `$(whoami)`, backticks, newlines, NUL, 4 KiB blob, emoji, RTL
  overrides), run the generated script through local `/bin/sh` with `herdr` replaced by a
  shim dumping `printf '%s\0' "$@"`, and assert the recovered argv is byte-identical to the
  input. This proves the escaping empirically rather than by inspection.
- **`TestBuildRemoteScriptNeverEmitsUnvalidatedBytes`** - assert the script matches a strict
  structural regex, so it fails if anyone ever adds a raw interpolation.
- `TestValidateHostRejectsFlags` (`-oProxyCommand=…`, `--`, `host;id`), `TestSSHArgsAlwaysHasDoubleDashAndBatchMode`.
- **`TestDashboardRoutesAbsentFromStandalone`** - build a mux via `registerTasksWebRoutes`
  only; assert `/dashboard` and `/api/bifrost/*` 404. This guards the whole registration
  decision against someone later "helpfully" moving a route into the shared registrar.
- `TestMuxRegistrationDoesNotPanic` - duplicate `ServeMux` patterns panic at *registration*,
  i.e. at daemon startup, i.e. in production. Cheap test, catches an outage class.
- `TestSnapshotNeverBlocks` (cold cache + 10s-delay fake → returns <50ms),
  `TestSingleflightCollapsesConcurrentRefreshes` (20 goroutines → 1 probe),
  `TestConcurrencyCapRespected`, `TestTailscaleMissingIsNotFatal`.
- `TestGuardLocalRejects` - table over bad Host, bad Origin, `Sec-Fetch-Site: cross-site`,
  non-JSON POST; plus the happy path.
- `TestBifrostSendValidatesTarget` - `..%2F..%2Fetc` → 400 **and** the fake recorded zero
  calls.
- `TestFlowEndpoint` over synthetic runs written via `runs.Save`; `TestFlowEndpointEmpty`
  (no runs dir → 200, not 500).
- `TestConfigRoundTripWithDevices` - assert a config *without* `devices:` does not gain an
  empty `devices: []` key on rewrite.

Page/API tests follow `tasks_web_test.go`: `t.Setenv("HOME", t.TempDir())`,
`brain.Open(dir, "work", "VOYAGE_TEST_KEY_UNSET", "")`, real mux, `httptest.NewServer`.
Golden fixtures in `testdata/` captured from real `tailscale status --json` and
`herdr agent list` output.

---

## Verification

```bash
make test
make install          # required - `make build` leaves ~/bin/aida stale
aida serve
```

1. `/dashboard` - expect edith online (self), minty online, **photon offline** (it genuinely
   is right now; showing that correctly is the test), beast/wanda online. Confirm a
   dead host degrades its card rather than hanging the page.
2. `/bifrost` - expect herdr `running` v0.7.4, one agent card `phone`, `idle`, cwd
   `/home/heimdall/work`. Open the pane and diff against
   `ssh minty 'sudo -iu heimdall herdr agent read phone --source recent --lines 40'`.
3. Send a message from the page; confirm it lands in the pane.
4. **Injection check** - send the literal ``x'; touch /tmp/pwned #``. Assert
   `ssh minty 'ls /tmp/pwned'` reports no such file *and* the literal string appears in the
   pane.
5. **CSRF check** - from a non-loopback page's console, POST to
   `/api/bifrost/agents/phone/send` must be rejected.
6. `curl -s localhost:1610/api/fleet | jq`, `…/api/brain/stats | jq`,
   `…/api/dashboard/flow | jq '.total_cost_usd, .run_count'`.

## Sequencing

Work on a **new branch off `main`**, not on `main` directly:

```bash
git fetch origin && git switch -c feat/dashboard-bifrost origin/main
```

Granular commits per repo policy - one logical change each, **never squashed**. If a single
file carries hunks for two changes, split them across commits (stage one hunk via a patch
and `git apply --cached`). Merge with `gh pr merge --merge` so the individual commits
survive.

In dependency order:

1. `execx` stdin support
2. **`internal/remotex` + tests** - merge before anything calls it; it is the security core
   and deserves review on its own
3. config `devices:`/`fleet:` blocks, defaults, validation
4. `internal/fleet` + tests
5. `GET /api/brain/stats` (tiny, independently useful)
6. `guardLocal` + `registerDashboardRoutes` skeleton + arrow.js asset
7. `/api/fleet`, `/api/dashboard/summary`, `/api/dashboard/flow`
8. `internal/bifrost` + tests, then `/api/bifrost/*`
9. fleet ticker wired into `runHTTPDaemon`
10. `dashboard_web.html`, `bifrost_web.html`, nav links
11. disable `/jarvis/ask` (410 + TODO) + remove the ask form + update the three docs that
    advertise it - independent of everything above, so it can land first if you prefer
12. `docs/reports/` + README

Separate follow-up PR: retrofit `guardLocal` onto the existing mutating `/api/runs/*`
routes (they have real callers, so they need a guard rather than removal).
