package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
)

// CompileRoutingWisdom reads all lessons from brain.db and uses an LLM
// to synthesize compiled routing guidance. Writes the result to
// knowledge/routing/compiled.md.
func (b *Brain) CompileRoutingWisdom(ctx context.Context, client *llm.Client) error {
	lessons, err := b.loadAllLessonsForCompile()
	if err != nil {
		return fmt.Errorf("loading lessons: %w", err)
	}
	if len(lessons) < 3 {
		return fmt.Errorf("need at least 3 lessons to compile (have %d)", len(lessons))
	}

	lessonsJSON, err := json.MarshalIndent(lessons, "", "  ")
	if err != nil {
		return err
	}

	systemPrompt := `You are compiling routing wisdom for an LLM-powered query orchestrator called Aida.
Below are lessons from past queries - each records which sources were used, whether they
worked, confidence scores, and any user feedback.

Analyze the patterns and write a routing guide in markdown. For each source, describe:
- What query types/strategies it's strong for (with confidence and sample count)
- What it's weak for or should be avoided for
- Any user feedback patterns (thumbs-down reasons, intended alternatives)

Be specific and evidence-based. Only include sources with >= 3 lessons.
Do NOT include raw evidence - this is the compiled summary.
Write in second person ("use X for...", "avoid Y when...").`

	userPrompt := fmt.Sprintf("## Lessons (%d total)\n\n%s", len(lessons), string(lessonsJSON))

	answer, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return fmt.Errorf("LLM compile: %w", err)
	}

	wisdomPath := filepath.Join(b.Path, "knowledge", "routing", "compiled.md")
	if err := os.WriteFile(wisdomPath, []byte(answer), 0644); err != nil {
		return fmt.Errorf("writing compiled wisdom: %w", err)
	}

	ui.PrintVerbose("Brain compile", fmt.Sprintf("wrote %d bytes to knowledge/routing/compiled.md", len(answer)))
	return nil
}

// CompileEntityPage re-synthesizes the compiled truth for a single entity page.
func (b *Brain) CompileEntityPage(ctx context.Context, client *llm.Client, pagePath string) error {
	fullPath := filepath.Join(b.Path, pagePath)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Errorf("reading page: %w", err)
	}
	content := string(data)

	// Split at HR to get evidence
	parts := strings.SplitN(content, "\n---\n", 2)
	evidence := ""
	if len(parts) == 2 {
		evidence = parts[1]
	}
	if strings.TrimSpace(evidence) == "" {
		return fmt.Errorf("no evidence trail found in %s", pagePath)
	}

	systemPrompt := `You are updating a brain page in a knowledge system for an LLM-powered query orchestrator.
Below is the current page content and its evidence trail.

Rewrite ONLY the section above the horizontal rule with an updated summary that
incorporates all the evidence. Keep it concise - this will be loaded into an LLM
context window during queries. Focus on routing-relevant facts: what works, what
doesn't, known issues, key identifiers.

Output ONLY the content above the horizontal rule (the compiled truth section).
Do NOT include the evidence trail or horizontal rule in your output.`

	userPrompt := fmt.Sprintf("## Current Page\n\n%s", content)

	answer, err := client.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return fmt.Errorf("LLM compile: %w", err)
	}

	// Reconstruct page: new compiled truth + HR + existing evidence
	newContent := strings.TrimSpace(answer) + "\n\n---\n" + evidence
	if err := os.WriteFile(fullPath, []byte(newContent), 0644); err != nil {
		return fmt.Errorf("writing page: %w", err)
	}

	return nil
}

type compileLessonSummary struct {
	Question       string            `json:"question"`
	Action         string            `json:"action"`
	Strategy       string            `json:"strategy"`
	Sources        []string          `json:"sources"`
	PerSource      map[string]string `json:"per_source_status"`
	Quality        int               `json:"quality"`
	Feedback       string            `json:"feedback,omitempty"`
	FeedbackReason string            `json:"feedback_reason,omitempty"`
}

