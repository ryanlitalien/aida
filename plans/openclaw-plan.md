# Plan: Integrate OpenClaw-RL learning concepts into aida

## Context

OpenClaw (247K GitHub stars, Feb 2026) is a self-hosted AI agent framework with a companion RL training system (OpenClaw-RL) that turns conversations into model training signals. The user likes the learning concept but NOT the action-taking agent model - aida is deliberately read-only ("ask questions, get grounded answers with citations"). This plan maps OpenClaw-RL's learning innovations onto aida's existing lesson/feedback system without adding agent-style side effects or requiring model weight updates.

**Core insight from OpenClaw-RL**: every interaction produces "next-state signals" - user re-queries, corrections, explicit feedback, environment outcomes - and all can train the same policy in the same loop. Their paper (arXiv 2603.10165) identifies two signal types:
- **Evaluative signals**: how well did the action perform? (scalar reward via Process Reward Model)
- **Directive signals**: how should the action have been different? (recovered via Hindsight-Guided On-Policy Distillation)

**What aida already has** (the "deterministic middle" equivalent):
- `lessons.jsonl` - append-only log of every query's routing + outcome
- `FindSimilar()` - jaccard-based similarity to surface past lessons for the router
- `thumbs-up / thumbs-down` - explicit user feedback with optional `--because` reason
- `RecoverableError` flag - marks adapter failures and "I don't know" answers as non-precedent
- `answerIsUseless()` - string-matching heuristic to detect useless answers
- Router prompt injects 5 most similar past lessons with feedback/outcome labels

