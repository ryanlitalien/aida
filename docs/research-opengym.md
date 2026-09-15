# Research: openGym as a source for the workouts project

**Date**: 2026-08-25 · **Status**: reference · **Target**: `~/dev/workouts` (not aida itself; stored here alongside `research-openviking.md` from the same research pass)

openGym is a self-hosted gym tracker by Duarte Santos: React 19 PWA, frameworkless Node API, flat JSON persistence, passkey auth, 1,324-exercise library, progression engines, AGPL-3.0. It has nothing to do with aida. The interesting question is whether its workouts substance (exercise data, media, planning/progression logic, importers) is worth extracting for the workouts project, which currently has no strength data layer at all. Webserver/PWA/API stack explicitly out of scope.

## Provenance: use GitLab, not GitHub

The GitHub repo people find (`arvids-unavailable/openGym`, ~5k stars) is an unofficial mirror frozen at v1.2.4 since 2026-08-03. The author's GitHub account (`DuarteSantos8`) was suspended ~2026-08-18 and the mirror inherited the traffic; issues and forks land there that the maintainer cannot touch. Canonical: **https://gitlab.com/DuarteSantos8/opengym** (v1.2.11 as of 2026-08-25), with https://gitea.com/DuarteSantos/openGym as an in-sync mirror. Never star/fork/file on the GitHub URL.

## Health and v1.2.4 → v1.2.11

Very active but very young: first commit 2026-07-18 (the whole project is ~5 weeks old), 306 commits and 11 releases in that window, 15 contributors with real outside contribution accelerating (v1.2.10 was 9 contributors / 13 MRs).

Highlights per release: **1.2.5** ships a read-only stdio MCP server (`mcp/`) and the enabling refactor that makes `frontend/src/lib/` load under bare node. **1.2.6** adds a fatigue muscle-map view (36h half-life decay) and moves curated secondary-muscle data to a read-time overlay, keeping the catalog pristine. **1.2.7** adds a per-muscle e1RM strength view and fixes imported warm-ups being counted as work. **1.2.8** corrects the media licensing story (media is © Gym visual, not CC). **1.2.9** moves to GitLab. **1.2.10** adds equipment profiles, planned warm-ups, drop-sets/rest-pause, three note kinds, and importer fixes. **1.2.11** (same day) fixes six defects from 1.2.10, none caught by a test, including warm-ups counted into permanently-written session volume.

The read on the data model: three of the last five releases contained a set-row schema bug (`phase` vs `warmup`, warm-up volume, intensifier round-trip). **The catalog is stable and MIT; the logic is young and churning.** Vendor data, port small pure pieces, let the big modules mature.

## Licensing: the gating split

| Asset | License | Verdict |
|---|---|---|
| openGym code (`frontend/src/lib/`, `mcp/`, `api/`) | AGPL-3.0 | Private, non-distributed, non-network-served personal use carries no obligation; porting is fine |
| Exercise metadata + English instructions | MIT (Hasan Emir Yıldırım, via ExerciseDB v1 → `hasaneyldrm/exercises-dataset`) | Clean |
| Muscle-map SVG geometry (`body-paths.js`) | MIT (MuscleMap) | Clean |
| **Images + GIFs (~137MB)** | **Contested** | **Skip** |

The media is the real problem: upstream's LICENSE carries a non-standard "MEDIA EXCEPTION" stating MIT covers only code/structure/instructions, the media is © Gym visual included by permission, and, verbatim, cloning the repository grants no license to the media. ExerciseDB/AscendAPI separately claims ownership with contradictory terms. openGym itself never redistributes it (instances clone it at first boot). Skip on legal grounds, and on utility grounds too: the workouts coach is a text LLM, GIFs add nothing.

## The gap in workouts

`workouts.db` has 280 `Strength` rows that are just a Garmin title + duration; grepping the whole schema for exercise/set/rep columns returns nothing. Meanwhile `CLAUDE.md` promises to answer "what should I lift today" and `knowledge/strength-programming.md` prescribes a full A/B/C double-progression split with no data layer under it. openGym pieces are complementary to TrainingPeaks/Garmin/FatSecret; the only overlap is Liftosaur (remote, data on liftosaur.com), which this would replace with something local and owned.

