package library

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
)

// ScaffoldRoot creates the standard directory layout for a library root at
// the given path. Idempotent: existing files are not overwritten, and an
// existing manifest/routes file is upgraded in-place to ensure the default
// global layer is registered and a default `always: true` route exists.
// This means running `aida library init` on an already-initialized root is
// always safe and will heal common misconfigurations.
func ScaffoldRoot(rootPath, name string) error {
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return fmt.Errorf("creating root: %w", err)
	}

	// Create standard subdirectories.
	subdirs := []string{
		"layers",
		"layers/domains",
		"layers/projects",
		"skills",
		"sources",
	}
	for _, sub := range subdirs {
		if err := os.MkdirAll(filepath.Join(rootPath, sub), 0755); err != nil {
			return fmt.Errorf("creating %s: %w", sub, err)
		}
	}

	// Write a starter global.md if missing.
	globalRel := filepath.Join("layers", "global.md")
	globalPath := filepath.Join(rootPath, globalRel)
	if _, err := os.Stat(globalPath); os.IsNotExist(err) {
		stub := "# Global Layer\n\n" +
			"Baseline context loaded by every aida query (via the `always: true` route).\n\n" +
			"Edit this file to capture identity, conventions, and principles that apply universally.\n"
		if err := os.WriteFile(globalPath, []byte(stub), 0644); err != nil {
			return err
		}
	}

	// Load or create the manifest, then ensure the `global` layer is
	// registered. This upgrades older roots that had the file but no
	// manifest entry (the pre-fix bug that caused aida library list to
	// report 0 layers).
	manifest, err := LoadManifest(rootPath)
	if err != nil {
		return err
	}
	if manifest.Name == "" {
		manifest.Name = name
	}
	if manifest.Description == "" {
		manifest.Description = "Aida library root"
	}
	if manifest.Layers == nil {
		manifest.Layers = map[string]LayerEntry{}
	}
	if manifest.Skills == nil {
		manifest.Skills = map[string]SkillEntry{}
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]SourceEntry{}
	}
	if _, ok := manifest.Layers["global"]; !ok {
		manifest.Layers["global"] = LayerEntry{File: globalRel}
	}

	// If the user has a personal ~/.claude/CLAUDE.md, register it as a
	// "personal" layer via a portable tilde path. This means aida gets
	// whatever identity/coworker/project context the user has already
	// written for Claude Code, without copying or drifting out of sync.
	// Using ~/ instead of the absolute home path keeps the manifest
	// portable across machines (expandPath resolves ~ at load time).
	if home, err := os.UserHomeDir(); err == nil {
		personalPath := filepath.Join(home, ".claude", "CLAUDE.md")
		if _, err := os.Stat(personalPath); err == nil {
			if _, ok := manifest.Layers["personal"]; !ok {
				manifest.Layers["personal"] = LayerEntry{File: "~/.claude/CLAUDE.md"}
			}
		}
	}

	if err := SaveManifest(rootPath, manifest); err != nil {
		return err
	}

	// Ensure routes.yaml exists and has at least an `always: true` route
	// pointing at the global layer. If the file already exists and
	// contains any routes at all, we leave it alone -- user intent wins.
	routesPath := filepath.Join(rootPath, "routes.yaml")
	existingRoutes, err := LoadRoutes(rootPath)
	if err != nil {
		return err
	}
	if len(existingRoutes) == 0 {
		// Include `personal` in the baseline layers even if it isn't
		// registered yet; the layer materializer silently skips names
		// that aren't in the registry, so this is forward-compatible
		// for users who create ~/.claude/CLAUDE.md later.
		stub := "# Routes map cwd globs and entity patterns to active layers/skills/sources.\n" +
			"# Example:\n" +
			"#   routes:\n" +
			"#     - match_cwd: \"~/dev/some-project/**\"\n" +
			"#       layers: [domains/some-project]\n" +
			"#       sources: [sqlite, plausible]\n" +
			"#     - match_entity: \"^[A-Z0-9]{16}$\"   # generic resource id\n" +
			"#       sources: [warehouse]\n" +
			"#\n" +
			"# Default baseline route: activate the global + personal layers\n" +
			"# for every query. `personal` is ~/.claude/CLAUDE.md if present.\n" +
			"routes:\n" +
			"  - always: true\n" +
			"    layers: [global, personal]\n"
		if err := os.WriteFile(routesPath, []byte(stub), 0644); err != nil {
			return err
		}
	}

	return nil
}

