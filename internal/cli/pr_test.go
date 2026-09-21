package cli

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/brain"
)

func TestExtractPRURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/ryanlitalien/aida/pull/83":                 "https://github.com/ryanlitalien/aida/pull/83",
		"Warning: foo\nhttps://github.com/ryanlitalien/aida/pull/84\n": "https://github.com/ryanlitalien/aida/pull/84",
		"Creating pull request for auto/x into main\n.../pull/85":      ".../pull/85",
		"no url here": "no url here",
	}
	for in, want := range cases {
		if got := extractPRURL(in); got != want {
			t.Errorf("extractPRURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrDefault(t *testing.T) {
	if orDefault("", "main") != "main" {
		t.Errorf("empty should yield default")
	}
	if orDefault("dev", "main") != "dev" {
		t.Errorf("non-empty should pass through")
	}
}

// TestReviewMirrorRefusal pins the deliberate --pr refusal: a repo whose base
// branch tracks a public GitHub remote while a non-GitHub mirror remote also
// exists (review happens on the mirror first) must fail fast, naming both
// remotes, instead of pushing straight to GitHub. Ordinary layouts pass.
func TestReviewMirrorRefusal(t *testing.T) {
	tests := []struct {
		name      string
		remotes   map[string]string
		remote    string
		wantErr   bool
		wantWords []string
	}{
		{
			name:    "origin only on github",
			remotes: map[string]string{"origin": "git@github.com:acme/x.git"},
			remote:  "origin",
		},
		{
			name:    "all remotes on github",
			remotes: map[string]string{"public": "git@github.com:acme/x.git", "archive": "https://github.com/acme/x-old.git"},
			remote:  "public",
		},
		{
			name:    "resolved remote is the non-github mirror",
			remotes: map[string]string{"origin": "forge:ryan/x.git", "public": "git@github.com:acme/x.git"},
			remote:  "origin",
		},
		{
			name:    "no remotes",
			remotes: map[string]string{},
			remote:  "origin",
		},
		{
			name:      "public github upstream with a forge mirror",
			remotes:   map[string]string{"origin": "forge:ryan/x.git", "public": "git@github.com:acme/x.git", "archive": "git@github.com:acme/x-old.git"},
			remote:    "public",
			wantErr:   true,
			wantWords: []string{"review-mirror", "--pr is not supported", `"public"`, `"origin"`, "forge:ryan/x.git", "git@github.com:acme/x.git"},
		},
		{
			name:      "https github upstream with a self-hosted mirror",
			remotes:   map[string]string{"upstream": "https://github.com/acme/x.git", "mirror": "ssh://git@forge.example.com/acme/x.git"},
			remote:    "upstream",
			wantErr:   true,
			wantWords: []string{`"upstream"`, `"mirror"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := gitTestRepo(t, tc.remotes, "")
			err := reviewMirrorRefusal(dir, tc.remote)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reviewMirrorRefusal = %v, wantErr %v", err, tc.wantErr)
			}
			for _, w := range tc.wantWords {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error should mention %q; got: %s", w, err)
				}
			}
		})
	}
}

// TestOpenTaskPRRefusesBeforePush checks the refusal fires before any push:
// the resolved remote is a fake github.com URL that would fail (or hang) if
// contacted, so a fast, refusal-shaped error is the only acceptable outcome.
func TestOpenTaskPRRefusesBeforePush(t *testing.T) {
	dir := gitTestRepo(t, map[string]string{"origin": "forge:ryan/x.git", "public": "git@github.com:acme/x.git"}, "")
	task := &brain.TaskRecord{TaskID: 7, Title: "x"}
	_, err := openTaskPR(dir, "auto/loop-7-x", task, "", "main", "public")
	if err == nil || !strings.Contains(err.Error(), "review-mirror") {
		t.Fatalf("expected the review-mirror refusal, got %v", err)
	}
}
