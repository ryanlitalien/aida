package lmd

// LMD bearer token: generation, on-disk persistence, and the normalization
// that lets a hand-typed token forgive Crockford base32's classic look-alike
// typos. See docs/lmd-protocol.md's "### Token" section for the wire spec
// this file implements exactly.

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// crockfordAlphabet is Crockford's base32 alphabet: the 10 digits plus 22 of
// the 26 letters, excluding I, L, O, and U - each one either looks like a
// digit (I/l → 1, O → 0) or, combined with adjacent letters, spells
// something better avoided (U). 32 symbols == 2^5, so each character
// encodes exactly 5 bits.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// tokenGroups and tokenGroupLen produce the "LMD-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX"
// shape: 5 groups of 5 characters == 25 characters == 125 bits of entropy
// (25 * 5 bits/char), comfortably past brute-forceable while still short
// enough to read off a terminal and thumb into a phone.
const (
	tokenGroups   = 5
	tokenGroupLen = 5
)

// tokenPath returns ~/.aida/lmd/token, the on-disk fallback location for the
// LMD bearer token.
func tokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aida", "lmd", "token"), nil
}

// generateToken draws tokenGroups*tokenGroupLen random bytes from
// crypto/rand and maps each one (masked to 5 bits) onto the Crockford
// alphabet, formatted as "LMD-XXXXX-XXXXX-XXXXX-XXXXX-XXXXX". Masking each
// byte to its low 5 bits and discarding the rest costs nothing but a few
// extra bytes of /dev/urandom - every character stays uniform over all 32
// symbols and independent of its neighbors, so the full 125 bits of entropy
// survive.
func generateToken() (string, error) {
	raw := make([]byte, tokenGroups*tokenGroupLen)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("LMD")
	for g := 0; g < tokenGroups; g++ {
		b.WriteByte('-')
		for i := 0; i < tokenGroupLen; i++ {
			b.WriteByte(crockfordAlphabet[raw[g*tokenGroupLen+i]&0x1F])
		}
	}
	return b.String(), nil
}

// LoadOrGenerateToken resolves the LMD bearer token: $AIDA_LMD_TOKEN first,
// then ~/.aida/lmd/token. If neither exists, it generates a fresh token (see
// generateToken), writes it to ~/.aida/lmd/token with mode 0600, and logs it
// once to stderr so it can be typed into the Android app. Every subsequent
// `aida serve` (or `aida lmd token`) reuses the same on-disk token silently.
func LoadOrGenerateToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("AIDA_LMD_TOKEN")); t != "" {
		return t, nil
	}
	path, err := tokenPath()
	if err != nil {
		return "", fmt.Errorf("lmd: resolve token path: %w", err)
	}
	if data, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(data)); t != "" {
			return t, nil
		}
	}
	return generateAndPersistToken(path)
}

// RotateToken unconditionally generates a new token and overwrites
// ~/.aida/lmd/token with it, invalidating whatever token the phone currently
// holds. Used by `aida lmd token --rotate`. Note: if $AIDA_LMD_TOKEN is set
// in the environment `aida serve` runs under, that env var still wins at
// startup per LoadOrGenerateToken's precedence - rotating only replaces the
// file-backed fallback.
func RotateToken() (string, error) {
	path, err := tokenPath()
	if err != nil {
		return "", fmt.Errorf("lmd: resolve token path: %w", err)
	}
	return generateAndPersistToken(path)
}

// generateAndPersistToken is the shared body of LoadOrGenerateToken's
// no-file-yet path and RotateToken's always-regenerate path.
func generateAndPersistToken(path string) (string, error) {
	tok, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("lmd: generate token: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("lmd: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(tok), 0o600); err != nil {
		return "", fmt.Errorf("lmd: write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "🔑 LMD token saved to %s - enter this into the Android app: %s\n", path, tok)
	return tok, nil
}

// normalizeToken uppercases, strips hyphens and an optional leading "LMD"
// human-readability prefix, then maps Crockford base32's excluded
// look-alike characters (I, L, O - easily mistyped for 1, 1, 0) onto their
// intended digit. Applied to BOTH the stored token and the one presented in
// the Authorization header before comparison (see (*Server).authenticate),
// so a hand-typed token that hit one of those traps - or a token pasted into
// AIDA_LMD_TOKEN with stray casing or hyphens - still authenticates. The
// "LMD" prefix must be stripped BEFORE the look-alike mapping runs, or
// mapping L→1 would turn it into "1MD" and the TrimPrefix below would miss
// it.
func normalizeToken(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.TrimPrefix(s, "LMD")
	return strings.NewReplacer("I", "1", "L", "1", "O", "0").Replace(s)
}

// constantTimeEqual reports whether a and b are equal without leaking
// timing information about where they first differ. Both sides should
// already be normalizeToken'd by the caller.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
