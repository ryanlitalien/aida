package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
)

// referentialPhraseRE matches phrases that signal a follow-up question
// referring to the previous turn ("do the same thing", "again for X",
// "what about Y", "and for Z", "also in Q"). Used by
// IsReferentialFollowUp to decide whether a zero-entity parse without
// prior-turn context should be flagged as ambiguous instead of silently
// widening scope.
//
// Deliberately conservative: false negatives (missing a referential
// phrase) are acceptable - the user gets a clean zero-entity parse and
// can restate. False positives (flagging a genuine standalone question)
// are not - they block valid queries.
var referentialPhraseRE = regexp.MustCompile(`(?i)\b(same (thing|question|query)|do the same|do that again|again(?: for\b|,|\.|$| but\b)|what about\b|how about\b|and (for|in)\b|but (for|in)\b|also (for|in|show)\b)`)

// IsReferentialFollowUp reports whether the question uses phrasing that
// only resolves against a prior turn ("same thing", "what about X",
// "and for Y"). When this is true and the parser returned no entities
// and no prior turn was available, the CLI should surface a clear
// ambiguity error instead of running an unscoped query.
func IsReferentialFollowUp(question string) bool {
	q := strings.TrimSpace(question)
	if q == "" {
		return false
	}
	return referentialPhraseRE.MatchString(q)
}

// Intent represents the structured output from parsing a user's natural language query.
type Intent struct {
	RawQuery            string   `json:"-"`                              // original query (not from LLM)
	RawEntities         []string `json:"raw_entities"`                   // extracted ARIs, names, URLs
	Timeframe           string   `json:"timeframe"`                      // "yesterday", "1h", "last week", or ""
	Action              string   `json:"action"`                         // investigate, query, lookup, record, test, search, task
	Keywords            []string `json:"keywords"`                       // relevant terms
	Amount              *float64 `json:"amount,omitempty"`               // financial amount if mentioned
	Category            string   `json:"category,omitempty"`             // expense category if applicable
	TaskAction          string   `json:"task_action,omitempty"`          // create, list, done (when action=task)
	TaskTitle           string   `json:"task_title,omitempty"`           // task description (when task_action=create)
	ComprehensiveIntent bool     `json:"comprehensive_intent,omitempty"` // true when user asks for "all", "every", "complete list"
	EffectiveQuestion   string   `json:"effective_question,omitempty"`   // set for referential follow-ups when a PriorTurn is available: a standalone rewrite that weaves prior-turn scope into the question text, so executor/synthesizer see full context
}

// EffectiveQuery returns the question text that downstream pipeline
// stages (executor query construction, router, synthesizer) should
// treat as the user's intent. When the parser has produced a
// self-contained rewrite for a referential follow-up it lives in
// EffectiveQuestion and is preferred; otherwise the original RawQuery
// is returned unchanged.
func (i *Intent) EffectiveQuery() string {
	if i == nil {
		return ""
	}
	if i.EffectiveQuestion != "" {
		return i.EffectiveQuestion
	}
	return i.RawQuery
}

// PriorTurn carries the resolved context of the most recent query in the
// same cwd so referential follow-ups ("do the same thing, but for issues",
// "what about last month", "and in acme_widgets?") can inherit scope
// instead of silently degrading to an empty parse.
//
// The caller (usually cli/query.go) looks up the prior run with
// runs.FindLatestInCwd and passes it in; the parser prompt presents it to
// the LLM as explicit context and instructs the LLM to inherit entities
// from the prior turn when the current question is referential.
type PriorTurn struct {
	Question  string   // as-typed prior user question
	Entities  []string // entities the prior parse extracted
	Action    string   // prior action (query, investigate, ...)
	Timestamp string   // RFC3339 of when the prior turn ran
}

// Parse is Step 1 of the pipeline: parse natural language into a structured Intent via LLM.
// soulContext is the pre-formatted user identity block from soul.yaml (may be "").
// priorTurn carries the most recent in-cwd run for referential follow-ups (may be nil).
func Parse(ctx context.Context, client *llm.Client, question, soulContext string, priorTurn *PriorTurn) (*Intent, error) {
	schema := llm.IntentSchema()

	response, err := client.CompleteJSONWithStage(
		ctx,
		"parse",
		llm.IntentParseSystemPrompt,
		llm.IntentParseUserPrompt(question, soulContext, toLLMPriorTurn(priorTurn)),
		schema,
	)
	if err != nil {
		return nil, fmt.Errorf("intent parsing failed: %w", err)
	}

	var intent Intent
	if err := json.Unmarshal([]byte(response), &intent); err != nil {
		return nil, fmt.Errorf("parsing intent JSON: %w", err)
	}

	intent.RawQuery = question

	// Normalize empty fields
	if intent.RawEntities == nil {
		intent.RawEntities = []string{}
	}
	if intent.Keywords == nil {
		intent.Keywords = []string{}
	}

	return &intent, nil
}

// toLLMPriorTurn converts the engine-side PriorTurn into the llm-package
// shape used by IntentParseUserPrompt. Keeps llm free of engine imports.
func toLLMPriorTurn(p *PriorTurn) *llm.PriorTurn {
	if p == nil {
		return nil
	}
	return &llm.PriorTurn{
		Question:  p.Question,
		Entities:  p.Entities,
		Action:    p.Action,
		Timestamp: p.Timestamp,
	}
}
