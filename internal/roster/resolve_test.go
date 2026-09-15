package roster

import (
	"errors"
	"testing"
)

// buildResolveTestRoster returns a hand-built Roster (no YAML involved) with
// a fixed set of entries designed to exercise every Resolve precedence tier
// plus the ambiguous / other-profile / not-found paths deterministically.
//
// Active profile: "work".
//   - aida:               Kind=aida, Profiles=nil (always present)
//   - product-manager:    CallSign=Pamela, Aliases=[pam,pamela], Profiles=[work]
//   - assistant-manager:  CallSign="",     Aliases=[deputy],     Profiles=[work]
//   - devops:             CallSign=Devin,  Aliases=nil,          Profiles=[work]
//   - workouts:           CallSign=Coach,  Aliases=[coach,trainer], Profiles=[home] (all-only)
func buildResolveTestRoster() *Roster {
	aida := &Entry{Name: "aida", Kind: KindAida}
	pm := &Entry{
		Name:     "product-manager",
		Kind:     KindSubagent,
		CallSign: "Pamela",
		Aliases:  []string{"pam", "pamela"},
		Profiles: []string{"work"},
	}
	am := &Entry{
		Name:     "assistant-manager",
		Kind:     KindSubagent,
		Aliases:  []string{"deputy"},
		Profiles: []string{"work"},
	}
	devops := &Entry{
		Name:     "devops",
		Kind:     KindSubagent,
		CallSign: "Devin",
		Profiles: []string{"work"},
	}
	workouts := &Entry{
		Name:     "workouts",
		Kind:     KindSubagent,
		CallSign: "Coach",
		Aliases:  []string{"coach", "trainer"},
		Profiles: []string{"home"},
	}

	return &Roster{
		profile: "work",
		entries: []*Entry{aida, am, devops, pm}, // sorted by Name
		all:     []*Entry{aida, am, devops, pm, workouts},
	}
}

func TestResolve_ExactName(t *testing.T) {
	r := buildResolveTestRoster()
	e, err := r.Resolve("devops")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if e.Name != "devops" {
		t.Errorf("Resolve(\"devops\") = %q, want devops", e.Name)
	}
}

func TestResolve_ExactCallSign(t *testing.T) {
	r := buildResolveTestRoster()
	e, err := r.Resolve("Pamela")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if e.Name != "product-manager" {
		t.Errorf("Resolve(\"Pamela\") = %q, want product-manager", e.Name)
	}
}

func TestResolve_ExactAliasCaseInsensitive(t *testing.T) {
	r := buildResolveTestRoster()
	e, err := r.Resolve("PAM")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if e.Name != "product-manager" {
		t.Errorf("Resolve(\"PAM\") = %q, want product-manager", e.Name)
	}
}

func TestResolve_NormalizeNameFuzzy(t *testing.T) {
	r := buildResolveTestRoster()
	e, err := r.Resolve("Product Manager") // space+case differ from the hyphenated Name
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if e.Name != "product-manager" {
		t.Errorf("Resolve(\"Product Manager\") = %q, want product-manager", e.Name)
	}
}

func TestResolve_SubstringFuzzyAmbiguous(t *testing.T) {
	r := buildResolveTestRoster()
	// "manager" normalizes to a substring of both "productmanager" and
	// "assistantmanager" -- tier 5 should surface both and report ambiguity.
	_, err := r.Resolve("manager")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("Resolve(\"manager\") error = %v, want ErrAmbiguous", err)
	}
}

func TestResolve_ErrOtherProfile(t *testing.T) {
	r := buildResolveTestRoster()
	_, err := r.Resolve("Coach")
	var otherProfile *ErrOtherProfile
	if !errors.As(err, &otherProfile) {
		t.Fatalf("Resolve(\"Coach\") error = %v, want *ErrOtherProfile", err)
	}
	if otherProfile.Entry.Name != "workouts" {
		t.Errorf("otherProfile.Entry.Name = %q, want workouts", otherProfile.Entry.Name)
	}
	if otherProfile.Profile != "home" {
		t.Errorf("otherProfile.Profile = %q, want home", otherProfile.Profile)
	}
	if otherProfile.Ref != "Coach" {
		t.Errorf("otherProfile.Ref = %q, want Coach", otherProfile.Ref)
	}
}

func TestResolve_ErrNotFound(t *testing.T) {
	r := buildResolveTestRoster()
	_, err := r.Resolve("nonexistent-agent-xyz")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve error = %v, want ErrNotFound", err)
	}
}

func TestResolve_EmptyRef(t *testing.T) {
	r := buildResolveTestRoster()
	for _, ref := range []string{"", "   "} {
		_, err := r.Resolve(ref)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) error = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Butter Stack":  "butterstack",
		"butter-stack":  "butterstack",
		"butter_stack":  "butterstack",
		"butter.stack":  "butterstack",
		"ButterStack":   "butterstack",
		"  spaced out ": "spacedout",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
