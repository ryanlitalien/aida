package engine

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func testSources() config.Sources {
	return config.Sources{
		"sqlite": &config.Source{
			Type:         "data-source",
			Description:  "Company SQL warehouse - partner, resource, checkout, booking, GMV queries",
			Capabilities: []string{"sql-query", "partner-lookup", "ari-lookup", "gmv-analysis", "transaction-history", "settlement-lookup"},
			Entities:     []string{"thrive", "viewer-core", "camp-butz", "mcp-perforce", "google"},
		},
		"plausible": &config.Source{
			Type:         "tool",
			Description:  "Plausible logs - nginx requests, API errors, resource debugging",
			Capabilities: []string{"log-query", "error-investigation", "api-monitoring"},
		},
		"all-the-things": &config.Source{
			Type:         "codebase",
			Description:  "Butterstack monorepo - all application source code",
			Capabilities: []string{"code-reference", "error-source-lookup", "config-lookup"},
		},
		"viewer-core-docs": &config.Source{
			Type:         "docs",
			Description:  "Viewer-core partner - openscreen investigation",
			Capabilities: []string{"partner-docs"},
			Entities:     []string{"viewer-core", "openscreen"},
		},
	}
}

// --- Fix 4 tests: name-match scoring block ---

func TestRankSources_ExactNameMatchBeatsOthers(t *testing.T) {
	sources := config.Sources{
		"csv-viewer": &config.Source{
			Type:        "codebase",
			Description: "csv viewer codebase",
			// no caps, no entities - purely name match
		},
		"sqlite": &config.Source{
			Type:         "data-source",
			Description:  "Strong SQL warehouse source with many caps",
			Capabilities: []string{"sql-query", "ari-lookup", "partner-lookup", "gmv-analysis"},
			Entities:     []string{"thrive", "viewer-core"},
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "search",
			Keywords:    []string{"viewer"},
			RawEntities: []string{"csv-viewer"},
		},
		Strategy: StrategySearch,
	}
	resolved := &ResolvedContext{}
	ranked := rankSources(classified, resolved, sources)
	if len(ranked) == 0 {
		t.Fatal("expected ranked sources")
	}
	if ranked[0].Name != "csv-viewer" {
		t.Errorf("expected csv-viewer first, got %q (full: %+v)", ranked[0].Name, ranked)
	}
	if ranked[0].Score < 30 {
		t.Errorf("expected score >= 30 for exact name match, got %d", ranked[0].Score)
	}
}

