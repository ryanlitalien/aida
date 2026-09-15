package sources

import (
	"context"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestIsNoTemplatePlaceholder(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"NO_MATCHING_TEMPLATE", true},
		{"  NO_MATCHING_TEMPLATE  ", true},
		{"NO_MATCHING_TEMPLATE;", true},
		{"no_matching_template", true},
		{"NO_MATCHING_TEMPLATE for vendor query", true},
		{"SELECT * FROM expenses", false},
		{"", false},
		{"NO MATCHING TEMPLATE", false}, // requires underscores
	}
	for _, c := range cases {
		got := isNoTemplatePlaceholder(c.in)
		if got != c.want {
			t.Errorf("isNoTemplatePlaceholder(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSQLiteAdapter_RejectsNoTemplatePlaceholder(t *testing.T) {
	a := &SQLiteAdapter{}
	src := config.Source{Path: "/tmp/nonexistent.db"} // not used
	r, err := a.Execute(context.Background(), "NO_MATCHING_TEMPLATE", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != "empty" {
		t.Errorf("status = %q, want empty (got summary: %s)", r.Status, r.Summary)
	}
}

func TestExecAdapter_RejectsNoTemplatePlaceholder(t *testing.T) {
	a := &ExecAdapter{}
	src := config.Source{
		Path: "/tmp",
		Exec: map[string]string{"query": "echo {query}"},
	}
	r, err := a.Execute(context.Background(), "NO_MATCHING_TEMPLATE", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Status != "empty" {
		t.Errorf("status = %q, want empty (got summary: %s)", r.Status, r.Summary)
	}
}
