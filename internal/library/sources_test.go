package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry_LoadSources(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	if err := os.MkdirAll(filepath.Join(rootA, "sources"), 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, rootA, `
version: 1
name: a
sources:
  plausible:
    file: sources/plausible.yaml
`)
	if err := os.WriteFile(filepath.Join(rootA, "sources", "plausible.yaml"), []byte(`
type: data-source
description: Plausible analytics
capabilities: [sql-query, ari-lookup]
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &LibraryConfig{Roots: []RootRef{{Name: "a", Path: rootA}}}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	srcs, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	got := srcs["plausible"]
	if got == nil {
		t.Fatal("expected plausible source")
	}
	if got.Type != "data-source" {
		t.Errorf("type: got %q, want %q", got.Type, "data-source")
	}
	if !got.HasCapability("sql-query") {
		t.Errorf("expected sql-query capability")
	}
}

// TestRegistry_LoadSources_AutoDiscoverSelfRoot verifies that source
// YAMLs and per-source layer markdowns dropped under a self-owned root
// are picked up without needing a library.yaml manifest entry. Manifest
// entries still take precedence on name collision; non-self-owned roots
// continue to require explicit registration.
func TestRegistry_LoadSources_AutoDiscoverSelfRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "self")
	for _, p := range []string{
		filepath.Join(root, "sources"),
		filepath.Join(root, "layers", "sources"),
	} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Manifest references one source; another exists on disk only.
	writeManifest(t, root, `
version: 1
name: self
sources:
  registered:
    file: sources/registered.yaml
`)
	if err := os.WriteFile(filepath.Join(root, "sources", "registered.yaml"), []byte(`
type: tool
description: Registered the old way
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sources", "drop-in.yaml"), []byte(`
type: tool
description: Drop-in source - no manifest entry
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "layers", "sources", "drop-in.md"), []byte(
		"# Layer for drop-in\n",
	), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &LibraryConfig{Roots: []RootRef{{Name: "self", Path: root, Owner: OwnerSelf}}}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	srcs, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("want 2 sources (manifest + auto-discovered), got %d", len(srcs))
	}
	if srcs["registered"] == nil {
		t.Error("expected manifest-registered source to load")
	}
	if srcs["drop-in"] == nil {
		t.Error("expected auto-discovered drop-in source to load")
	}
	if srcs["drop-in"].Description != "Drop-in source - no manifest entry" {
		t.Errorf("drop-in description: got %q", srcs["drop-in"].Description)
	}
	if reg.Layers["sources/drop-in"] == nil {
		t.Error("expected auto-discovered layer sources/drop-in")
	}
}

// TestRegistry_LoadSources_NoAutoDiscoverNonSelf verifies that non-self
// roots still require manifest entries - auto-discovery is intentionally
// scoped to owner: self.
func TestRegistry_LoadSources_NoAutoDiscoverNonSelf(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "shared")
	if err := os.MkdirAll(filepath.Join(root, "sources"), 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, root, `
version: 1
name: shared
`)
	if err := os.WriteFile(filepath.Join(root, "sources", "drop-in.yaml"), []byte(`
type: tool
description: Should not auto-load on non-self root
`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &LibraryConfig{Roots: []RootRef{{Name: "shared", Path: root, Owner: OwnerWork}}}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	srcs, err := reg.LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if _, found := srcs["drop-in"]; found {
		t.Error("non-self root should not auto-discover drop-in.yaml")
	}
}

func TestRegistry_LoadSources_LaterRootOverrides(t *testing.T) {
	dir := t.TempDir()
	rootA := filepath.Join(dir, "a")
	rootB := filepath.Join(dir, "b")
	for _, p := range []string{
		filepath.Join(rootA, "sources"),
		filepath.Join(rootB, "sources"),
	} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, rootA, `
version: 1
name: a
sources:
  plausible:
    file: sources/plausible.yaml
`)
	writeManifest(t, rootB, `
version: 1
name: b
sources:
  plausible:
    file: sources/plausible.yaml
`)
	if err := os.WriteFile(filepath.Join(rootA, "sources", "plausible.yaml"), []byte("type: a-version\ndescription: A\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "sources", "plausible.yaml"), []byte("type: b-version\ndescription: B\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &LibraryConfig{Roots: []RootRef{
		{Name: "a", Path: rootA},
		{Name: "b", Path: rootB},
	}}
	reg, err := BuildRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srcs, err := reg.LoadSources()
	if err != nil {
		t.Fatal(err)
	}
	if srcs["plausible"].Type != "b-version" {
		t.Errorf("expected later root B to override A, got %q", srcs["plausible"].Type)
	}
}
