package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const SoulFile = "soul.yaml"

// Soul describes the USER - their role, context, and routing preferences.
// Inspired by OpenClaw's SOUL.md (which describes the agent), aida'
// soul.yaml describes the human so every LLM call in the pipeline
// understands who is asking and how to answer.
//
// The file is read once per query and injected into:
//   - the parser prompt (LLM Call #1) - so domain jargon is understood
//   - the router prompt (LLM Call #4) - so routing preferences are respected
//   - the synthesis prompt (LLM Call #3) - so the answer style matches
type Soul struct {
	Name        string      `yaml:"name"`
	Role        string      `yaml:"role"`
	Context     string      `yaml:"context"`
	Preferences []string    `yaml:"preferences,omitempty"`
	Family      *FamilyInfo `yaml:"family,omitempty"`
	People      []Contact   `yaml:"people,omitempty"`
	Notes       string      `yaml:"notes,omitempty"`

	// NOTE: the large `voice:` email-style block in soul.yaml is deliberately
	// NOT captured here - it's drafting-specific and would bloat every LLM
	// prompt in the pipeline. Add a dedicated accessor if a drafting path
	// ever needs it.
}

// FamilyInfo mirrors the `family:` block in soul.yaml so names resolve in
// queries, drafts, and transcripts ("what's my son's name", "email Roxy").
type FamilyInfo struct {
	Kids    []Person `yaml:"kids,omitempty"`
	Spouse  *Person  `yaml:"spouse,omitempty"`
	ExWife  *Person  `yaml:"ex_wife,omitempty"`
	Parents []Person `yaml:"parents,omitempty"`
}

// Person is one family member. Extra soul.yaml keys (e.g. `born`) are ignored.
type Person struct {
	Name     string `yaml:"name"`
	Relation string `yaml:"relation,omitempty"`
	Nickname string `yaml:"nickname,omitempty"`
}

// Contact is a person in the user's orbit (partners, coworkers, collaborators)
// so the assistant recognizes names in queries, drafts, and transcripts instead
// of stalling on "who is that?". Note is freeform - a compact bio; deep detail
// belongs in a brain entity page the assistant can pull on demand.
type Contact struct {
	Name string   `yaml:"name"`
	Aka  []string `yaml:"aka,omitempty"`  // alternate names / emails for name resolution
	Role string   `yaml:"role,omitempty"` // short descriptor
	Note string   `yaml:"note,omitempty"` // compact context
}

// LoadSoul reads ~/.aida/soul.yaml. Returns a zero Soul (not an error)
// if the file doesn't exist - the user hasn't created one yet, and
// that's fine.
func LoadSoul() (*Soul, error) {
	path := filepath.Join(Dir(), SoulFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Soul{}, nil
		}
		return nil, fmt.Errorf("reading soul: %w", err)
	}
	var s Soul
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parsing soul: %w", err)
	}
	return &s, nil
}

// IsEmpty returns true when no soul.yaml exists or it has no content.
func (s *Soul) IsEmpty() bool {
	return s == nil || (s.Name == "" && s.Role == "" && s.Context == "" &&
		len(s.Preferences) == 0 && s.Family == nil && len(s.People) == 0 && s.Notes == "")
}

// peopleForPrompt renders the people roster as compact bullet lines, or "".
func peopleForPrompt(people []Contact) string {
	if len(people) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("- People:\n")
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "  - %s", p.Name)
		if len(p.Aka) > 0 {
			fmt.Fprintf(&b, " (aka %s)", strings.Join(p.Aka, ", "))
		}
		if p.Role != "" {
			fmt.Fprintf(&b, " - %s", p.Role)
		}
		b.WriteString("\n")
		if p.Note != "" {
			fmt.Fprintf(&b, "    %s\n", strings.TrimSpace(p.Note))
		}
	}
	return b.String()
}

// forPrompt renders the family block as compact bullet lines, or "" if empty.
func (f *FamilyInfo) forPrompt() string {
	if f == nil {
		return ""
	}
	var lines []string
	add := func(p *Person, rel string) {
		if p == nil || p.Name == "" {
			return
		}
		line := "  - " + p.Name
		var extras []string
		if rel != "" {
			extras = append(extras, rel)
		}
		if p.Nickname != "" {
			extras = append(extras, "aka "+p.Nickname)
		}
		if len(extras) > 0 {
			line += " (" + strings.Join(extras, "; ") + ")"
		}
		lines = append(lines, line)
	}
	for i := range f.Kids {
		add(&f.Kids[i], f.Kids[i].Relation)
	}
	add(f.Spouse, "spouse")
	add(f.ExWife, "ex-wife")
	for i := range f.Parents {
		rel := f.Parents[i].Relation
		if rel == "" {
			rel = "parent"
		}
		add(&f.Parents[i], rel)
	}
	if len(lines) == 0 {
		return ""
	}
	return "- Family:\n" + strings.Join(lines, "\n") + "\n"
}

// ForPrompt returns the soul as a pre-formatted block suitable for
// injection into any LLM prompt. Returns "" if the soul is empty.
func (s *Soul) ForPrompt() string {
	if s.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("User context:\n")
	if s.Name != "" {
		fmt.Fprintf(&b, "- Name: %s\n", s.Name)
	}
	if s.Role != "" {
		fmt.Fprintf(&b, "- Role: %s\n", s.Role)
	}
	if s.Context != "" {
		fmt.Fprintf(&b, "- Background: %s\n", strings.TrimSpace(s.Context))
	}
	if len(s.Preferences) > 0 {
		b.WriteString("- Routing preferences:\n")
		for _, p := range s.Preferences {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if fam := s.Family.forPrompt(); fam != "" {
		b.WriteString(fam)
	}
	if ppl := peopleForPrompt(s.People); ppl != "" {
		b.WriteString(ppl)
	}
	if s.Notes != "" {
		fmt.Fprintf(&b, "- Notes: %s\n", strings.TrimSpace(s.Notes))
	}
	return b.String()
}

// SaveSoul writes soul.yaml to ~/.aida/. Used by `aida init` onboarding.
func SaveSoul(s *Soul) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	data, err := yaml.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshaling soul: %w", err)
	}
	path := filepath.Join(Dir(), SoulFile)
	return os.WriteFile(path, data, 0644)
}
