package roster

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpandDiscovery_GoodFixture(t *testing.T) {
	parent := &Entry{
		Name:          "acme-widgets-team",
		Discover:      "testdata/agents",
		CallSignsFrom: "testdata/org-chart.md",
		Profiles:      []string{"work"},
		Subagent:      &SubagentSpec{Dir: "testdata/acme-widgets-stub", Timeout: 45},
	}

	entries, err := expandDiscovery(parent)
	if err != nil {
		t.Fatalf("expandDiscovery: %v", err)
	}

	wantNames := []string{"devops", "product-manager", "qa"}
	if len(entries) != len(wantNames) {
		t.Fatalf("got %d entries, want %d: %v", len(entries), len(wantNames), entryNames(entries))
	}
	for i, want := range wantNames {
		if entries[i].Name != want {
			t.Errorf("entries[%d].Name = %q, want %q", i, entries[i].Name, want)
		}
	}

	byName := map[string]*Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}

	pm := byName["product-manager"]
	if pm.Description != "Backlog funnel and grooming for the Acme Widgets team." {
		t.Errorf("product-manager.Description = %q", pm.Description)
	}
	if pm.CallSign != "Pamela" {
		t.Errorf("product-manager.CallSign = %q, want Pamela", pm.CallSign)
	}
	if !containsStr(pm.Aliases, "pamela") {
		t.Errorf("product-manager.Aliases = %v, want to contain \"pamela\"", pm.Aliases)
	}
	if pm.Kind != KindSubagent {
		t.Errorf("product-manager.Kind = %q, want %q", pm.Kind, KindSubagent)
	}
	if pm.Subagent.Agent != "product-manager" || pm.Subagent.Dir != "testdata/acme-widgets-stub" {
		t.Errorf("product-manager.Subagent = %+v", pm.Subagent)
	}
	if pm.Subagent.Timeout != parent.Subagent.Timeout {
		t.Errorf("product-manager.Subagent.Timeout = %d, want inherited parent timeout %d", pm.Subagent.Timeout, parent.Subagent.Timeout)
	}
	if pm.DiscoveredFrom != "testdata/agents" {
		t.Errorf("product-manager.DiscoveredFrom = %q, want %q", pm.DiscoveredFrom, "testdata/agents")
	}

	devops := byName["devops"]
	if devops.CallSign != "Devin" {
		t.Errorf("devops.CallSign = %q, want Devin", devops.CallSign)
	}

	qa := byName["qa"]
	if qa.Description != "" {
		t.Errorf("qa.Description = %q, want empty (qa.md has no frontmatter)", qa.Description)
	}
	if qa.CallSign != "" {
		t.Errorf("qa.CallSign = %q, want empty (org-chart line for qa is not bold)", qa.CallSign)
	}
	if len(qa.Aliases) != 0 {
		t.Errorf("qa.Aliases = %v, want empty", qa.Aliases)
	}
}

func TestExpandDiscovery_MissingSubagentDir(t *testing.T) {
	parent := &Entry{Name: "broken", Discover: "testdata/agents"}
	_, err := expandDiscovery(parent)
	if err == nil {
		t.Fatal("expected an error when parent.Subagent is nil")
	}
	if got := err.Error(); !strings.Contains(got, "needs a subagent.dir") {
		t.Errorf("error = %q, want to mention subagent.dir", got)
	}
}

func TestExpandDiscovery_MissingDir(t *testing.T) {
	parent := &Entry{
		Name:     "broken",
		Discover: filepath.Join(t.TempDir(), "does-not-exist"),
		Subagent: &SubagentSpec{Dir: "somewhere"},
	}
	_, err := expandDiscovery(parent)
	if err == nil {
		t.Fatal("expected an error for a nonexistent discover dir")
	}
	if got := err.Error(); !strings.Contains(got, "missing or empty") {
		t.Errorf("error = %q, want to mention \"missing or empty\"", got)
	}
}

