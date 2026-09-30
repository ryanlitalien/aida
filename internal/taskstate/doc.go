// Package taskstate implements the arbiter's per-task state on disk: the
// harness-owned STATE.json, the model-written HANDOFF.md, the human LOG.md,
// deliverable-based acceptance checking, and the git-ref lease used to
// claim a task across machines. See docs/arbiter-plan.md section 6 and
// docs/arbiter-pros-cons-analysis.md section 3 for the design this package
// implements.
//
// It encodes three decisions Ryan made on 2026-09-17 (docs/arbiter-plan.md
// section 6, the pros/cons review):
//
//  1. Hand-off resumes, never restarts. When a lane nears exhaustion the
//     harness arms a HANDOFF.md while the current model still has full
//     context; the next model reads HANDOFF.md, not the raw transcript
//     (pros-cons pitfall 1: cross-vendor transcripts degrade reasoning).
//     A stale or missing hand-off is a detected error (Check, IsStale),
//     never silently ignored.
//  2. Done means acceptance. A "DONE" string from a model is a claim, not
//     a fact -- Acceptance names the deliverables and checks that decide
//     whether a task is actually finished (acceptance.go).
//  3. Lease contention is expected and handled with backoff, not treated
//     as an error. Two claimers racing for the same task is a normal
//     event on N machines; the loser gets ErrHeld or ErrContended and
//     either moves to another task or retries with jittered exponential
//     backoff (lease.go, pros-cons pitfall 3).
//
// STATE.json is written ONLY by the harness loop in internal/cli -- never
// by a model, and never by this package on the model's behalf. Everything
// here that mutates state (Save, BeginAttempt) is called from that loop.
// LOG.md is a human narrative; nothing in the arbiter decides anything by
// reading it back.
package taskstate
