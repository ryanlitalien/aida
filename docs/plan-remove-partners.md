# Mini-plan: remove the "partners" concept

**Decision**: 2026-09-12 (checklist, chunk A) - the partner registry was a work-tenure feature with no value to anyone else. It is removed, not renamed; there is no fallback reading of the old files. Routing keeps working off what already exists without it: sources' `entities:` token lists and `routes.yaml` `match_entity` (+100).

**Scope today** (surveyed 2026-09-13 on `oss`): 78 Go files mention it (most are test fallout, six hotspots below), plus CLAUDE.md, README, a dozen older plan docs, `testdata/golden-queries.yaml`, and `~/.aida/partners.yaml` on the private side.

## Order of work (granular commits, one step each)

1. **Engine - planner** (`internal/engine/planner.go`): delete the `+50` registry boost and the `ptnr` column from `FormatExplain`. The `match_entity` route boost (+100) is untouched and becomes the only entity signal, matching `docs/diagrams/routing-walkthrough.md`, which already documents the post-registry story.
2. **Engine - resolver** (`internal/engine/resolver.go`): drop the registry lookups (`FindByAlias`, `FindByIdentifier`, `FindByCheckoutToken` call sites) and the `partner_name` attachment. Resolve keeps typing entities and passing tokens through.
3. **Engine - classifier** (`internal/engine/classifier.go`): remove `EntityCheckoutToken` and its regex. (The generic replacement - configurable ID patterns in `config.yaml` - is the separate `ari_extractor.go` checklist item; do not fold it in here.)
4. **Config** (`internal/config/partners.go`): delete the file - `Partner`, `Partners`, `LoadPartners`, `SavePartners`, `ExamplePartners` - plus the `partners/<profile>.yaml` override loading and any `config.yaml` references (`internal/config/config.go`, `soul.go`).
5. **CLI**: delete `aida partners` (`internal/cli/partners.go` + cobra wiring); `aida init` stops writing `partners.yaml`; sweep `internal/cli/query.go`, `library.go`, `tasks_ingest.go`.
6. **LLM prompts** (`internal/llm/prompts.go`): remove the partner-context blocks from the parse/route prompt templates.
7. **Incidental callers**: `internal/dailybriefing/` (triage/format), `internal/investigations/`, `internal/mcp/server.go`, `internal/library/` (import/scan/routes), `internal/roster/load.go` - mechanical sweeps, one commit per package.
8. **Tests**: fix the ~70-file fallout; re-cut `testdata/golden-queries.yaml` and the in-code golden fixtures with entity-token routing instead of registry hits.
9. **Guard**: add the removed identifiers (`partners.yaml`, `LoadPartners`, `checkout_token_patterns`, `partner_ari`) to `scripts/oss-scan.sh` so they can't creep back. Scan for those specific tokens, not the bare word "partner" (too many legitimate uses in prose).
10. **Docs**: CLAUDE.md (routing contract step 3/4, the "Partner YAML shape" section, classifier patterns), README, `docs/plan-open-source.md` Part 2 prose. Older plan docs (`gap-analysis`, `plan-investigate`, `harness-evolution`, …) get handled by chunk B's keep/trim/move disposition table - don't sweep them here.

## Out of scope, tracked elsewhere

- `ari_extractor.go` generalization - its own chunk A checklist item.
- Blog part 2 steps 3–4 re-cut (`aida-content/reviews/part-2-review.md` section 0, option 1) - after steps 1–2 above land.
- `~/.aida/partners.yaml` (private config) - stays on disk, simply stops being read. Behavior change accepted: work-profile queries lose registry alias/token resolution and the +50 boost, which is the point.

## Sizing

One agent, one branch session against `oss`, gate green (`go build && go vet && go test && scripts/oss-scan.sh`). Steps 1–8 are the day's work; 9–10 are an hour. This unblocks blog part 2 (its only blocking decision is registry staleness).
