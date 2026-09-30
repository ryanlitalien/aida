package taskstate

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Acceptance is what "done" means for a task (docs/arbiter-plan.md
// section 6, decision 2): the named deliverables must exist on disk and
// every Checks command must exit 0. A model saying "DONE" is a claim,
// not a fact -- this is what turns the claim into a verified fact.
type Acceptance struct {
	Checks       []string `json:"checks,omitempty"`
	Deliverables []string `json:"deliverables,omitempty"`
}

// Problem describes one deliverable that failed verification.
type Problem struct {
	Deliverable string `json:"deliverable"`
	Reason      string `json:"reason"`
}

var deliverableHeadingRE = regexp.MustCompile(`(?i)^#{1,6}\s*(.+?)\s*$`)

// deliverableSectionNames are the heading texts (case-insensitive, after
// trimming trailing punctuation like a colon) that mark a deliverables
// list in a task body.
var deliverableSectionNames = map[string]bool{
	"deliverables":         true,
	"acceptance":           true,
	"acceptance criteria":  true,
	"acceptance criterion": true,
}

// bulletPrefixRE strips a leading list marker ("- ", "* ", "1. ", etc).
var bulletPrefixRE = regexp.MustCompile(`^\s*(?:[-*]|\d+[.)])\s+`)

// looksLikePath is a conservative heuristic: an entry is kept as a
// deliverable only if it contains a path separator or a dotted extension,
// and has no whitespace (a prose sentence never survives this).
func looksLikePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t") {
		return false
	}
	if strings.Contains(s, "/") {
		return true
	}
	// A dotted extension: a `.` followed by 1-8 word characters, not at
	// the very start (so ".gitignore"-style dotfiles alone still count,
	// since Index looks anywhere in the string).
	return regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`).MatchString(s)
}

// ParseDeliverables scans a task's markdown body for a heading whose text
// (case-insensitive, trailing punctuation ignored) is "Deliverables" or
// one of the "Acceptance..." variants, and collects path-shaped entries
// from the bullet or plain-text lines under it, up to the next heading.
// Backticks and trailing punctuation are stripped; duplicates are
// dropped, preserving first-seen order.
func ParseDeliverables(taskBody string) []string {
	lines := strings.Split(taskBody, "\n")
	var out []string
	seen := make(map[string]bool)
	inSection := false

	for _, line := range lines {
		if m := deliverableHeadingRE.FindStringSubmatch(line); m != nil {
			heading := strings.ToLower(strings.TrimRight(m[1], ":"))
			inSection = deliverableSectionNames[heading]
			continue
		}
		if !inSection {
			continue
		}
		entry := bulletPrefixRE.ReplaceAllString(line, "")
		entry = strings.TrimSpace(entry)
		entry = strings.ReplaceAll(entry, "`", "")
		entry = strings.TrimRight(entry, ".,;:")
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if !looksLikePath(entry) {
			continue
		}
		if seen[entry] {
			continue
		}
		seen[entry] = true
		out = append(out, entry)
	}
	return out
}

// VerifyDeliverables checks that every deliverable exists relative to
// workDir. A deliverable may be a glob (filepath.Glob); zero matches is a
// Problem. A deliverable that resolves to a directory when the entry
// looks like a file path (has a dotted extension) is also a Problem, as
// is one that exists but can't be read. Deliverables that already look
// like directories (trailing slash, or no extension) are accepted as
// directories.
func VerifyDeliverables(workDir string, deliverables []string) []Problem {
	var problems []Problem
	for _, d := range deliverables {
		pattern := d
		if !filepath.IsAbs(pattern) {
			pattern = filepath.Join(workDir, pattern)
		}

		matches, err := filepath.Glob(pattern)
		if err != nil {
			problems = append(problems, Problem{Deliverable: d, Reason: "invalid glob: " + err.Error()})
			continue
		}
		if len(matches) == 0 {
			problems = append(problems, Problem{Deliverable: d, Reason: "missing"})
			continue
		}

		expectFile := looksLikeFileNotDir(d)
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				problems = append(problems, Problem{Deliverable: d, Reason: "unreadable: " + err.Error()})
				continue
			}
			if expectFile && info.IsDir() {
				problems = append(problems, Problem{Deliverable: d, Reason: "is a directory, expected a file"})
			}
		}
	}
	return problems
}

// looksLikeFileNotDir says whether a deliverable string names a file
// (has a dotted extension and no trailing slash) as opposed to a
// directory.
func looksLikeFileNotDir(d string) bool {
	if strings.HasSuffix(d, "/") {
		return false
	}
	return regexp.MustCompile(`\.[A-Za-z0-9]{1,8}$`).MatchString(d)
}
