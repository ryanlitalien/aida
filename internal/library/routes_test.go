package library

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRoutes(t *testing.T, rootPath, body string) {
	t.Helper()
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, RoutesFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMatchGlob_DoubleStarSuffix(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/work/payments/**", "/work/payments", true},
		{"/work/payments/**", "/work/payments/checkout", true},
		{"/work/payments/**", "/work/payments/checkout/handlers", true},
		{"/work/payments/**", "/work/other", false},
		{"/work/payments/**", "/work/paymentsX", false},
	}
	for _, c := range cases {
		got, err := matchGlob(c.pattern, c.path)
		if err != nil {
			t.Errorf("matchGlob(%q, %q) error: %v", c.pattern, c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestRoute_AlwaysMatches(t *testing.T) {
	r := &Route{Always: true}
	ok, err := r.matches("/anywhere", nil)
	if err != nil || !ok {
		t.Errorf("always route should match, got ok=%v err=%v", ok, err)
	}
}

func TestRoute_NoPredicatesNeverMatches(t *testing.T) {
	r := &Route{Layers: []string{"x"}}
	ok, _ := r.matches("/anywhere", nil)
	if ok {
		t.Errorf("route with no predicates and not always should not match")
	}
}

func TestRoute_EntityRegex(t *testing.T) {
	r := &Route{MatchEntity: `^[A-Z0-9]{16}$`}
	ok, err := r.matches("/cwd", []string{"DKYAYQ3S195JMSND"})
	if err != nil || !ok {
		t.Errorf("expected entity match, got ok=%v err=%v", ok, err)
	}
	ok, _ = r.matches("/cwd", []string{"not-an-ari"})
	if ok {
		t.Errorf("expected no entity match for non-matching value")
	}
}

func TestRegistry_ResolveAccumulatesAcrossRoots(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")

	writeManifest(t, rootA, `version: 1
name: a`)
	writeRoutes(t, rootA, `
routes:
  - always: true
    layers: [global]
  - match_cwd: "/work/payments/**"
    layers: [domains/sqlite]
    sources: [sqlite]
`)
	writeManifest(t, rootB, `version: 1
name: b`)
	writeRoutes(t, rootB, `
routes:
  - match_cwd: "/work/payments/**"
    layers: [projects/work-payments]
    sources: [plausible]
`)

	cfg := &LibraryConfig{
		Roots: []RootRef{
			{Name: "a", Path: rootA},
			{Name: "b", Path: rootB},
		},
	}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	res, err := reg.Resolve("/work/payments/checkout", nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantLayers := []string{"global", "domains/sqlite", "projects/work-payments"}
	if !equalStrings(res.Layers, wantLayers) {
		t.Errorf("layers: got %v, want %v", res.Layers, wantLayers)
	}
	wantSources := []string{"sqlite", "plausible"}
	if !equalStrings(res.Sources, wantSources) {
		t.Errorf("sources: got %v, want %v", res.Sources, wantSources)
	}
}

func TestRegistry_ResolveDedupes(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	writeManifest(t, rootA, `version: 1
name: a`)
	writeRoutes(t, rootA, `
routes:
  - always: true
    layers: [global, foo]
  - match_cwd: "/x/**"
    layers: [foo, bar]
`)
	cfg := &LibraryConfig{Roots: []RootRef{{Name: "a", Path: rootA}}}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, err := reg.Resolve("/x/y", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"global", "foo", "bar"}
	if !equalStrings(res.Layers, want) {
		t.Errorf("expected dedup, got %v want %v", res.Layers, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