**What aida is missing** (gaps OpenClaw-RL's approach exposes):
1. **No answer quality scoring** - binary "useless or not" via string matching. No gradient.
2. **No re-query detection** - if the user asks the same question twice, aida doesn't recognize it as implicit negative feedback.
3. **No directive signal extraction** - when the user says `--because "wrong source, should have been workouts"`, aida stores the text but doesn't parse the intended source out of it.
4. **No lesson weighting** - all past lessons shown equally to the router. A 0.95-similarity thumbs-up and a 0.51-similarity auto-lesson have the same visual weight.
5. **No SOUL-equivalent** - no top-level "who is the user" context that shapes all routing decisions (OpenClaw's SOUL.md).

**Intended outcome**: aida's routing gets smarter over time based on natural usage patterns, explicit feedback, and answer quality - without model training, without taking actions, and without sending data externally.

---

## What OpenClaw-RL does vs. what aida should do

| OpenClaw-RL Concept | What it does | Aida equivalent | Gap? |
|---|---|---|---|
| **Next-state signals** | Every interaction (chat, terminal, GUI) produces training data | `lessons.jsonl` records every run | Partial - lessons exist but are coarse-grained |
| **Process Reward Model (PRM)** | Async LLM judge scores each turn on a scalar quality scale | `answerIsUseless()` string heuristic | **Yes** - need a real quality scorer |
| **Binary RL (GRPO)** | Scalar rewards feed PPO-style policy gradient | Router prompt: thumbs-up → PREFER, thumbs-down → AVOID | Partial - in-context only, no weight updates (by design) |
| **On-Policy Distillation (OPD)** | Hindsight-guided teacher extracts "what should have been different" | `--because` reason stored but not parsed | **Yes** - need directive extraction |
| **Re-query detection** | User asking again = implicit negative feedback | Nothing | **Yes** - need re-query detection |
| **SOUL.md** | Static personality file injected into every prompt | CLAUDE.md per source, but no user-level identity | **Yes** - need `~/.aida/soul.yaml` |
| **Async four-loop architecture** | Serving / rollout / judging / training run independently | Everything synchronous in query pipeline | Not needed - aida's lesson system is lightweight enough to be sync |
| **Model weight updates** | Actually trains the underlying model | In-context learning only | By design - aida uses API models, no fine-tuning needed |

---

## Implementation: 5 enhancements

### Enhancement 1 - Answer Quality Scorer (PRM-lite)

**Replaces**: `answerIsUseless()` string heuristic
**Inspired by**: OpenClaw-RL's Process Reward Model

After synthesis, make one additional fast LLM call (Haiku) that scores the answer:

```
Given this question and answer, rate the answer quality:
Question: {question}
Answer: {first 500 chars of answer}

Return JSON:
{
  "quality": 1-5,     // 1=useless, 2=wrong-domain, 3=partial, 4=good, 5=excellent
  "has_data": bool,    // does the answer contain specific facts/numbers?
  "reason": "..."      // one sentence explaining the score
}
```

Store `quality`, `has_data`, and `reason` on the Lesson struct. The router prompt then annotates each past lesson with its quality score:

```
- "what did i eat today" -> workouts [1 ok] (quality: 4/5 ✓) (similarity 0.87)
- "what did i eat today" -> acme-widgets [1 ok] (quality: 1/5 ✗ useless answer) (similarity 0.87)
```

This gives the LLM router a GRADIENT rather than binary signal. A quality-4 lesson from workouts beats a quality-1 lesson from acme-widgets even though both show "1 ok" status.

**Files to modify:**
- `internal/lessons/lessons.go` - add `Quality int`, `HasData bool`, `QualityReason string` to Lesson struct
- `internal/cli/query.go` - add PRM call after synthesis, before lesson recording
- `internal/llm/prompts.go` - new `QualityScorePrompt()` template
- `internal/engine/router.go` - annotate past-learning lines with quality score
- `internal/llm/client.go` - new `ScoreQuality()` method (or reuse `CompleteJSON`)

**Cost**: ~0.01c per query (Haiku, ~200 token prompt + ~50 token response). Adds ~500ms latency but can run async after the answer is displayed.

**Replaces `answerIsUseless()`**: the quality score subsumes the string heuristic. `quality <= 2` = RecoverableError. `quality >= 4` = positive precedent. 3 = neutral.

### Enhancement 2 - Re-query Detection

**Inspired by**: OpenClaw-RL's implicit negative feedback from user re-queries

Before parsing a new question, check if the user asked a very similar question in the last N minutes. If jaccard similarity > 0.7 AND the prior run was within 10 minutes:

1. Mark the prior lesson as `RecoverableError=true` (the user re-querying means the first answer wasn't satisfactory)
2. Add `requery_of: <prior-run-id>` field to the new lesson
3. In the router prompt, annotate the prior lesson: `(user re-queried - first answer was unsatisfactory)`

**Files to modify:**
- `internal/lessons/lessons.go` - add `RequeryOf string` field, new `FindRecentSimilar(question string, withinMinutes int)` that also filters by recency
- `internal/cli/query.go` - pre-query check before Step 1 (Parse), call `FindRecentSimilar`, update prior lesson if found
- `internal/engine/router.go` - annotate re-queried lessons in the prompt

**Why this matters**: currently if a user asks "what did I eat today" and gets a bad answer from acme-widgets, then immediately asks again, aida shows the FIRST run as "1 ok" (positive precedent!) - reinforcing the wrong source. Re-query detection flips that to negative.

### Enhancement 3 - Directive Extraction from Feedback

**Inspired by**: OpenClaw-RL's On-Policy Distillation (hindsight-guided)

When the user gives `thumbs-down --because "wrong source, should have been workouts"`, parse the `--because` text to extract:
- **Intended source**: regex/keyword extract source names mentioned in the reason
- **Failure type**: "wrong source" vs "wrong answer" vs "too slow" vs other

Store these as structured fields on the lesson:

```json
{
  "feedback": "thumbs-down",
  "feedback_reason": "wrong source, should have been workouts",
  "feedback_intended_source": "workouts",
  "feedback_failure_type": "wrong_source"
}
```

The router prompt then shows: `👎 (user wanted: workouts)` - a much stronger signal than just `👎`.

On the next similar question, the router sees: "last time the user explicitly said this should go to workouts" and weights that as the strongest possible routing signal.

**Files to modify:**
- `internal/lessons/lessons.go` - add `FeedbackIntendedSource string`, `FeedbackFailureType string`
- `internal/cli/feedback.go` - in `appendFeedback()`, call `extractDirective(reason, allSourceNames)` to parse
- `internal/engine/router.go` - annotate past-learning lines with intended source when present

**Extraction heuristic**: check if any registered source name appears in the reason text. "should have been X" / "wrong source, wanted X" / "X would have been right" patterns. Fall back to fuzzy name matching via `normalizeSourceToken`.

### Enhancement 4 - Lesson Weighting

**Inspired by**: OpenClaw-RL's scalar reward weighting in GRPO advantage estimation

Currently all 5 past lessons shown equally. Instead, compute a composite weight:

```
weight = similarity_score * quality_multiplier * recency_factor * feedback_boost
```

Where:
- `quality_multiplier`: quality 5 → 1.5x, quality 4 → 1.2x, quality 3 → 1.0x, quality 2 → 0.5x, quality 1 → 0.2x
- `recency_factor`: exponential decay - lessons from today → 1.0, yesterday → 0.9, last week → 0.7, older → 0.5
- `feedback_boost`: thumbs-up → 2.0x, thumbs-down → 2.0x (strong signal either way), none → 1.0x

Sort past lessons by composite weight, show top 5. The router prompt shows the weight:

```
Past learning (strongest signals first):
- [weight 1.87] "what did i eat today" -> workouts [4/5 ✓] 👍 (0.92 sim)
- [weight 0.34] "what did i eat today" -> acme-widgets [1/5 ✗] 👎 (0.92 sim, user wanted: workouts)
```

**Files to modify:**
- `internal/lessons/lessons.go` - add `CompositeWeight(similarity float64) float64` method
- `internal/cli/query.go` - use weighted sort instead of raw jaccard sort
- `internal/engine/router.go` - show weight in prompt

### Enhancement 5 - Soul file (`~/.aida/soul.yaml`)

**Inspired by**: OpenClaw's SOUL.md - a static identity file read on every interaction

Create `~/.aida/soul.yaml` that describes the USER, not the agent:

```yaml
# Who you are - aida uses this to make better routing decisions.
name: Ryan
role: Senior engineer + side-project builder
context: |
  I work at a former employer on payment infrastructure. "Partners" means payment
  merchants (pine-hollow, first-chair, etc). At home I build macOS viewer
  apps, game prototypes, and fitness tracking tools. I use aida to
  query across all of these - work and personal - from a single CLI.
preferences:
  - Always prefer the source closest to my question topic
  - Never route fitness/nutrition questions to code sources
  - When I mention a project by name, always go to that project's folder
  - I care about specific data (numbers, dates, file paths), not vague summaries
```

Load this once at query start and inject it into:
1. **The parser prompt** (LLM Call #1) - so "partner" is understood as a former employer's merchant
2. **The router prompt** (LLM Call #4) - so routing preferences are respected
3. **The synthesis prompt** (LLM Call #3) - so the answer style matches the user's expectations

**Files to modify:**
- `internal/config/soul.go` (new) - Soul struct + `LoadSoul()` from `~/.aida/soul.yaml`
- `internal/cli/query.go` - load soul at start, pass through pipeline
- `internal/llm/prompts.go` - inject soul context into parser, router, synthesizer prompts
- `internal/engine/router.go` - add soul.preferences to the picking rules section

**`aida init` integration**: prompt the user for name/role/context during onboarding and write `soul.yaml`.

---

## Implementation order

1. **Enhancement 5 (Soul file)** - highest user-visible impact, simplest to implement (YAML load + prompt injection). The user's routing preferences become durable. **~30 min.**
2. **Enhancement 1 (PRM-lite quality scorer)** - replaces the fragile string heuristic with a real quality signal. Makes every subsequent enhancement stronger. **~45 min.**
3. **Enhancement 2 (Re-query detection)** - quick win that closes the feedback loop on the most common failure mode (asking the same question twice). **~20 min.**
4. **Enhancement 3 (Directive extraction)** - makes explicit feedback much more actionable. **~20 min.**
5. **Enhancement 4 (Lesson weighting)** - ties it all together: quality + recency + feedback into a single composite signal. **~30 min.**

Each enhancement is independently useful and testable. They compose: quality scoring feeds into weighting, re-query detection marks low-quality lessons, directive extraction enriches the weighting signal.

---

## Critical files

| File | Enhancements | What changes |
|---|---|---|
| `internal/config/soul.go` (new) | 5 | Soul struct, LoadSoul() |
| `internal/lessons/lessons.go` | 1, 2, 3, 4 | New fields: Quality, HasData, QualityReason, RequeryOf, FeedbackIntendedSource, FeedbackFailureType. New methods: CompositeWeight, FindRecentSimilar |
| `internal/cli/query.go` | 1, 2, 5 | PRM call after synthesis, re-query pre-check, soul loading |
| `internal/cli/feedback.go` | 3 | Directive extraction from --because text |
| `internal/engine/router.go` | 1, 2, 3, 4, 5 | Richer past-learning annotations, soul preferences in prompt |
| `internal/llm/prompts.go` | 1, 5 | QualityScorePrompt, soul context injection |
| `internal/llm/client.go` | 1 | ScoreQuality method (or reuse CompleteJSON) |

## Verification

After each enhancement:
```bash
make test                                         # unit tests
aida "what did I eat today" --verbose               # check routing + quality score
aida bad --because "should have gone to workouts"   # test directive extraction
aida "what did I eat today" --verbose               # re-query detection fires
go run ./cmd/golden-log -group learning           # full 40-question regression
```

After Enhancement 5 specifically:
```bash
cat ~/.aida/soul.yaml                           # verify soul file exists
aida "what partner timeout is configured?" --verbose # "partner" interpreted per soul context
```
