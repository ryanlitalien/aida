package engine

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// Golden routing tests derived from two real failed aida runs, re-cut
// 2026-09-13 (docs/plan-remove-partners.md step 8) after the partner
// registry was removed. They used to encode "post-Phase-2 partner-aware
// routing" expectations built on the registry's +50 boost and
// FindByAlias/checkout-token resolution; that mechanism is gone with no
// fallback. Routing is now driven purely by entity tokens: a source's own
// name against the question's raw entities (name match), and a source's
// `entities:` list against the question's keywords/raw entities (topic
// match) - plus `routes.yaml` `match_entity` (+100) when a route applies,
// which neither fixture exercises. The expectations below capture what that
// deterministic scoring actually produces today so a future scoring change
// shows up here instead of drifting silently; they are not a claim that
// this is the ideal ranking.
//
// Run A - "what was the issue camp-alder had with live testing back in early march"
//   Actual answer lived in a Slack thread + a monorepo PR. Without the
//   partner boost, camp-alder still wins outright on an exact name match, but
//   the generic "issue" keyword now pulls github (pr-lookup capability +
//   a description hit) ahead of cakestack's plain topic match.
//
// Run B - a Slack paste with checkout token 68KP-902F + sandbox UAT question.
//   "FC" topic-matches star-pass's `entities:` list; without the
//   registry's checkout-token classification or alias fallback (both
//   registry-only, never built as Phase 2 items), github and cakestack's
//   capability/keyword hits now outrank slack for the top-3.

// postFixSources returns the source catalog with the Phase 1 (adapter
// correctness) and Phase 5 (Slack) fixes applied, plus the `entities:`
// tokens each source needs for entity-token routing to find it at all.
func postFixSources() config.Sources {
	return config.Sources{
		"camp-alder": &config.Source{
			Type:         "docs",
			Path:         "/Users/fakehome/dev/camp-alder",
			Description:  "Documentation and investigation resources for the Camp Alder partner integration.",
			Capabilities: []string{"partner-docs", "sql-query", "ari-lookup", "partner-lookup", "error-investigation", "code-reference"},
			Entities:     []string{"camp-alder", "alder", "campalder"},
			// post-Phase-1.1: search: grep
		},
		"star-pass": &config.Source{
			Type:         "docs",
			Path:         "/Users/fakehome/dev/star-pass",
			Description:  "StarPass ski-pass comparison site's partnership integration for cross-border payments.",
			Capabilities: []string{"partner-docs", "ari-lookup", "partner-lookup", "error-investigation"},
			Entities:     []string{"star-pass", "starpass", "fc"},
		},
		"novacore": &config.Source{
			Type:         "codebase",
			Path:         "/Users/fakehome/dev/novacore",
			Description:  "Novacore game prototype partner integration project",
			Capabilities: []string{"partner-docs", "ari-lookup", "error-investigation"},
			Entities:     []string{"novacore", "novacore-game"},
		},
		"cakestack": &config.Source{
			Type:        "codebase",
			Path:        "/Users/fakehome/dev/cake_stack",
			Description: "CakeStack's monorepo containing deployable services, packages, and shared libraries.",
			// post-Phase-2.5: entities populated with cakestack + monorepo + all partner aliases
			Capabilities: []string{"code-reference", "git-history", "partner-lookup", "error-investigation"},
			Entities:     []string{"cakestack", "monorepo", "service", "pr", "star-pass", "camp-alder", "novacore", "viewer-core", "late-cli"},
		},
		"slack": &config.Source{
			Type:         "tool",
			Description:  "Slack workspace - threads, channels, messages in #ask-engineering and other channels the user participates in.",
			Capabilities: []string{"thread-lookup", "channel-search", "message-search", "error-investigation"},
			Entities:     []string{"slack", "message", "thread", "star-pass", "camp-alder", "novacore"},
		},
		"github": &config.Source{
			Type:         "tool",
			Description:  "GitHub CLI (gh) for querying pull requests, issues, repos, and code reviews across exampleuser, exampleuser-apps, exampleuser-work orgs.",
			Capabilities: []string{"pr-lookup", "pr-review", "pr-diff", "issue-lookup", "github-api"},
			Entities:     []string{"github", "pr", "pull-request", "issue", "issues", "review", "gh"},
		},
		"credbot": &config.Source{
			Type:         "codebase",
			Path:         "/Users/fakehome/dev/credbot",
			Description:  "A Node.js tool for testing API credentials across multiple environments.",
			Capabilities: []string{"api-testing", "credential-testing", "documentation"},
			Entities:     []string{"credbot", "credentials"},
		},
	}
}

