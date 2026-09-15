package fleet

import (
	"regexp"
	"strconv"
)

// exitLineRe matches the completion marker a lane 4 launcher script
// appends to its own log after the agent process exits, e.g.
// `~/dev/aida-agents/scripts/lane4-scarlett-978.sh`'s
// `echo "EXIT=$? $(date -Is)" | tee -a <log>`. Group 1 is the numeric
// exit code, group 2 is the `date -Is` timestamp.
var exitLineRe = regexp.MustCompile(`^EXIT=(\d+) (\S+)$`)

// ParseExitLine reports whether lastLine is a lane 4 completion marker,
// returning its exit code and timestamp when it is. lastLine is expected
// to already be a single line (no embedded newline) with surrounding
// whitespace trimmed by the caller -- this function only matches, it
// does not scan a multi-line log itself.
func ParseExitLine(lastLine string) (code int, timestamp string, ok bool) {
	m := exitLineRe.FindStringSubmatch(lastLine)
	if m == nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		// \d+ only matches digits, so this is unreachable in practice
		// (an exit code that overflows int would need to be absurd),
		// but treat it as "not a match" rather than panic either way.
		return 0, "", false
	}
	return n, m[2], true
}

// prURLRe finds a GitHub pull-request URL: https://github.com/<owner>/<repo>/pull/<n>.
// Owner/repo segments exclude '/' and whitespace so the match stops at the
// URL's natural boundary even when it's embedded in prose with no
// surrounding punctuation.
var prURLRe = regexp.MustCompile(`https://github\.com/[^/\s]+/[^/\s]+/pull/\d+`)

// ExtractPRURL returns the first GitHub pull-request URL found in text,
// or "" if none is present. When a report contains more than one PR link
// (e.g. a superseded draft mentioned in passing), the first one wins --
// callers should have the agent lead its report with the PR that matters.
func ExtractPRURL(text string) string {
	return prURLRe.FindString(text)
}

// prRefRe finds an issue-style PR reference: "PR #1617", "pr#1617",
// or "pull request #1617", case-insensitive. Word-bounded on the left
// so it doesn't fire inside an unrelated word ("APR #5" is not a PR
// reference); \s* between the keyword and '#' accepts both "PR #123"
// and "PR#123".
var prRefRe = regexp.MustCompile(`(?i)\b(?:pull request|pr)\b\s*#(\d+)`)

// ExtractPRRef returns the first issue-style PR number found in text
// ("PR #1617", "pull request #1617"), or (0, false) if none is
// present. Used as a fallback when a lane 4 report names the PR by
// number without a full GitHub URL -- ExtractPRURL alone can't
// recover a URL from that, so a caller that knows the repo (--repo)
// can combine this with it.
func ExtractPRRef(text string) (int, bool) {
	m := prRefRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		// \d+ only matches digits; see ParseExitLine's identical guard.
		return 0, false
	}
	return n, true
}
