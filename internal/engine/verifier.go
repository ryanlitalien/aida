package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/sources"
	"github.com/ryanlitalien/aida/internal/ui"
)

// VerifiedResult wraps a SourceResult with confidence scoring.
type VerifiedResult struct {
	Result           sources.SourceResult
	Confidence       float64 `json:"confidence"` // 0.0-1.0
	Relevance        string  `json:"relevance"`  // direct, partial, tangential, irrelevant
	ConfidenceReason string  `json:"confidence_reason"`
	ShouldRetry      bool    `json:"should_retry"`
}

// Verify runs confidence scoring on execution results. Uses a two-tier approach:
//  1. Computational tier (always runs, free, ~0ms): heuristic scoring
//  2. Inferential tier (Haiku, optional): LLM judgment for ambiguous results
//
// Results with confidence < 0.2 are flagged for exclusion from synthesis.
func Verify(ctx context.Context, client *llm.Client, question string, intent *Intent, results []sources.SourceResult) []VerifiedResult {
	verified := make([]VerifiedResult, len(results))

	for i, r := range results {
		vr := VerifiedResult{Result: r}

		// Tier 1: Computational scoring (always runs)
		vr.Confidence, vr.Relevance, vr.ConfidenceReason = computationalScore(r, question, intent)

		// Tier 2: Inferential scoring (only for ambiguous results 0.3-0.7)
		if client != nil && vr.Confidence >= 0.3 && vr.Confidence <= 0.7 {
			inf := inferentialScore(ctx, client, question, r)
			if inf != nil {
				vr.Confidence = inf.Confidence
				vr.Relevance = inf.Relevance
				vr.ConfidenceReason = inf.Reason
				vr.ShouldRetry = inf.ShouldRetry
			}
		}

		// Mark retry for very low confidence with non-error status
		if vr.Confidence < 0.2 && r.Status == "success" {
			vr.ShouldRetry = true
		}

		verified[i] = vr
		ui.PrintVerbose("Verify "+r.Source, formatVerifyResult(vr))
	}

	return verified
}

func computationalScore(r sources.SourceResult, question string, intent *Intent) (float64, string, string) {
	// Brain source: user-provided feedback data - always high confidence.
	if r.Source == "brain" {
		return 0.9, "direct", "user-provided feedback data"
	}

	// Error/timeout → 0.0
	if r.Status == "error" || r.Status == "timeout" {
		return 0.0, "irrelevant", "source returned " + r.Status
	}

	// Empty results
	if r.Status == "empty" || (len(r.Artifacts) == 0 && r.Summary == "") {
		return 0.1, "irrelevant", "no data returned"
	}

	confidence := 0.5 // baseline for success with artifacts
	relevance := "partial"
	reasons := []string{}

	// Artifact count boost
	switch {
	case len(r.Artifacts) >= 5:
		confidence += 0.2
		reasons = append(reasons, "rich artifacts")
	case len(r.Artifacts) >= 2:
		confidence += 0.1
		reasons = append(reasons, "multiple artifacts")
	case len(r.Artifacts) == 0:
		confidence -= 0.2
		reasons = append(reasons, "no artifacts")
	}

	// Check if result contains entities from the question
	qLower := strings.ToLower(question)
	summaryLower := strings.ToLower(r.Summary)
	if intent != nil {
		for _, entity := range intent.RawEntities {
			eLower := strings.ToLower(entity)
			if len(eLower) > 3 && strings.Contains(summaryLower, eLower) {
				confidence += 0.15
				relevance = "direct"
				reasons = append(reasons, "contains entity: "+entity)
				break
			}
		}
	}

	// Keyword overlap between question and result
	qWords := strings.Fields(qLower)
	matchCount := 0
	for _, w := range qWords {
		if len(w) > 3 && strings.Contains(summaryLower, w) {
			matchCount++
		}
	}
	if matchCount >= 3 {
		confidence += 0.1
		reasons = append(reasons, "good keyword overlap")
	}

	// Very short summary for non-lookup queries
	if len(r.Summary) < 50 && intent != nil && intent.Action != "lookup" {
		confidence -= 0.15
		reasons = append(reasons, "very short summary")
	}

	// Clamp to [0, 1]
	if confidence > 1.0 {
		confidence = 1.0
	}
	if confidence < 0.0 {
		confidence = 0.0
	}

	if confidence >= 0.7 {
		relevance = "direct"
	} else if confidence < 0.3 {
		relevance = "tangential"
	}

	return confidence, relevance, strings.Join(reasons, "; ")
}

type inferentialResult struct {
	Confidence  float64 `json:"confidence"`
	Relevance   string  `json:"relevance"`
	Reason      string  `json:"reason"`
	ShouldRetry bool    `json:"should_retry"`
}

func inferentialScore(ctx context.Context, client *llm.Client, question string, r sources.SourceResult) *inferentialResult {
	systemPrompt := `You are a result quality assessor. Given a question and a source result, rate how well the result answers the question.

Return JSON:
{
  "confidence": <0.0-1.0>,
  "relevance": "<direct|partial|tangential|irrelevant>",
  "reason": "<one sentence>",
  "should_retry": <true if a different query might get better results>
}`

	snippet := r.Summary
	if len(snippet) > 500 {
		snippet = snippet[:500]
	}
	for _, a := range r.Artifacts {
		if len(snippet) > 800 {
			break
		}
		snippet += "\n[" + a.Type + "] " + a.ID
		if a.Snippet != "" {
			s := a.Snippet
			if len(s) > 100 {
				s = s[:100]
			}
			snippet += ": " + s
		}
	}

	userPrompt := "Question: " + question + "\n\nSource: " + r.Source + "\nStatus: " + r.Status + "\nResult:\n" + snippet

	raw, err := client.CompleteJSONWithStage(ctx, "verify", systemPrompt, userPrompt, nil)
	if err != nil {
		ui.PrintVerbose("Verify LLM", "failed: "+err.Error())
		return nil
	}

	var result inferentialResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil
	}
	return &result
}

func formatVerifyResult(vr VerifiedResult) string {
	retry := ""
	if vr.ShouldRetry {
		retry = " [retry]"
	}
	return fmt.Sprintf("confidence=%.2f relevance=%s%s (%s)",
		vr.Confidence, vr.Relevance, retry, vr.ConfidenceReason)
}
