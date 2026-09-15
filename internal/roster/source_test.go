package roster

import (
	"reflect"
	"testing"
)

func TestStripANSI(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no escapes", "plain text", "plain text"},
		{"color code", "\x1b[32mgreen\x1b[0m text", "green text"},
		{"bold and reset", "\x1b[1mbold\x1b[0m", "bold"},
		{"multiple codes", "\x1b[31mred\x1b[0m and \x1b[34mblue\x1b[0m", "red and blue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripANSI(tc.in); got != tc.want {
				t.Errorf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSourceArgs(t *testing.T) {
	got := sourceArgs("workouts", "what's my mile time")
	want := []string{"--source", "workouts", "what's my mile time"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sourceArgs() = %v, want %v", got, want)
	}
}

func TestNewSourceBackend_RequiresSourceSpec(t *testing.T) {
	if _, err := newSourceBackend(&Entry{Name: "x", Kind: KindSource}, Deps{}); err == nil {
		t.Fatal("expected an error when Source is nil")
	}
	if _, err := newSourceBackend(&Entry{Name: "x", Kind: KindSource, Source: &SourceSpec{}}, Deps{}); err == nil {
		t.Fatal("expected an error when Source.Source is empty")
	}
}

func TestNewSourceBackend_OK(t *testing.T) {
	e := &Entry{Name: "workouts", Kind: KindSource, Source: &SourceSpec{Source: "workouts"}}
	b, err := newSourceBackend(e, Deps{})
	if err != nil {
		t.Fatalf("newSourceBackend: %v", err)
	}
	sb, ok := b.(*sourceBackend)
	if !ok {
		t.Fatalf("newSourceBackend returned %T, want *sourceBackend", b)
	}
	if sb.source != "workouts" {
		t.Errorf("source = %q, want %q", sb.source, "workouts")
	}
	if sb.Kind() != KindSource {
		t.Errorf("Kind() = %q, want %q", sb.Kind(), KindSource)
	}
}
