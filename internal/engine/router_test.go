package engine

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// helper: build an allSources map for the router tests.
func routerTestSources() config.Sources {
	return config.Sources{
		"aida": &config.Source{
			Type:        "codebase",
			Description: "Aida itself - agent of agents CLI",
		},
		"3d-viewer": &config.Source{
			Type:        "codebase",
			Description: "3d-viewer codebase",
		},
		"audio-viewer": &config.Source{
			Type:        "codebase",
			Description: "audio-viewer codebase",
		},
		"acme-widgets": &config.Source{
			Type:        "claude-project",
			Description: "Acme Widgets - Rails app",
		},
		"csv-viewer": &config.Source{
			Type:        "codebase",
			Description: "csv-viewer codebase",
		},
		"mcp-perforce": &config.Source{
			Type:        "tool",
			Description: "P4 commands via MCP",
		},
		"thrive": &config.Source{
			Type:        "codebase",
			Description: "Thrive game source",
		},
		"late-cli": &config.Source{
			Type:        "tool",
			Description: "Social posting CLI",
		},
	}
}

func TestAddNameMatchedDescriptors_ExactMatch(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{"aida": true}
	descs := []sourceDesc{{Name: "aida"}}

	got := addNameMatchedDescriptors(descs, seen, all, []string{"csv-viewer"}, maxSources)

	if !containsName(got, "csv-viewer") {
		t.Errorf("expected csv-viewer in descs, got %v", names(got))
	}
	// aida should still be there (descs starts with it).
	if !containsName(got, "aida") {
		t.Errorf("expected aida preserved, got %v", names(got))
	}
	// csv-viewer should have the prior score we set.
	for _, d := range got {
		if d.Name == "csv-viewer" && d.Score != nameMatchPriorScore {
			t.Errorf("csv-viewer score = %d, want %d", d.Score, nameMatchPriorScore)
		}
	}
}