func TestRankSources_SubstringNameMatch(t *testing.T) {
	sources := config.Sources{
		"mcp-perforce": &config.Source{
			Type:        "tool",
			Description: "P4 commands via MCP",
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"perforce"},
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	if len(ranked) == 0 {
		t.Fatal("expected ranked sources")
	}
	if ranked[0].Name != "mcp-perforce" {
		t.Errorf("expected mcp-perforce, got %q", ranked[0].Name)
	}
	if ranked[0].Score < 15 {
		t.Errorf("expected score >= 15 for substring match, got %d", ranked[0].Score)
	}
}

func TestRankSources_NameMatchCaseInsensitive(t *testing.T) {
	sources := config.Sources{
		"thrive": &config.Source{Type: "codebase", Description: "Thrive"},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"Thrive"},
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	if len(ranked) == 0 || ranked[0].Name != "thrive" {
		t.Errorf("expected thrive, got %v", ranked)
	}
}

func TestRankSources_NameMatchUnderscoreNormalize(t *testing.T) {
	sources := config.Sources{
		"mcp-perforce": &config.Source{Type: "tool", Description: "P4 via MCP"},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"mcp_perforce"},
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	if len(ranked) == 0 || ranked[0].Name != "mcp-perforce" {
		t.Errorf("expected mcp-perforce for 'mcp_perforce', got %v", ranked)
	}
}

func TestRankSources_MultiWordEntityTokenMatch(t *testing.T) {
	sources := config.Sources{
		"thrive": &config.Source{
			Type:        "tool",
			Description: "Thrive partner workspace",
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"thrive game"},
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	if len(ranked) == 0 || ranked[0].Name != "thrive" {
		t.Errorf("expected thrive for 'thrive game' token match, got %v", ranked)
	}
}

func TestRankSources_ShortTokenNoFalsePositive(t *testing.T) {
	sources := config.Sources{
		"late-cli": &config.Source{
			Type:        "tool",
			Description: "Social posting CLI",
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"cli"}, // 3 chars, below the length-4 gate
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	for _, r := range ranked {
		if r.Name == "late-cli" && r.Score >= 15 {
			t.Errorf("late-cli should not score from short 'cli' entity, got %d", r.Score)
		}
	}
}

func TestRankSources_GenericTokenNoFalsePositive(t *testing.T) {
	sources := config.Sources{
		"data-tool": &config.Source{Type: "tool", Description: "A data tool"},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:      "lookup",
			RawEntities: []string{"tool"},
		},
		Strategy: StrategyLookup,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	for _, r := range ranked {
		if r.Name == "data-tool" && r.Score >= 15 {
			t.Errorf("data-tool should not score from generic 'tool' entity, got %d", r.Score)
		}
	}
}

func TestRankSources_TopicKeywordMatchesSourceEntities(t *testing.T) {
	// A personal-topic source (no partner) should still be picked when
	// its entities overlap intent.Keywords. This is the path aida uses
	// for workouts, finances, notes, etc.
	sources := config.Sources{
		"workouts": &config.Source{
			Type:         "csv",
			Description:  "Personal workout CSV data",
			Capabilities: []string{"csv-query", "fitness-tracking"},
			Entities:     []string{"workouts", "fitness"},
		},
		"sqlite": &config.Source{
			Type:         "data-source",
			Description:  "Company SQL warehouse",
			Capabilities: []string{"sql-query"},
			Entities:     []string{"thrive"},
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "query",
			Keywords: []string{"workout", "latest"},
		},
		Strategy: StrategyQuery,
	}
	resolved := &ResolvedContext{}
	ranked := rankSources(classified, resolved, sources)
	if len(ranked) == 0 {
		t.Fatal("expected ranked sources, got none")
	}
	if ranked[0].Name != "workouts" {
		t.Errorf("expected workouts first, got %q (full: %+v)", ranked[0].Name, ranked)
	}
}

func TestRankSources_EntityMatch(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "query",
			Keywords: []string{"GMV"},
		},
		Strategy: StrategyQuery,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "viewer-core", Type: EntityPartnerName, Resolved: map[string]string{"partner_name": "viewer-core"}},
		},
	}

	ranked := rankSources(classified, resolved, sources)

	if len(ranked) == 0 {
		t.Fatal("expected ranked sources, got none")
	}

	// Sqlite should rank highest (entity match + capability match)
	if ranked[0].Name != "sqlite" {
		t.Errorf("expected sqlite first, got %s", ranked[0].Name)
	}

	// Viewer-core docs should also appear (entity match)
	found := false
	for _, s := range ranked {
		if s.Name == "viewer-core-docs" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected viewer-core-docs in ranked sources")
	}
}

func TestRankSources_CapabilityMatch(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "investigate",
			Keywords: []string{"error", "500"},
		},
		Strategy: StrategyInvestigate,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{},
	}

	ranked := rankSources(classified, resolved, sources)

	if len(ranked) == 0 {
		t.Fatal("expected ranked sources, got none")
	}

	// Plausible should rank high for error investigation
	found := false
	for _, s := range ranked {
		if s.Name == "plausible" {
			found = true
			if s.Score == 0 {
				t.Error("plausible should have non-zero score for error investigation")
			}
			break
		}
	}
	if !found {
		t.Error("expected plausible in ranked sources")
	}
}

func TestPlan_LookupStrategy(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent:   &Intent{Action: "lookup", Keywords: []string{}},
		Strategy: StrategyLookup,
		Entities: []TypedEntity{{Raw: "PE29GD8AFPK054NZ", Type: EntityGenericARI}},
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "PE29GD8AFPK054NZ", Type: EntityGenericARI, Resolved: map[string]string{"partner_name": "viewer-core"}},
		},
	}

	plan := Plan(classified, resolved, sources, nil, nil)

	if len(plan.Phases) != 1 {
		t.Fatalf("expected 1 phase for lookup, got %d", len(plan.Phases))
	}
	if plan.Phases[0].Name != "lookup" {
		t.Errorf("expected phase name 'lookup', got %q", plan.Phases[0].Name)
	}
	if len(plan.Phases[0].Sources) != 1 {
		t.Errorf("expected 1 source for lookup, got %d", len(plan.Phases[0].Sources))
	}
}

func TestPlan_QueryStrategy(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent:   &Intent{Action: "query", Keywords: []string{"GMV"}},
		Strategy: StrategyQuery,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "viewer-core", Type: EntityPartnerName, Resolved: map[string]string{"partner_name": "viewer-core"}},
		},
	}

	plan := Plan(classified, resolved, sources, nil, nil)

	if len(plan.Phases) != 1 {
		t.Fatalf("expected 1 phase for query, got %d", len(plan.Phases))
	}
	if !plan.Phases[0].Parallel {
		t.Error("expected parallel execution for query strategy")
	}
}

