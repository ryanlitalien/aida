package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractLayerDescription_FromConventionalFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "x.md")
	body := "# Source: x\n\nA short description that ought to land in L1.\n\n# Body\n\nLorem ipsum.\n"
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	got := extractLayerDescription(path)
	want := "A short description that ought to land in L1."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractLayerDescription_SkipsHeadingsAndComments(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "y.md")
	body := "<!-- generated -->\n## H2\n# H1\n\nReal description here.\n"
	os.WriteFile(path, []byte(body), 0644)
	got := extractLayerDescription(path)
	if got != "Real description here." {
		t.Errorf("got %q, want plain description", got)
	}
}

func TestExtractLayerDescription_TruncatesLongLine(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "z.md")
	long := strings.Repeat("a", maxDescriptionLen+50)
	os.WriteFile(path, []byte("# title\n\n"+long), 0644)
	got := extractLayerDescription(path)
	if len(got) > maxDescriptionLen+5 { // +5 for the "…" overhead
		t.Errorf("description not truncated: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}
}

func TestExtractLayerDescription_MissingFileReturnsEmpty(t *testing.T) {
	got := extractLayerDescription("/nonexistent/path/file.md")
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestLayerSummaries_AlphabeticalAndAvailableOnly(t *testing.T) {
	tmp := t.TempDir()
	// Two layer files on disk.
	makeLayer := func(name, body string) string {
		p := filepath.Join(tmp, name+".md")
		os.WriteFile(p, []byte(body), 0644)
		return p
	}
	pathA := makeLayer("zeta", "# Source: zeta\n\nzeta desc.\n")
	pathB := makeLayer("alpha", "# Source: alpha\n\nalpha desc.\n")
	pathC := makeLayer("missing-tool", "# Source: missing-tool\n\nshould not appear.\n")

	root := &Root{Ref: RootRef{Name: "test"}, AbsPath: tmp}
	reg := &Registry{
		Layers: map[string]*ResolvedLayer{
			"zeta":         {Name: "zeta", Root: root, AbsFile: pathA, Available: true},
			"alpha":        {Name: "alpha", Root: root, AbsFile: pathB, Available: true},
			"missing-tool": {Name: "missing-tool", Root: root, AbsFile: pathC, Available: false},
		},
	}
	got := reg.LayerSummaries()
	if len(got) != 2 {
		t.Fatalf("got %d summaries, want 2", len(got))
	}
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Errorf("not alphabetical: %+v", got)
	}
	if got[0].Description != "alpha desc." {
		t.Errorf("alpha description = %q", got[0].Description)
	}
	for _, s := range got {
		if s.Name == "missing-tool" {
			t.Errorf("unavailable layer leaked into summaries")
		}
	}
}

func TestL1IndexString_FormatsAsBulletList(t *testing.T) {
	got := L1IndexString([]LayerSummary{
		{Name: "a", Description: "alpha"},
		{Name: "b", Description: ""},
		{Name: "c", Description: "charlie"},
	})
	wantLines := []string{
		"- a: alpha",
		"- b",
		"- c: charlie",
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("missing line %q in output:\n%s", w, got)
		}
	}
}

func TestL1IndexString_EmptyForNilOrEmpty(t *testing.T) {
	if L1IndexString(nil) != "" {
		t.Errorf("nil input should return empty")
	}
	if L1IndexString([]LayerSummary{}) != "" {
		t.Errorf("empty slice should return empty")
	}
}

func TestLayerSummaries_NilRegistry(t *testing.T) {
	var r *Registry
	if got := r.LayerSummaries(); got != nil {
		t.Errorf("nil registry should return nil, got %v", got)
	}
}
