package lessons

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// withTempHome redirects ~/.aida to a tempdir for the duration of the test.
// We do this by setting HOME so config.Dir() resolves there.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	hd := filepath.Join(dir, ".aida")
	if err := os.MkdirAll(hd, 0755); err != nil {
		t.Fatal(err)
	}
	if config.Dir() != hd {
		t.Fatalf("config.Dir() = %q, want %q", config.Dir(), hd)
	}
	return hd
}

func TestAppendAndLoad(t *testing.T) {
	withTempHome(t)
	if err := Append(&Lesson{Question: "what was the latest workout", Sources: []string{"workouts"}, ArtifactCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Append(&Lesson{Question: "aida commits", Sources: []string{"git-dev"}}); err != nil {
		t.Fatal(err)
	}
	all, err := LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 lessons, got %d", len(all))
	}
	if all[0].Sources[0] != "workouts" {
		t.Errorf("first lesson sources: %v", all[0].Sources)
	}
}

func TestRecoverableErrorRoundTrip(t *testing.T) {
	withTempHome(t)
	if err := Append(&Lesson{
		Question: "git fork distance",
		Sources:  []string{"excalidraw"},
		PerSourceStatus: map[string]Status{
			"excalidraw": StatusError,
		},
		RecoverableError: true,
	}); err != nil {
		t.Fatal(err)
	}
	all, err := LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want 1 lesson, got %d", len(all))
	}
	if !all[0].RecoverableError {
		t.Errorf("RecoverableError lost in roundtrip: %+v", all[0])
	}
}

func TestFindSimilarRanksByOverlap(t *testing.T) {
	withTempHome(t)
	mustAppend := func(q string, srcs ...string) {
		if err := Append(&Lesson{Question: q, Sources: srcs}); err != nil {
			t.Fatal(err)
		}
	}
	mustAppend("how much did I spend last month", "finances")
	mustAppend("what's the latest workout I did", "workouts")
	mustAppend("what was the last commit Ralph did", "git-dev")
	mustAppend("when did I work out yesterday", "workouts")

	got, err := FindSimilar("how much did I spend last week on groceries", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one similar lesson, got 0")
	}
	if got[0].Lesson.Sources[0] != "finances" {
		t.Errorf("expected best match to be the finances lesson, got %v score=%v", got[0].Lesson.Sources, got[0].Score)
	}
}

func TestFindSimilar_NoOverlapReturnsEmpty(t *testing.T) {
	withTempHome(t)
	if err := Append(&Lesson{Question: "aida's latest commit", Sources: []string{"git-dev"}}); err != nil {
		t.Fatal(err)
	}
	got, err := FindSimilar("how is the weather today", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected no similar lessons, got %d", len(got))
	}
}

func TestFindByRunIDReturnsLatest(t *testing.T) {
	withTempHome(t)
	_ = Append(&Lesson{RunID: "abc", Question: "old", Sources: []string{"x"}})
	_ = Append(&Lesson{RunID: "abc", Question: "newer", Sources: []string{"y"}})
	l, err := FindByRunID("abc")
	if err != nil || l == nil {
		t.Fatalf("FindByRunID: l=%v err=%v", l, err)
	}
	if l.Question != "newer" {
		t.Errorf("expected newest match, got %q", l.Question)
	}
}

func TestTokenizeDropsStopWords(t *testing.T) {
	got := tokenize("How much did I spend on groceries last month?")
	for _, w := range []string{"how", "much", "did", "i"} {
		for _, g := range got {
			if g == w {
				t.Errorf("expected stop word %q to be dropped, got %v", w, got)
			}
		}
	}
	wantContain := []string{"spend", "groceries", "last", "month"}
	for _, w := range wantContain {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected token %q to be kept, got %v", w, got)
		}
	}
}
