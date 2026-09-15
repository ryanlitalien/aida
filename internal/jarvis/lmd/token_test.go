package lmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeToken covers the Crockford base32 look-alike traps a
// hand-typed token can hit: O for 0, I (and L) for 1, stray casing, stray
// hyphens, and the "LMD-" human-readability prefix. All of these must
// normalize down to the same canonical form so authenticate() still
// accepts them.
func TestNormalizeToken(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical body", "3K7H2", "3K7H2"},
		{"lowercase", "lmd-3k7h2-9qxzp-4rtvw-2h9jk-5m3n7", "3K7H29QXZP4RTVW2H9JK5M3N7"},
		{"uppercase with hyphens and prefix", "LMD-3K7H2-9QXZP-4RTVW-2H9JK-5M3N7", "3K7H29QXZP4RTVW2H9JK5M3N7"},
		{"no prefix, no hyphens", "3K7H29QXZP4RTVW2H9JK5M3N7", "3K7H29QXZP4RTVW2H9JK5M3N7"},
		// Look-alike typos: a human copying a token off a screen mistypes
		// the digits 0/1 as the excluded letters O/I/L.
		{"O typed for 0", "3K7HO", "3K7H0"},
		{"I typed for 1", "3K7HI", "3K7H1"},
		{"L typed for 1", "3K7HL", "3K7H1"},
		{"mixed case and whitespace", "  Lmd-3k7H2-9qxZp-4rtVw-2H9jk-5m3N7  ", "3K7H29QXZP4RTVW2H9JK5M3N7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeToken(c.in); got != c.want {
				t.Errorf("normalizeToken(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestNormalizeToken_LookAlikeTyposStillAuthenticate is the end-to-end
// version of the look-alike cases above: a stored token containing 0/1 must
// still match a presented token where the human typed O/I/L instead, while
// a genuinely different token must not.
func TestNormalizeToken_LookAlikeTyposStillAuthenticate(t *testing.T) {
	stored := "LMD-3K7H0-9QXZP-1RTVW-2H9JK-5M3N1" // contains 0s and 1s
	typedWithLookAlikes := "lmd-3k7ho-9qxzp-irtvw-2h9jk-5m3nl"

	if !constantTimeEqual(normalizeToken(stored), normalizeToken(typedWithLookAlikes)) {
		t.Error("a token typed with O/I/L instead of 0/1 should still authenticate")
	}

	wrong := "LMD-00000-00000-00000-00000-00000"
	if constantTimeEqual(normalizeToken(stored), normalizeToken(wrong)) {
		t.Error("a genuinely wrong token must not authenticate")
	}
}

func TestConstantTimeEqual(t *testing.T) {
	if !constantTimeEqual("abc123", "abc123") {
		t.Error("identical strings should compare equal")
	}
	if constantTimeEqual("abc123", "abc124") {
		t.Error("differing strings should not compare equal")
	}
	if constantTimeEqual("abc123", "abc12") {
		t.Error("differing lengths should not compare equal")
	}
}

// TestGenerateToken checks the wire format from docs/lmd-protocol.md:
// "LMD-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX", 25 Crockford base32 characters
// across 5 hyphen-groups, and that two calls don't collide.
func TestGenerateToken(t *testing.T) {
	tok, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if !strings.HasPrefix(tok, "LMD-") {
		t.Fatalf("token %q should start with LMD-", tok)
	}
	groups := strings.Split(strings.TrimPrefix(tok, "LMD-"), "-")
	if len(groups) != tokenGroups {
		t.Fatalf("expected %d groups, got %d: %v", tokenGroups, len(groups), groups)
	}
	for _, g := range groups {
		if len(g) != tokenGroupLen {
			t.Errorf("group %q: len = %d, want %d", g, len(g), tokenGroupLen)
		}
		for _, ch := range g {
			if !strings.ContainsRune(crockfordAlphabet, ch) {
				t.Errorf("group %q contains non-Crockford character %q", g, ch)
			}
		}
	}

	tok2, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken (2nd): %v", err)
	}
	if tok == tok2 {
		t.Error("two generated tokens collided - rand.Read broken?")
	}
}

// TestLoadOrGenerateToken_EnvTakesPrecedence covers the documented
// precedence: AIDA_LMD_TOKEN wins over the on-disk file even when a file
// already exists.
func TestLoadOrGenerateToken_EnvTakesPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIDA_LMD_TOKEN", "LMD-ENVTK-ENENV-ENVEN-VENVE-NVENV")

	got, err := LoadOrGenerateToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateToken: %v", err)
	}
	if got != "LMD-ENVTK-ENENV-ENVEN-VENVE-NVENV" {
		t.Errorf("got %q, want the env var value verbatim", got)
	}
}

// TestLoadOrGenerateToken_GeneratesAndPersists covers the no-env, no-file
// path: a token is generated, written to ~/.aida/lmd/token with mode 0600,
// and a second call reuses the same on-disk value instead of generating
// another one.
func TestLoadOrGenerateToken_GeneratesAndPersists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	first, err := LoadOrGenerateToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateToken: %v", err)
	}
	if first == "" {
		t.Fatal("expected a non-empty generated token")
	}

	path := filepath.Join(home, ".aida", "lmd", "token")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 0600", perm)
	}

	second, err := LoadOrGenerateToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateToken (2nd): %v", err)
	}
	if second != first {
		t.Errorf("second call should reuse the persisted token: got %q, want %q", second, first)
	}
}

// TestRotateToken_ReplacesPersistedToken covers `aida lmd token --rotate`:
// it must overwrite the on-disk token with a fresh one, and a subsequent
// LoadOrGenerateToken must pick up the new value.
func TestRotateToken_ReplacesPersistedToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	original, err := LoadOrGenerateToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateToken: %v", err)
	}

	rotated, err := RotateToken()
	if err != nil {
		t.Fatalf("RotateToken: %v", err)
	}
	if rotated == original {
		t.Error("RotateToken should generate a token different from the original")
	}

	reloaded, err := LoadOrGenerateToken()
	if err != nil {
		t.Fatalf("LoadOrGenerateToken after rotate: %v", err)
	}
	if reloaded != rotated {
		t.Errorf("LoadOrGenerateToken after rotate = %q, want the rotated value %q", reloaded, rotated)
	}
}
