package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
)

// ExecutionPlan describes what sources to query and in what order.
type ExecutionPlan struct {
	Phases        []Phase
	AllCandidates []ScoredSource // every source ranked, including zero-score - powers --explain
}

// Phase is a group of source queries that can execute together.
type Phase struct {
	Name           string
	Sources        []ScoredSource
	Parallel       bool
	DependsOnPrior bool
}

// ScoredSource is a source with its relevance score.
type ScoredSource struct {
	Name   string
	Source *config.Source
	Score  int
	Trace  *ScoreTrace // per-bucket breakdown; nil in hot paths that skip trace capture
}

// ScoreTrace records how a source's score was built up, bucket by bucket.
// Populated in rankSources and restrictAndBoost; rendered by PrintExplain.
type ScoreTrace struct {
	Components []ScoreComponent
}

// ScoreComponent is one contribution to a source's score.
type ScoreComponent struct {
	Bucket string // "name-match", "topic", "capability", "keyword-desc", "type-bonus", "route-boost"
	Points int
	Note   string // human-readable explanation, optional
}

// Add appends a component to the trace. Safe on nil (no-op), so call sites
// can unconditionally record without null-checks.
func (t *ScoreTrace) Add(bucket string, points int, note string) {
	if t == nil {
		return
	}
	t.Components = append(t.Components, ScoreComponent{Bucket: bucket, Points: points, Note: note})
}

// PointsFor returns the total points recorded under a bucket name. Returns 0
// for unknown buckets or nil traces - used by the explain renderer.
func (t *ScoreTrace) PointsFor(bucket string) int {
	if t == nil {
		return 0
	}
	sum := 0
	for _, c := range t.Components {
		if c.Bucket == bucket {
			sum += c.Points
		}
	}
	return sum
}

// Plan is Step 4 of the pipeline: deterministic capability matching and execution planning.
//
// routedSources, when non-empty, restricts ranking to only sources whose
// names appear in the slice -- this is the route-driven path that gives
// directory- and entity-based priors precedence over the global capability
// scoring. When empty, the planner falls back to global ranking.
func Plan(classified *ClassifiedIntent, resolved *ResolvedContext, sources config.Sources, profile *config.Profile, routedSources []string) *ExecutionPlan {
	allRanked := rankSources(classified, resolved, sources)
	// Split into "scoring candidates" (non-zero, feed the planner) and the
	// full ranked list (drives --explain). Route-restriction below may
	// further narrow candidates; AllCandidates keeps the full picture so
	// the explain output can show why a source was cut.
	candidates := nonZero(allRanked)
	if len(routedSources) > 0 {
		candidates = restrictAndBoost(candidates, sources, routedSources)
	}
	if len(candidates) == 0 {
		return &ExecutionPlan{AllCandidates: allRanked}
	}

	phases := planPhases(classified.Strategy, candidates)
	return &ExecutionPlan{Phases: phases, AllCandidates: allRanked}
}

// planPhases builds the Phases slice for the chosen strategy. Separated from
// Plan so the top-level function can set AllCandidates uniformly.
func planPhases(strategy Strategy, candidates []ScoredSource) []Phase {
	switch strategy {
	case StrategyLookup:
		return []Phase{{Name: "lookup", Sources: top(candidates, 1), Parallel: false}}

	case StrategyQuery:
		return []Phase{{Name: "query", Sources: top(candidates, 5), Parallel: true}}

	case StrategyInvestigate:
		diagnoseSources := filterByCapabilities(candidates,
			[]string{"sql-query", "log-query", "error-investigation", "api-testing", "ari-lookup", "partner-lookup", "pr-lookup"})
		contextSources := filterByCapabilities(candidates,
			[]string{"code-reference", "partner-docs"})

		phases := []Phase{
			{Name: "diagnose", Sources: top(diagnoseSources, 3), Parallel: true},
		}
		if len(contextSources) > 0 {
			phases = append(phases, Phase{
				Name:           "contextualize",
				Sources:        top(contextSources, 3),
				Parallel:       true,
				DependsOnPrior: true,
			})
		}
		return phases

	case StrategyRecord:
		recordSources := filterByCapabilities(candidates,
			[]string{"expense-tracking", "record"})
		if len(recordSources) == 0 {
			recordSources = top(candidates, 1)
		}
		return []Phase{{Name: "record", Sources: top(recordSources, 1), Parallel: false}}

	case StrategyExecute:
		return []Phase{{Name: "execute", Sources: top(candidates, 1), Parallel: false}}

	case StrategySearch:
		searchSources := filterByCapabilities(candidates,
			[]string{"code-reference", "partner-docs", "sql-query"})
		if len(searchSources) == 0 {
			searchSources = candidates
		}
		return []Phase{{Name: "search", Sources: top(searchSources, 5), Parallel: true}}

	default:
		return []Phase{{Name: "query", Sources: top(candidates, 3), Parallel: true}}
	}
}

