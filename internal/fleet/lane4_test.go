package fleet

import "testing"

func TestParseExitLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantCode int
		wantTS   string
		wantOK   bool
	}{
		{
			name:     "exit 0 with iso timestamp",
			line:     "EXIT=0 2026-09-02T16:06:39-04:00",
			wantCode: 0,
			wantTS:   "2026-09-02T16:06:39-04:00",
			wantOK:   true,
		},
		{
			name:     "exit 1",
			line:     "EXIT=1 2026-09-02T16:06:39-04:00",
			wantCode: 1,
			wantTS:   "2026-09-02T16:06:39-04:00",
			wantOK:   true,
		},
		{
			name:     "multi-digit exit code",
			line:     "EXIT=137 2026-09-02T16:06:39Z",
			wantCode: 137,
			wantTS:   "2026-09-02T16:06:39Z",
			wantOK:   true,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "not an exit line",
			line:   "some agent report text",
			wantOK: false,
		},
		{
			name:   "exit with no timestamp",
			line:   "EXIT=0",
			wantOK: false,
		},
		{
			name:   "exit with non-numeric code",
			line:   "EXIT=abc 2026-09-02T16:06:39-04:00",
			wantOK: false,
		},
		{
			name:   "lowercase exit keyword doesn't match",
			line:   "exit=0 2026-09-02T16:06:39-04:00",
			wantOK: false,
		},
		{
			name:   "leading whitespace breaks the anchor",
			line:   " EXIT=0 2026-09-02T16:06:39-04:00",
			wantOK: false,
		},
		{
			name:   "trailing whitespace breaks the timestamp token",
			line:   "EXIT=0 2026-09-02T16:06:39-04:00 ",
			wantOK: false,
		},
		{
			name:   "timestamp with embedded space doesn't match \\S+",
			line:   "EXIT=0 2026-09-02 16:06:39",
			wantOK: false,
		},
		{
			name:   "EXIT= line embedded mid-string",
			line:   "prefix EXIT=0 2026-09-02T16:06:39-04:00",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, ts, ok := ParseExitLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ParseExitLine(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if code != tt.wantCode {
				t.Errorf("ParseExitLine(%q) code = %d, want %d", tt.line, code, tt.wantCode)
			}
			if ts != tt.wantTS {
				t.Errorf("ParseExitLine(%q) ts = %q, want %q", tt.line, ts, tt.wantTS)
			}
		})
	}
}

func TestExtractPRURL(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			name: "no url",
			text: "the run finished but did not open a PR",
			want: "",
		},
		{
			name: "empty text",
			text: "",
			want: "",
		},
		{
			name: "bare url",
			text: "https://github.com/ButterStack/butter_stack/pull/1617",
			want: "https://github.com/ButterStack/butter_stack/pull/1617",
		},
		{
			name: "url embedded in a report",
			text: "Tracer bullet done.\n\nPR: https://github.com/ButterStack/butter_stack/pull/1617\n\nTests green.",
			want: "https://github.com/ButterStack/butter_stack/pull/1617",
		},
		{
			name: "url in parentheses -- trailing paren is not part of the path",
			text: "see the PR (https://github.com/owner/repo/pull/42) for details",
			want: "https://github.com/owner/repo/pull/42",
		},
		{
			name: "first of two urls wins",
			text: "superseded https://github.com/owner/repo/pull/40, actually https://github.com/owner/repo/pull/41",
			want: "https://github.com/owner/repo/pull/40",
		},
		{
			name: "issue url is not a pull url",
			text: "closes https://github.com/owner/repo/issues/9, no PR yet",
			want: "",
		},
		{
			name: "non-github pull-shaped url is not matched",
			text: "https://gitlab.com/owner/repo/pull/9",
			want: "",
		},
		{
			name: "owner or repo with a dash and dot",
			text: "https://github.com/Ryan-Litalien/aida-agents.go/pull/9",
			want: "https://github.com/Ryan-Litalien/aida-agents.go/pull/9",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractPRURL(tt.text); got != tt.want {
				t.Errorf("ExtractPRURL(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

func TestExtractPRRef(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		wantN  int
		wantOK bool
	}{
		{
			name:   "PR with hash and space",
			text:   "Review + merge PR #1617",
			wantN:  1617,
			wantOK: true,
		},
		{
			name:   "no space between PR and hash",
			text:   "see PR#1617 for details",
			wantN:  1617,
			wantOK: true,
		},
		{
			name:   "pull request phrasing",
			text:   "opened pull request #42 against main",
			wantN:  42,
			wantOK: true,
		},
		{
			name:   "case-insensitive keyword",
			text:   "pr #99 is ready, PULL REQUEST #5 is not",
			wantN:  99,
			wantOK: true,
		},
		{
			name:   "heading with arrow before PR",
			text:   "## Issue #978 → PR #1617, CI GREEN",
			wantN:  1617,
			wantOK: true,
		},
		{
			name:   "issue number alone does not match",
			text:   "Closes #978, no PR reference here",
			wantOK: false,
		},
		{
			name:   "word boundary excludes embedded pr substring",
			text:   "APR #5 is a month, not a pull request",
			wantOK: false,
		},
		{
			name:   "first of two PR refs wins",
			text:   "Root cause: PR #962 rewrote docs. Review + merge PR #1617.",
			wantN:  962,
			wantOK: true,
		},
		{
			name:   "empty text",
			text:   "",
			wantOK: false,
		},
		{
			name:   "full url present but no issue-style ref",
			text:   "https://github.com/ButterStack/butter_stack/pull/1617",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := ExtractPRRef(tt.text)
			if ok != tt.wantOK {
				t.Fatalf("ExtractPRRef(%q) ok = %v, want %v", tt.text, ok, tt.wantOK)
			}
			if ok && n != tt.wantN {
				t.Errorf("ExtractPRRef(%q) n = %d, want %d", tt.text, n, tt.wantN)
			}
		})
	}
}
