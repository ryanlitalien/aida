// Package arbiter grants scarce model capacity to one requestor at a time
// by policy, in the bus-arbitration sense (docs/arbiter-plan.md section 1):
// not a router, not a proxy, not a gateway. Given a task's tags and the
// live capacity view from internal/burndown, it answers two questions:
// which lane has headroom for this task's data class right now, and is
// now the time.
//
// This package holds the deciding half of the arbiter:
//
//   - lanes.go: the lane roster (~/.aida/lanes.yaml, config.LanesPath) --
//     machine + runtime + credential + data-class allowlist + model
//     mapping per lane, cheapest first.
//   - dataclass.go: task tags -> data class, most-restrictive-wins.
//   - capacity.go: matches a lane's configured windows against
//     burndown.Report's live rows.
//   - signals.go: the append-only NDJSON signal log and the per-lane
//     empty_until state a "lane reported empty" hook or wrapper writes
//     (plan section 5, decision 2: an ambiguous signal FAILS OPEN --
//     treated as empty, logged with ambiguous=true so the false-positive
//     rate stays measurable).
//   - stophook.go: parses a Claude Code Stop hook payload and its
//     transcript to find the last assistant message, one input a signal
//     can be built from.
//   - pick.go: Pick walks the lane roster, cheapest first, and returns
//     the first eligible lane with a precise rejection reason for every
//     lane it skips.
//   - ledger.go: the append-only NDJSON run ledger (plan section 7).
//   - runner.go: the pure command-building step (BuildCommand) plus a
//     thin execx-backed Runner for the "exec" lane kind.
//
// What this package deliberately does NOT do: it never merges anything,
// it never picks new paid spend on its own (spend is a dial Ryan turns;
// the arbiter's job stops at reporting which lanes are eligible and
// close to exhaustion), and it never dispatches a task by itself -- some
// other loop (aida loop, aida serve --arbiter) calls Pick and acts on the
// Decision it returns. Task state, leases, and hand-off files (STATE.json,
// HANDOFF.md, LOG.md under the brain repo's arbiter/<slug>/) are also out
// of scope here; see docs/arbiter-plan.md section 6 for that layer.
package arbiter
