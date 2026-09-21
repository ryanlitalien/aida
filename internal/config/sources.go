package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const SourcesFile = "sources.yaml"

type Source struct {
	Path         string            `yaml:"path,omitempty"`
	Type         string            `yaml:"type"`              // data-source, tool, codebase, docs, app
	Repo         string            `yaml:"repo,omitempty"`    // GitHub owner/repo (e.g., "ryanlitalien-apps/csv-viewer")
	Context      string            `yaml:"context,omitempty"` // CLAUDE.md or README.md path
	Description  string            `yaml:"description"`
	Capabilities []string          `yaml:"capabilities"`
	Entities     []string          `yaml:"entities,omitempty"`
	Profiles     []string          `yaml:"profiles,omitempty"` // restrict to named profiles; empty = all
	Exec         map[string]string `yaml:"exec,omitempty"`
	Search       *SearchConfig     `yaml:"search,omitempty"`
	Assets       map[string]string `yaml:"assets,omitempty"`
	// Example marks a shipped example source (see examples/sources/) whose
	// `path:` is a placeholder that doesn't exist on a real machine by
	// design - `aida lint` downgrades a missing-path finding to a warning
	// for these instead of an error, since the source file name ending in
	// "-example.yaml" is the only other signal lint has for the same
	// thing. Never set outside the shipped examples.
	Example bool `yaml:"example,omitempty"`
	// SelfContained marks a BuiltinToolSources entry as a pure Go adapter
	// with no external CLI dependency (e.g. "current-time", which reads
	// the system clock) -- there's no PATH binary to look up, so
	// InjectAvailableTools activates these unconditionally instead of
	// gating on exec.LookPath. Never set via user YAML (yaml:"-"); some
	// PATH-gated builtins also happen to declare no Exec templates
	// because their adapters build commands programmatically, so "no
	// Exec" alone is NOT a safe proxy for "no PATH dependency" -- this
	// field is the explicit, unambiguous signal instead.
	SelfContained bool `yaml:"-"`
}

type SearchConfig struct {
	// Mode opts a docs-typed source into full-path search via GrepAdapter
	// instead of the default DocsAdapter (which reads only the context file).
	// Valid values: "" (default, preserves DocsAdapter), "grep".
	// Present on type: docs sources that have a path with searchable docs,
	// e.g. pine-hollow, first-chair, thrive - those ship with CLAUDE.md + docs/ +
	// gdrive/ trees that the synthesizer can't extract from context alone.
	Mode       string   `yaml:"mode,omitempty"`
	Include    []string `yaml:"include,omitempty"`
	Exclude    []string `yaml:"exclude,omitempty"`
	MaxResults int      `yaml:"max_results,omitempty"`
	// TimeoutSeconds overrides the GrepAdapter default (30s). Large
	// codebases (a monorepo at ~300k files) need a longer budget
	// even with aggressive include filters. Capped at 5 min by the
	// adapter.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
}

// Sources is a map of source name → source config.
type Sources map[string]*Source

// LoadSources reads sources.yaml from ~/.aida/
func LoadSources() (Sources, error) {
	path := filepath.Join(Dir(), SourcesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(Sources), nil
		}
		return nil, fmt.Errorf("reading sources: %w", err)
	}
	var sources Sources
	if err := yaml.Unmarshal(data, &sources); err != nil {
		return nil, fmt.Errorf("parsing sources: %w", err)
	}
	if sources == nil {
		sources = make(Sources)
	}
	return sources, nil
}

// SaveSources writes sources.yaml to ~/.aida/
func SaveSources(sources Sources) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	data, err := yaml.Marshal(sources)
	if err != nil {
		return fmt.Errorf("marshaling sources: %w", err)
	}
	path := filepath.Join(Dir(), SourcesFile)
	return os.WriteFile(path, data, 0644)
}

// LoadContextFile reads the context file (CLAUDE.md/README.md) for a source.
// It also loads any referenced skill files or supplementary docs.
func (s *Source) LoadContextFile() (string, error) {
	if s.Context == "" || s.Path == "" {
		return "", nil
	}
	path := filepath.Join(expandPath(s.Path), s.Context)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil // non-fatal: context file is optional
	}
	content := string(data)

	// Look for references to skill files or other docs and append them.
	// Patterns: ~/.claude/skills/*/SKILL.md, ~/.claude/skills/*/helpful-tables.md
	content = appendReferencedDocs(content)

	return content, nil
}

