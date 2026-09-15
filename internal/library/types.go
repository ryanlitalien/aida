// Package library implements multi-root agentic intelligence aggregation.
//
// A "library root" is a folder with a known structure (layers/, skills/,
// sources/, output-styles/, routes.yaml, library.yaml) that contributes
// context, reusable procedures, output styles, and source-adapter configs to
// aida. Multiple roots can be registered in ~/.aida/library.yaml; at runtime
// they are merged into a single Registry, filtered by which tools are
// installed on the current machine.
//
// Library roots NEVER live inside target project folders. The link between
// library content and a target project is routes.yaml glob-matching against
// paths -- the project itself is never read or written. This is what makes
// aida work with read-only work folders.
package library

// Owner identifies who controls a library root.
type Owner string

const (
	OwnerSelf Owner = "self"
	OwnerWork Owner = "work"
)

// RootRef is one entry in ~/.aida/library.yaml -- a pointer to a library
// root on this machine.
type RootRef struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path"`
	Owner    Owner  `yaml:"owner,omitempty"`
	Git      string `yaml:"git,omitempty"`
	Optional bool   `yaml:"optional,omitempty"`
}

// LibraryConfig is the parsed contents of ~/.aida/library.yaml.
type LibraryConfig struct {
	Version int       `yaml:"version"`
	Roots   []RootRef `yaml:"roots"`
}

// Manifest is the parsed contents of a per-root library.yaml file.
type Manifest struct {
	Version      int                         `yaml:"version"`
	Name         string                      `yaml:"name"`
	Description  string                      `yaml:"description,omitempty"`
	Layers       map[string]LayerEntry       `yaml:"layers,omitempty"`
	Skills       map[string]SkillEntry       `yaml:"skills,omitempty"`
	Sources      map[string]SourceEntry      `yaml:"sources,omitempty"`
	OutputStyles map[string]OutputStyleEntry `yaml:"output-styles,omitempty"`
}

// LayerEntry declares one markdown context fragment.
type LayerEntry struct {
	File          string   `yaml:"file"`
	RequiresTool  string   `yaml:"requires_tool,omitempty"`
	RequiresTools []string `yaml:"requires_tools,omitempty"`
}

// SkillEntry declares one reusable procedure (a directory of files).
type SkillEntry struct {
	Dir           string   `yaml:"dir"`
	RequiresTool  string   `yaml:"requires_tool,omitempty"`
	RequiresTools []string `yaml:"requires_tools,omitempty"`
}

// SourceEntry declares one source-adapter config (a YAML file).
type SourceEntry struct {
	File          string   `yaml:"file"`
	RequiresTool  string   `yaml:"requires_tool,omitempty"`
	RequiresTools []string `yaml:"requires_tools,omitempty"`
}

// OutputStyleEntry declares one Claude Code output style file.
type OutputStyleEntry struct {
	File          string   `yaml:"file"`
	RequiresTool  string   `yaml:"requires_tool,omitempty"`
	RequiresTools []string `yaml:"requires_tools,omitempty"`
}

// Root is a present, parsed library root.
type Root struct {
	Ref      RootRef
	AbsPath  string
	Manifest *Manifest
}

// ResolvedLayer is a layer in the aggregated registry, with availability
// determined by the current machine's tools.
type ResolvedLayer struct {
	Name         string
	Root         *Root
	AbsFile      string
	Available    bool
	MissingTools []string
}

// ResolvedSkill is a skill in the aggregated registry.
type ResolvedSkill struct {
	Name         string
	Root         *Root
	AbsDir       string
	Available    bool
	MissingTools []string
}

// ResolvedSource is a source-adapter config in the aggregated registry.
type ResolvedSource struct {
	Name         string
	Root         *Root
	AbsFile      string
	Available    bool
	MissingTools []string
}

// ResolvedOutputStyle is an output style in the aggregated registry.
type ResolvedOutputStyle struct {
	Name         string
	Root         *Root
	AbsFile      string
	Available    bool
	MissingTools []string
}

// Registry is the aggregated view of all present library roots, post tool
// filtering. Later roots override earlier ones on name collision.
type Registry struct {
	Roots        []*Root
	Layers       map[string]*ResolvedLayer
	Skills       map[string]*ResolvedSkill
	Sources      map[string]*ResolvedSource
	OutputStyles map[string]*ResolvedOutputStyle
	// MissingRoots are RootRefs declared in ~/.aida/library.yaml that
	// were not found on disk. Optional roots are silently skipped here;
	// non-optional ones are returned for aida library doctor to surface.
	MissingRoots []RootRef

	// loadIssues accumulates rejection records from the most recent
	// LoadSources() call. Read via Issues(). Populated for surfacing
	// to the agent (cli/agent.go) and lint (cli/lint.go) as
	// remediation context - see internal/library/issues.go.
	loadIssues []LibraryIssue
}

// requiredTools returns the union of RequiresTool + RequiresTools from any
// entry that embeds those fields.
func requiredTools(single string, list []string) []string {
	out := make([]string, 0, len(list)+1)
	if single != "" {
		out = append(out, single)
	}
	out = append(out, list...)
	return out
}
