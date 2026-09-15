package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilterByProfile(t *testing.T) {
	sources := Sources{
		"finances": {Description: "personal", Profiles: []string{"home"}},
		"sqlite":   {Description: "work db", Profiles: []string{"work"}},
		"github":   {Description: "shared tool"}, // no profiles = all
		"aida":     {Description: "both", Profiles: []string{"home", "work"}},
	}

	t.Run("work profile excludes home-only", func(t *testing.T) {
		got := FilterByProfile(sources, "work")
		if _, ok := got["finances"]; ok {
			t.Error("finances should be excluded on work profile")
		}
		if _, ok := got["sqlite"]; !ok {
			t.Error("sqlite should be included on work profile")
		}
		if _, ok := got["github"]; !ok {
			t.Error("github (no profiles) should be included on work profile")
		}
		if _, ok := got["aida"]; !ok {
			t.Error("aida (both) should be included on work profile")
		}
	})

	t.Run("home profile excludes work-only", func(t *testing.T) {
		got := FilterByProfile(sources, "home")
		if _, ok := got["finances"]; !ok {
			t.Error("finances should be included on home profile")
		}
		if _, ok := got["sqlite"]; ok {
			t.Error("sqlite should be excluded on home profile")
		}
		if _, ok := got["github"]; !ok {
			t.Error("github (no profiles) should be included on home profile")
		}
	})

	t.Run("empty profile returns all", func(t *testing.T) {
		got := FilterByProfile(sources, "")
		if len(got) != len(sources) {
			t.Errorf("expected %d sources, got %d", len(sources), len(got))
		}
	})
}

func TestFilterByReachablePath(t *testing.T) {
	// Create a temp directory that exists.
	existingDir := t.TempDir()

	// A path that definitely does not exist.
	missingDir := filepath.Join(existingDir, "nonexistent-subdir")

	sources := Sources{
		"local-project": {Path: existingDir, Description: "exists on disk"},
		"work-project":  {Path: missingDir, Description: "path does not exist"},
		"plausible":     {Path: "", Description: "no path (tool-type)"},
	}

	got := FilterByReachablePath(sources)

	if _, ok := got["local-project"]; !ok {
		t.Error("local-project (existing path) should be kept")
	}
	if _, ok := got["work-project"]; ok {
		t.Error("work-project (missing path) should be excluded")
	}
	if _, ok := got["plausible"]; !ok {
		t.Error("plausible (empty path) should be kept")
	}
}

func TestFilterByReachablePath_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	sources := Sources{
		"home-project": {Path: "~/", Description: "tilde path to home"},
		"bad-tilde":    {Path: "~/nonexistent-aida-test-dir-xyz", Description: "tilde but missing"},
	}

	got := FilterByReachablePath(sources)

	if _, ok := got["home-project"]; !ok {
		t.Errorf("home-project (~/ -> %s) should be kept", home)
	}
	if _, ok := got["bad-tilde"]; ok {
		t.Error("bad-tilde (nonexistent under ~) should be excluded")
	}
}

func TestFilterByReachablePath_FileNotDir(t *testing.T) {
	// A regular file (not a directory) should be excluded.
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "not-a-dir.txt")
	os.WriteFile(filePath, []byte("hi"), 0644)

	sources := Sources{
		"file-src": {Path: filePath, Description: "path is a file, not dir"},
	}

	got := FilterByReachablePath(sources)
	if _, ok := got["file-src"]; ok {
		t.Error("file-src (path is a file, not a directory) should be excluded")
	}
}

// TestBuildKnownGitHubReposHint covers the helper that injects the
// "known repos" table into gh-based query construction. Regression:
// prior to this helper, the query-construction LLM had no map from
// entity names the user speaks (e.g. "butterstack", "aida") to the
// actual owner/repo pairs on library sources, and fell back to
// guessing `--owner ryanlitalien`.
func TestBuildKnownGitHubReposHint(t *testing.T) {
	srcs := Sources{
		"butter-stack": {Repo: "butterstack/butter_stack", Entities: []string{"butter", "stack"}},
		"aida":         {Repo: "ryanlitalien/aida", Entities: []string{"aida"}},
		"sqlite":       {Repo: "", Entities: []string{"db"}}, // no repo → excluded
		"nil-source":   nil,                                  // nil → excluded without panic
	}
	got := BuildKnownGitHubReposHint(srcs)
	if got == "" {
		t.Fatal("expected non-empty hint")
	}
	// Each populated source's owner/repo should appear.
	for _, want := range []string{"butterstack/butter_stack", "ryanlitalien/aida"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint missing %q, got:\n%s", want, got)
		}
	}
	// Aliases (source name + Entities + owner/name tokens) should appear.
	for _, want := range []string{"butter-stack", "butter", "stack", "butterstack", "butter_stack", "aida", "aida", "ryanlitalien"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint missing alias %q, got:\n%s", want, got)
		}
	}
	// Sources with no Repo must be excluded.
	if strings.Contains(got, "sqlite") {
		t.Errorf("no-repo source should not appear in hint, got:\n%s", got)
	}
	// Header should steer the LLM toward --repo, not --owner.
	if !strings.Contains(got, "--repo") {
		t.Errorf("expected --repo instruction in hint, got:\n%s", got)
	}
}

func TestBuildKnownGitHubReposHint_Empty(t *testing.T) {
	if got := BuildKnownGitHubReposHint(nil); got != "" {
		t.Errorf("nil sources should produce empty hint, got: %s", got)
	}
	if got := BuildKnownGitHubReposHint(Sources{}); got != "" {
		t.Errorf("empty Sources should produce empty hint, got: %s", got)
	}
	// All sources with empty Repo → empty hint.
	if got := BuildKnownGitHubReposHint(Sources{"a": {Repo: ""}}); got != "" {
		t.Errorf("sources without Repo should produce empty hint, got: %s", got)
	}
}