// appendReferencedDocs scans content for references to ~/.claude/skills/ paths
// and appends those files' contents.
func appendReferencedDocs(content string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return content
	}

	// Find skill directory references like ~/.claude/skills/sqlite-sql/
	skillsDir := filepath.Join(home, ".claude", "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return content
	}

	var extras []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillPath := filepath.Join(skillsDir, entry.Name())
		// Only include if the context doc references this skill
		if !strings.Contains(content, entry.Name()) {
			continue
		}
		// Load key files from the skill
		for _, docName := range []string{"SKILL.md", "helpful-tables.md"} {
			docPath := filepath.Join(skillPath, docName)
			data, err := os.ReadFile(docPath)
			if err != nil {
				continue
			}
			extras = append(extras, fmt.Sprintf("\n\n--- %s/%s ---\n%s", entry.Name(), docName, string(data)))
		}
	}

	if len(extras) > 0 {
		content += "\n\n# Referenced Documentation\n" + strings.Join(extras, "\n")
	}
	return content
}

// LoadQueryTemplates reads SQL files from the source's query assets that match
// the given partner name or a general directory. Returns the contents as context.
func (s *Source) LoadQueryTemplates(partnerName string) string {
	if s.Path == "" || s.Assets == nil {
		return ""
	}
	queriesGlob, ok := s.Assets["queries"]
	if !ok {
		return ""
	}

	basePath := expandPath(s.Path)
	var templates []string

	// If partner name is set, look for partner-specific query directory first
	if partnerName != "" {
		partnerDir := filepath.Join(basePath, "queries", strings.ToLower(partnerName))
		entries, err := os.ReadDir(partnerDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(partnerDir, entry.Name()))
				if err != nil {
					continue
				}
				templates = append(templates, fmt.Sprintf("--- %s/%s ---\n%s", partnerName, entry.Name(), string(data)))
			}
		}
	}

	// Also try generic query directories
	for _, subdir := range []string{"ari", "loan", "partner", "sla", "api-health", "treasury"} {
		dir := filepath.Join(basePath, "queries", subdir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				continue
			}
			templates = append(templates, fmt.Sprintf("--- %s/%s ---\n%s", subdir, entry.Name(), string(data)))
		}
	}

	_ = queriesGlob // used to check existence

	if len(templates) == 0 {
		return ""
	}
	return "\n\n# Available SQL Query Templates\nUse these as reference for table names, column names, and JOIN patterns:\n\n" + strings.Join(templates, "\n\n")
}

