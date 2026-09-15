# Engineering note: feedback mechanics

How `aida thumbs-down --because "..."` changes the next similar query's routing
*mechanically*, not as a prose hint the model may ignore. From the code:
`internal/cli/feedback.go` (directive extraction), `internal/engine/router.go`
(deterministic boosts), `internal/lessons/` (lesson weighting), and the voice
mirror in `internal/jarvis/tools/jarvis_feedback.go`.

## The problem with "noted, thanks"

Most assistant feedback loops store the correction as text and hope the model
reads it sympathetically next time. That fails in a specific, reproducible way:
the router's prompt says "past learning shows source X worked", the model
reasons its way back to X, and the user's correction evaporates. Aida's rule is
that a correction must land as **arithmetic the model cannot argue with**, with
the prose kept only as supporting context.

## Step 1: a correction is an append, never a mutation

`aida thumbs-down [run-id] --because "..."` (aliases `bad`, `-1`; `thumbs-up`,
`good`, `+1`; `note`, `ok` for unrated observations) writes a **new** lesson
record pointing at the run via `RunID`. The lessons log stays append-only;
readers take the latest lesson per run, so an explicit verdict written later
overrides the silently auto-recorded one. On a thumbs-down, any auto-recorded
lesson for the same run is additionally *superseded* in the brain index - the
real incident behind this: a `quality>=4` auto-recorded routing hint and a
thumbs-down for the same run both persisted, and the stale praise kept
competing with the user's correction in recall forever.

## Step 2: directive extraction from free text

The `--because` reason is parsed twice, for two different consumers.

**Routing directives** (`extractDirective`, thumbs-down only): a polarity-
tracking tokenizer walks the reason left-to-right carrying a positive/negative
polarity. Multi-word cue phrases flip it - "no need for", "do not use",
"instead of", "rather than" go negative; "should have used", "should route to",
"right source" go positive - with longer phrases matched before shorter so
"do not use" wins over "not". Bare cue words (`avoid`, `skip`, `exclude` /
`use`, `prefer`, `want`) flip it too. Any token matching a registered source
name is bucketed under the current polarity, and **sentence boundaries reset
polarity to positive**, so:

> "We used github. Should have used first-chair."

buckets only `first-chair` as intended. The tokenizer preserves internal
hyphens so multi-word source names survive as single tokens. The predecessor
implementation iterated a Go map of sources and substring-matched - map
iteration order is randomized, so "no need for github or spell-checker, should
route to first-chair" could return *any of the three* as the intended source
depending on the run. The rewrite made the parser order-deterministic and
negation-aware. The extractor also classifies a failure type
(`wrong_source` / `wrong_answer` / `too_slow`) from cue phrases.

**Output directives** (`extractOutputDirectives`, all verdicts): clauses that
look like format guidance - "max 3 sentences", "include the link", "just the
answer" - are kept and surfaced to the *synthesizer* as must-follow rules on
similar future questions. This matcher is intentionally permissive: a false
positive costs a few prompt tokens; a false negative means the user has to
repeat themselves. It runs for notes and thumbs-up too, because "3 sentences,
please" is forward-looking style preference regardless of verdict - before
this, a note's directive was filed as soft prose and ignored on re-runs.

Both extractions are echoed back at the terminal ("on next similar question:
PREFER [first-chair] AVOID [github]") so the user sees the parser worked.

## Step 3: deterministic router arithmetic

On the next query, the router embeds the question and pulls similar past
lessons. Before the LLM router prompt is even built:

- The top **3** most-similar thumbs-down lessons (capped so one directive
  can't dominate unrelated questions) apply **+100** to each intended source's
  prior score and **−100** to each excluded one. The magnitude was raised from
  ±50 after a real failure: ±50 sat below the +100 route-boost baseline, so a
  demote couldn't dislodge a previously-blessed source - the LLM would flip
  back to it on prose reasoning ("past learning shows it worked") even with an
  explicit exclusion on file.
- Intended sources the candidate list *missed entirely* are injected into it,
  so "route to X" works even when X's name/entity match wouldn't have
  surfaced X for this question.
- Auto-generated signals stay subordinate by design: per-source adjustments
  mined from recent failed eval-runs are ±25 per occurrence, capped at ±50
  absolute - deliberately half the explicit-feedback magnitude, so machine
  inference can never outvote a human correction.

Then the LLM router sees the same lessons as prose, sorted by
`CompositeWeight` - similarity × a quality multiplier (0.2–1.5) × a recency
decay (1.0 within a day, 0.5 past a week) × **2.0 if the lesson carries
explicit feedback** - so the strongest signals appear first in the prompt. The
LLM's own pick then adds +1000 to chosen sources (capped at +100 for "filler"
candidates that were only injected alphabetically to pad a thin list - an LLM
whim about an arbitrary candidate shouldn't outrank scored ones).

The layering is the point: *human correction (±100) > eval inference (±25..50)
> capability/topic scoring (single digits)*, and the LLM chooses among
candidates whose order those layers already set.

## The voice mirror

The voice layer has the same loop with a conversational surface. Saying
"thumbs down - you should have used the weather tool" invokes
`jarvis_thumbs_down`, which rates the **most recent non-rating turn** - the
assistant caches the last substantive turn, and rating turns are transparent
(they don't supersede the turn they're rating, so two corrections in a row
both land on the same real turn). The rating writes to a voice-side lessons
table (query + embedding + rating + reason + reply); at the start of every
subsequent voice turn, the top-3 most-similar past lessons are recalled into
the system prompt as a "Past feedback" block. `jarvis_note` records standing
directives ("I prefer Fahrenheit") the same way, unrated.

**Engine passthrough**: when the rated turn used the `aida_query` tool, the
voice tool also shells out to `aida thumbs-<verb> <run-id> --because ...`, so
the engine's lesson store gets the same correction and the router boost fires
on the next similar *typed* query too. The run id travels via a sentinel the
engine prints on stderr after saving the run - `[aida-run-id:<id>]` - which
the tool parses out of the subprocess's captured output. The sentinel exists
to kill a race: "the most recent run" is ambiguous when terminal queries run
concurrently with voice turns; a captured id is not.

## Why this design holds up

- **Corrections are cheap** - one flag at the terminal, one sentence by voice - 
  so they actually get given.
- **Corrections are legible** - the extraction is echoed back immediately, and
  `aida lessons --for "<question>"` previews exactly which lessons a
  hypothetical question would match, i.e. what the router will see.
- **Corrections are bounded** - top-3 similar lessons, fixed magnitudes,
  polarity that resets per sentence. A bad parse can nudge one query family,
  never poison the registry.
- **Prose and arithmetic carry the same fact** - the LLM gets the reason as
  context; the score gets the correction as math. When they disagree, the math
  wins, which is the entire lesson of the ±50→±100 bump.
