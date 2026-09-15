package library

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
)

// ImportResult summarizes a migration of legacy sources.yaml
// into a library root.
type ImportResult struct {
	RootPath       string
	SourceLayers   []string // layer names written from source CLAUDE.md
	SourceConfigs  []string // source config files written under sources/
	SkippedSources []string // sources without a context file
}

// ImportLegacy migrates the existing ~/.aida/sources.yaml content into
// <rootPath> as library layers. The legacy file is not modified or
// deleted -- it continues to drive the existing query path until Step 6
// deprecates it.
//
// Outputs (created if missing, otherwise overwritten):
//
//	<rootPath>/layers/sources/<source-name>.md
//
// Each written layer is also added to the root's library.yaml manifest.
func ImportLegacy(rootPath string) (*ImportResult, error) {
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return nil, err
	}

	srcs, err := config.LoadSources()
	if err != nil {
		return nil, fmt.Errorf("loading sources.yaml: %w", err)
	}
	manifest, err := LoadManifest(rootPath)
	if err != nil {
		return nil, err
	}
	if manifest.Layers == nil {
		manifest.Layers = make(map[string]LayerEntry)
	}
	if manifest.Sources == nil {
		manifest.Sources = make(map[string]SourceEntry)
	}
	if manifest.Name == "" {
		manifest.Name = "imported"
	}

	result := &ImportResult{RootPath: rootPath}

	// Ensure target directories exist.
	for _, sub := range []string{
		filepath.Join("layers", "sources"),
		"sources",
	} {
		if err := os.MkdirAll(filepath.Join(rootPath, sub), 0755); err != nil {
			return nil, err
		}
	}

	srcNames := make([]string, 0, len(srcs))
	for name := range srcs {
		srcNames = append(srcNames, name)
	}
	sort.Strings(srcNames)

	for _, name := range srcNames {
		src := srcs[name]

		// 1. Write the source config file (machine-readable, used by the
		//    library-driven planner). The legacy ~/.aida/sources.yaml
		//    will be loaded as a fallback only when the library has none.
		cfgRel := filepath.Join("sources", name+".yaml")
		cfgAbs := filepath.Join(rootPath, cfgRel)
		cfgYAML, err := yaml.Marshal(src)
		if err != nil {
			return nil, fmt.Errorf("marshaling source %q: %w", name, err)
		}
		if err := os.WriteFile(cfgAbs, cfgYAML, 0644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", cfgRel, err)
		}
		manifest.Sources[name] = SourceEntry{File: cfgRel}
		result.SourceConfigs = append(result.SourceConfigs, name)

		// 2. Write the per-source markdown layer (LLM-readable context),
		//    if there's a context file to copy.
		body, _ := src.LoadContextFile()
		if strings.TrimSpace(body) == "" {
			result.SkippedSources = append(result.SkippedSources, name)
			continue
		}
		header := fmt.Sprintf("# Source: %s\n\n", name)
		if src.Description != "" {
			header += src.Description + "\n\n"
		}
		layerName := "sources/" + name
		relFile := filepath.Join("layers", "sources", name+".md")
		absFile := filepath.Join(rootPath, relFile)
		if err := os.WriteFile(absFile, []byte(header+body), 0644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", relFile, err)
		}
		manifest.Layers[layerName] = LayerEntry{File: relFile}
		result.SourceLayers = append(result.SourceLayers, layerName)
	}

	if err := SaveManifest(rootPath, manifest); err != nil {
		return nil, fmt.Errorf("writing manifest: %w", err)
	}

	return result, nil
}
