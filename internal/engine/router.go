package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/eval"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/llm"
)

// LLMRouteResult is what the router LLM call returns: a picked subset of
// the candidate sources, plus a one-line reason. The reason is preserved
// for verbose output and run logs so the user can audit why a particular
// source was chosen.
//
// NoneViable is the honest "nothing here can answer this" escape (added
// alongside the fillerRouteBoost cap to fix the "gworkspace answers a
// clock question" incident): the router used to be forced by prompt
// rule 11 to always return one source, even when none of the candidates
// were plausible, which let an arbitrary alphabetical-filler source win
// via the flat +1000 llmRouteBoost. When NoneViable is true, Reason
// carries the one-line explanation and Sources is expected to be empty.
// See resolveRoutePicks for the guard that prevents the LLM from using
// NoneViable to override a query the deterministic planner already
// handles confidently.
type LLMRouteResult struct {
	Sources    []string           `json:"sources"`
	Reason     string             `json:"reason"`
	Confidence map[string]float64 `json:"confidence,omitempty"` // per-source routing confidence 0.0-1.0
	NoneViable bool               `json:"none_viable,omitempty"`
}

// maxSources caps how many source descriptions we put into the LLM router
// prompt. Larger lists waste tokens and dilute the LLM's attention.
const maxSources = 15

// nameMatchPriorScore is the prior_score we attach to a source descriptor
// that was injected into the router's candidate list because the question
// literally named it. The value sits above topic-match (+8) so the
// LLM sees this as a strong
// deterministic signal but does not over-claim certainty (the deterministic
// route boost is +100, the LLM-pick boost is +1000).
const nameMatchPriorScore = 12

// pickingRulesPrompt is the static "rules" section that ends the LLM
// router prompt. Extracted to a const so router_test can verify rule
// presence without invoking the LLM. Rule 3 is the LITERAL NAME OVERRIDE
// added by issue #14 fix 2 - it short-circuits all fit-based scoring
// when the question literally names a source from the candidate list.
const pickingRulesPrompt = `
Return JSON with four fields:
- sources: array of 1-3 source names from the list above (use the exact names) -- leave this empty if none_viable is true
- reason: one short sentence explaining the choice (or, when none_viable is true, explaining why no candidate fits)
- confidence: object mapping each picked source name to a confidence score 0.0-1.0
  (1.0 = strong past evidence this source answers this type of question,
   0.7 = good fit but limited evidence, 0.4 = plausible but uncertain, 0.1 = speculative)
- none_viable: true only if NOT ONE of the candidate sources could plausibly answer the question; omit or set false otherwise

Picking rules (in priority order):
1. LITERAL NAME OVERRIDE (HIGHEST PRIORITY): If the question text contains any source's exact name or slug from the list above (e.g. "csv-viewer", "mcp-perforce", "thrive", "GeminiWatermarkTool", "camp-butz", "nytimes"), pick THAT source. The user named the source explicitly - that intent ALWAYS wins over past learning, fit scoring, and the web-search default. Past lessons that picked a different source for similar questions DO NOT override this - the user's explicit naming is the strongest signal possible. Only fall through if no listed source's name appears in the question. EXCEPTION: if the question is about GitHub concepts (issues, pull requests, PRs, repos, stars, forks), ALWAYS prefer the "github" source even if other source names appear in the question - the user is asking about the project's GitHub presence, not searching its code.
2. If a past learning entry has thumbs-down for a source on a similar question, AVOID that source.
3. If a past learning entry has thumbs-up or success for a source on a similar question, PREFER that source - UNLESS rule 1 applies (the user named a different source explicitly).
4. Otherwise pick by best fit between the question's intent and each source's description, capabilities, and topics.
5. Prefer ONE source over many when one source clearly fits. Only pick multiple if the question genuinely spans domains.
5a. INVESTIGATION FAN-OUT: If the parsed action is "investigate" (or the question asks "what was the issue/problem/bug", "why did X fail", "what happened with Y"), pick 2-3 sources spanning different vantage points - e.g. the entity's own repo (docs/runbook), a monorepo (service code/agent docs), and a data source (sqlite/plausible logs) when relevant. Investigation questions rarely have a single authoritative source; fan-out lets the synthesizer triangulate. This overrides rule 5 for investigate actions.
6. Do NOT pick a source whose description is unrelated to the question, even if it has a high deterministic prior.
7. NEVER pick a financial / expense / spending source unless the question explicitly mentions money, cost, dollars, spending, spend, expenses, budget, balance, income, transaction, payment, vendor, category, subcategory, or bucket. "Activity", "what did I do", "how am I doing" do NOT count as financial questions.
8. NEVER pick a workout / fitness source unless the question mentions exercise, running, lifting, workouts, miles, calories, weight, sleep, health metrics, eating, food, meals, nutrition, diet, macros, body composition, recipe, recipes, cooking, meal plan, grocery, or protein. "Activity" alone does not count.
9. NEVER pick a git / commit source unless the question mentions code, commits, branches, PRs, files, authors-of-changes, or similar repo concepts.
10. PREFER a web-search source when the question asks about current events, weather, news, real-time information, general knowledge ("what is X", "who is X", "how to X"), or anything that would benefit from a live internet search. EXCEPTION: if a more specific named source is in the candidate list (e.g. "nytimes" for news, an entity-named source for questions about that entity), prefer the specific source over generic web-search. Web-search is the fallback when no specific source fits.
11. If the question is genuinely ambiguous, pick the SINGLE source whose description best matches the most likely interpretation -- as long as at least one candidate could plausibly answer it. If NONE of the candidates could plausibly answer the question (e.g. every candidate is unrelated to the question's domain), do NOT force a pick: set none_viable to true, give a one-line reason, and return an empty sources list. An honest "no source can answer this" beats picking an irrelevant source just to have an answer.
`