func TestPlan_InvestigateStrategy(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent:   &Intent{Action: "investigate", Keywords: []string{"error"}},
		Strategy: StrategyInvestigate,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{},
	}

	plan := Plan(classified, resolved, sources, nil, nil)

	if len(plan.Phases) < 1 {
		t.Fatal("expected at least 1 phase for investigate")
	}
	if plan.Phases[0].Name != "diagnose" {
		t.Errorf("expected first phase 'diagnose', got %q", plan.Phases[0].Name)
	}
}

func TestPlan_ACHDepositQuery(t *testing.T) {
	sources := testSources()

	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "query",
			Keywords: []string{"ACH deposit", "thrive game", "latest", "ID"},
		},
		Strategy: StrategyQuery,
	}
	resolved := &ResolvedContext{
		Entities: []ResolvedEntity{
			{Raw: "thrive game", Type: EntityPartnerName, Resolved: map[string]string{"partner_name": "thrive"}},
		},
	}

	plan := Plan(classified, resolved, sources, nil, nil)

	if len(plan.Phases) != 1 {
		t.Fatalf("expected 1 phase, got %d", len(plan.Phases))
	}
	if !plan.Phases[0].Parallel {
		t.Error("expected parallel execution for query strategy")
	}
	// Sqlite should be in the plan (entity match + sql-query capability)
	found := false
	for _, s := range plan.Phases[0].Sources {
		if s.Name == "sqlite" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected sqlite in plan for ACH deposit query")
	}
}

func TestCapabilitiesForAction(t *testing.T) {
	tests := []struct {
		action   string
		keywords []string
		wantCap  string
	}{
		{"investigate", nil, "log-query"},
		// "query" no longer returns sql-query unconditionally -- it's
		// added by specific keyword triggers (gmv, count, total, etc.)
		// to keep generic sqlite sources from matching every question.
		{"query", nil, "partner-lookup"},
		{"lookup", nil, "ari-lookup"},
		{"test", nil, "api-testing"},
		{"search", nil, "code-reference"},
		{"query", []string{"gmv"}, "gmv-analysis"},
		{"investigate", []string{"error"}, "error-investigation"},
	}

	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			caps := capabilitiesForAction(tt.action, tt.keywords)
			found := false
			for _, c := range caps {
				if c == tt.wantCap {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("capabilitiesForAction(%q, %v) missing %q, got %v", tt.action, tt.keywords, tt.wantCap, caps)
			}
		})
	}
}

func TestCapabilitiesForAction_CurrentTime(t *testing.T) {
	tests := []struct {
		name     string
		keywords []string
		wantCap  bool
	}{
		{"time keyword matches", []string{"time"}, true},
		{"date keyword matches", []string{"date"}, true},
		{"clock keyword matches", []string{"clock"}, true},
		{"timezone keyword matches", []string{"timezone"}, true},
		{"today also matches current-time", []string{"today"}, true},
		{"timeline must not false-positive", []string{"timeline"}, false},
		{"runtime must not false-positive", []string{"runtime"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := capabilitiesForAction("query", tt.keywords)
			found := false
			for _, c := range caps {
				if c == "current-time" {
					found = true
					break
				}
			}
			if found != tt.wantCap {
				t.Errorf("capabilitiesForAction(query, %v) current-time = %v, want %v (caps: %v)", tt.keywords, found, tt.wantCap, caps)
			}
		})
	}
}

func TestCapabilitiesForAction_TodayKeepsWebSearch(t *testing.T) {
	// "today" is date-shaped AND current-events-shaped -- it must keep
	// the pre-existing web-search mapping alongside the new current-time one.
	caps := capabilitiesForAction("query", []string{"today"})
	for _, want := range []string{"current-time", "web-search", "current-events"} {
		found := false
		for _, c := range caps {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("capabilitiesForAction(query, [today]) missing %q, got %v", want, caps)
		}
	}
}

func TestRankSources_CurrentTimeOutranksWebSearch(t *testing.T) {
	sources := config.Sources{
		"current-time": &config.Source{
			Type:         "current-time",
			Description:  "Local system clock - current time, date, day of week, timezone",
			Capabilities: []string{"current-time", "date-lookup"},
		},
		"web-search": &config.Source{
			Type:         "web-search",
			Description:  "General web search",
			Capabilities: []string{"web-search"},
		},
	}
	classified := &ClassifiedIntent{
		Intent: &Intent{
			Action:   "query",
			Keywords: []string{"time"},
		},
		Strategy: StrategyQuery,
	}
	ranked := rankSources(classified, &ResolvedContext{}, sources)
	if len(ranked) < 2 {
		t.Fatalf("expected 2 ranked sources, got %d", len(ranked))
	}
	if ranked[0].Name != "current-time" {
		t.Errorf("expected current-time to outrank web-search for a time question, got %q first (full: %+v)", ranked[0].Name, ranked)
	}
}
