package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
)

// InventoryAdapter answers "what projects/sources do I have?" by enumerating
// every source yaml under the library's sources/ directory, filtered to the
// active profile.
//
// No LLM call, no shell exec - the answer is a deterministic projection of
// the registry the user has already curated. Bypasses LLM Call #2 in the
// executor (see executor.go), so the `command` argument is the user's raw
// question and is recorded only for logging.
//
// Reads ~/.aida/library/sources/*.yaml directly to avoid an import cycle
// (sources -> library -> ui -> sources). Multi-root libraries are not yet
// supported by this adapter; the user's library.yaml is single-root.
type InventoryAdapter struct{}

func (a *InventoryAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source:  "library-inventory",
		Status:  "success",
		Command: command,
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("loading config: %v", err)
		return result, err
	}
	_, profileName := cfg.ActiveProfileConfig()

	sourcesDir := filepath.Join(config.Dir(), "library", "sources")
	entries, err := filepath.Glob(filepath.Join(sourcesDir, "*.yaml"))
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("globbing %s: %v", sourcesDir, err)
		return result, err
	}

	type loaded struct {
		name string
		src  *config.Source
	}
	var all []loaded
	for _, path := range entries {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s config.Source
		if err := yaml.Unmarshal(data, &s); err != nil {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")
		all = append(all, loaded{name: name, src: &s})
	}

	// Filter by active profile. Empty Profiles list = available to every profile.
	filtered := make([]loaded, 0, len(all))
	for _, l := range all {
		if profileName == "" || len(l.src.Profiles) == 0 {
			filtered = append(filtered, l)
			continue
		}
		for _, p := range l.src.Profiles {
			if p == profileName {
				filtered = append(filtered, l)
				break
			}
		}
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].name < filtered[j].name })

	artifacts := make([]Artifact, 0, len(filtered))
	for _, l := range filtered {
		s := l.src
		entry := map[string]interface{}{
			"name":         l.name,
			"type":         s.Type,
			"description":  s.Description,
			"capabilities": s.Capabilities,
			"profiles":     s.Profiles,
		}
		if s.Repo != "" {
			entry["repo"] = s.Repo
			entry["url"] = "https://github.com/" + s.Repo
		}
		if s.Path != "" {
			entry["path"] = s.Path
		}
		raw, _ := json.Marshal(entry)

		artifacts = append(artifacts, Artifact{
			Type:    "source",
			ID:      sanitizeRowID(l.name),
			Snippet: string(raw),
		})
	}

	ensureUniqueIDs(artifacts)
	result.Artifacts = artifacts
	result.Summary = fmt.Sprintf("%d sources active in profile %q", len(artifacts), profileName)

	names := make([]string, len(filtered))
	for i, l := range filtered {
		names[i] = l.name
	}
	if data, err := json.Marshal(map[string]interface{}{
		"profile": profileName,
		"count":   len(artifacts),
		"sources": names,
	}); err == nil {
		result.Data = data
	}

	return result, nil
}

func (a *InventoryAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return nil, nil
}

func init() {
	RegisterAdapter("inventory", &InventoryAdapter{})
	RegisterAdapter("library-inventory", &InventoryAdapter{})
}

var _ Adapter = (*InventoryAdapter)(nil)
