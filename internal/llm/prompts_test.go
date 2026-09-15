package llm

import (
	"strings"
	"testing"
)

// TestQueryConstructUserPromptFlatKeyList covers the plain sorted
// key/value rendering that replaced the registry-era grouped multi-ARI
// emission (groupResolvedKeys and its plural "_aris"/IN(...) hint were
// removed along with the partner registry - every resolved key, however
// named, now renders as its own "- key: value" line).
func TestQueryConstructUserPromptFlatKeyList(t *testing.T) {
	resolved := map[string]string{
		"entity_name":      "camp-butz",
		"resource_id":      "HBRPOIG8F1CBFNO6",
		"resource_id_ca":   "MECOSFOGYR3XKXWN",
		"resource_id_test": "B9M80O2RAK1VRJNV",
		"empty_key":        "",
	}
	prompt := QueryConstructUserPrompt(
		"sqlite", "ctx", "sqlite3 -q {query}",
		"latest camp-butz booking and amount transacted",
		resolved, "", "", "",
	)

	for _, want := range []string{
		"entity_name: camp-butz",
		"resource_id: HBRPOIG8F1CBFNO6",
		"resource_id_ca: MECOSFOGYR3XKXWN",
		"resource_id_test: B9M80O2RAK1VRJNV",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "empty_key") {
		t.Errorf("empty-valued key should be dropped:\n%s", prompt)
	}
	// No grouped/plural rendering survives the registry removal.
	if strings.Contains(prompt, "resource_ids") {
		t.Errorf("prompt should not render a plural group label:\n%s", prompt)
	}
}

func TestQueryConstructUserPromptDeterministic(t *testing.T) {
	// Go map iteration is random - verify the prompt output is stable
	// across invocations so the LLM sees the same context each time.
	resolved := map[string]string{
		"z_last":      "Z",
		"a_first":     "A",
		"resource_id": "ARI1",
		"account_id":  "ARI2",
	}
	first := QueryConstructUserPrompt("s", "", "", "q", copyMap(resolved), "", "", "")
	for i := 0; i < 20; i++ {
		got := QueryConstructUserPrompt("s", "", "", "q", copyMap(resolved), "", "", "")
		if got != first {
			t.Fatalf("non-deterministic prompt output on iteration %d:\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}
}

func TestQueryConstructUserPromptSingletonKeepsOldFormat(t *testing.T) {
	// A single-valued key should render as "resource_id: VALUE", never a
	// pluralized "resource_ids (...)" group label - there is no grouping
	// left to trigger one (see TestQueryConstructUserPromptFlatKeyList).
	resolved := map[string]string{
		"entity_name": "thrive",
		"resource_id": "ARI1",
	}
	prompt := QueryConstructUserPrompt("s", "", "", "q", resolved, "", "", "")
	if strings.Contains(prompt, "resource_ids") {
		t.Errorf("single-value key should not render plural label:\n%s", prompt)
	}
	if !strings.Contains(prompt, "resource_id: ARI1") {
		t.Errorf("prompt missing singular form:\n%s", prompt)
	}
}

func TestQueryConstructUserPromptIncludesCurrentDatetime(t *testing.T) {
	prompt := QueryConstructUserPrompt("s", "ctx", "tmpl", "what time is it",
		map[string]string{}, "", "", "Tue, 07 Jul 2026 14:32:05 EDT")
	if !strings.Contains(prompt, "Current local datetime: Tue, 07 Jul 2026 14:32:05 EDT") {
		t.Errorf("prompt missing current datetime line:\n%s", prompt)
	}
}

func TestQueryConstructUserPromptOmitsCurrentDatetimeWhenEmpty(t *testing.T) {
	prompt := QueryConstructUserPrompt("s", "ctx", "tmpl", "q", map[string]string{}, "", "", "")
	if strings.Contains(prompt, "Current local datetime") {
		t.Errorf("prompt should omit current datetime line when empty:\n%s", prompt)
	}
}

func TestSynthesisUserPromptIncludesCurrentDatetime(t *testing.T) {
	prompt := SynthesisUserPrompt("what time is it", "", "", nil, "", "Tue, 07 Jul 2026 14:32:05 EDT")
	if !strings.Contains(prompt, "Current local datetime: Tue, 07 Jul 2026 14:32:05 EDT") {
		t.Errorf("prompt missing current datetime line:\n%s", prompt)
	}
}

func TestSynthesisUserPromptOmitsCurrentDatetimeWhenEmpty(t *testing.T) {
	prompt := SynthesisUserPrompt("q", "", "", nil, "", "")
	if strings.Contains(prompt, "Current local datetime") {
		t.Errorf("prompt should omit current datetime line when empty:\n%s", prompt)
	}
}

func TestSynthesisSystemPromptHasVolatileDataGuard(t *testing.T) {
	if !strings.Contains(SynthesisSystemPrompt, "VOLATILE DATA GUARD") {
		t.Error("SynthesisSystemPrompt missing VOLATILE DATA GUARD section")
	}
	if !strings.Contains(SynthesisSystemPrompt, "stale") {
		t.Error("SynthesisSystemPrompt volatile data guard should mention staleness")
	}
}

func TestSynthesisSystemPromptHasVerificationHonesty(t *testing.T) {
	if !strings.Contains(SynthesisSystemPrompt, "VERIFICATION HONESTY") {
		t.Error("SynthesisSystemPrompt missing VERIFICATION HONESTY section")
	}
	if !strings.Contains(SynthesisSystemPrompt, "never successfully queried") {
		t.Error("SynthesisSystemPrompt verification honesty should distinguish zero-results from never-queried")
	}
}

func TestSynthesisUserPromptEmitsDataKindLineWhenFromContextDoc(t *testing.T) {
	results := []SourceResultSummary{
		{Source: "codebase", Status: "success", FromContextDoc: true},
	}
	prompt := SynthesisUserPrompt("q", "", "", results, "", "")
	if !strings.Contains(prompt, "Data kind: DOCUMENTATION ABOUT THIS SOURCE, NOT DATA FROM IT") {
		t.Errorf("prompt missing data kind marker line for FromContextDoc result:\n%s", prompt)
	}
}

func TestSynthesisUserPromptOmitsDataKindLineWhenNotFromContextDoc(t *testing.T) {
	results := []SourceResultSummary{
		{Source: "codebase", Status: "success", FromContextDoc: false},
	}
	prompt := SynthesisUserPrompt("q", "", "", results, "", "")
	if strings.Contains(prompt, "Data kind:") {
		t.Errorf("prompt should omit data kind marker line when FromContextDoc is false:\n%s", prompt)
	}
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
