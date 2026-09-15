package sources

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ryanlitalien/aida/internal/config"
)

// DocsAdapter handles "docs" sources by returning the context file
// (CLAUDE.md, README.md) as a single artifact. Unlike GrepAdapter, which
// searches the source directory with a grep pattern, DocsAdapter treats the
// context document as the primary data source - the synthesizer LLM reads
// the document and extracts the answer directly.
type DocsAdapter struct{}

// Execute reads the source's context file and returns it as an artifact.
// The "command" argument (the user's raw question) is stored for logging
// but not used for file selection - the context file path comes from the
// source config.
func (a *DocsAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "docs",
		Status: "success",
	}

	contextDoc, err := src.LoadContextFile()
	if err != nil || contextDoc == "" {
		result.Status = "empty"
		result.Summary = "No context document found"
		return result, nil
	}

	result.Artifacts = []Artifact{{
		Type:    "page",
		ID:      src.Context,
		Snippet: contextDoc,
	}}
	result.Data = json.RawMessage(fmt.Sprintf(`{"doc_length":%d}`, len(contextDoc)))
	result.Summary = fmt.Sprintf("Loaded context document: %s (%d chars)", src.Context, len(contextDoc))
	return result, nil
}

// ParseOutput wraps raw content as a single page artifact.
func (a *DocsAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return []Artifact{{
		Type:    "page",
		ID:      "context",
		Snippet: string(raw),
	}}, nil
}
