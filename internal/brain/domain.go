package brain

import (
	"os"
	"path/filepath"
	"strings"
)

// DomainKeywords maps source name → list of keywords that should trigger
// routing to that source. Loaded from brain/knowledge/domains/*.md files.
type DomainKeywords map[string][]string

// LoadDomainKeywords reads all domain profile files from brain/knowledge/domains/
// and parses the "Keywords:" line from the "When To Route Here" section.
func LoadDomainKeywords(brainPath string) DomainKeywords {
	domainsDir := filepath.Join(brainPath, "knowledge", "domains")
	entries, err := os.ReadDir(domainsDir)
	if err != nil {
		return nil
	}

	result := make(DomainKeywords)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".md")
		path := filepath.Join(domainsDir, entry.Name())
		keywords := parseDomainKeywords(path)
		if len(keywords) > 0 {
			result[name] = keywords
		}
	}
	return result
}

// parseDomainKeywords reads a domain profile and extracts keywords from
// the "When To Route Here" section. Supports two formats:
//  1. "Keywords: term1, term2, term3" on a line
//  2. The entire section body as a comma-separated list (no prefix)
func parseDomainKeywords(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	content := string(data)

	// Find the "When To Route Here" section
	sectionIdx := strings.Index(content, "## When To Route Here")
	if sectionIdx < 0 {
		return nil
	}
	section := content[sectionIdx+len("## When To Route Here"):]

	// Find the next section boundary
	nextSection := strings.Index(section, "\n## ")
	if nextSection > 0 {
		section = section[:nextSection]
	}

	// Try "Keywords:" prefix first
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Keywords:") {
			return splitKeywords(strings.TrimPrefix(trimmed, "Keywords:"))
		}
	}

	// Fall back: treat the entire section body as comma-separated keywords
	body := strings.TrimSpace(section)
	if body != "" {
		return splitKeywords(body)
	}

	return nil
}

func splitKeywords(s string) []string {
	var keywords []string
	for _, kw := range strings.Split(s, ",") {
		kw = strings.TrimSpace(strings.ToLower(kw))
		if kw != "" && len(kw) > 1 {
			keywords = append(keywords, kw)
		}
	}
	return keywords
}
