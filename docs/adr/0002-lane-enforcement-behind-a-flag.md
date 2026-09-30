# ADR-0002: Lane enforcement ships behind `--arbiter`, default off in this PR

Status: superseded 2026-09-25 by Ryan's decision: enforcement on everywhere; the flag stays as the escape hatch.

## Reversal

This ADR's default-off stance is reversed: `--arbiter` (aida loop) and `--loop-arbiter` (aida serve) now default to true, and defaultLoopOpts() carries that default into every other caller (aida swarm included).

The reason the original hedge in this ADR (calibrate with enforcement on for one loop while the daemon keeps its old behavior) is no longer needed: a lane roster is now required from the file, so a misconfigured machine fails loudly at startup instead of silently spawning on the metered path.

The flags themselves are unchanged and stay as the escape hatch: `--arbiter=false` or `--loop-arbiter=false` reverts to the always-metered `aida --agent` path this ADR originally shipped as the only option.

## Original decision

Status: accepted 2026-09-24. Reversible: yes, flip the default once the calibration numbers exist.

### Context

Task #481 says the capacity view must be consulted, not merely visible: the worked example is Fable burning on sweeps that `models.yaml` said should be Sonnet. Today `aida loop` spawns `aida --agent`, which uses the metered Anthropic API key, that is plan lane 6, the last lane, on every task. Enforcing lane order means the loop must route each spawn through the picker and run the chosen lane's own CLI (`claude`, `codex exec`, `agy`) instead.

`aida serve --loop` runs on minty today with that metered path and no `~/.aida/lanes.yaml`. Flipping every spawn to the picker in the same change would break that daemon on the first task, on a box nobody is watching.

### Decision

`aida loop --arbiter` (and `aida serve --loop-arbiter`) turns enforcement on. When on, every attempt asks `arbiter.Pick` for a lane and model, refuses to spawn when no lane is eligible (waits, logs the rejections, never picks new spend), and runs the chosen lane's runner. When off, the loop behaves exactly as before. Plan section 8 step 0 (calibrate five tasks by hand and record the window deltas) has not been done; the flag exists so that calibration can happen with enforcement on for one loop while the daemon keeps its old behavior.

### Consequences

Enforcement is opt-in until the ledger shows a week of clean lane choices, then the default flips and the flag stays as an escape hatch. Until then the daemon on minty is not enforced, which is the status quo, not a regression.