func TestExpandDiscovery_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	parent := &Entry{
		Name:     "broken",
		Discover: dir,
		Subagent: &SubagentSpec{Dir: "somewhere"},
	}
	_, err := expandDiscovery(parent)
	if err == nil {
		t.Fatal("expected an error for a discover dir with zero *.md files")
	}
	if got := err.Error(); !strings.Contains(got, "missing or empty") {
		t.Errorf("error = %q, want to mention \"missing or empty\"", got)
	}
}

// TestExpandDiscovery_CacheInvalidatesOnMtime verifies that a directory
// scan is cached (so a second call with an unchanged dir doesn't re-parse)
// but a change to the directory's contents -- which bumps its mtime -- is
// picked up on the next call.
func TestExpandDiscovery_CacheInvalidatesOnMtime(t *testing.T) {
	dir := t.TempDir()
	writeAgent(t, dir, "solo.md", "solo agent, no frontmatter")

	parent := &Entry{
		Name:     "cache-test",
		Discover: dir,
		Subagent: &SubagentSpec{Dir: "somewhere"},
	}

	first, err := expandDiscovery(parent)
	if err != nil {
		t.Fatalf("expandDiscovery (first): %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first scan: got %d entries, want 1", len(first))
	}

	// Mutating the returned slice must never corrupt the cache.
	first[0] = &Entry{Name: "corrupted"}

	second, err := expandDiscovery(parent)
	if err != nil {
		t.Fatalf("expandDiscovery (second, cache hit): %v", err)
	}
	if len(second) != 1 || second[0].Name != "solo" {
		t.Fatalf("cache hit returned mutated/incorrect data: %v", entryNames(second))
	}

	// Add a second file and force the directory mtime forward so the miss
	// is unambiguous even on filesystems with coarse mtime resolution.
	writeAgent(t, dir, "backup.md", "backup agent, no frontmatter")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(dir, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	third, err := expandDiscovery(parent)
	if err != nil {
		t.Fatalf("expandDiscovery (third, after mtime bump): %v", err)
	}
	if len(third) != 2 {
		t.Fatalf("third scan: got %d entries, want 2 (cache should have invalidated): %v", len(third), entryNames(third))
	}
}

func TestParseAgentFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		wantName string
		wantDesc string
	}{
		{
			name:     "full frontmatter",
			data:     "---\nname: Product Manager\ndescription: Backlog funnel.\n---\n\nBody.\n",
			wantName: "Product Manager",
			wantDesc: "Backlog funnel.",
		},
		{
			name:     "no frontmatter",
			data:     "Just a plain stub file.\n",
			wantName: "",
			wantDesc: "",
		},
		{
			name:     "unterminated frontmatter",
			data:     "---\nname: Oops\ndescription: Never closed\n",
			wantName: "",
			wantDesc: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, desc := parseAgentFrontmatter([]byte(tc.data))
			if name != tc.wantName || desc != tc.wantDesc {
				t.Errorf("parseAgentFrontmatter() = (%q, %q), want (%q, %q)", name, desc, tc.wantName, tc.wantDesc)
			}
		})
	}
}

func TestLoadCallSigns(t *testing.T) {
	callSigns := loadCallSigns("testdata/org-chart.md")
	want := map[string]string{
		"product-manager": "Pamela",
		"devops":          "Devin",
	}
	if len(callSigns) != len(want) {
		t.Fatalf("loadCallSigns() = %v, want %v", callSigns, want)
	}
	for slug, wantCallSign := range want {
		if got := callSigns[slug]; got != wantCallSign {
			t.Errorf("callSigns[%q] = %q, want %q", slug, got, wantCallSign)
		}
	}
	if _, ok := callSigns["qa"]; ok {
		t.Error("\"qa\" line in the org-chart snippet is not bold; should not produce a call-sign")
	}
}

func TestLoadCallSigns_MissingFile(t *testing.T) {
	if got := loadCallSigns("testdata/does-not-exist.md"); len(got) != 0 {
		t.Errorf("loadCallSigns(missing file) = %v, want empty map", got)
	}
	if got := loadCallSigns(""); len(got) != 0 {
		t.Errorf("loadCallSigns(\"\") = %v, want empty map", got)
	}
}

func writeAgent(t *testing.T, dir, filename, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(body), 0644); err != nil {
		t.Fatalf("writing %s: %v", filename, err)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