// WriteBuiltinToolSources persists a library source YAML + manifest entry for
// each detected CLI tool that has a BuiltinToolSources definition. This gives
// `aida init` real, useful source configs out of the box instead of leaving the
// library empty. Existing source files are not overwritten.
func WriteBuiltinToolSources(rootPath string, detected map[string]string) (int, error) {
	srcDir := filepath.Join(rootPath, "sources")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		return 0, err
	}

	manifest, err := LoadManifest(rootPath)
	if err != nil {
		return 0, err
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]SourceEntry{}
	}

	written := 0
	// Sort for deterministic output.
	names := make([]string, 0, len(detected))
	for name := range detected {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		builtin, ok := config.BuiltinToolSources[name]
		if !ok {
			continue
		}
		cfgRel := filepath.Join("sources", name+".yaml")
		cfgAbs := filepath.Join(rootPath, cfgRel)
		// Don't overwrite user customizations.
		if _, err := os.Stat(cfgAbs); err == nil {
			continue
		}
		data, marshalErr := yaml.Marshal(builtin)
		if marshalErr != nil {
			return written, fmt.Errorf("marshaling source %q: %w", name, marshalErr)
		}
		if writeErr := os.WriteFile(cfgAbs, data, 0644); writeErr != nil {
			return written, fmt.Errorf("writing %s: %w", cfgRel, writeErr)
		}
		manifest.Sources[name] = SourceEntry{
			File:         cfgRel,
			RequiresTool: name,
		}
		written++
	}

	if written > 0 {
		if err := SaveManifest(rootPath, manifest); err != nil {
			return written, err
		}
	}
	return written, nil
}

// WriteExampleSources copies every "*.yaml" file in the given fs.FS dir
// (examples.FS's "sources" subtree, embedded at build time -- see
// examples/embed.go) into rootPath/sources/ and registers each in the
// manifest, so a fresh `aida init` has something to route to even before
// the user configures a real source. Mirrors WriteBuiltinToolSources:
// existing files are never overwritten, so a stranger's later hand edits
// or a second `aida init` run are safe.
func WriteExampleSources(rootPath string, exampleSources fs.FS, dir string) (int, error) {
	entries, err := fs.ReadDir(exampleSources, dir)
	if err != nil {
		return 0, fmt.Errorf("reading embedded example sources: %w", err)
	}

	srcDir := filepath.Join(rootPath, "sources")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		return 0, err
	}

	manifest, err := LoadManifest(rootPath)
	if err != nil {
		return 0, err
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]SourceEntry{}
	}

	written := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".yaml" {
			continue
		}
		cfgRel := filepath.Join("sources", name)
		cfgAbs := filepath.Join(rootPath, cfgRel)
		// Don't overwrite user customizations (or a previous init run).
		if _, err := os.Stat(cfgAbs); err == nil {
			continue
		}
		data, readErr := fs.ReadFile(exampleSources, filepath.Join(dir, name))
		if readErr != nil {
			return written, fmt.Errorf("reading embedded %s: %w", name, readErr)
		}
		if writeErr := os.WriteFile(cfgAbs, data, 0644); writeErr != nil {
			return written, fmt.Errorf("writing %s: %w", cfgRel, writeErr)
		}
		key := strings.TrimSuffix(name, ".yaml")
		if _, exists := manifest.Sources[key]; !exists {
			manifest.Sources[key] = SourceEntry{File: cfgRel}
		}
		written++
	}

	if written > 0 {
		if err := SaveManifest(rootPath, manifest); err != nil {
			return written, err
		}
	}
	return written, nil
}

// EnsureDefaultRegistered makes sure ~/.aida/library.yaml exists with at
// least a default root pointing at <aidaDir>/library. If the registry file
// already exists, it is left untouched. Returns the (possibly newly written)
// config.
func EnsureDefaultRegistered(aidaDir string) (*LibraryConfig, error) {
	path := ConfigPath(aidaDir)
	if _, err := os.Stat(path); err == nil {
		return LoadConfig(aidaDir)
	}
	cfg := DefaultConfig(aidaDir)
	if err := SaveConfig(aidaDir, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
