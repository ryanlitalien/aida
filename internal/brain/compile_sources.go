package brain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
	"gopkg.in/yaml.v3"
)

const domainCompileSystemPromptWithContext = `You are analyzing a data source for an LLM-powered query orchestrator called Aida.
Given the source's configuration and context documentation, extract a semantic routing profile.

Return markdown with EXACTLY these sections:

## What This Source Does
One paragraph explaining what data/capabilities this source provides to the user.

## When To Route Here
Keywords: comma-separated list of 10-20 words/phrases that should trigger routing to this source.
Include synonyms, abbreviations, slang, and natural language variations a user might say.
Think about what questions a human would ask that this source can answer.

## When NOT To Route Here
Bullet list of 2-3 common misroutes to avoid (e.g., "code questions about this repo → use as codebase, not this adapter").

## How To Query
Brief notes on query format, schema, or special requirements the LLM needs to know when constructing queries.

## Related Sources
Bullet list of which other sources complement this one (if any are obvious from the config).`

const domainCompileSystemPromptWithoutContext = `You are analyzing a data source for an LLM-powered query orchestrator called Aida.
This source has NO documentation. Infer its purpose from the folder structure and configuration.

Return markdown with EXACTLY these sections:

## What This Source Does
One paragraph inferring what this source provides based on folder contents and config.

## When To Route Here
Keywords: comma-separated list of 10-15 words/phrases that should trigger routing to this source.
Infer from the folder name, file types, and structure what questions this source might answer.

## When NOT To Route Here
Bullet list of 2-3 common misroutes to avoid.

## How To Query
Brief notes on how to interact with this source type.

## Related Sources
Leave empty if unknown.`

// CompileSources generates semantic domain profiles for library sources.
// For each source, reads its config + context doc (or folder structure),
// calls the LLM to generate a routing profile, and writes it to
// brain/knowledge/domains/{source}.md.
func CompileSources(ctx context.Context, client *llm.Client, brainPath, libraryPath string) (int, error) {
	domainsDir := filepath.Join(brainPath, "knowledge", "domains")
	if err := os.MkdirAll(domainsDir, 0755); err != nil {
		return 0, err
	}

	// Load all source configs from the library
	sourcesDir := filepath.Join(libraryPath, "sources")
	entries, err := os.ReadDir(sourcesDir)
	if err != nil {
		return 0, fmt.Errorf("reading sources dir: %w", err)
	}

	compiled := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".yaml")

		// Read source config
		srcPath := filepath.Join(sourcesDir, entry.Name())
		srcData, err := os.ReadFile(srcPath)
		if err != nil {
			continue
		}
		var src config.Source
		if err := yaml.Unmarshal(srcData, &src); err != nil {
			continue
		}

		// Read context layer (if exists)
		layerPath := filepath.Join(libraryPath, "layers", "sources", name+".md")
		contextDoc := ""
		if data, err := os.ReadFile(layerPath); err == nil {
			contextDoc = string(data)
			// Truncate very long context docs to avoid token waste
			if len(contextDoc) > 4000 {
				contextDoc = contextDoc[:4000] + "\n\n[truncated]"
			}
		}

		// Build the LLM prompt
		var systemPrompt, userPrompt string
		srcYAML := string(srcData)

		if contextDoc != "" {
			systemPrompt = domainCompileSystemPromptWithContext
			userPrompt = fmt.Sprintf("Source name: %s\n\nSource config:\n```yaml\n%s```\n\nContext documentation:\n%s", name, srcYAML, contextDoc)
		} else {
			systemPrompt = domainCompileSystemPromptWithoutContext
			// Scan folder structure for inference
			folderStructure := InferFolderStructure(src.Path)
			userPrompt = fmt.Sprintf("Source name: %s\n\nSource config:\n```yaml\n%s```\n\nFolder structure:\n```\n%s```", name, srcYAML, folderStructure)
		}

		// Generate domain profile via LLM
		profile, err := client.Complete(ctx, systemPrompt, userPrompt)
		if err != nil {
			ui.PrintVerbose("Domain compile", fmt.Sprintf("LLM error for %s: %s", name, err))
			continue
		}

		// Write domain profile
		header := fmt.Sprintf("# %s\n\n", name)
		domainPath := filepath.Join(domainsDir, name+".md")
		if err := os.WriteFile(domainPath, []byte(header+profile), 0644); err != nil {
			ui.PrintVerbose("Domain compile", fmt.Sprintf("write error for %s: %s", name, err))
			continue
		}

		compiled++
		ui.PrintVerbose("Domain compile", fmt.Sprintf("%s → brain/knowledge/domains/%s.md", name, name))
	}

	return compiled, nil
}

// InferFolderStructure scans a folder up to 2 levels deep and returns
// a tree-like listing for the LLM to infer the source's purpose.
func InferFolderStructure(path string) string {
	if path == "" {
		return "(no path configured)"
	}
	path = config.ExpandPath(path)

	var lines []string
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Sprintf("(cannot read %s: %s)", path, err)
	}

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, ".venv": true, "venv": true,
		"__pycache__": true, "dist": true, "build": true, ".next": true,
		".cache": true, ".idea": true, ".vscode": true, "vendor": true,
		"Pods": true, ".build": true, "DerivedData": true,
	}

	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") && name != ".mcp.json" {
			continue
		}
		if skipDirs[name] {
			continue
		}

		if entry.IsDir() {
			lines = append(lines, name+"/")
			// One level deeper
			subEntries, err := os.ReadDir(filepath.Join(path, name))
			if err != nil {
				continue
			}
			count := 0
			for _, sub := range subEntries {
				if count >= 10 {
					remaining := len(subEntries) - count
					lines = append(lines, fmt.Sprintf("  ... and %d more", remaining))
					break
				}
				if strings.HasPrefix(sub.Name(), ".") {
					continue
				}
				prefix := "  "
				suffix := ""
				if sub.IsDir() {
					suffix = "/"
				}
				lines = append(lines, prefix+sub.Name()+suffix)
				count++
			}
		} else {
			lines = append(lines, name)
		}

		if len(lines) > 50 {
			lines = append(lines, "... (truncated)")
			break
		}
	}

	return strings.Join(lines, "\n")
}