// HasCapability checks if the source has a specific capability.
func (s *Source) HasCapability(cap string) bool {
	for _, c := range s.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// HasEntity checks if the source is associated with an entity.
func (s *Source) HasEntity(entity string) bool {
	for _, e := range s.Entities {
		if e == entity {
			return true
		}
	}
	return false
}

// FilterByCapability returns sources that have at least one of the given capabilities.
func FilterByCapability(sources Sources, caps []string) Sources {
	result := make(Sources)
	for name, src := range sources {
		for _, cap := range caps {
			if src.HasCapability(cap) {
				result[name] = src
				break
			}
		}
	}
	return result
}

// FilterByReachablePath removes sources whose Path field references a
// directory that does not exist on this machine. Sources with an empty
// Path (tool-type, etc.) are always kept.
func FilterByReachablePath(sources Sources) Sources {
	result := make(Sources)
	for name, src := range sources {
		if src.Path == "" {
			result[name] = src
			continue
		}
		expanded := expandPath(src.Path)
		if info, err := os.Stat(expanded); err == nil && info.IsDir() {
			result[name] = src
		}
	}
	return result
}

// ExpandRepoGlob resolves a single prefix-glob entity ("aida*") to the
// full list of owner/repo paths it matches. Matching is case-insensitive
// and hits three surfaces: the source name, any alias declared in the
// source's Entities list, and the repo's name portion (after "/").
// Non-glob entities (no trailing "*") return nil - the caller should
// leave them alone. Returns nil when no configured source matches.
func ExpandRepoGlob(entity string, srcs Sources) []string {
	if !strings.HasSuffix(entity, "*") {
		return nil
	}
	prefix := strings.ToLower(strings.TrimSuffix(entity, "*"))
	if prefix == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var matches []string
	for name, src := range srcs {
		if src == nil || src.Repo == "" {
			continue
		}
		candidates := []string{strings.ToLower(name)}
		for _, e := range src.Entities {
			candidates = append(candidates, strings.ToLower(e))
		}
		if parts := strings.SplitN(src.Repo, "/", 2); len(parts) == 2 {
			candidates = append(candidates, strings.ToLower(parts[1]))
		}
		for _, c := range candidates {
			if strings.HasPrefix(c, prefix) {
				if _, dup := seen[src.Repo]; !dup {
					seen[src.Repo] = struct{}{}
					matches = append(matches, src.Repo)
				}
				break
			}
		}
	}
	sort.Strings(matches)
	return matches
}

// BuildGlobExpansionHint returns a "Resolved globs" context block
// enumerating every glob entity (e.g. "aida*") in the given list
// together with the owner/repo paths it resolved to via ExpandRepoGlob.
// Intended to be appended to the gh query-construction context so the
// LLM fans out a single wildcard into multiple --repo flags deterministically.
// Returns "" when entities contains no globs or no configured source matches.
func BuildGlobExpansionHint(entities []string, srcs Sources) string {
	type expansion struct {
		glob  string
		repos []string
	}
	var found []expansion
	for _, e := range entities {
		repos := ExpandRepoGlob(e, srcs)
		if len(repos) == 0 {
			continue
		}
		found = append(found, expansion{glob: e, repos: repos})
	}
	if len(found) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Glob entity expansion\n\n")
	b.WriteString("The user referenced glob-style entities. Expand each one into EVERY listed owner/repo using multiple --repo flags (do NOT pick just one, do NOT use --owner):\n\n")
	for _, e := range found {
		fmt.Fprintf(&b, "- %s → %s\n", e.glob, strings.Join(e.repos, ", "))
	}
	return b.String()
}

// BuildKnownGitHubReposHint builds a context block enumerating every source
// whose YAML has a non-empty Repo field. It's intended to be appended to the
// contextDoc passed to LLM Call #2 (query construction) when the source being
// queried is gh-based - without it, the LLM has no map from entity names the
// user speaks (e.g. "acme-widgets", "aida") to the actual owner/repo pairs
// those sources point at, and historically fell back to guessing.
//
// Each line lists owner/repo plus the source's Name and Entities as aliases
// so a keyword match in the question steers the LLM to the right --repo.
// Returns "" when no source has a Repo - the caller can unconditionally
// append without worrying about an empty section.
func BuildKnownGitHubReposHint(srcs Sources) string {
	if len(srcs) == 0 {
		return ""
	}
	type entry struct {
		repo    string
		aliases []string
	}
	var entries []entry
	for name, src := range srcs {
		if src == nil || src.Repo == "" {
			continue
		}
		aliasSet := map[string]struct{}{
			strings.ToLower(name): {},
		}
		for _, e := range src.Entities {
			aliasSet[strings.ToLower(e)] = struct{}{}
		}
		// Also fold in the repo's own owner/repo tokens (common alias hits).
		for _, tok := range strings.Split(src.Repo, "/") {
			if tok != "" {
				aliasSet[strings.ToLower(tok)] = struct{}{}
			}
		}
		aliases := make([]string, 0, len(aliasSet))
		for a := range aliasSet {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		entries = append(entries, entry{repo: src.Repo, aliases: aliases})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].repo < entries[j].repo })

	var b strings.Builder
	b.WriteString("\n\n# Known local GitHub repositories\n\n")
	b.WriteString("When the user's question references any of the aliases below, use the paired owner/repo VERBATIM for `--repo`. Do NOT invent other owners or guess based on your account. If the user asks about MULTIPLE repos (\"across acme-widgets and aida\"), pass multiple --repo flags to `gh search prs` rather than using --owner with a single account.\n\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- %s - aliases: %s\n", e.repo, strings.Join(e.aliases, ", "))
	}
	return b.String()
}

// BuildGHRepoAllowlist returns a lowercase set of every owner/repo currently
// declared by a source yaml's `repo:` field. Callers pass the already-
// profile-filtered Sources map; the result is the active profile's allowlist
// of GitHub repos that are safe to query and surface in answers. Used by the
// engine to drop off-profile artifacts after a `gh` execution overshoots its
// scope (e.g. an `--owner` flag that returns repos outside the active profile).
//
// Returns nil when no source has a Repo - the caller treats nil as "no
// allowlist enforcement", same as the legacy behavior.
func BuildGHRepoAllowlist(srcs Sources) map[string]bool {
	if len(srcs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(srcs))
	for _, src := range srcs {
		if src == nil || src.Repo == "" {
			continue
		}
		out[strings.ToLower(src.Repo)] = true
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FilterByProfile returns sources whose Profiles list includes profileName.
// Sources with an empty Profiles list are available to every profile.
func FilterByProfile(sources Sources, profileName string) Sources {
	if profileName == "" {
		return sources
	}
	result := make(Sources)
	for name, src := range sources {
		if len(src.Profiles) == 0 {
			result[name] = src
			continue
		}
		for _, p := range src.Profiles {
			if p == profileName {
				result[name] = src
				break
			}
		}
	}
	return result
}
