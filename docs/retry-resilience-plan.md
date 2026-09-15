# Retry, Self-Correction, and Resilience Plan

Inspired by patterns from [NousResearch/aida-agent](https://github.com/NousResearch/aida-agent) and [garrytan/gbrain](https://github.com/garrytan/gbrain). The core problem: aida "tries once and gives up" in several places.

## Design Principle

All new LLM calls stay at the **edges** (query construction, synthesis). The deterministic middle (classify, resolve, plan) is untouched. Source selection is unchanged - same sources, different phrasing.

---

## Phase 1: Transient Failure Retry with Backoff (smallest, safest)

**Problem:** The `OpenAIProvider` (`internal/llm/openai.go`) uses raw `http.Client` - a 429 or 503 from Groq/Cerebras/OpenRouter causes immediate failure. The Anthropic SDK already has built-in retry, but OpenAI-compatible endpoints don't.

**Change:** Add a retry loop around the HTTP call in the `do` method with jittered exponential backoff (2 retries, 500ms base, 8s cap). Respects context cancellation during backoff.

**Files:** `internal/llm/openai.go` only.

**Why first:** One file, zero API surface changes, immediately improves reliability.

### Implementation Details

Add constants:

```go
const (
    openaiMaxRetries = 2
    openaiBaseDelay  = 500 * time.Millisecond
    openaiMaxDelay   = 8 * time.Second
)
```

Add helpers:

```go
func isTransientHTTPError(statusCode int) bool {
    return statusCode == 408 || statusCode == 429 || statusCode == 409 || statusCode >= 500
}

func openaiRetryDelay(retryCount int) time.Duration {
    delay := time.Duration(float64(openaiBaseDelay) * math.Pow(2, float64(retryCount)))
    if delay > openaiMaxDelay {
        delay = openaiMaxDelay
    }
    jitter := time.Duration(rand.Int63n(int64(delay / 4)))
    return delay - jitter
}
```

Wrap the HTTP call in `do` with a retry loop that:
- Retries on network errors and transient HTTP status codes
- Checks `ctx.Done()` between retries
- Re-creates the request body for each retry attempt
- Returns the last error after exhausting retries

### Testing

- Mock HTTP server returns 429 twice then 200 → verify success after retries
- Mock HTTP server returns 429 three times → verify failure after exhausting retries
- Verify context cancellation during backoff aborts immediately
- Confirm existing Anthropic path is unaffected

---

## Phase 2: Executor Retry with Rephrase (highest impact)

**Problem:** `executeSource` (`internal/engine/executor.go`) retries once on *error* status only. Empty results just trigger the docs fallback. Low-confidence results are invisible to the executor - the verifier sets `ShouldRetry=true` but nobody acts on it.

### Implementation Details

**Refactor `executeSource`** into two functions:

- `executeSourceOnce` - the current single-attempt logic
- `executeSourceWithRetry` - new wrapper with retry loop

**Retry loop logic:**

```
for attempt := 0; attempt <= maxSourceRetries; attempt++ {
    result := executeSourceOnce(...)

    if result.Status == "error" && attempt < maxSourceRetries {
        // rephrase with error context
        continue
    }

    if result.Status == "empty" && attempt < maxSourceRetries {
        // rephrase with "prior query returned no results, try broader approach"
        continue
    }

    if result.Status == "success" && attempt < maxSourceRetries {
        // quick computational confidence check (~0ms, no LLM)
        confidence, _, _ := computationalScore(result, question, intent)
        if confidence < 0.3 {
            // rephrase with "results seem unrelated"
            continue
        }
    }

    return result, nil
}
```

**Rephrase prompt construction** - new helper `buildRephrasePrompt` that builds context-aware instructions:

| Condition | Rephrase guidance |
|-----------|-------------------|
| `status="error"` | "Your prior query failed with this error: {error}. Fix the query." |
| `status="empty"` | "Your prior query returned no results. Try a broader search pattern or different table/field names." |
| Low confidence | "Your prior query returned results but they seem unrelated. The results contained: {summary snippet}. Try a more targeted query." |

**Constants:**

```go
const (
    maxSourceRetries       = 2   // retries per source (not counting original attempt)
    lowConfidenceThreshold = 0.3 // below this, retry with rephrase
)
```

**Safety rails:**
- Max 2 retries per source (3 total attempts), hard-coded
- Skip retry for `claude-project` and `docs` sources
- Attempt counter visible to LLM so it tries something different
- Total duration accumulates across retries
- Context cancellation respected between retries

**Files:** `internal/engine/executor.go` (main changes). `computationalScore` in `verifier.go` is already callable from the same package.

### Threading the question parameter

`executeSourceWithRetry` needs the raw question for confidence scoring. Thread `intent.RawQuery` through:
- `Execute` → `executePhase` → `executeSourceWithRetry`

### Testing

- Mock LLM + mock adapter: empty on first call, success on second → verify retry happens
- Mock adapter: always empty → verify exactly 3 total attempts then returns
- Verify claude-project and docs sources skip retry
- Verify context cancellation stops retry loop
- Verify rephrase prompt includes appropriate context per condition
- Verify total duration accumulates correctly

---

## Phase 3: Synthesis Validation Loop (most complex)

**Problem:** Synthesis is one-shot. Quality scoring exists (`query.go:548-578`) but runs *after* the answer is shown - it only helps future queries via lessons, not the current one.

### Implementation Details

**New function in `internal/engine/synthesizer.go`:**

```go
func SynthesizeWithValidation(
    ctx context.Context,
    client *llm.Client,
    question, soulContext string,
    results []sources.SourceResult,
    pastLessons []lessons.SimilarLesson,
    reExecuteFn func(ctx context.Context, guidance string) ([]sources.SourceResult, error),
) (string, *QualityAssessment, error)
```

**New struct:**

```go
type QualityAssessment struct {
    Quality        int    `json:"quality"`
    HasData        bool   `json:"has_data"`
    Reason         string `json:"reason"`
    MissingInfo    string `json:"missing_info"`
    SuggestedQuery string `json:"suggested_query"`
}
```

**Flow:**

```
1. First synthesis (existing Synthesize call)
2. Quality check:
   - If quality >= 3 → return answer (good enough)
   - If quality <= 2 AND reExecuteFn != nil:
     a. Extract missing_info and suggested_query from quality assessment
     b. Call reExecuteFn(ctx, guidance)
     c. Merge new results with original results
     d. Second synthesis with merged results
     e. Return second answer (no further loops)
3. Return answer + quality assessment
```

**Extend quality scoring schema** (`internal/llm/prompts.go`):

Add `missing_info` and `suggested_query` fields to `QualityScoreSchema` so the quality scorer tells us *what's missing*, not just a number.

**Pipeline orchestrator change** (`internal/cli/query.go`):

Replace `engine.Synthesize(...)` with `engine.SynthesizeWithValidation(...)`, providing a `reExecuteFn` callback that re-runs execution with guided intent:

```go
reExecuteFn := func(ctx context.Context, guidance string) ([]sources.SourceResult, error) {
    guidedIntent := &engine.Intent{
        RawQuery:    question + "\n\nAdditional guidance: " + guidance,
        // ... copy other fields from original intent
    }
    reExecResult, err := engine.Execute(ctx, client, plan, guidedIntent, resolved, libContext)
    if err != nil {
        return nil, err
    }
    return reExecResult.AllResults, nil
}
```

Same change needed in `internal/cli/session.go` for `runSessionQuery`.

**Safety rails:**
- Max 1 re-query cycle (hard-coded, not a general loop)
- `reExecuteFn` can be nil to disable (session mode, offline/Ollama mode)
- Budget cap: check `client.TotalCost()` before re-execution
- Re-execution benefits from Phase 2's retry-with-rephrase automatically

### Testing

- Mock client: first synthesis quality=1, re-execution returns better results, second synthesis quality=4 → verify loop runs exactly once
- Mock client: first synthesis quality=3 → verify no re-execution
- Verify `reExecuteFn=nil` disables the loop
- Verify budget cap prevents re-execution when cost is high
- End-to-end: question hitting empty source → verify answer improves after validation loop

---

## Summary

| Phase | Risk | Impact | Files Changed |
|-------|------|--------|---------------|
| 1. Transient retry | Low | Medium | `internal/llm/openai.go` |
| 2. Executor rephrase | Medium | High | `internal/engine/executor.go` |
| 3. Synthesis validation | Higher | High | `synthesizer.go`, `prompts.go`, `query.go`, `session.go` |

Each phase builds on the previous - Phase 3's re-execution uses Phase 2's retry logic, which uses Phase 1's transient retry.
