# ADR-0003: Untagged tasks are personal; an unprobed lane is ineligible unless it says otherwise

Status: accepted 2026-09-24. Reversible: yes, both are lane-config or tag changes, no code.

## Context

The data class gate (plan section 3, recommendations `sensitivity-gate`) decides which lanes may even see a task. Two inputs are routinely missing: a task with no class-bearing tag, and a lane with no live usage probe (qwen on EC2 has none; a provider whose probe failed has none for that tick).

## Decision

A task whose tags map to no data class is `personal`. Personal is the most restrictive class (only the personal logins may run it), so a mislabeled task can leak nothing to a company lane or a metered key. A task whose tags map to both `personal` and `butterstack` is a configuration error the picker returns rather than resolving, because no lane may carry both and silently choosing one would put one party's data on the other's credential.

A lane with zero capacity rows for its provider is ineligible unless its config sets `allow_unprobed: true`. This is the opposite polarity from the rate-limit signal (decision 2 fails open): a missing probe is not evidence the lane is empty, but spending on a lane whose remaining quota is unknown is exactly what floors exist to prevent. The two lanes that have no probe by design (qwen on EC2, and any future local model) opt in explicitly.

## Consequences

Tag hygiene matters: a ButterStack task without `project:butterstack` will wait for a personal lane it may never get. `aida arbiter plan` (dry run) shows the class each task resolved to so this is visible before a wave, not after.
