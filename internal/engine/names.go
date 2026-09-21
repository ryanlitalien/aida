package engine

import "strings"

// This file contains string helpers shared by the planner (rankSources)
// and the router (LLMRoute) for matching source names against entities
// extracted from the user's question. Both call sites need the same
// normalization rules so a name match scored in the planner is also
// recognized by the router's top-up.

// normalizeSourceToken lowercases the input and replaces underscores
// with hyphens so "Mcp_Perforce" and "mcp-perforce" both compare equal.
// Leading and trailing whitespace and surrounding punctuation are
// trimmed. Internal punctuation is preserved so a slug like
// "csv-viewer" survives intact.
func normalizeSourceToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	// Strip surrounding non-alnum characters but leave internal hyphens.
	s = strings.TrimFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	return s
}

// tokenizeEntity splits a normalized entity into atomic tokens by hyphen
// and space. CamelCase splitting is handled at scan time (see
// splitNameTokens in internal/library/scan.go) because the source name
// has already been slug-lowercased by the time the planner sees it.
func tokenizeEntity(normalized string) []string {
	if normalized == "" {
		return nil
	}
	parts := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '-' || r == ' '
	})
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// genericNameTokens are noise tokens that appear in many source names
// without carrying topic signal. We refuse to register them as entities
// or as match targets to avoid false-positive routing (e.g. a question
// about "the tool I use" should not match every source whose name ends
// in "-tool"). Keep this list short and high-signal.
var genericNameTokens = map[string]bool{
	"main":  true,
	"core":  true,
	"base":  true,
	"util":  true,
	"utils": true,
	"test":  true,
	"tests": true,
	"data":  true,
	"docs":  true,
	"tool":  true,
	"tools": true,
	"lib":   true,
	"libs":  true,
	"app":   true,
	"apps":  true,
	"src":   true,
}

// compactToken strips hyphens so "acme-widgets" and "acmewidgets"
// compare equal. Used as a fallback when the hyphenated comparison
// fails - handles the common case where users omit separators.
func compactToken(s string) string {
	return strings.ReplaceAll(s, "-", "")
}

// isGenericNameToken returns true if tok is on the noise blocklist.
// Callers should also enforce a minimum length (typically >= 4) to drop
// short tokens like "cli", "api", "mcp" that would cause false positives.
func isGenericNameToken(tok string) bool {
	return genericNameTokens[strings.ToLower(tok)]
}