func TestAddNameMatchedDescriptors_TokenMatch(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{}
	// "perforce" alone should match mcp-perforce via substring.
	got := addNameMatchedDescriptors(nil, seen, all, []string{"perforce"}, maxSources)
	if !containsName(got, "mcp-perforce") {
		t.Errorf("expected mcp-perforce in descs (perforce substring), got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_MultiWordEntity(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{}
	// "thrive game" should split into ["thrive","game"] and the "thrive" token
	// should match the "thrive" source name.
	got := addNameMatchedDescriptors(nil, seen, all, []string{"thrive game"}, maxSources)
	if !containsName(got, "thrive") {
		t.Errorf("expected thrive in descs (token match for 'thrive game'), got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_NoRawEntities(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{}
	got := addNameMatchedDescriptors(nil, seen, all, nil, maxSources)
	if len(got) != 0 {
		t.Errorf("expected empty descs for nil rawEntities, got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_ShortTokenRejected(t *testing.T) {
	all := routerTestSources()
	all["api-gateway"] = &config.Source{Type: "tool", Description: "An API gateway"}
	seen := map[string]bool{}
	// "cli" is 3 chars, below the length-4 gate. Should not match late-cli.
	got := addNameMatchedDescriptors(nil, seen, all, []string{"cli"}, maxSources)
	if containsName(got, "late-cli") {
		t.Errorf("did not expect late-cli for short entity 'cli', got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_GenericTokenRejected(t *testing.T) {
	all := routerTestSources()
	all["data-tool"] = &config.Source{Type: "tool", Description: "A data tool"}
	seen := map[string]bool{}
	// "tool" is on the generic blocklist; should not match data-tool by token.
	got := addNameMatchedDescriptors(nil, seen, all, []string{"tool"}, maxSources)
	if containsName(got, "data-tool") {
		t.Errorf("did not expect data-tool for generic entity 'tool', got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_CapAtMaxSources(t *testing.T) {
	all := routerTestSources()
	// Build a list of raw entities that all match different sources.
	rawEnts := []string{"csv-viewer", "mcp-perforce", "thrive", "late-cli", "audio-viewer"}
	seen := map[string]bool{}
	descs := make([]sourceDesc, 14) // already 14 of 15 slots full
	got := addNameMatchedDescriptors(descs, seen, all, rawEnts, maxSources)
	if len(got) > maxSources {
		t.Errorf("expected len <= %d, got %d", maxSources, len(got))
	}
}

func TestAddNameMatchedDescriptors_RespectsSeen(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{"csv-viewer": true} // already added
	got := addNameMatchedDescriptors(nil, seen, all, []string{"csv-viewer"}, maxSources)
	if containsName(got, "csv-viewer") {
		t.Errorf("did not expect duplicate csv-viewer (already seen), got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_CaseInsensitive(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{}
	got := addNameMatchedDescriptors(nil, seen, all, []string{"Thrive"}, maxSources)
	if !containsName(got, "thrive") {
		t.Errorf("expected thrive (case-insensitive match for Thrive), got %v", names(got))
	}
}

func TestAddNameMatchedDescriptors_UnderscoreNormalize(t *testing.T) {
	all := routerTestSources()
	seen := map[string]bool{}
	// User types "mcp_perforce" (underscores). Should normalize to mcp-perforce.
	got := addNameMatchedDescriptors(nil, seen, all, []string{"mcp_perforce"}, maxSources)
	if !containsName(got, "mcp-perforce") {
		t.Errorf("expected mcp-perforce for entity 'mcp_perforce', got %v", names(got))
	}
}

// --- helpers ---

func containsName(descs []sourceDesc, name string) bool {
	for _, d := range descs {
		if d.Name == name {
			return true
		}
	}
	return false
}

func names(descs []sourceDesc) []string {
	out := make([]string, len(descs))
	for i, d := range descs {
		out[i] = d.Name
	}
	return out
}

func TestNormalizeLLMPick(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"csv-viewer", "csv-viewer"},
		{"csv-viewer (tool)", "csv-viewer"},
		{"csv-viewer (codebase)", "csv-viewer"},
		{`"csv-viewer"`, "csv-viewer"},
		{` "csv-viewer (tool)" `, "csv-viewer"},
		{"aida", "aida"},
		{"git-dev (git)", "git-dev"},
	}
	for _, c := range cases {
		got := normalizeLLMPick(c.in)
		if got != c.want {
			t.Errorf("normalizeLLMPick(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- resolveRoutePicks (Fix 3: none_viable escape + filler boost cap) ---

func TestResolveRoutePicks_NoneViablePropagates(t *testing.T) {
	result := &LLMRouteResult{NoneViable: true, Reason: "no candidate can answer a clock question"}
	candidates := []ScoredSource{
		{Name: "gworkspace", Source: &config.Source{Type: "tool"}, Score: 0},
	}
	got, refined, err := resolveRoutePicks(result, candidates, routerTestSources(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.NoneViable {
		t.Errorf("expected NoneViable to propagate as true, got false")
	}
	if len(refined) != len(candidates) || refined[0].Name != "gworkspace" {
		t.Errorf("expected deterministic candidates passed through unchanged, got %v", refined)
	}
}

func TestResolveRoutePicks_NoneViableIgnoredWhenStrongDeterministicCandidate(t *testing.T) {
	result := &LLMRouteResult{NoneViable: true, Reason: "nothing fits"}
	candidates := []ScoredSource{
		{Name: "current-time", Source: &config.Source{Type: "tool"}, Score: 100}, // route-boosted
		{Name: "gworkspace", Source: &config.Source{Type: "tool"}, Score: 0},
	}
	got, refined, err := resolveRoutePicks(result, candidates, routerTestSources(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.NoneViable {
		t.Errorf("expected NoneViable to be cleared when a candidate has score >= 100, got true")
	}
	if len(refined) != len(candidates) {
		t.Errorf("expected deterministic candidates passed through unchanged, got %v", refined)
	}
}

func TestResolveRoutePicks_NoneViableIgnoredWhenPicksNonEmpty(t *testing.T) {
	// Contradiction: LLM said none_viable but also returned a pick. The
	// pick should win (treated as a normal, non-none_viable response).
	result := &LLMRouteResult{NoneViable: true, Sources: []string{"aida"}, Reason: "contradiction"}
	candidates := []ScoredSource{
		{Name: "aida", Source: &config.Source{Type: "codebase"}, Score: 5},
	}
	got, refined, err := resolveRoutePicks(result, candidates, routerTestSources(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.NoneViable {
		t.Errorf("expected NoneViable cleared when Sources is non-empty, got true")
	}
	if len(refined) != 1 || refined[0].Name != "aida" || refined[0].Score != 5+llmRouteBoost {
		t.Errorf("expected aida picked with full llmRouteBoost, got %v", refined)
	}
}

func TestResolveRoutePicks_FillerPickCapped(t *testing.T) {
	result := &LLMRouteResult{Sources: []string{"gworkspace"}, Reason: "best of a bad list"}
	candidates := []ScoredSource{
		{Name: "gworkspace", Source: &config.Source{Type: "tool"}, Score: 0},
	}
	fillerNames := map[string]bool{"gworkspace": true}
	_, refined, err := resolveRoutePicks(result, candidates, routerTestSources(), fillerNames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refined) != 1 {
		t.Fatalf("expected 1 picked source, got %d", len(refined))
	}
	if want := 0 + fillerRouteBoost; refined[0].Score != want {
		t.Errorf("filler pick score = %d, want %d (fillerRouteBoost, not llmRouteBoost)", refined[0].Score, want)
	}
}

func TestResolveRoutePicks_NonFillerPickGetsFullBoost(t *testing.T) {
	result := &LLMRouteResult{Sources: []string{"csv-viewer"}, Reason: "named explicitly"}
	candidates := []ScoredSource{
		{Name: "csv-viewer", Source: &config.Source{Type: "codebase"}, Score: 5},
	}
	// fillerNames does not contain csv-viewer -- it was a real, evaluated candidate.
	fillerNames := map[string]bool{"gworkspace": true}
	_, refined, err := resolveRoutePicks(result, candidates, routerTestSources(), fillerNames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refined) != 1 {
		t.Fatalf("expected 1 picked source, got %d", len(refined))
	}
	if want := 5 + llmRouteBoost; refined[0].Score != want {
		t.Errorf("non-filler pick score = %d, want %d (llmRouteBoost)", refined[0].Score, want)
	}
}

func TestResolveRoutePicks_FillerPickViaAllSourcesFallback(t *testing.T) {
	// Pick is a filler name that never made it into `candidates` (e.g. it
	// was only added to the LLM's prompt list, not the deterministic
	// candidate set) -- resolveRoutePicks falls back to allSources.
	result := &LLMRouteResult{Sources: []string{"thrive"}, Reason: "filler fallback"}
	fillerNames := map[string]bool{"thrive": true}
	_, refined, err := resolveRoutePicks(result, nil, routerTestSources(), fillerNames)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refined) != 1 || refined[0].Score != fillerRouteBoost {
		t.Errorf("expected thrive picked with fillerRouteBoost via allSources fallback, got %v", refined)
	}
}

// Sanity check: the router prompt builder should include the LITERAL NAME
// OVERRIDE rule once Fix 2 lands. This test is here so we lock that in.
func TestLLMRoutePrompt_LiteralNameOverridePresent(t *testing.T) {
	// We can't call LLMRoute without an LLM client, so we re-derive the
	// prompt fragment from the router.go source by inspecting the const
	// string at the top of the prompt builder. The simplest sanity check
	// is to confirm the rule is present in the file via a smoke string.
	// (A stronger test would extract the prompt builder into its own
	// helper, but for the scope of issue #14 a presence check is enough
	// to detect accidental removal.)
	want := "LITERAL NAME OVERRIDE"
	body := pickingRulesPromptForTest()
	if !strings.Contains(body, want) {
		t.Errorf("router prompt rules missing %q. Got:\n%s", want, body)
	}
}

// TestLLMRoutePrompt_NoneViableEscapePresent locks in the Fix 3 prompt
// change: rule 11 must offer an honest none_viable escape instead of
// forcing a pick ("return one source anyway"), which is how gworkspace
// won a "what time is it" query in the original incident.
func TestLLMRoutePrompt_NoneViableEscapePresent(t *testing.T) {
	body := pickingRulesPromptForTest()
	if !strings.Contains(body, "none_viable") {
		t.Errorf("router prompt rules missing none_viable escape. Got:\n%s", body)
	}
	if strings.Contains(body, "return one source anyway") {
		t.Errorf("router prompt still contains the old forced-pick language 'return one source anyway'")
	}
}