// pickingRulesPromptForTest exposes the rules block for unit tests in
// the same package. Kept as a separate identifier so the const above
// stays unexported in the public package surface.
func pickingRulesPromptForTest() string { return pickingRulesPrompt }

// normalizeLLMPick cleans a source name returned by the router LLM. The
// LLM is told to return exact source names, but it sometimes parrots the
// formatted prompt line "csv-viewer (tool)" or wraps a name in quotes.
// We strip those decorations so the lookup against allSources/candidates
// hits. Trims whitespace, surrounding quotes, and any trailing
// parenthesized type annotation.
func normalizeLLMPick(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"'`)
	// Strip trailing " (type)" annotation if present.
	if idx := strings.LastIndex(name, " ("); idx > 0 && strings.HasSuffix(name, ")") {
		name = strings.TrimSpace(name[:idx])
	}
	return name
}

// sourceDesc is one row in the LLM router's candidate list. It is built
// from a config.Source plus the deterministic prior score. Lifted to
// package scope so addNameMatchedDescriptors can return slices of it.
type sourceDesc struct {
	Name         string
	Type         string
	Description  string
	Capabilities []string
	Entities     []string
	Score        int  // deterministic prior, 0 if not in candidates
	Filler       bool // true if injected only by the alphabetical top-up (see below); caps its LLM-pick boost at fillerRouteBoost
}

