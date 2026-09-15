package dispatch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/roster"
)

// fakeCompleter is a stub Completer for the select and aggregate edges. Tests
// set jsonResp/jsonErr for the route step (CompleteJSONWithStage) and
// textResp/textErr for the aggregate step (CompleteWithStage). It satisfies
// dispatch.Completer and is assigned directly to Dispatcher.LLM.
type fakeCompleter struct {
	jsonResp string
	jsonErr  error
	textResp string
	textErr  error

	lastJSONUser string // capture for assertions if useful
}

func (f *fakeCompleter) CompleteJSONWithStage(ctx context.Context, stage, sys, user string, schema map[string]interface{}) (string, error) {
	f.lastJSONUser = user
	return f.jsonResp, f.jsonErr
}

func (f *fakeCompleter) CompleteWithStage(ctx context.Context, stage, sys, user string) (string, error) {
	return f.textResp, f.textErr
}

const testdataDir = "testdata"

// loadTestRoster loads the dispatch package's own testdata/roster.yaml fixture
// (aida + product-manager/Pamela + devops/Devin + a handbook source, work
// profile) rather than reaching into the roster package's fixtures.
func loadTestRoster(t *testing.T, profile string) *roster.Roster {
	t.Helper()
	r, err := roster.Load(testdataDir, profile)
	if err != nil {
		t.Fatalf("roster.Load(%q, %q): %v", testdataDir, profile, err)
	}
	return r
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestWordPresent(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		phrase string
		want   bool
	}{
		{"whole word present", "have pamela draft it", "pamela", true},
		{"phrase absent entirely", "pamphlet review", "pamela", false},
		{"word boundary rejects a substring match", "pamphlet review", "pam", false},
		{"case-insensitive when caller lowercases both sides", strings.ToLower("Have Pamela Draft It"), strings.ToLower("Pamela"), true},
		{"multi-word phrase matches", "let's ask the product manager about this", "the product manager", true},
		{"multi-word phrase requires the whole phrase", "the manager reviewed it", "the product manager", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wordPresent(tt.text, tt.phrase)
			if got != tt.want {
				t.Errorf("wordPresent(%q, %q) = %v, want %v", tt.text, tt.phrase, got, tt.want)
			}
		})
	}
}

func TestNamePhrases(t *testing.T) {
	e := &roster.Entry{
		Name:     "product-manager",
		CallSign: "Pamela",
		Aliases:  []string{"pam", "the product manager", "pm"},
	}
	got := namePhrases(e)

	wantPresent := []string{"Pamela", "pam", "the product manager", "product manager"}
	for _, w := range wantPresent {
		if !containsStr(got, w) {
			t.Errorf("namePhrases() = %v, missing %q", got, w)
		}
	}
	if containsStr(got, "pm") {
		t.Errorf("namePhrases() = %v, want the 2-char alias %q dropped", got, "pm")
	}
	if len(got) != len(wantPresent) {
		t.Errorf("namePhrases() = %v (len %d), want exactly %v (len %d)", got, len(got), wantPresent, len(wantPresent))
	}
}

func TestNamedInText(t *testing.T) {
	r := loadTestRoster(t, "work")

	t.Run("one call-sign named returns exactly that route over the whole task", func(t *testing.T) {
		task := "Ask Pamela to draft the roadmap doc"
		routes := namedInText(r, task)
		if len(routes) != 1 {
			t.Fatalf("namedInText() = %d routes, want 1: %+v", len(routes), routes)
		}
		if routes[0].entry.Name != "product-manager" {
			t.Errorf("routed entry = %q, want %q", routes[0].entry.Name, "product-manager")
		}
		if routes[0].subtask != task {
			t.Errorf("subtask = %q, want the whole task %q", routes[0].subtask, task)
		}
	})

	t.Run("two distinct entries named returns two routes", func(t *testing.T) {
		task := "Have Pamela draft the doc and ask Devin to deploy it"
		routes := namedInText(r, task)
		if len(routes) != 2 {
			t.Fatalf("namedInText() = %d routes, want 2: %+v", len(routes), routes)
		}
		var names []string
		for _, rt := range routes {
			names = append(names, rt.entry.Name)
		}
		if !containsStr(names, "product-manager") || !containsStr(names, "devops") {
			t.Errorf("routed entries = %v, want product-manager and devops", names)
		}
	})

	t.Run("nobody named returns zero routes", func(t *testing.T) {
		task := "What's the weather like today"
		routes := namedInText(r, task)
		if len(routes) != 0 {
			t.Fatalf("namedInText() = %d routes, want 0: %+v", len(routes), routes)
		}
	})

	t.Run("the reserved aida entry is never returned even when named", func(t *testing.T) {
		task := "Aida, can you summarize my day"
		routes := namedInText(r, task)
		for _, rt := range routes {
			if rt.entry.Kind == roster.KindAida {
				t.Fatalf("namedInText() returned the reserved aida entry: %+v", rt)
			}
		}
	})
}

func TestSelectRoutes(t *testing.T) {
	r := loadTestRoster(t, "work")

	t.Run("maps a known entry name and subtask", func(t *testing.T) {
		f := &fakeCompleter{jsonResp: `{"routes":[{"entry":"product-manager","subtask":"draft an issue"}]}`}
		d := &Dispatcher{Roster: r, LLM: f}
		routes, err := d.selectRoutes(context.Background(), "some task")
		if err != nil {
			t.Fatalf("selectRoutes: %v", err)
		}
		if len(routes) != 1 {
			t.Fatalf("selectRoutes() = %d routes, want 1: %+v", len(routes), routes)
		}
		if routes[0].entry.Name != "product-manager" {
			t.Errorf("entry = %q, want %q", routes[0].entry.Name, "product-manager")
		}
		if routes[0].subtask != "draft an issue" {
			t.Errorf("subtask = %q, want %q", routes[0].subtask, "draft an issue")
		}
	})

	t.Run("drops an invented entry name", func(t *testing.T) {
		f := &fakeCompleter{jsonResp: `{"routes":[{"entry":"nonexistent-agent","subtask":"do something"}]}`}
		d := &Dispatcher{Roster: r, LLM: f}
		routes, err := d.selectRoutes(context.Background(), "some task")
		if err != nil {
			t.Fatalf("selectRoutes: %v", err)
		}
		if len(routes) != 0 {
			t.Fatalf("selectRoutes() = %d routes, want 0 for an invented entry: %+v", len(routes), routes)
		}
	})

	t.Run("empty routes array yields zero routeSel", func(t *testing.T) {
		f := &fakeCompleter{jsonResp: `{"routes":[]}`}
		d := &Dispatcher{Roster: r, LLM: f}
		routes, err := d.selectRoutes(context.Background(), "some task")
		if err != nil {
			t.Fatalf("selectRoutes: %v", err)
		}
		if len(routes) != 0 {
			t.Fatalf("selectRoutes() = %d routes, want 0: %+v", len(routes), routes)
		}
	})

	t.Run("a select failure makes chooseRoutes fall back to nil", func(t *testing.T) {
		f := &fakeCompleter{jsonErr: errors.New("boom")}
		d := &Dispatcher{Roster: r, LLM: f}
		// A task that names nobody, so chooseRoutes reaches the LLM branch.
		routes := d.chooseRoutes(context.Background(), "what's the weather like today")
		if routes != nil {
			t.Fatalf("chooseRoutes() = %+v, want nil on a select failure", routes)
		}
	})
}
