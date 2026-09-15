package cli

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseSinceFilter(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
		check   func(time.Time) bool
	}{
		{"", false, func(t time.Time) bool { return t.IsZero() }},
		{"2026-05-01", false, func(t time.Time) bool { return t.Year() == 2026 && t.Month() == 5 }},
		{"24h", false, func(t time.Time) bool { return time.Since(t) > 23*time.Hour && time.Since(t) < 25*time.Hour }},
		{"7d", false, func(t time.Time) bool { return time.Since(t) > 6*24*time.Hour && time.Since(t) < 8*24*time.Hour }},
		{"30m", false, func(t time.Time) bool { return time.Since(t) > 29*time.Minute && time.Since(t) < 31*time.Minute }},
		{"garbage", true, nil},
	}
	for _, c := range cases {
		got, err := parseSinceFilter(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseSinceFilter(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.check != nil && !c.check(got) {
			t.Errorf("parseSinceFilter(%q) returned unexpected time %v", c.in, got)
		}
	}
}

func TestStateMarker(t *testing.T) {
	if stateMarker("queued") != "·" {
		t.Error("queued marker wrong")
	}
	if stateMarker("done") != "✓" {
		t.Error("done marker wrong")
	}
	if stateMarker("failed") != "✗" {
		t.Error("failed marker wrong")
	}
	if stateMarker("running") != "→" {
		t.Error("running marker wrong")
	}
	if stateMarker("incomplete") != "⚠" {
		t.Error("incomplete marker wrong")
	}
	if stateMarker("garbage") != "?" {
		t.Error("unknown marker wrong")
	}
}

func TestListProfileDirsEmpty(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	got, err := listProfileDirs()
	if err != nil {
		t.Fatalf("listProfileDirs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

func TestListProfileDirsAfterOpen(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	// Create two profile dirs by importing the jobs package and Open'ing.
	// We avoid importing here directly because of test cycle concerns;
	// stub the dirs by hand instead. The structure is: <home>/.aida/jobs/<name>/
	for _, name := range []string{"work", "home"} {
		os.MkdirAll(strings.Join([]string{tmp, ".aida", "jobs", name}, "/"), 0755)
	}

	got, err := listProfileDirs()
	if err != nil {
		t.Fatalf("listProfileDirs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 profiles, got %v", got)
	}
	// Sorted.
	if got[0] != "home" || got[1] != "work" {
		t.Errorf("expected [home work], got %v", got)
	}
}

func TestParseDurationCutoff(t *testing.T) {
	if _, err := parseDurationCutoff("garbage"); err == nil {
		t.Error("expected error on garbage")
	}
	if _, err := parseDurationCutoff(""); err == nil {
		t.Error("expected error on empty (zero time)")
	}
	if got, err := parseDurationCutoff("30d"); err != nil {
		t.Errorf("parseDurationCutoff(30d): %v", err)
	} else if time.Since(got) < 29*24*time.Hour {
		t.Errorf("30d cutoff seems wrong: %v", got)
	}
}