// addNameMatchedDescriptors injects sources from allSources into descs
// when their name (or one of its tokens) matches a raw entity from the
// parsed intent. This is the escape hatch for cwd-routed queries: the
// route restricts the planner's candidate list, but the router can still
// surface a directly-named source by reaching into allSources here.
//
// Tokens shorter than 4 characters and tokens on the generic blocklist
// are ignored to prevent "cli" / "data" / "tool" from causing false
// positives. The descs slice is returned with new entries appended.
func addNameMatchedDescriptors(
	descs []sourceDesc,
	seen map[string]bool,
	allSources config.Sources,
	rawEntities []string,
	cap int,
) []sourceDesc {
	if len(rawEntities) == 0 || len(allSources) == 0 {
		return descs
	}
	for _, rawEnt := range rawEntities {
		if len(descs) >= cap {
			break
		}
		normEnt := normalizeSourceToken(rawEnt)
		if normEnt == "" {
			continue
		}
		entTokens := tokenizeEntity(normEnt)
		for n, src := range allSources {
			if seen[n] || len(descs) >= cap {
				continue
			}
			normName := normalizeSourceToken(n)
			if normName == "" {
				continue
			}
			matched := false
			// Whole-entity exact match. Generic single-token entities
			// like "tool" or "data" are skipped here so we don't match
			// every source whose name contains them.
			if normEnt == normName && !isGenericNameToken(normEnt) {
				matched = true
			}
			// Compact match: "butterstack" == "butter-stack".
			if !matched && compactToken(normEnt) == compactToken(normName) && !isGenericNameToken(normEnt) {
				matched = true
			}
			// Whole-entity substring (entity contained in name) gated
			// by length 4 AND not on the generic blocklist. This drops
			// "cli", "api", "mcp" by length and "tool", "data", "docs"
			// by blocklist.
			if !matched && len(normEnt) >= 4 && !isGenericNameToken(normEnt) && strings.Contains(normName, normEnt) {
				matched = true
			}
			// Token-level match for multi-word entities like "thrive game".
			if !matched {
				for _, tok := range entTokens {
					if len(tok) < 4 || isGenericNameToken(tok) {
						continue
					}
					if tok == normName || strings.Contains(normName, tok) {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
			seen[n] = true
			descs = append(descs, sourceDesc{
				Name:         n,
				Type:         src.Type,
				Description:  src.Description,
				Capabilities: src.Capabilities,
				Entities:     src.Entities,
				Score:        nameMatchPriorScore,
			})
		}
	}
	return descs
}

// LLMRoute is the 4th LLM call in the pipeline. It runs AFTER the
// deterministic Plan as a refining filter: given the user's question,
// the candidate sources the planner ranked, and the most relevant past
// lessons, the LLM picks the best 1-3 sources to actually execute.
//
// Why a 4th LLM call instead of replacing the planner: the deterministic
// planner narrows 50+ sources to ~12 candidates cheaply, then the LLM
// applies semantic judgment to those candidates with full context. This
// is "LLM at edges, deterministic middle" upgraded to "LLM at edges +
// LLM picking the right edge."
//
// If the LLM call fails for any reason (network, timeout, malformed JSON)
// the function returns the input candidates unchanged so the pipeline
// degrades gracefully back to the deterministic planner. The error is
// returned for logging but never surfaced to the user.
//
// allSources is the full registered source map; LLMRoute consults it to
// build descriptions for sources the LLM might want to add even if the
// deterministic planner missed them. The LLM can pick names from
// allSources beyond the candidate set if a similar past lesson points
// at one.
func LLMRoute(
	ctx context.Context,
	client *llm.Client,
	question string,
	intent *Intent,
	candidates []ScoredSource,
	allSources config.Sources,
	pastLessons []lessons.SimilarLesson,
	soulContext string,
	domainKeywords map[string][]string,
	recentFailedReviews []eval.ReviewRecord,
) (*LLMRouteResult, []ScoredSource, error) {
	if client == nil || len(allSources) == 0 {
		return nil, candidates, nil
	}

	// Build the source description list. Include all candidates plus any
	// allSources entry referenced by a past lesson, capped at maxSources.
	seen := map[string]bool{}
	descs := make([]sourceDesc, 0, len(candidates))
	for _, c := range candidates {
		if c.Source == nil || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		descs = append(descs, sourceDesc{
			Name:         c.Name,
			Type:         c.Source.Type,
			Description:  c.Source.Description,
			Capabilities: c.Source.Capabilities,
			Entities:     c.Source.Entities,
			Score:        c.Score,
		})
	}
	qLower := strings.ToLower(question)

	// Inject sources based on domain keyword matching from brain/knowledge/domains/.
	// These profiles are generated by CompileSources during scan/index and replace
	// hardcoded domain injections. Each domain profile has a "Keywords:" line that
	// lists terms which should trigger routing to that source.
	if domainKeywords != nil {
		for source, keywords := range domainKeywords {
			if seen[source] {
				continue
			}
			for _, kw := range keywords {
				if strings.Contains(qLower, kw) {
					if src, ok := allSources[source]; ok {
						seen[source] = true
						descs = append(descs, sourceDesc{
							Name:         source,
							Type:         src.Type,
							Description:  src.Description,
							Capabilities: src.Capabilities,
							Entities:     src.Entities,
							Score:        nameMatchPriorScore,
						})
					}
					break
				}
			}
		}
	}

	// Pull in feedback-nominated sources from thumbs-down lessons that
	// the planner/candidates missed. User explicitly said "route to X"
	// in --because text; we should surface X even if the current question
	// doesn't trigger X's name/entity match directly. Capped at maxSources
	// like the other injection paths.
	for _, ls := range pastLessons {
		if ls.Lesson.Feedback != lessons.FeedbackThumbsDown {
			continue
		}
		intended := append([]string(nil), ls.Lesson.FeedbackIntendedSources...)
		if ls.Lesson.FeedbackIntendedSource != "" {
			intended = append(intended, ls.Lesson.FeedbackIntendedSource)
		}
		for _, name := range intended {
			if seen[name] || len(descs) >= maxSources {
				continue
			}
			if src, ok := allSources[name]; ok {
				seen[name] = true
				descs = append(descs, sourceDesc{
					Name:         name,
					Type:         src.Type,
					Description:  src.Description,
					Capabilities: src.Capabilities,
					Entities:     src.Entities,
					Score:        nameMatchPriorScore,
				})
			}
		}
	}

	// Pull in lesson-referenced sources the planner missed.
	for _, ls := range pastLessons {
		for _, name := range ls.Lesson.Sources {
			if seen[name] || len(descs) >= maxSources {
				continue
			}
			if src, ok := allSources[name]; ok {
				seen[name] = true
				descs = append(descs, sourceDesc{
					Name:         name,
					Type:         src.Type,
					Description:  src.Description,
					Capabilities: src.Capabilities,
					Entities:     src.Entities,
					Score:        0,
				})
			}
		}
	}
	// First top-up pass: inject sources whose name (or name token) matches
	// a raw entity extracted by the parser. This handles "what does
	// csv-viewer do?" where the parser puts "csv-viewer" into RawEntities
	// but the deterministic planner scored the source 0 because its
	// config has no entities list. Without this pass, the LLM would never
	// see csv-viewer in its candidate list and would default to the
	// cwd-routed source (e.g. aida).
	if intent != nil {
		descs = addNameMatchedDescriptors(descs, seen, allSources, intent.RawEntities, maxSources)
	}
	// Second top-up pass: if we still have very few descs, fall back to
	// alphabetical so the LLM at least has a non-empty list. These
	// entries are arbitrary filler -- never evaluated by the
	// deterministic planner nor matched to the question by name/entity/
	// domain-keyword -- so they're tagged Filler and get a capped boost
	// if the LLM picks one anyway (see fillerRouteBoost).
	if len(descs) < 5 {
		names := make([]string, 0, len(allSources))
		for n := range allSources {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if seen[n] || len(descs) >= maxSources {
				continue
			}
			src := allSources[n]
			seen[n] = true
			descs = append(descs, sourceDesc{
				Name:         n,
				Type:         src.Type,
				Description:  src.Description,
				Capabilities: src.Capabilities,
				Entities:     src.Entities,
				Filler:       true,
			})
		}
	}
	if len(descs) == 0 {
		return nil, candidates, nil
	}

	// Snapshot which names are filler before descs gets consumed further
	// (feedback/eval boosts below only touch Score, not Name/Filler), so
	// resolveRoutePicks can cap the LLM-pick boost for any filler name
	// the LLM ends up choosing.
	fillerNames := make(map[string]bool)
	for _, d := range descs {
		if d.Filler {
			fillerNames[d.Name] = true
		}
	}

	// Phase 3.3: apply deterministic feedback boost/demote from past
	// thumbs-down lessons. The LLM router reads the same directive in
	// prose form later, but a +/-100 adjustment on prior_score makes the
	// preference enforce even when the LLM would otherwise ignore the
	// prose hint. Cap at the top-3 most-similar thumbs-down lessons so a
	// single directive does not dominate unrelated questions.
	//
	// Magnitude raised from 50 → 100 (2026-05-01) because the prior
	// value sat below the deterministic +100 baseline of high-cap
	// sources like `snow`, so the demote on a thumbs-down didn't
	// dislodge a previously-blessed source even with explicit
	// excluded_sources guidance - the LLM router would still flip back
	// to it on prose reasoning ("past learning shows snow worked").
	const feedbackBoostMagnitude = 100
	const feedbackLessonCap = 3
	applied := 0
	for _, ls := range pastLessons {
		if applied >= feedbackLessonCap {
			break
		}
		if ls.Lesson.Feedback != lessons.FeedbackThumbsDown {
			continue
		}
		intended := append([]string(nil), ls.Lesson.FeedbackIntendedSources...)
		if ls.Lesson.FeedbackIntendedSource != "" {
			intended = append(intended, ls.Lesson.FeedbackIntendedSource)
		}
		excluded := ls.Lesson.FeedbackExcludedSources
		if len(intended) == 0 && len(excluded) == 0 {
			continue
		}
		intendedSet := make(map[string]bool, len(intended))
		for _, n := range intended {
			intendedSet[n] = true
		}
		excludedSet := make(map[string]bool, len(excluded))
		for _, n := range excluded {
			excludedSet[n] = true
		}
		for i := range descs {
			switch {
			case intendedSet[descs[i].Name]:
				descs[i].Score += feedbackBoostMagnitude
			case excludedSet[descs[i].Name]:
				descs[i].Score -= feedbackBoostMagnitude
			}
		}
		applied++
	}
	if applied > 0 {
		sort.SliceStable(descs, func(i, j int) bool { return descs[i].Score > descs[j].Score })
	}

	// Phase 4 (sensor side of the harness): apply per-source
	// adjustments derived from recent failed eval-runs that touched
	// similar entities. Magnitudes are deliberately small (±25 per
	// occurrence, capped at ±50 absolute) so this auto-generated
	// signal can't overpower explicit thumbs-down feedback (±100).
	// See internal/eval/boosts.go for the policy table.
	if len(recentFailedReviews) > 0 {
		evalAdjustments := eval.FailureBoosts(recentFailedReviews)
		if len(evalAdjustments) > 0 {
			for i := range descs {
				if delta, ok := evalAdjustments[descs[i].Name]; ok {
					descs[i].Score += delta
				}
			}
			sort.SliceStable(descs, func(i, j int) bool { return descs[i].Score > descs[j].Score })
		}
	}

	// Build the prompt sections.
	var sb strings.Builder
	if soulContext != "" {
		sb.WriteString(soulContext)
		sb.WriteString("\n")
	}
	sb.WriteString("Question: ")
	sb.WriteString(question)
	sb.WriteString("\n\n")
	if intent != nil && intent.Action != "" {
		fmt.Fprintf(&sb, "Parsed action: %s\n", intent.Action)
	}
	if intent != nil && intent.Timeframe != "" {
		fmt.Fprintf(&sb, "Parsed timeframe: %s\n", intent.Timeframe)
	}
	if intent != nil && len(intent.Keywords) > 0 {
		fmt.Fprintf(&sb, "Parsed keywords: %s\n", strings.Join(intent.Keywords, ", "))
	}
	if intent != nil && len(intent.RawEntities) > 0 {
		fmt.Fprintf(&sb, "Parsed entities: %s\n", strings.Join(intent.RawEntities, ", "))
	}
	sb.WriteString("\nAvailable sources (ranked by deterministic prior; pick the BEST 1-3 for this question):\n")
	for i, d := range descs {
		fmt.Fprintf(&sb, "%d. %s (%s)\n", i+1, d.Name, d.Type)
		if d.Description != "" {
			fmt.Fprintf(&sb, "   description: %s\n", truncateLine(d.Description, 200))
		}
		if len(d.Capabilities) > 0 {
			fmt.Fprintf(&sb, "   capabilities: %s\n", strings.Join(d.Capabilities, ", "))
		}
		if len(d.Entities) > 0 {
			fmt.Fprintf(&sb, "   topics: %s\n", strings.Join(d.Entities, ", "))
		}
		if d.Score > 0 {
			fmt.Fprintf(&sb, "   prior_score: %d\n", d.Score)
		}
	}

	if len(pastLessons) > 0 {
		// Sort by composite weight (quality * recency * feedback boost)
		// so the strongest signals appear first in the prompt.
		sort.SliceStable(pastLessons, func(i, j int) bool {
			wi := pastLessons[i].Lesson.CompositeWeight(pastLessons[i].Score)
			wj := pastLessons[j].Lesson.CompositeWeight(pastLessons[j].Score)
			return wi > wj
		})
		sb.WriteString("\nPast learning (strongest signals first; treat thumbs-down as DO NOT pick):\n")
		for _, ls := range pastLessons {
			l := ls.Lesson
			// Issue #14 Bug D: a lesson is RecoverableError when EITHER
			// every source errored at the adapter layer (bad SQL/grep/
			// exec) OR every source returned success/empty but the
			// synthesized answer was an "I don't know" disclaimer. In
			// both cases the lesson is NOT positive precedent - it does
			// not mean the source was the right pick. The LLM should
			// neither prefer nor avoid the source on similar future
			// questions; it should pick by description fit instead.
			outcome := l.SuccessSummary() // helper below
			feedback := ""
			switch l.Feedback {
			case lessons.FeedbackThumbsUp:
				feedback = " 👍"
			case lessons.FeedbackThumbsDown:
				feedback = " 👎"
			case lessons.FeedbackNote:
				feedback = " 📝"
			}
			noPrecedentNote := ""
			if l.RecoverableError && l.Feedback == lessons.FeedbackNone {
				noPrecedentNote = " (NOT positive precedent: source ran but produced no useful answer; ignore this lesson when picking)"
			}
			qualityNote := ""
			if l.Quality > 0 {
				icon := "✗"
				if l.Quality >= 4 {
					icon = "✓"
				} else if l.Quality == 3 {
					icon = "~"
				}
				qualityNote = fmt.Sprintf(" [quality %d/5 %s]", l.Quality, icon)
			}
			intendedNote := ""
			if l.FeedbackIntendedSource != "" {
				intendedNote = fmt.Sprintf(" (user wanted: %s)", l.FeedbackIntendedSource)
			}
			weight := l.CompositeWeight(ls.Score)
			fmt.Fprintf(&sb, "- [w=%.2f] %q -> %s [%s]%s%s%s%s (sim %.2f)\n",
				weight, truncateLine(l.Question, 80), strings.Join(l.Sources, ","),
				outcome, qualityNote, feedback, intendedNote, noPrecedentNote, ls.Score)
			if l.FeedbackReason != "" {
				fmt.Fprintf(&sb, "    user said: %s\n", truncateLine(l.FeedbackReason, 200))
			}
		}
	}

	sb.WriteString(pickingRulesPrompt)

	const systemPrompt = `You are a query router for an "agent of agents" CLI. Your job is to pick the right data source for the user's question. You see the question, the parsed intent, descriptions of available sources, and past learning from previous queries. Be decisive and concise. Return ONLY the JSON.`

	// NOTE: Anthropic's structured output JSON schema enforcement does
	// NOT support `minItems` / `maxItems` on arrays. We constrain the
	// list size via the prompt instead and trim defensively below.
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"sources": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "string"},
			},
			"reason": map[string]interface{}{"type": "string"},
			"none_viable": map[string]interface{}{
				"type": "boolean",
			},
		},
		// none_viable is intentionally NOT in required: it's a new field
		// (issue: gworkspace-answers-a-clock-question incident) and must
		// stay optional so older-shaped responses (e.g. cached prompts,
		// offline/Ollama models that ignore additionalProperties) still
		// parse without it.
		"required":             []string{"sources", "reason"},
		"additionalProperties": false,
	}

	raw, err := client.CompleteJSONWithStage(ctx, "route", systemPrompt, sb.String(), schema)
	if err != nil {
		return nil, candidates, fmt.Errorf("llm route call failed: %w", err)
	}

	var result LLMRouteResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return nil, candidates, fmt.Errorf("llm route json parse: %w", err)
	}
	return resolveRoutePicks(&result, candidates, allSources, fillerNames)
}

