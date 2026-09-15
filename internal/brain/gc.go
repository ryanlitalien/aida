package brain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// GCResult holds the results of a garbage collection run.
type GCResult struct {
	ArchivedEvidence  int
	StaleTruth        []string
	DuplicateEntities []string
	OldLessons        int
	TotalCleaned      int
}

// GC performs garbage collection on the brain:
// 1. Archives evidence lines older than maxDays
// 2. Flags entity pages with stale compiled truth
// 3. Reports duplicate entity slugs
// 4. Counts lessons older than maxDays
func (b *Brain) GC(maxDays int) (*GCResult, error) {
	if maxDays <= 0 {
		maxDays = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -maxDays)
	result := &GCResult{}

	// 1. Archive old evidence from entity pages
	for _, entityType := range []string{"partners", "tools", "people"} {
		dir := filepath.Join(b.Path, "entities", entityType)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			archived, err := archiveOldEvidence(path, cutoff)
			if err != nil {
				continue
			}
			result.ArchivedEvidence += archived
		}
	}

	// 2. Flag stale compiled truth - pages where the compiled section
	// hasn't been updated but new evidence has been added since.
	for _, entityType := range []string{"partners", "tools", "people"} {
		dir := filepath.Join(b.Path, "entities", entityType)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if isStaleCompiledTruth(path) {
				slug := strings.TrimSuffix(entry.Name(), ".md")
				result.StaleTruth = append(result.StaleTruth, entityType+"/"+slug)
			}
		}
	}

	// 3. Count old lessons
	var oldCount int
	b.DB.conn.QueryRow("SELECT COUNT(*) FROM lessons WHERE timestamp < ?",
		cutoff.Format(time.RFC3339)).Scan(&oldCount)
	result.OldLessons = oldCount

	result.TotalCleaned = result.ArchivedEvidence
	return result, nil
}

// archiveOldEvidence reads an entity page and moves evidence lines older
// than cutoff into an "Archived Evidence" section. Returns count of lines archived.
func archiveOldEvidence(path string, cutoff time.Time) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	content := string(data)

	// Find the evidence trail section
	hrIdx := strings.Index(content, "\n---\n")
	if hrIdx < 0 {
		return 0, nil
	}

	compiled := content[:hrIdx]
	evidenceSection := content[hrIdx+5:]

	lines := strings.Split(evidenceSection, "\n")
	var current, archived []string
	archivedCount := 0

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			current = append(current, line)
			continue
		}
		// Parse date from "- 2026-04-11: ..."
		dateStr := strings.TrimPrefix(trimmed, "- ")
		if len(dateStr) >= 10 {
			dateStr = dateStr[:10]
			if t, err := time.Parse("2006-01-02", dateStr); err == nil && t.Before(cutoff) {
				archived = append(archived, line)
				archivedCount++
				continue
			}
		}
		current = append(current, line)
	}

	if archivedCount == 0 {
		return 0, nil
	}

	// Rebuild page: compiled + HR + current evidence + archived section
	var newContent strings.Builder
	newContent.WriteString(compiled)
	newContent.WriteString("\n---\n")
	newContent.WriteString(strings.Join(current, "\n"))
	if len(archived) > 0 {
		newContent.WriteString("\n\n## Archived Evidence (>" + fmt.Sprintf("%d", cutoff.Year()) + ")\n\n")
		newContent.WriteString(strings.Join(archived, "\n"))
	}
	newContent.WriteString("\n")

	return archivedCount, os.WriteFile(path, []byte(newContent.String()), 0644)
}

// isStaleCompiledTruth checks if an entity page has the default placeholder
// compiled truth (meaning it was never compiled).
func isStaleCompiledTruth(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	content := string(data)

	// Check if it has the default placeholder
	if strings.Contains(content, "No compiled truth yet") {
		// Only flag as stale if there IS evidence below
		if strings.Contains(content, "## Evidence Trail") {
			hrIdx := strings.Index(content, "\n---\n")
			if hrIdx >= 0 {
				evidence := strings.TrimSpace(content[hrIdx+5:])
				// Has at least one evidence line
				return strings.Contains(evidence, "- 20")
			}
		}
	}
	return false
}
