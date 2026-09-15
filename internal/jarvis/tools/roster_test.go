package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/dispatch"
	"github.com/ryanlitalien/aida/internal/roster"
)

// loadTestRoster loads this package's own testdata/roster.yaml fixture (aida
// + workouts/Coach + product-manager/Pamela + two intentionally-ambiguous
// "helper" entries), profile-agnostic since none of the fixture entries
// declare a profiles: list.
func loadTestRoster(t *testing.T) *roster.Roster {
	t.Helper()
	r, err := roster.Load("testdata", "home")
	if err != nil {
		t.Fatalf("roster.Load: %v", err)
	}
	return r
}

// testDispatcher builds a Dispatcher over the fixture roster with no LLM and
// no jobs store -- enough for RegisterRosterTools' two tools, none of whose
// tested paths in this file need either. Deliberately never exercises the
// live subagent/source Ask paths (they shell out to `aida`/`claude`); tests
// here stick to resolution and branching, which return before any backend
// executes.
func testDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	return &dispatch.Dispatcher{
		Roster: loadTestRoster(t),
		Deps:   roster.Deps{Profile: "home"},
	}
}

func TestRegisterRosterTools_AddsExactlyTwoTools(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	before := len(r.All())

	RegisterRosterTools(r, testDispatcher(t))

	after := r.All()
	if len(after)-before != 2 {
		t.Fatalf("RegisterRosterTools added %d tools, want 2", len(after)-before)
	}

	var names []string
	for _, tool := range after {
		names = append(names, tool.Name)
	}
	if !containsName(names, "roster_list") {
		t.Errorf("registry missing roster_list; got %v", names)
	}
	if !containsName(names, "ask_agent") {
		t.Errorf("registry missing ask_agent; got %v", names)
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// roster_list must name the source-kind fixture entry (via its call-sign,
// Display()) and must never surface the reserved aida entry as one of its
// own delegates.
func TestRosterListTool_ExcludesAida(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	d := testDispatcher(t)
	RegisterRosterTools(r, d)

	out, err := r.Run(context.Background(), "roster_list", nil)
	if err != nil {
		t.Fatalf("roster_list: %v", err)
	}
	if !strings.Contains(out, "Coach") {
		t.Errorf("roster_list output %q missing the workouts entry's call-sign", out)
	}
	if !strings.Contains(out, "Pamela") {
		t.Errorf("roster_list output %q missing the product-manager entry's call-sign", out)
	}
	if strings.Contains(out, "Aida") {
		t.Errorf("roster_list output %q leaks the reserved aida entry", out)
	}
}

func TestDescribeRoster_EmptyRoster(t *testing.T) {
	got := describeRoster(nil)
	if !strings.Contains(got, "empty") {
		t.Errorf("describeRoster(nil) = %q, want a message noting the roster is empty", got)
	}
}

func TestFirstClause(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"Personal trainer and nutrition coach.", "Personal trainer and nutrition coach"},
		{"Personal trainer and nutrition coach. Tracks workouts too.", "Personal trainer and nutrition coach"},
		{"No terminal punctuation here", "No terminal punctuation here"},
		{"  Leading space, trimmed.  ", "Leading space, trimmed"},
	}
	for _, c := range cases {
		if got := firstClause(c.in); got != c.want {
			t.Errorf("firstClause(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ask_agent's tool description must embed the live roster so the model
// knows who it can delegate to without a separate roster_list round trip.
func TestAskAgentTool_DescriptionEmbedsRoster(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	RegisterRosterTools(r, testDispatcher(t))

	var desc string
	for _, tool := range r.All() {
		if tool.Name == "ask_agent" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("ask_agent tool not found in registry")
	}
	if !strings.Contains(desc, "Coach") {
		t.Errorf("ask_agent description %q missing the workouts entry's call-sign", desc)
	}
	if !strings.Contains(desc, "Pamela") {
		t.Errorf("ask_agent description %q missing the product-manager entry's call-sign", desc)
	}
	if strings.Contains(desc, "Aida,") || strings.Contains(desc, "\nAida") {
		t.Errorf("ask_agent description %q lists the reserved aida entry as a delegate", desc)
	}
}

// An unknown agent name must come back as a spoken, non-error reply -- the
// caller (voice layer) speaks whatever string comes back, so a real Go error
// here would read to the user as a crash rather than "I don't know them."
func TestAskAgentTool_UnknownAgent(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	RegisterRosterTools(r, testDispatcher(t))

	raw, _ := json.Marshal(map[string]string{
		"agent": "nonexistent-agent-xyz",
		"task":  "do something",
	})
	out, err := r.Run(context.Background(), "ask_agent", raw)
	if err != nil {
		t.Fatalf("ask_agent(unknown): unexpected error: %v", err)
	}
	if !strings.Contains(out, "nonexistent-agent-xyz") {
		t.Errorf("ask_agent(unknown) = %q, want it to name the unresolved agent", out)
	}
	if !strings.Contains(out, "don't have anyone called") {
		t.Errorf("ask_agent(unknown) = %q, want the not-found phrasing", out)
	}
}

// Two fixture entries deliberately share the alias "helper" so Resolve
// returns ErrAmbiguous; ask_agent must turn that into a clarifying spoken
// reply rather than guessing or erroring. This returns before touching any
// backend, so it stays hermetic.
func TestAskAgentTool_AmbiguousAgent(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	RegisterRosterTools(r, testDispatcher(t))

	raw, _ := json.Marshal(map[string]string{
		"agent": "helper",
		"task":  "do something",
	})
	out, err := r.Run(context.Background(), "ask_agent", raw)
	if err != nil {
		t.Fatalf("ask_agent(ambiguous): unexpected error: %v", err)
	}
	if !strings.Contains(out, "more than one") {
		t.Errorf("ask_agent(ambiguous) = %q, want a clarifying reply", out)
	}
}

// Missing required fields must fail fast with an error, before Resolve ever
// runs.
func TestAskAgentTool_EmptyInputIsError(t *testing.T) {
	r := New(nil, "", nil, nil, nil, nil, nil, false)
	RegisterRosterTools(r, testDispatcher(t))

	raw, _ := json.Marshal(map[string]string{"agent": "", "task": ""})
	if _, err := r.Run(context.Background(), "ask_agent", raw); err == nil {
		t.Error("ask_agent with empty agent/task: want an error, got nil")
	}
}
