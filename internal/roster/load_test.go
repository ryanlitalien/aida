package roster

import (
	"os"
	"testing"
)

// findEntry returns the entry named name, or nil.
func findEntry(entries []*Entry, name string) *Entry {
	for _, e := range entries {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func TestLoad_GoodFixture_WorkProfile(t *testing.T) {
	r, err := Load("testdata", "work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Issues()) != 0 {
		t.Fatalf("expected no issues, got %v", r.Issues())
	}

	// aida + product-manager (override) + devops + qa (discovered),
	// workouts excluded (home-only).
	entries := r.Entries()
	wantNames := []string{"aida", "devops", "product-manager", "qa"}
	if len(entries) != len(wantNames) {
		t.Fatalf("Entries() = %d entries, want %d: %v", len(entries), len(wantNames), entryNames(entries))
	}
	for i, want := range wantNames {
		if entries[i].Name != want {
			t.Errorf("Entries()[%d].Name = %q, want %q (full: %v)", i, entries[i].Name, want, entryNames(entries))
		}
	}

	if findEntry(entries, "workouts") != nil {
		t.Error("workouts (profiles: [home]) leaked into the work profile's entries")
	}
}

func TestLoad_GoodFixture_HomeProfile(t *testing.T) {
	r, err := Load("testdata", "home")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entries := r.Entries()
	wantNames := []string{"aida", "workouts"}
	if len(entries) != len(wantNames) {
		t.Fatalf("Entries() = %d entries, want %d: %v", len(entries), len(wantNames), entryNames(entries))
	}
	for i, want := range wantNames {
		if entries[i].Name != want {
			t.Errorf("Entries()[%d].Name = %q, want %q", i, entries[i].Name, want)
		}
	}
}

func TestLoad_DiscoveryExpansion(t *testing.T) {
	r, err := Load("testdata", "work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	devops := findEntry(r.Entries(), "devops")
	if devops == nil {
		t.Fatal("expected discovered \"devops\" entry")
	}
	if devops.Kind != KindSubagent {
		t.Errorf("devops.Kind = %q, want %q", devops.Kind, KindSubagent)
	}
	if devops.Description != "Deploys, infra, on-call rotations." {
		t.Errorf("devops.Description = %q", devops.Description)
	}
	if devops.CallSign != "Devin" {
		t.Errorf("devops.CallSign = %q, want %q", devops.CallSign, "Devin")
	}
	if devops.Subagent == nil || devops.Subagent.Agent != "devops" {
		t.Errorf("devops.Subagent.Agent = %+v, want Agent=devops", devops.Subagent)
	}
	if devops.Subagent.Dir != "testdata/butterstack-stub" {
		t.Errorf("devops.Subagent.Dir = %q, want inherited parent dir", devops.Subagent.Dir)
	}
	if len(devops.Profiles) != 1 || devops.Profiles[0] != "work" {
		t.Errorf("devops.Profiles = %v, want [work] (inherited from parent)", devops.Profiles)
	}

	qa := findEntry(r.Entries(), "qa")
	if qa == nil {
		t.Fatal("expected discovered \"qa\" entry (no frontmatter, tolerant parse)")
	}
	if qa.Description != "" {
		t.Errorf("qa.Description = %q, want empty (no frontmatter in qa.md)", qa.Description)
	}
	if qa.CallSign != "" {
		t.Errorf("qa.CallSign = %q, want empty (not in org-chart snippet)", qa.CallSign)
	}

	// The discovery parent itself must never appear as a dispatch target.
	if findEntry(r.Entries(), "butterstack-team") != nil {
		t.Error("discovery parent \"butterstack-team\" leaked into entries")
	}
}

func TestLoad_HandAuthoredOverrideWins(t *testing.T) {
	r, err := Load("testdata", "work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	pm := findEntry(r.Entries(), "product-manager")
	if pm == nil {
		t.Fatal("expected \"product-manager\" entry")
	}
	// The hand-authored fixture description, not the discovered frontmatter's.
	const wantDesc = "Hand-authored override pinning aliases the frontmatter can't express."
	if pm.Description != wantDesc {
		t.Errorf("product-manager.Description = %q, want hand-authored override %q", pm.Description, wantDesc)
	}
	if pm.Subagent == nil || pm.Subagent.Model != "sonnet" {
		t.Errorf("product-manager.Subagent.Model = %+v, want Model=sonnet (round-tripped from testdata/roster.yaml)", pm.Subagent)
	}
	if pm.DiscoveredFrom != "" {
		t.Errorf("product-manager.DiscoveredFrom = %q, want empty (hand-authored, not discovered)", pm.DiscoveredFrom)
	}
	wantAliases := map[string]bool{"pam": true, "pamela": true, "the product manager": true}
	if len(pm.Aliases) != len(wantAliases) {
		t.Fatalf("product-manager.Aliases = %v, want %d aliases", pm.Aliases, len(wantAliases))
	}
	for _, a := range pm.Aliases {
		if !wantAliases[a] {
			t.Errorf("unexpected alias %q", a)
		}
	}

	// Exactly one "product-manager" survives -- the discovered twin is gone.
	all := r.AllEntries()
	count := 0
	for _, e := range all {
		if e.Name == "product-manager" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("found %d entries named product-manager in AllEntries(), want 1 (override should dedupe)", count)
	}
}

func TestLoad_MissingRosterFile(t *testing.T) {
	dir := t.TempDir()
	r, err := Load(dir, "work")
	if err != nil {
		t.Fatalf("Load with no roster.yaml: unexpected error %v", err)
	}
	if len(r.Entries()) != 0 {
		t.Errorf("Entries() = %v, want empty", r.Entries())
	}
	if len(r.AllEntries()) != 0 {
		t.Errorf("AllEntries() = %v, want empty", r.AllEntries())
	}
	if len(r.Issues()) != 0 {
		t.Errorf("Issues() = %v, want empty", r.Issues())
	}
	if r.Profile() != "work" {
		t.Errorf("Profile() = %q, want %q", r.Profile(), "work")
	}
}

func TestLoad_BrokenDiscoveryDoesNotFailWholeLoad(t *testing.T) {
	r, err := Load("testdata/baddiscover", "work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Issues()) == 0 {
		t.Fatal("expected an issue recorded for the broken discovery parent")
	}

	// The sibling standalone entry must still load fine.
	if findEntry(r.Entries(), "standalone") == nil {
		t.Error("expected \"standalone\" entry despite the broken sibling discovery parent")
	}
	if findEntry(r.Entries(), "broken-team") != nil {
		t.Error("broken discovery parent should never appear as an entry")
	}
}

func TestRoster_Get(t *testing.T) {
	r, err := Load("testdata", "work")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if e, ok := r.Get("DEVOPS"); !ok || e.Name != "devops" {
		t.Errorf("Get(%q) = %v, %v, want devops entry", "DEVOPS", e, ok)
	}
	if _, ok := r.Get("nonexistent"); ok {
		t.Error("Get(nonexistent) = ok, want not found")
	}
	// workouts is home-only; not reachable via Get() on a work-profile roster.
	if _, ok := r.Get("workouts"); ok {
		t.Error("Get(workouts) on the work profile roster = ok, want not found")
	}
}

func entryNames(entries []*Entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}

// Sanity check that the fixture directory exists, so a future rename of
// testdata/ fails loudly here instead of as a silent "0 entries" pass.
func TestMain_FixtureSanity(t *testing.T) {
	if _, err := os.Stat("testdata/roster.yaml"); err != nil {
		t.Fatalf("testdata/roster.yaml missing: %v", err)
	}
}
