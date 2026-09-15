package index

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
)

// classificationResult is the JSON schema the LLM returns.
type classificationResult struct {
	Type         string   `json:"type"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
}

const classifySystemPrompt = `You are classifying a software project based on its documentation file.
Return a JSON object with:
- type: one of "codebase", "data-source", "docs", "tool", "app"
- description: a one-sentence description of what this project does (do NOT start with "This")
- capabilities: array of relevant strings from this list:
  code-reference, sql-query, log-query, error-investigation, api-testing,
  partner-docs, csv-data, task-management, documentation, git-history,
  gmv-analysis, checkout-analytics, ari-lookup, partner-lookup, pr-lookup,
  metrics, search, fitness, nutrition, health, billing, expenses`

func classificationSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"type": map[string]interface{}{
				"type": "string",
				"enum": []string{"codebase", "data-source", "docs", "tool", "app"},
			},
			"description": map[string]interface{}{
				"type": "string",
			},
			"capabilities": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "string"},
			},
		},
		"required":             []string{"type", "description", "capabilities"},
		"additionalProperties": false,
	}
}

// GenerateSourceEntry creates a basic source entry using heuristics only
// (no LLM call). Used as the fallback when LLM is unavailable.
func GenerateSourceEntry(folder ScannedFolder) GeneratedSource {
	name := filepath.Base(folder.Path)

	return GeneratedSource{
		Name:         strings.ToLower(name),
		Path:         folder.Path,
		Type:         "codebase",
		Context:      folder.DocFile,
		Description:  name + " codebase",
		Capabilities: []string{"code-reference"},
	}
}

// GeneratedSource holds the result of source classification.
type GeneratedSource struct {
	Name         string
	Path         string
	Type         string
	Context      string
	Description  string
	Capabilities []string
}

// GenerateSourceEntryLLM reads the doc file and calls the LLM to classify
// the source. Falls back to GenerateSourceEntry on any error.
func GenerateSourceEntryLLM(ctx context.Context, client *llm.Client, folder ScannedFolder) GeneratedSource {
	docPath := filepath.Join(folder.Path, folder.DocFile)
	content, err := os.ReadFile(docPath)
	if err != nil || len(content) == 0 {
		return GenerateSourceEntry(folder)
	}

	// Truncate to avoid token limits
	doc := string(content)
	if len(doc) > 4000 {
		doc = doc[:4000]
	}

	userPrompt := fmt.Sprintf("Project directory: %s\nDoc file: %s\n\nContents:\n%s",
		folder.Path, folder.DocFile, doc)

	raw, err := client.CompleteJSON(ctx, classifySystemPrompt, userPrompt, classificationSchema())
	if err != nil {
		return GenerateSourceEntry(folder)
	}

	var result classificationResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return GenerateSourceEntry(folder)
	}

	if result.Type == "" || result.Description == "" {
		return GenerateSourceEntry(folder)
	}

	name := filepath.Base(folder.Path)
	return GeneratedSource{
		Name:         strings.ToLower(name),
		Path:         folder.Path,
		Type:         result.Type,
		Context:      folder.DocFile,
		Description:  result.Description,
		Capabilities: result.Capabilities,
	}
}
