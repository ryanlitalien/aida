package library

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LibraryConfigFile is the registry file at ~/.aida/library.yaml.
const LibraryConfigFile = "library.yaml"

// ConfigPath returns the absolute path to ~/.aida/library.yaml.
func ConfigPath(aidaDir string) string {
	return filepath.Join(aidaDir, LibraryConfigFile)
}

// LoadConfig reads ~/.aida/library.yaml. Returns a default
// (single-root) config if the file does not exist.
func LoadConfig(aidaDir string) (*LibraryConfig, error) {
	path := ConfigPath(aidaDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(aidaDir), nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg LibraryConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	return &cfg, nil
}

// SaveConfig writes ~/.aida/library.yaml.
func SaveConfig(aidaDir string, cfg *LibraryConfig) error {
	if err := os.MkdirAll(aidaDir, 0755); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling library config: %w", err)
	}
	return os.WriteFile(ConfigPath(aidaDir), data, 0644)
}

// DefaultConfig returns a single-root config pointing at <aidaDir>/library.
func DefaultConfig(aidaDir string) *LibraryConfig {
	return &LibraryConfig{
		Version: 1,
		Roots: []RootRef{
			{
				Name:  "defaults",
				Path:  filepath.Join(aidaDir, "library"),
				Owner: OwnerSelf,
			},
		},
	}
}

// LoadRegistry loads ~/.aida/library.yaml, parses every present root, and
// returns the aggregated, tool-filtered registry.
//
// Roots that are missing on disk:
//   - Optional roots are silently skipped (not an error).
//   - Non-optional roots are recorded in Registry.MissingRoots so doctor can
//     surface them; loading still proceeds.
//
// Within each root, entries are filtered by tool availability via
// exec.LookPath. Filtered-out entries are still tracked (Available=false)
// so doctor can explain what's missing and why.
//
// Roots are merged in declared order; later roots override earlier ones on
// name collision.
//
// Phase 6.2: auto-append a local override root when one exists at
// ~/.aida/library-local/, and a per-profile root at
// ~/.aida/library-local/<profile>/ when the active profile is known.
// These roots are appended AFTER the declared roots so manual tunes in
// the local tree win on name collision - and because `aida index` only
// writes to ~/.aida/library, local overrides are never clobbered on
// rescan. Missing roots are silently skipped (they're optional).
func LoadRegistry(aidaDir string) (*Registry, error) {
	cfg, err := LoadConfig(aidaDir)
	if err != nil {
		return nil, err
	}
	cfg = appendLocalRoots(cfg, aidaDir)
	return BuildRegistry(cfg)
}

// appendLocalRoots returns a copy of cfg with up to two additional roots
// appended: a global library-local root and a per-profile one. No-op when
// neither directory exists.
func appendLocalRoots(cfg *LibraryConfig, aidaDir string) *LibraryConfig {
	out := &LibraryConfig{
		Version: cfg.Version,
		Roots:   append([]RootRef(nil), cfg.Roots...),
	}
	localDir := filepath.Join(aidaDir, "library-local")
	if info, err := os.Stat(localDir); err == nil && info.IsDir() {
		out.Roots = append(out.Roots, RootRef{
			Name:     "local",
			Path:     localDir,
			Owner:    OwnerSelf,
			Optional: true,
		})
	}
	if profile := activeProfileName(); profile != "" {
		profileDir := filepath.Join(aidaDir, "library-local", profile)
		if info, err := os.Stat(profileDir); err == nil && info.IsDir() {
			out.Roots = append(out.Roots, RootRef{
				Name:     "local-" + profile,
				Path:     profileDir,
				Owner:    OwnerSelf,
				Optional: true,
			})
		}
	}
	return out
}

// activeProfileName reads AIDA_PROFILE as a cheap profile indicator.
// Used by Phase 6.2 to locate ~/.aida/library-local/<profile>/. Returns
// "" when the env var is unset; the full config-driven lookup lives in
// config.LoadConfig which we avoid importing here to prevent a cycle.
func activeProfileName() string {
	return strings.TrimSpace(os.Getenv("AIDA_PROFILE"))
}

// BuildRegistry constructs a Registry from a parsed LibraryConfig. Exposed
// separately for testability.
func BuildRegistry(cfg *LibraryConfig) (*Registry, error) {
	reg := &Registry{
		Layers:       make(map[string]*ResolvedLayer),
		Skills:       make(map[string]*ResolvedSkill),
		Sources:      make(map[string]*ResolvedSource),
		OutputStyles: make(map[string]*ResolvedOutputStyle),
	}

	for _, ref := range cfg.Roots {
		absPath := expandPath(ref.Path)
		info, err := os.Stat(absPath)
		if err != nil || !info.IsDir() {
			if !ref.Optional {
				reg.MissingRoots = append(reg.MissingRoots, ref)
			}
			continue
		}
		manifest, err := LoadManifest(absPath)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", ref.Name, err)
		}
		root := &Root{
			Ref:      ref,
			AbsPath:  absPath,
			Manifest: manifest,
		}
		reg.Roots = append(reg.Roots, root)
		mergeRoot(reg, root)
		if ref.Owner == OwnerSelf {
			mergeAutoDiscovered(reg, root)
		}
	}

	return reg, nil
}

