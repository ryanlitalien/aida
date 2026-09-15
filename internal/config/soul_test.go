package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The soul serializer used to drop the top-level `family:` and `notes:` blocks
// because they weren't in the Soul struct - yaml.Unmarshal silently discards
// unknown keys - so the assistant answered "I don't have your family" despite
// the data being present. Guard against that regression.
func TestSoulFamilyAndNotesReachPrompt(t *testing.T) {
	const raw = `
name: Ryan
role: Engineer
context: works at ButterStack
family:
  kids:
    - name: Jack Reyes
      relation: son
      born: 2012-03-15
    - name: Mia Reyes
      relation: daughter
  ex_wife:
    name: Jane Doe
    nickname: Roxy
notes: |
  The home NAS is named Faraday.
`
	var s Soul
	if err := yaml.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := s.ForPrompt()

	for _, want := range []string{
		"- Family:",
		"Jack Reyes (son)",
		"Mia Reyes (daughter)",
		"Jane Doe (ex-wife; aka Roxy)",
		"- Notes: The home NAS is named Faraday.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ForPrompt missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// The `people:` roster must reach the prompt so the assistant recognizes
// partners/coworkers by name (and by alias) instead of stalling on "who?".
func TestSoulPeopleReachPrompt(t *testing.T) {
	const raw = `
name: Ryan
people:
  - name: Kevin
    aka: [partner@example.com, CTA]
    role: ButterStack BizOps + AWS partnership
    note: Former AWS Games counterpart; involved with ButterStack via his LLC.
`
	var s Soul
	if err := yaml.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := s.ForPrompt()
	for _, want := range []string{
		"- People:",
		"Kevin (aka partner@example.com, CTA) - ButterStack BizOps + AWS partnership",
		"Former AWS Games counterpart",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ForPrompt missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// An empty soul still yields no block, and a family-only soul is not "empty".
func TestSoulEmptyAndFamilyOnly(t *testing.T) {
	if (&Soul{}).ForPrompt() != "" {
		t.Error("empty soul should produce no prompt block")
	}
	famOnly := &Soul{Family: &FamilyInfo{Kids: []Person{{Name: "Jack", Relation: "son"}}}}
	if famOnly.IsEmpty() {
		t.Error("a soul with only family should not be IsEmpty")
	}
	if !strings.Contains(famOnly.ForPrompt(), "Jack (son)") {
		t.Error("family-only soul should render the family block")
	}
}
