package taskstate

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestParseDeliverables(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "h2 bullet list",
			body: "# Task\n\nDo the thing.\n\n## Deliverables\n\n- internal/foo/bar.go\n- internal/foo/bar_test.go\n\n## Notes\n\nignore this bullet\n- not/a/deliverable/but/still/parsed/because/no/heading\n",
			want: []string{"internal/foo/bar.go", "internal/foo/bar_test.go"},
		},
		{
			name: "h3 acceptance criteria heading variant",
			body: "## Plan\n\nsome prose\n\n### Acceptance criteria\n\n* docs/adr/0001.md\n* README.md\n",
			want: []string{"docs/adr/0001.md", "README.md"},
		},
		{
			name: "no deliverables section at all",
			body: "## Plan\n\n- this is a prose bullet, not under the right heading\n",
			want: nil,
		},
		{
			name: "prose lines under heading are ignored, only path-shaped kept",
			body: "## Deliverables\n\n- the tests should pass\n- internal/x/y.go\n- make sure it builds\n",
			want: []string{"internal/x/y.go"},
		},
		{
			name: "backticked paths and trailing punctuation stripped",
			body: "## Deliverables\n\n- `internal/x/y.go`.\n- `docs/plan.md`,\n",
			want: []string{"internal/x/y.go", "docs/plan.md"},
		},
		{
			name: "dedupes preserving first-seen order",
			body: "## Deliverables\n\n- internal/x/y.go\n- internal/z/w.go\n- internal/x/y.go\n",
			want: []string{"internal/x/y.go", "internal/z/w.go"},
		},
		{
			name: "case-insensitive heading match",
			body: "## deliverables\n\n- a/b.go\n",
			want: []string{"a/b.go"},
		},
		{
			name: "section ends at next heading of any level",
			body: "## Deliverables\n- a/b.go\n# Unrelated\n- c/d.go\n",
			want: []string{"a/b.go"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseDeliverables(tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseDeliverables() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestVerifyDeliverables(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "foo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "foo", "bar.go"), []byte("package foo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "foo", "bar_test.go"), []byte("package foo\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		deliverables []string
		wantProblems []string // deliverable strings expected to have a problem
	}{
		{
			name:         "all present, no problems",
			deliverables: []string{"internal/foo/bar.go", "internal/foo/bar_test.go"},
			wantProblems: nil,
		},
		{
			name:         "missing file",
			deliverables: []string{"internal/foo/missing.go"},
			wantProblems: []string{"internal/foo/missing.go"},
		},
		{
			name:         "directory named where a file was expected",
			deliverables: []string{"internal/foo"},
			// "internal/foo" has no dotted extension, so it's treated as
			// an expected directory and passes.
			wantProblems: nil,
		},
		{
			name:         "file-shaped glob with zero matches",
			deliverables: []string{"internal/foo/*.md"},
			wantProblems: []string{"internal/foo/*.md"},
		},
		{
			name:         "glob matching an actual file passes",
			deliverables: []string{"internal/foo/*.go"},
			wantProblems: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := VerifyDeliverables(dir, tc.deliverables)
			var got []string
			for _, p := range problems {
				got = append(got, p.Deliverable)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.wantProblems...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("VerifyDeliverables() problems = %#v, want %#v (full: %#v)", got, want, problems)
			}
		})
	}
}

func TestVerifyDeliverables_FileNamedButIsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "weird.go"), 0755); err != nil {
		t.Fatal(err)
	}
	problems := VerifyDeliverables(dir, []string{"weird.go"})
	if len(problems) != 1 {
		t.Fatalf("expected 1 problem, got %#v", problems)
	}
	if problems[0].Reason == "" {
		t.Errorf("expected a reason explaining the directory mismatch")
	}
}