// nonZero returns only sources with score > 0. Used by Plan to separate the
// full traced list (for --explain) from the subset that feeds phase selection.
func nonZero(scored []ScoredSource) []ScoredSource {
	out := make([]ScoredSource, 0, len(scored))
	for _, s := range scored {
		if s.Score > 0 {
			out = append(out, s)
		}
	}
	return out
}

// rankSources scores and ranks all sources by relevance to the query.
//
// Note on the cwd-routed path: when a route is active, restrictAndBoost
// (called from Plan) hard-drops every source not in the routed set.
// This means a +30 name-match score here has NO effect for cwd-routed
// queries - the escape hatch for those is addNameMatchedDescriptors in
// router.go, which can inject named sources directly into the LLM
// router's candidate list. This scoring block exists for the
// non-cwd-routed (global) path so the two paths align.
func rankSources(classified *ClassifiedIntent, resolved *ResolvedContext, sources config.Sources) []ScoredSource {
	scored := make([]ScoredSource, 0, len(sources))

	for name, src := range sources {
		trace := &ScoreTrace{}
		score := 0

		// Name match: strongest signal. If the question's raw_entities
		// contain the source's exact name or one of its tokens, this
		// source is almost certainly the right answer.
		nameMatched := false
		normName := normalizeSourceToken(name)
		if normName != "" && classified.Intent != nil {
			for _, rawEnt := range classified.Intent.RawEntities {
				normEnt := normalizeSourceToken(rawEnt)
				if normEnt == "" {
					continue
				}
				if normEnt == normName && !isGenericNameToken(normEnt) {
					score += 30
					trace.Add("name-match", 30, fmt.Sprintf("raw entity '%s' == source name", rawEnt))
					nameMatched = true
					break
				}
				if compactToken(normEnt) == compactToken(normName) && !isGenericNameToken(normEnt) {
					score += 30
					trace.Add("name-match", 30, fmt.Sprintf("raw entity '%s' compact-matches source", rawEnt))
					nameMatched = true
					break
				}
				if len(normEnt) >= 4 && !isGenericNameToken(normEnt) && strings.Contains(normName, normEnt) {
					score += 15
					trace.Add("name-match", 15, fmt.Sprintf("raw entity '%s' substring of source name", rawEnt))
					nameMatched = true
					break
				}
				for _, tok := range tokenizeEntity(normEnt) {
					if len(tok) < 4 || isGenericNameToken(tok) {
						continue
					}
					if tok == normName {
						score += 30
						trace.Add("name-match", 30, fmt.Sprintf("token '%s' of entity '%s' == source name", tok, rawEnt))
						nameMatched = true
						break
					}
					if strings.Contains(normName, tok) {
						score += 15
						trace.Add("name-match", 15, fmt.Sprintf("token '%s' of entity '%s' substring of source", tok, rawEnt))
						nameMatched = true
						break
					}
				}
				if nameMatched {
					break
				}
			}
		}
		if nameMatched {
			// Name match is terminal: skip the other scoring blocks to
			// avoid double-counting (Fix 3 added source names to their
			// own entities list, which would cause topic-match to refire).
			scored = append(scored, ScoredSource{Name: name, Source: src, Score: score, Trace: trace})
			continue
		}

		// Topic match: src.Entities vs intent.Keywords / intent.RawEntities.
		topicWords := append([]string{}, classified.Intent.Keywords...)
		topicWords = append(topicWords, classified.Intent.RawEntities...)
		for _, ent := range src.Entities {
			lowerEnt := strings.ToLower(ent)
			for _, w := range topicWords {
				lw := strings.ToLower(w)
				if lw == "" {
					continue
				}
				if lw == lowerEnt || strings.Contains(lw, lowerEnt) || strings.Contains(lowerEnt, lw) {
					score += 8
					trace.Add("topic", 8, fmt.Sprintf("source entity '%s' ~ topic '%s'", ent, w))
					break
				}
			}
		}

		// Capability match (+5 per matching capability)
		needed := capabilitiesForAction(classified.Intent.Action, classified.Intent.Keywords)
		for _, cap := range needed {
			if src.HasCapability(cap) {
				score += 5
				trace.Add("capability", 5, fmt.Sprintf("source has '%s'", cap))
			}
		}

		// Keyword match against description (+2)
		for _, kw := range classified.Intent.Keywords {
			if strings.Contains(strings.ToLower(src.Description), strings.ToLower(kw)) {
				score += 2
				trace.Add("keyword-desc", 2, fmt.Sprintf("description contains '%s'", kw))
			}
		}

		// Source type bonus for certain strategies
		switch classified.Strategy {
		case StrategySearch:
			if src.Type == "codebase" {
				score += 3
				trace.Add("type-bonus", 3, "codebase + search strategy")
			}
		case StrategyInvestigate:
			if src.Type == "data-source" || src.Type == "tool" {
				score += 3
				trace.Add("type-bonus", 3, fmt.Sprintf("%s + investigate strategy", src.Type))
			}
		case StrategyQuery:
			if src.Type == "web-search" {
				score += 3
				trace.Add("type-bonus", 3, "web-search + query strategy")
			}
		}

		scored = append(scored, ScoredSource{Name: name, Source: src, Score: score, Trace: trace})
	}

	applyNewsTierAdjustments(scored, classified)

	sort.Slice(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	return scored
}

// newsCapabilities are the capabilities that mark a source as a news-domain
// answerer. A source advertising any of these competes with web-search for
// "news" / "current events" questions.
var newsCapabilities = map[string]bool{
	"news":           true,
	"headlines":      true,
	"current-events": true,
}

// newsKeywords are query tokens that mark a question as news-domain. When any
// of these appear in the parsed keywords or raw entities, news-capable sources
// get a +20 boost (the "news tier") so they outrank generic web-search on
// explicit news queries.
var newsKeywords = map[string]bool{
	"news":      true,
	"headline":  true,
	"headlines": true,
	"breaking":  true,
}

// applyNewsTierAdjustments boosts news-capable sources on news queries and
// demotes web-search when a more specific named source is in the candidate
// set. Mutates `scored` in place. Issue #43.
func applyNewsTierAdjustments(scored []ScoredSource, classified *ClassifiedIntent) {
	if classified == nil || classified.Intent == nil {
		return
	}

	hasNewsToken := false
	for _, kw := range classified.Intent.Keywords {
		if newsKeywords[strings.ToLower(kw)] {
			hasNewsToken = true
			break
		}
	}
	if !hasNewsToken {
		for _, ent := range classified.Intent.RawEntities {
			if newsKeywords[strings.ToLower(ent)] {
				hasNewsToken = true
				break
			}
		}
	}

	hasNamedNonWebSource := false
	for i := range scored {
		if scored[i].Name == "web-search" {
			continue
		}
		if scored[i].Trace != nil && scored[i].Trace.PointsFor("name-match") > 0 {
			hasNamedNonWebSource = true
			break
		}
	}

	for i := range scored {
		s := &scored[i]
		if hasNewsToken && s.Name != "web-search" && s.Source != nil {
			for _, cap := range s.Source.Capabilities {
				if newsCapabilities[strings.ToLower(cap)] {
					s.Score += 20
					s.Trace.Add("news-tier", 20, fmt.Sprintf("news query + source has '%s'", cap))
					break
				}
			}
		}
		if hasNamedNonWebSource && s.Name == "web-search" {
			s.Score -= 15
			s.Trace.Add("web-demote", -15, "specific named source is in candidates")
		}
	}
}

// timeWordPattern matches whole-word time/date vocabulary in a lowercased
// keyword, guarding against substring false positives like "timeline" or
// "runtime" (which contain "time" but aren't time-lookup questions).
var timeWordPattern = regexp.MustCompile(`\b(time|date|clock|timezone|today)\b`)

// capabilitiesForAction maps action types to relevant capabilities.
func capabilitiesForAction(action string, keywords []string) []string {
	caps := make([]string, 0)

	switch action {
	case "investigate":
		caps = append(caps, "log-query", "error-investigation", "ari-lookup", "pr-lookup")
	case "query":
		// Do NOT include "sql-query" as a default here -- it's too
		// generic and causes any sqlite-backed source (e.g. finances)
		// to match every question. sql-query is added below by
		// keyword triggers when the question mentions counts, amounts,
		// totals, or other concrete data.
		caps = append(caps, "partner-lookup")
	case "lookup":
		caps = append(caps, "ari-lookup", "partner-lookup")
	case "record":
		caps = append(caps, "expense-tracking", "record")
	case "test":
		caps = append(caps, "api-testing", "credential-testing", "endpoint-replay")
	case "search":
		caps = append(caps, "code-reference", "partner-docs")
	}

	// Keyword-based capability matching
	for _, kw := range keywords {
		lower := strings.ToLower(kw)
		switch {
		case strings.Contains(lower, "gmv") || strings.Contains(lower, "revenue"):
			caps = append(caps, "gmv-analysis", "gmv-analytics")
		case strings.Contains(lower, "error") || strings.Contains(lower, "500") || strings.Contains(lower, "5xx"):
			caps = append(caps, "log-query", "error-investigation", "api-monitoring", "sql-query")
		case strings.Contains(lower, "200") || strings.Contains(lower, "2xx") || strings.Contains(lower, "20x"):
			caps = append(caps, "log-query", "api-monitoring", "sql-query")
		case strings.Contains(lower, "400") || strings.Contains(lower, "4xx") || strings.Contains(lower, "40x"):
			caps = append(caps, "log-query", "api-monitoring", "sql-query")
		case strings.Contains(lower, "latency") || strings.Contains(lower, "timeout") || strings.Contains(lower, "p99") || strings.Contains(lower, "p95"):
			caps = append(caps, "log-query", "api-monitoring", "sql-query")
		case strings.Contains(lower, "request") || strings.Contains(lower, "response") || strings.Contains(lower, "api"):
			caps = append(caps, "log-query", "api-monitoring")
		case strings.Contains(lower, "checkout"):
			caps = append(caps, "sql-query", "checkout-analytics")
		case strings.Contains(lower, "settlement"):
			caps = append(caps, "settlement-lookup")
		case strings.Contains(lower, "code") || strings.Contains(lower, "source"):
			caps = append(caps, "code-reference")
		case strings.Contains(lower, "doc") || strings.Contains(lower, "docs"):
			caps = append(caps, "partner-docs")
		case strings.Contains(lower, "log"):
			caps = append(caps, "log-query")
		case strings.Contains(lower, "workout") || strings.Contains(lower, "exercise") || strings.Contains(lower, "run"):
			caps = append(caps, "workout-lookup", "fitness-tracking")
		case strings.Contains(lower, "weight") || strings.Contains(lower, "sleep") || strings.Contains(lower, "vo2") || strings.Contains(lower, "heart"):
			caps = append(caps, "health-metrics")
		case strings.Contains(lower, "expense") || strings.Contains(lower, "spending") || strings.Contains(lower, "budget") || strings.Contains(lower, "spend") ||
			strings.Contains(lower, "transaction") || strings.Contains(lower, "payment") || strings.Contains(lower, "vendor") ||
			strings.Contains(lower, "category") || strings.Contains(lower, "subcategor") || strings.Contains(lower, "bucket"):
			caps = append(caps, "expense-tracking", "budget-lookup", "csv-query", "sql-query")
		case strings.Contains(lower, "note") || strings.Contains(lower, "journal"):
			caps = append(caps, "note-search", "doc-search")
		case strings.Contains(lower, "commit") || strings.Contains(lower, "author") || strings.Contains(lower, "branch") || strings.Contains(lower, "pr") || strings.Contains(lower, "pull request") || strings.Contains(lower, "git "):
			caps = append(caps, "git-history", "commit-lookup", "author-lookup", "branch-lookup", "pr-lookup")
		case timeWordPattern.MatchString(lower):
			caps = append(caps, "current-time")
			if strings.Contains(lower, "today") {
				// "today" also reads as a current-events question
				// ("what's happening today") -- keep the existing
				// web-search mapping in addition to current-time.
				caps = append(caps, "web-search", "current-events")
			}
		case strings.Contains(lower, "weather") || strings.Contains(lower, "news") || strings.Contains(lower, "current") || strings.Contains(lower, "latest") || strings.Contains(lower, "recent"):
			caps = append(caps, "web-search", "current-events")
		case strings.Contains(lower, "what is") || strings.Contains(lower, "who is") || strings.Contains(lower, "how to") || strings.Contains(lower, "define") || strings.Contains(lower, "explain"):
			caps = append(caps, "web-search", "documentation-lookup")
		case strings.Contains(lower, "search") || strings.Contains(lower, "look up") || strings.Contains(lower, "find"):
			caps = append(caps, "web-search")
		}
	}

	return caps
}

// filterByCapabilities returns sources that have at least one of the given capabilities.
func filterByCapabilities(sources []ScoredSource, caps []string) []ScoredSource {
	var result []ScoredSource
	for _, s := range sources {
		for _, cap := range caps {
			if s.Source.HasCapability(cap) {
				result = append(result, s)
				break
			}
		}
	}
	return result
}

// top returns the first n sources from the list.
func top(sources []ScoredSource, n int) []ScoredSource {
	if len(sources) <= n {
		return sources
	}
	return sources[:n]
}

// restrictAndBoost narrows the candidate list to sources that appear in
// routedSources, gives them a strong priority boost, and adds any routed
// sources that the global ranker missed (so route-only sources still run
// even if they had no capability/keyword/entity hits).
func restrictAndBoost(candidates []ScoredSource, allSources config.Sources, routedSources []string) []ScoredSource {
	routedSet := make(map[string]bool, len(routedSources))
	for _, n := range routedSources {
		routedSet[n] = true
	}

	// Step 1: keep candidates that are in the routed set, with a priority
	// boost so routed sources outrank any non-routed leftovers.
	var kept []ScoredSource
	for _, s := range candidates {
		if routedSet[s.Name] {
			s.Score += 100 // strong route boost
			s.Trace.Add("route-boost", 100, "cwd/entity route matched")
			kept = append(kept, s)
		}
	}

	// Step 2: ensure every routed source is represented, even if the
	// global ranker scored it zero (capability matching may not cover it).
	present := make(map[string]bool, len(kept))
	for _, s := range kept {
		present[s.Name] = true
	}
	for _, name := range routedSources {
		if present[name] {
			continue
		}
		if src, ok := allSources[name]; ok {
			baseline := &ScoreTrace{}
			baseline.Add("route-boost", 100, "baseline route score (global ranker missed)")
			kept = append(kept, ScoredSource{
				Name:   name,
				Source: src,
				Score:  100, // baseline route score
				Trace:  baseline,
			})
		}
	}

	// Re-sort by descending score to keep the contract that callers expect.
	sortByScoreDesc(kept)
	return kept
}

func sortByScoreDesc(s []ScoredSource) {
	sort.Slice(s, func(i, j int) bool { return s[i].Score > s[j].Score })
}

// FormatExplain returns a human-readable scoring table for the plan's full
// candidate list. Columns: score | source | name | topic | cap | kw
// | type | route. Zero-score sources are shown in a separate "considered but
// cut" tail so the user can see why a source wasn't picked. Sources in
// plan.Phases are marked with an asterisk.
func FormatExplain(plan *ExecutionPlan) string {
	if plan == nil || len(plan.AllCandidates) == 0 {
		return "(no candidates scored)"
	}

	picked := make(map[string]string)
	for _, p := range plan.Phases {
		for _, s := range p.Sources {
			picked[s.Name] = p.Name
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Planner scoring (%d candidates, %d picked)\n",
		len(plan.AllCandidates), len(picked))
	fmt.Fprintln(&b, "  score  source                          name  topic  cap   kw   type  route  picked")
	fmt.Fprintln(&b, "  -----  ------------------------------  ----  -----  ---  ----  ----  -----  ------")

	writeRow := func(s ScoredSource) {
		phaseTag := ""
		if phase, ok := picked[s.Name]; ok {
			phaseTag = "→ " + phase
		}
		fmt.Fprintf(&b, "  %-5d  %-30s  %4d  %5d  %3d  %4d  %4d  %5d  %s\n",
			s.Score,
			truncateName(s.Name, 30),
			s.Trace.PointsFor("name-match"),
			s.Trace.PointsFor("topic"),
			s.Trace.PointsFor("capability"),
			s.Trace.PointsFor("keyword-desc"),
			s.Trace.PointsFor("type-bonus"),
			s.Trace.PointsFor("route-boost"),
			phaseTag,
		)
	}

	// Non-zero first, then zeros. The AllCandidates slice is already
	// sorted by score desc, so we can just walk it.
	firstZero := -1
	for i, s := range plan.AllCandidates {
		if s.Score == 0 && firstZero == -1 {
			firstZero = i
			fmt.Fprintln(&b, "  ---  zero-score (considered, cut)  ---")
		}
		writeRow(s)
	}

	// Collapse reasoning notes for picked sources (they're what actually
	// matter for debugging routing).
	if len(picked) > 0 {
		fmt.Fprintln(&b, "\nNotes for picked sources:")
		for _, s := range plan.AllCandidates {
			if _, ok := picked[s.Name]; !ok {
				continue
			}
			fmt.Fprintf(&b, "  %s (score=%d):\n", s.Name, s.Score)
			if s.Trace == nil || len(s.Trace.Components) == 0 {
				fmt.Fprintln(&b, "    (no trace)")
				continue
			}
			for _, c := range s.Trace.Components {
				fmt.Fprintf(&b, "    +%d  %s  %s\n", c.Points, c.Bucket, c.Note)
			}
		}
	}

	return b.String()
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