## Extraction shortlist

**Pull:**

1. **The exercise catalog** (`frontend/src/lib/exercises-data.js`): 1,324 records, uniform keys (id, name, body part, equipment, target, muscle groups, secondary muscles, steps), 7,710 instruction steps. Extractable with ~15 lines of stock Python (`json.loads` on the bracket slice, verified, no node) into a `workouts.db` `exercises` table. Add an ATTRIBUTION.md (MIT, Hasan Emir Yıldırım, ExerciseDB v1). Optionally fold in the `exercise-muscle-batch-{1,2}.json` overlays (223 curated muscle corrections for compound lifts).
2. **`onerm.js` → a ~40-line Python port**: Epley/Brzycki/Lombardi e1RM, `REP_CAP=12`, one import to strip. Port its 25 test cases too.
3. **The `double` progression policy** from `progression.js`, plus its honesty rules (a set never checked off is a miss; fewer sets than prescribed is a miss). Not the whole 14KB engine. This is literally the protocol already written in `strength-programming.md`; formalizing it is what makes "what should I lift today" answerable from data.
4. **`workout-model.js` as a schema reference, not code**: two orthogonal discriminators on a flat set row (`phase`: work/warmup; `type`: straight/dropset/restpause), drop-sets as additional volume, rest-pause as a breakdown of reps. Design the `strength_sets` SQLite table on this and warm-ups/drop-sets/rest-pause come out right the first time. Known upstream edge cases to carry: rest-pause almost always reads as goal-hit in progression, and never produces a 1RM because totals exceed `REP_CAP`.
5. **The CSV header-alias table as a spec** (21 canonical fields, `docs/DATA_IMPORTS.md`), one table worth copying. The importer is not four app-specific parsers, it is one generic header-alias reader with a 3-tier name-matching cascade; the algorithm ports, the alias data doesn't (opaque ids in openGym's id space).
6. **Conditional: vendor `mcp/opengym`** only if lifts actually get logged in openGym (sideloaded APK or self-host). The MCP server reads `state-<uid>.json` straight off disk (`OPENGYM_DATA`/`OPENGYM_UID`), no API server needed, 8 read-only tools (routines, week plan, workouts, bodyweight, e1RM, muscle balance). Vendoring means copying `mcp/` plus the four `frontend/src/lib` files it reaches into. The question is behavioral, not technical: it only pays if the UI is really used for logging. It would also be the first Node server in a workouts MCP fleet that is otherwise all Python/uv.

**Skip:** the ~137MB media (licensing + no utility); `recovery.js` (cleverest module, 36h-half-life intensity-weighted fatigue, but most entangled and it duplicates Garmin's training-load/body-battery signals); `history.js` (29KB of mostly UI-adjacent session semantics; cherry-pick `rerampWarmups` at most); `import-csv.js` as code (no FitNotes/Strong/Hevy history to import); translations, body-map SVG, UI state machines, and the entire web/API/Docker/Capacitor stack.

## Caveats

- Five-week-old project with a schema bug in three of the last five releases. Take the data, port the small pure pieces, revisit the big modules in a few months.
- The media fetch script points at github.com/hasaneyldrm, the same platform that suspended the author; alive today (~21k stars, HEAD matches openGym's CI pin) but a single point of failure. Irrelevant if the media is skipped.

## Sources

- https://gitlab.com/DuarteSantos8/opengym (canonical; CHANGELOG, tags, `frontend/src/lib/`, `mcp/`, `docs/DATA_IMPORTS.md`)
- https://gitea.com/DuarteSantos/openGym (mirror)
- https://github.com/hasaneyldrm/exercises-dataset (catalog upstream; LICENSE with the MEDIA EXCEPTION)
- https://opengym.duarte-santos.ch (live demo)