// topN returns the first n source names from a sorted ScoredSource list.
func topN(ranked []ScoredSource, n int) []string {
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	names := make([]string, 0, len(ranked))
	for _, s := range ranked {
		names = append(names, s.Name)
	}
	return names
}

// containsAll returns true when every name in want appears somewhere in got.
func containsAll(got []string, want ...string) bool {
	set := make(map[string]bool, len(got))
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// TestGoldenRouting_CampAlderMarchIssue captures Run A.
//
// Top-3 without the partner registry: camp-alder, github, slack. camp-alder
// wins outright on an exact name match (+30). github and slack tie on topic
// (+8, "issue") and capability (+5, error-investigation/pr-lookup) plus the
// investigate-strategy tool type-bonus (+3); github edges ahead on a
// keyword-description hit ("issues" contains "issue", +2). cakestack only
// gets the topic match (+8) plus a capability hit (+5, error-investigation)
// - no type-bonus, since it's a codebase not a tool/data-source - so it
// ranks below both without a registry boost to lift it.
func TestGoldenRouting_CampAlderMarchIssue(t *testing.T) {
	sources := postFixSources()

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "investigate",
			RawEntities: []string{"camp-alder"},
			Keywords:    []string{"issue", "camp-alder", "live", "testing", "march"},
		},
		Strategy: StrategyInvestigate,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "camp-alder"},
		},
	}

	ranked := rankSources(classified, resolved, sources)
	top := topN(nonZero(ranked), 3)

	if !containsAll(top, "camp-alder", "github", "slack") {
		t.Errorf("Run A top-3 should include camp-alder, github, slack; got %v", top)
	}
}

// TestGoldenRouting_StarPassCheckoutToken captures Run B.
//
// The query mentions "68KP-902F" and "FC"/"sandbox". The registry's
// checkout-token classification and alias fallback (issue #58's Phase
// 2.1/2.2, never built) are gone with no replacement, so "FC" only earns
// star-pass a topic match (+8) via its `entities:` list. Without a
// registry boost, github's and cakestack's capability/keyword hits
// outrank slack for the top-3; credbot must still not appear.
func TestGoldenRouting_StarPassCheckoutToken(t *testing.T) {
	sources := postFixSources()

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "investigate",
			RawEntities: []string{"68KP-902F", "FC"},
			Keywords:    []string{"sandbox", "uat", "validated", "production", "credentials"},
		},
		Strategy: StrategyInvestigate,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "FC"},
			{Raw: "68KP-902F"},
		},
	}

	ranked := rankSources(classified, resolved, sources)
	top := topN(nonZero(ranked), 3)

	if !containsAll(top, "github", "star-pass", "cakestack") {
		t.Errorf("Run B top-3 should include github, star-pass, cakestack; got %v", top)
	}
	for _, name := range top {
		if name == "credbot" {
			t.Errorf("credbot should not appear in top-3; got %v", top)
		}
	}
}

// TestGoldenRouting_NoRegressionExactNameMatch sanity-checks that entity-token
// scoring doesn't invert ordering when a source is named directly. "sqlite"
// must win when the user mentions sqlite even if other entity-tagged sources
// also score well.
func TestGoldenRouting_NoRegressionExactNameMatch(t *testing.T) {
	sources := postFixSources()
	sources["sqlite"] = &config.Source{
		Type:         "data-source",
		Description:  "A SQL warehouse - partner, merchant, checkout queries.",
		Capabilities: []string{"sql-query", "partner-lookup", "ari-lookup"},
		Entities:     []string{"sqlite", "warehouse", "novacore", "camp-alder", "star-pass"},
	}

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "query",
			RawEntities: []string{"sqlite"},
			Keywords:    []string{"sqlite", "query"},
		},
		Strategy: StrategyQuery,
	}
	resolved := &ResolvedContext{}

	ranked := rankSources(classified, resolved, sources)
	top := topN(nonZero(ranked), 1)
	if len(top) == 0 || top[0] != "sqlite" {
		t.Errorf("exact name match should win; got %v", top)
	}
}