// mergeAutoDiscovered registers source YAMLs and per-source layer
// markdowns that exist on disk under a self-owned root but aren't in
// its library.yaml manifest. Drop a file in `sources/` or
// `layers/sources/` and it loads on the next run - no manifest edit
// needed. Multi-root collaboration (owner != self) still requires
// manifest entries so cross-root namespacing stays deterministic.
//
// Manifest wins on name collision: entries already in reg.Sources or
// reg.Layers (from this root's manifest, or an earlier root's) are not
// overwritten.
func mergeAutoDiscovered(reg *Registry, root *Root) {
	if entries, err := os.ReadDir(filepath.Join(root.AbsPath, "sources")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".yaml")
			if _, exists := reg.Sources[name]; exists {
				continue
			}
			reg.Sources[name] = &ResolvedSource{
				Name:      name,
				Root:      root,
				AbsFile:   filepath.Join(root.AbsPath, "sources", e.Name()),
				Available: true,
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root.AbsPath, "layers", "sources")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			name := "sources/" + strings.TrimSuffix(e.Name(), ".md")
			if _, exists := reg.Layers[name]; exists {
				continue
			}
			reg.Layers[name] = &ResolvedLayer{
				Name:      name,
				Root:      root,
				AbsFile:   filepath.Join(root.AbsPath, "layers", "sources", e.Name()),
				Available: true,
			}
		}
	}
}

func mergeRoot(reg *Registry, root *Root) {
	for name, entry := range root.Manifest.Layers {
		missing := missingTools(requiredTools(entry.RequiresTool, entry.RequiresTools))
		reg.Layers[name] = &ResolvedLayer{
			Name:         name,
			Root:         root,
			AbsFile:      resolveUnderRoot(root.AbsPath, entry.File),
			Available:    len(missing) == 0,
			MissingTools: missing,
		}
	}
	for name, entry := range root.Manifest.Skills {
		missing := missingTools(requiredTools(entry.RequiresTool, entry.RequiresTools))
		reg.Skills[name] = &ResolvedSkill{
			Name:         name,
			Root:         root,
			AbsDir:       resolveUnderRoot(root.AbsPath, entry.Dir),
			Available:    len(missing) == 0,
			MissingTools: missing,
		}
	}
	for name, entry := range root.Manifest.Sources {
		missing := missingTools(requiredTools(entry.RequiresTool, entry.RequiresTools))
		reg.Sources[name] = &ResolvedSource{
			Name:         name,
			Root:         root,
			AbsFile:      resolveUnderRoot(root.AbsPath, entry.File),
			Available:    len(missing) == 0,
			MissingTools: missing,
		}
	}
	for name, entry := range root.Manifest.OutputStyles {
		missing := missingTools(requiredTools(entry.RequiresTool, entry.RequiresTools))
		reg.OutputStyles[name] = &ResolvedOutputStyle{
			Name:         name,
			Root:         root,
			AbsFile:      resolveUnderRoot(root.AbsPath, entry.File),
			Available:    len(missing) == 0,
			MissingTools: missing,
		}
	}
}

// resolveUnderRoot returns an absolute path. If `file` is already absolute
// (e.g. ~/.claude/CLAUDE.md after ~ expansion) it is returned verbatim;
// otherwise it is joined under rootAbs. This lets library manifests
// reference files outside the library root -- useful for pulling in the
// user's ~/.claude/CLAUDE.md as a personal context layer without copying.
func resolveUnderRoot(rootAbs, file string) string {
	expanded := expandPath(file)
	if filepath.IsAbs(expanded) {
		return expanded
	}
	return filepath.Join(rootAbs, expanded)
}

// missingTools returns the subset of required tools that are NOT on PATH.
// Empty result == all required tools are available.
func missingTools(required []string) []string {
	var missing []string
	for _, tool := range required {
		if tool == "" {
			continue
		}
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	return missing
}

// AvailableLayers returns the layer names that pass tool filtering.
func (r *Registry) AvailableLayers() []string {
	out := make([]string, 0, len(r.Layers))
	for name, l := range r.Layers {
		if l.Available {
			out = append(out, name)
		}
	}
	return out
}

// AvailableSources returns the source names that pass tool filtering.
func (r *Registry) AvailableSources() []string {
	out := make([]string, 0, len(r.Sources))
	for name, s := range r.Sources {
		if s.Available {
			out = append(out, name)
		}
	}
	return out
}

// AvailableSkills returns the skill names that pass tool filtering.
func (r *Registry) AvailableSkills() []string {
	out := make([]string, 0, len(r.Skills))
	for name, s := range r.Skills {
		if s.Available {
			out = append(out, name)
		}
	}
	return out
}

// AvailableOutputStyles returns the output style names that pass tool
// filtering.
func (r *Registry) AvailableOutputStyles() []string {
	out := make([]string, 0, len(r.OutputStyles))
	for name, s := range r.OutputStyles {
		if s.Available {
			out = append(out, name)
		}
	}
	return out
}

// expandPath expands a leading ~/ to the user's home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// ExpandPath is the exported version, for callers outside this package.
func ExpandPath(path string) string {
	return expandPath(path)
}

// homeRelative is the inverse of expandPath: when path lives under the
// user's home directory, return it as `~/...`. Used when serializing
// scanned source paths so the YAML stays portable across machines with
// different usernames (the home/work dual-machine setup). Returns the
// path unchanged when it doesn't share a HOME prefix.
func homeRelative(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	if rel == "." {
		return "~"
	}
	return "~/" + rel
}
