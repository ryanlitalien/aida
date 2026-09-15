// Package refusal detects LLM/sub-agent refusal prose ("I cannot build a
// query for...", "I'm unable to access...") so callers can treat it as a
// failure instead of a citable result.
//
// It lives in its own leaf package (rather than internal/engine, where the
// query-construction refusal check is used) because internal/engine already
// imports internal/sources -- and internal/sources needs this same check for
// the claude-project sub-agent's stdout. Putting it in engine would create
// an import cycle; putting it here lets both engine and sources depend on
// it without either depending on the other.
package refusal

import "strings"

// RefusalMarkers is the BROAD vocabulary of refusal/uselessness phrases,
// for classifying PROSE: synthesized final answers
// (internal/cli.answerIsUseless) and claude-project sub-agent stdout
// (internal/sources.subagentRefusal). Prose that opens with a bare
// "I don't know" or "I can't ..." genuinely is a refusal/useless answer,
// so aggressive markers are correct in those contexts.
//
// Do NOT use this list to classify LLM-GENERATED COMMANDS: a generated
// web-search query that echoes the user's own phrasing (question "why
// can't I focus in the mornings" -> command "why i can't focus in the
// mornings") has "i can't" right at the head of the string and would be
// misclassified. Command classification uses CommandRefusalMarkers via
// LooksLikeCommandRefusal.
//
// It's a superset of two originally-separate lists:
//
//   - The narrower "useless final answer" markers used by
//     internal/cli.answerIsUseless to keep a synthesized answer that
//     amounts to "I don't know" from poisoning future routing lessons.
//   - The refusal-specific markers ("cannot build a query", "no
//     applicable", ...) that catch LLM Call #2 emitting apology prose
//     instead of a command, and a delegated claude-project sub-agent
//     declining instead of answering.
//
// Keeping one merged, exported list means both consumers gain coverage
// from either origin without duplicating strings. Order doesn't matter --
// callers do a substring scan over the whole list.
var RefusalMarkers = []string{
	"i don't have access",
	"i do not have access",
	"i don't have information",
	"i do not have information",
	"i cannot find",
	"i can't find",
	"i cannot answer",
	"i can't answer",
	"i don't know",
	"the search returned no",
	"no results were found",
	"no data is available",
	"not enough information",
	"the available data source only",
	"the data source available to me only",
	"i cannot",
	"i can't",
	"i am unable",
	"i'm unable",
	"cannot build a query",
	"no applicable",
}

// CommandRefusalMarkers is the NARROW vocabulary for classifying
// LLM-GENERATED COMMANDS (LLM Call #2 output in the engine executor).
// Every marker is verb-anchored or otherwise unambiguous, so a search
// query that echoes the user's phrasing ("why i can't focus in the
// mornings", "why can't i connect to the vpn") never matches. Bare
// "i cannot" / "i can't" / "i don't know" are deliberately absent -- those
// belong to RefusalMarkers for prose contexts only.
var CommandRefusalMarkers = []string{
	"cannot build a query",
	"can't build a query",
	"cannot construct",
	"cannot generate",
	"i cannot build",
	"i can't build",
	"i cannot answer",
	"i can't answer",
	"i cannot query",
	"i am unable",
	"i'm unable",
	"i don't have access",
	"i do not have access",
	"no applicable",
}

// headWindow bounds how much of the head of a string LooksLikeRefusal scans
// for a marker. Refusal prose front-loads its disclaimer ("I cannot ..."),
// so checking only the first sentence (or, failing that, the first ~120
// characters) is enough to catch it while staying conservative -- a real
// command is short, and even a long one is very unlikely to open with one
// of these phrases.
const headWindow = 120

// LooksLikeRefusal reports whether s opens with a refusal marker from the
// BROAD RefusalMarkers list. Use for PROSE (sub-agent stdout, synthesized
// answers) -- see LooksLikeCommandRefusal for LLM-generated commands. The
// check is case-insensitive and scoped to the head of s (first sentence,
// capped at headWindow characters) so it stays conservative: a legitimate
// SQL query or shell command could in principle contain "cannot" somewhere
// deep in a string literal, but it won't appear in the first ~120
// characters.
func LooksLikeRefusal(s string) bool {
	return headMatchesAny(s, RefusalMarkers)
}

// LooksLikeCommandRefusal reports whether s -- an LLM-generated command --
// opens with a marker from the NARROW CommandRefusalMarkers list. Same
// head-window behavior as LooksLikeRefusal, but the verb-anchored marker
// list won't false-positive on a search query that echoes the user's own
// phrasing (e.g. "why i can't focus in the mornings").
func LooksLikeCommandRefusal(s string) bool {
	return headMatchesAny(s, CommandRefusalMarkers)
}

// headMatchesAny is the shared engine behind both classifiers: scan the
// head of s (first sentence, capped at headWindow chars) for any marker,
// case-insensitively.
func headMatchesAny(s string, markers []string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	head := s
	if i := strings.IndexAny(head, ".!?\n"); i >= 0 && i < headWindow {
		head = head[:i+1]
	} else if len(head) > headWindow {
		head = head[:headWindow]
	}
	head = strings.ToLower(head)
	for _, marker := range markers {
		if strings.Contains(head, marker) {
			return true
		}
	}
	return false
}

// firstLineMaxLen bounds FirstLine's output so a one-line status Summary
// field doesn't balloon into a full refusal essay.
const firstLineMaxLen = 200

// FirstLine returns the first line of s (up to a newline), trimmed and
// truncated to a length reasonable for a one-line status Summary. Used to
// build a short "why did this fail" message out of a refusal that may
// otherwise span multiple sentences or paragraphs.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > firstLineMaxLen {
		s = strings.TrimSpace(s[:firstLineMaxLen-3]) + "..."
	}
	return s
}
