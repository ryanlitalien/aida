package cli

import "testing"

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