func (b *Brain) loadAllLessonsForCompile() ([]compileLessonSummary, error) {
	rows, err := b.DB.conn.Query(`
		SELECT question, action, strategy, sources_used, per_source_status,
		       quality, feedback, feedback_reason
		FROM lessons
		WHERE COALESCE(superseded, 0) = 0
		ORDER BY timestamp DESC
		LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []compileLessonSummary
	for rows.Next() {
		var l compileLessonSummary
		var sourcesJSON, perSourceJSON string
		if err := rows.Scan(&l.Question, &l.Action, &l.Strategy,
			&sourcesJSON, &perSourceJSON, &l.Quality, &l.Feedback, &l.FeedbackReason); err != nil {
			continue
		}
		json.Unmarshal([]byte(sourcesJSON), &l.Sources)
		json.Unmarshal([]byte(perSourceJSON), &l.PerSource)
		out = append(out, l)
	}
	return out, nil
}

// Index rebuilds brain.db from all lesson JSON files and entity markdown pages.
func (b *Brain) Index(ctx context.Context) error {
	// 1. Index lessons from JSON files
	lessonsDir := filepath.Join(b.Path, "lessons")
	lessonCount := 0
	err := filepath.Walk(lessonsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var record LessonRecord
		if err := json.Unmarshal(data, &record); err != nil {
			ui.PrintVerbose("Brain index", fmt.Sprintf("skip %s: %s", path, err))
			return nil
		}

		// Generate embedding if missing and embeddings are available
		if record.Embedding == nil && b.Embeddings.Available() && record.Question != "" {
			if emb, err := b.Embeddings.EmbedQuery(ctx, record.Question); err == nil {
				record.Embedding = emb
			}
		}

		if err := b.DB.InsertLesson(&record); err != nil {
			ui.PrintVerbose("Brain index", fmt.Sprintf("insert error %s: %s", record.ID, err))
		} else {
			lessonCount++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking lessons: %w", err)
	}

	// 2. Index entities from markdown pages
	entityCount := 0
	for _, entityType := range []string{"partners", "tools", "people"} {
		entityDir := filepath.Join(b.Path, "entities", entityType)
		entries, err := os.ReadDir(entityDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			slug := strings.TrimSuffix(entry.Name(), ".md")
			pagePath := filepath.Join("entities", entityType, entry.Name())
			content := loadCompiledTruth(filepath.Join(b.Path, pagePath))

			// Extract title as name (first # heading)
			name := slug
			for _, line := range strings.Split(content, "\n") {
				if strings.HasPrefix(line, "# ") {
					name = strings.TrimPrefix(line, "# ")
					// Strip parenthetical type suffix like "Sqlite (Tool)"
					if idx := strings.LastIndex(name, " ("); idx > 0 {
						name = name[:idx]
					}
					break
				}
			}

			record := &EntityRecord{
				Slug:     slug,
				Type:     strings.TrimSuffix(entityType, "s"), // partners → partner
				Name:     name,
				PagePath: pagePath,
				Summary:  truncateString(content, 500),
			}

			if b.Embeddings.Available() && content != "" {
				if emb, err := b.Embeddings.EmbedQuery(ctx, content); err == nil {
					record.Embedding = emb
				}
			}

			if err := b.DB.UpsertEntity(record); err != nil {
				ui.PrintVerbose("Brain index", fmt.Sprintf("entity error %s: %s", slug, err))
			} else {
				entityCount++
			}
		}
	}

	// 3. Index tasks from markdown files. IndexTasks already logs each
	// skipped file itself (see its doc comment), so this call site doesn't
	// need to re-log the returned []error - it just needs the count.
	taskCount, _ := b.IndexTasks()

	// 4. Count existing domain profiles (generated by scan or compile-sources)
	domainCount := 0
	domainsDir := filepath.Join(b.Path, "knowledge", "domains")
	if entries, err := os.ReadDir(domainsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				domainCount++
			}
		}
	}

	// 5. Index typed-memory records (facts/events/instructions) from their
	// JSON mirrors. Supersession state lives in brain.db only (every JSON
	// mirror has superseded_by=""), so it's recomputed below from Created
	// order after all records are inserted.
	memoryCount := 0
	var indexedMemories []MemoryRecord
	for _, memType := range []MemoryType{MemoryFact, MemoryEvent, MemoryInstruction} {
		typeDir := filepath.Join(MemoryDir(b.Path), string(memType))
		entries, err := os.ReadDir(typeDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			record, err := ReadMemoryFile(b.Path, memType, id)
			if err != nil {
				ui.PrintVerbose("Brain index", fmt.Sprintf("skip %s/%s: %s", memType, id, err))
				continue
			}

			// Generate embedding if missing and embeddings are available.
			if record.Embedding == nil && b.Embeddings.Available() && record.Body != "" {
				if emb, err := b.Embeddings.EmbedQuery(ctx, record.Body); err == nil {
					record.Embedding = emb
				}
			}

			if err := b.DB.InsertMemory(record); err != nil {
				ui.PrintVerbose("Brain index", fmt.Sprintf("memory error %s: %s", record.ID, err))
				continue
			}
			memoryCount++
			indexedMemories = append(indexedMemories, *record)
		}
	}

	// Recompute supersession: for each (type, key, profile) group that
	// supersedes by key, the newest-Created record wins and every other
	// active record in the group gets marked superseded.
	type memGroupKey struct {
		typ     MemoryType
		key     string
		profile string
	}
	groups := make(map[memGroupKey][]MemoryRecord)
	for _, m := range indexedMemories {
		if !m.Type.SupersedesByKey() || m.Key == "" {
			continue
		}
		gk := memGroupKey{typ: m.Type, key: m.Key, profile: m.Profile}
		groups[gk] = append(groups[gk], m)
	}
	for gk, members := range groups {
		newest := members[0]
		for _, m := range members[1:] {
			if m.Created > newest.Created {
				newest = m
			}
		}
		if err := b.DB.SupersedePriorMemory(gk.typ, gk.key, gk.profile, newest.ID); err != nil {
			ui.PrintVerbose("Brain index", fmt.Sprintf("supersede error %s/%s: %s", gk.typ, gk.key, err))
		}
	}

	fmt.Printf("Indexed %d lessons, %d entities, %d tasks, %d domains, %d memories\n", lessonCount, entityCount, taskCount, domainCount, memoryCount)
	return nil
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