// llmRouteBoost is the score bonus applied to a source the LLM router
// picked. It is large enough to steamroll any deterministic prior so the
// LLM's semantic judgment wins over the planner's raw ranking.
const llmRouteBoost = 1000

// fillerRouteBoost caps the LLM-pick bonus for sources that only appear
// in the candidate list because of the alphabetical top-up filler (see
// the "Second top-up pass" above), which pads the list to a minimum of
// 5 entries so the LLM always has a non-trivial list to reason over.
// Filler sources were never evaluated by the deterministic planner nor
// matched to the question by name/entity/domain-keyword -- they're
// arbitrary. Before this cap, a filler pick got the same +1000 as a
// genuinely-evaluated candidate; that's how a Google-Workspace source
// won a "what time is it" query (rule 11 forced a pick, the filler pad
// supplied gworkspace, and +1000 steamrolled everything). Capping filler
// picks at +100 keeps them roughly on par with the deterministic
// route-boost baseline instead of letting them dominate.
const fillerRouteBoost = 100

// resolveRoutePicks turns the parsed LLM router response into the final
// (*LLMRouteResult, []ScoredSource) pair LLMRoute returns. Split out from
// LLMRoute so it can be unit tested without an LLM client: the caller
// supplies the already-parsed result plus the same candidates/allSources/
// fillerNames LLMRoute had in scope.
//
// Handles three concerns:
//  1. The none_viable escape: honored only when the LLM didn't also
//     return picks AND deterministic routing didn't already find a
//     strong candidate (score >= 100, e.g. route-boosted or heavily
//     capability-matched) -- the LLM must not be able to nuke a query
//     the deterministic planner already handles confidently.
//  2. The defensive 3-source cap and name normalization.
//  3. Applying llmRouteBoost per pick, capped at fillerRouteBoost for
//     any pick whose name is in fillerNames.
func resolveRoutePicks(
	result *LLMRouteResult,
	candidates []ScoredSource,
	allSources config.Sources,
	fillerNames map[string]bool,
) (*LLMRouteResult, []ScoredSource, error) {
	if result.NoneViable {
		strongDeterministic := false
		for _, c := range candidates {
			if c.Score >= 100 {
				strongDeterministic = true
				break
			}
		}
		if len(result.Sources) > 0 || strongDeterministic {
			// The LLM contradicted itself (claimed none_viable but still
			// returned picks), or deterministic routing already found a
			// strong candidate. Either way, don't let none_viable override
			// that: clear the flag and fall through to normal handling
			// below (which passes through the deterministic candidates
			// unchanged if Sources also ended up empty).
			result.NoneViable = false
		}
	}
	if len(result.Sources) == 0 {
		return result, candidates, nil
	}
	// Defensive cap: trim to 3 even if the LLM ignored the prompt rule.
	if len(result.Sources) > 3 {
		result.Sources = result.Sources[:3]
	}

	// Build a new ScoredSource list from the LLM picks. Preserve the
	// original Source pointer (with its config) by looking up against
	// allSources, falling back to candidates by name if missing.
	//
	// Normalize each pick before lookup: the LLM sometimes parrots back
	// the formatted prompt line ("csv-viewer (tool)") instead of just
	// the source name. Strip a trailing " (type)" suffix and any quoting.
	candidateByName := map[string]*ScoredSource{}
	for i := range candidates {
		candidateByName[candidates[i].Name] = &candidates[i]
	}
	picked := make([]ScoredSource, 0, len(result.Sources))
	for _, name := range result.Sources {
		name = normalizeLLMPick(name)
		boost := llmRouteBoost
		if fillerNames[name] {
			boost = fillerRouteBoost
		}
		if existing, ok := candidateByName[name]; ok {
			ss := *existing
			ss.Score += boost
			picked = append(picked, ss)
			continue
		}
		if src, ok := allSources[name]; ok {
			picked = append(picked, ScoredSource{
				Name:   name,
				Source: src,
				Score:  boost,
			})
		}
	}
	// Update result.Sources in place so the surfaced "reason" log lines
	// reference the cleaned-up source names rather than "(tool)" garbage.
	for i, n := range result.Sources {
		result.Sources[i] = normalizeLLMPick(n)
	}
	if len(picked) == 0 {
		// LLM picked names that aren't real sources -- log and degrade.
		return result, candidates, fmt.Errorf("llm route picks not in registry: %v", result.Sources)
	}
	return result, picked, nil
}

// SuccessSummary is exported because the router prompt builder needs it
// across the package boundary.
func init() {
	// no-op; keeps the import list stable for grep
}
