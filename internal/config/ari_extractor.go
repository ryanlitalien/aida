package config

import (
	"fmt"
	"regexp"
	"strings"
)

// ExtractedID represents an identifier found in free text with an
// inferred semantic key name based on surrounding context (table headers,
// environment labels, or whatever categories/modifiers the configured
// IDPattern defines).
type ExtractedID struct {
	Value       string // the matched ID token, e.g. a 16-char alphanumeric string
	InferredKey string // e.g. "primary_id_test_ca" - see IDPattern
	RawLabel    string // surrounding text hint (e.g. a table row's name column)
}

// IDPattern configures one identifier shape this extractor recognizes --
// e.g. "16-character uppercase alphanumeric resource IDs used by a
// particular data source." This package ships no domain-specific
// taxonomy of its own; configure real patterns under `id_patterns:` in
// config.yaml to get semantic key inference tailored to your own sources.
type IDPattern struct {
	// Name is the inferred-key prefix used when no Category matches
	// (or when Categories is empty).
	Name string `yaml:"name"`
	// Regex matches the ID token itself.
	Regex string `yaml:"regex"`
	// Categories classify a match by scanning its surrounding context
	// (the current section header plus the matched line, lower-cased) for
	// a substring. The first matching category wins; list a category with
	// no Match entries last to act as a catch-all default.
	Categories []IDCategory `yaml:"categories,omitempty"`
	// Modifiers each append "_<Suffix>" to the inferred key when any of
	// their Match substrings appear in context. Evaluated in order; every
	// modifier that matches is applied (e.g. an environment modifier and
	// a region modifier can both fire on the same match).
	Modifiers []IDModifier `yaml:"modifiers,omitempty"`
}

// IDCategory names one context-based classification bucket for IDPattern.
type IDCategory struct {
	Key   string   `yaml:"key"`
	Match []string `yaml:"match,omitempty"` // case-insensitive substrings; empty = catch-all
}

// IDModifier appends a suffix to the inferred key when any of its Match
// substrings appear in the surrounding context.
type IDModifier struct {
	Suffix string   `yaml:"suffix"`
	Match  []string `yaml:"match"`
}

// DefaultIDPatterns is used when config.yaml defines no id_patterns: a
// generic 16-character uppercase-alphanumeric resource ID with no
// category/modifier classification -- every match just infers the key
// "id" (deduplicated via resolveCollisions into "id_2", "id_3", ...).
// Configure your own patterns in config.yaml for semantic key names.
func DefaultIDPatterns() []IDPattern {
	return []IDPattern{{Name: "id", Regex: `\b[A-Z0-9]{16}\b`}}
}

// ExtractIDsFromText finds every ID matching the given patterns in text,
// deduplicates against existingIDs, and infers a semantic key name for
// each match from its surrounding context (markdown table rows, section
// headers, inline labels) using that pattern's Categories/Modifiers. An
// empty patterns list falls back to DefaultIDPatterns.
func ExtractIDsFromText(text string, existingIDs map[string]string, patterns []IDPattern) ([]ExtractedID, error) {
	if len(patterns) == 0 {
		patterns = DefaultIDPatterns()
	}
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("id pattern %q: invalid regex %q: %w", p.Name, p.Regex, err)
		}
		compiled[i] = re
	}

	// Build a set of already-known ID values for fast lookup.
	known := make(map[string]bool, len(existingIDs))
	for _, v := range existingIDs {
		known[strings.ToUpper(v)] = true
	}

	lines := strings.Split(text, "\n")

	// Detect the current section header so context-based categories and
	// modifiers can see it (e.g. a "## Staging" heading over a table).
	var sectionHeader string
	var results []ExtractedID
	seen := make(map[string]bool) // deduplicate within this extraction

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track section headers (markdown ## or **bold** headers).
		if isHeaderLine(trimmed) {
			sectionHeader = trimmed
			continue
		}

		// Skip table separator rows (|---|---|---|).
		if isTableSeparator(trimmed) {
			continue
		}

		combined := strings.ToLower(sectionHeader + " " + trimmed)
		label := extractLabel(trimmed)

		for pi, re := range compiled {
			for _, value := range re.FindAllString(trimmed, -1) {
				upper := strings.ToUpper(value)
				if known[upper] || seen[upper] {
					continue
				}
				seen[upper] = true

				results = append(results, ExtractedID{
					Value:       value,
					InferredKey: inferKeyName(patterns[pi], combined),
					RawLabel:    label,
				})
			}
		}
	}

	// Resolve key collisions: if two IDs infer the same key, append _2, _3.
	resolveCollisions(results, existingIDs)

	return results, nil
}

// inferKeyName builds a semantic key name from a pattern's configured
// Categories and Modifiers against the lower-cased combined context
// (section header + matched line).
func inferKeyName(pattern IDPattern, combinedContext string) string {
	key := pattern.Name
	for _, cat := range pattern.Categories {
		if len(cat.Match) == 0 || containsAny(combinedContext, cat.Match) {
			key = cat.Key
			break
		}
	}

	for _, mod := range pattern.Modifiers {
		if containsAny(combinedContext, mod.Match) {
			key = key + "_" + mod.Suffix
		}
	}

	return key
}

// containsAny reports whether haystack (expected lower-case) contains any
// of needles, compared case-insensitively.
func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

// resolveCollisions appends _2, _3, etc. when multiple IDs infer the
// same key name, or when the inferred key already exists in existingIDs.
func resolveCollisions(results []ExtractedID, existingIDs map[string]string) {
	usedKeys := make(map[string]int)
	// Pre-populate with existing identifier keys.
	for k := range existingIDs {
		usedKeys[k] = 1
	}

	for i := range results {
		key := results[i].InferredKey
		if count, exists := usedKeys[key]; exists {
			// Key collision - append suffix.
			newKey := fmt.Sprintf("%s_%d", key, count+1)
			for usedKeys[newKey] > 0 {
				count++
				newKey = fmt.Sprintf("%s_%d", key, count+1)
			}
			results[i].InferredKey = newKey
			usedKeys[newKey] = 1
			usedKeys[key] = count + 1
		} else {
			usedKeys[key] = 1
		}
	}
}

// isHeaderLine returns true if the line looks like a section header
// (markdown ## or **bold text**).
func isHeaderLine(line string) bool {
	if strings.HasPrefix(line, "#") {
		return true
	}
	if strings.HasPrefix(line, "**") && strings.HasSuffix(line, "**") {
		return true
	}
	// Also match "**Bold text (N):**" patterns.
	if strings.HasPrefix(line, "**") && strings.Contains(line, ":**") {
		return true
	}
	return false
}

// isTableSeparator returns true for markdown table separator rows like |---|---|.
func isTableSeparator(line string) bool {
	if !strings.Contains(line, "|") {
		return false
	}
	cleaned := strings.ReplaceAll(line, "|", "")
	cleaned = strings.ReplaceAll(cleaned, "-", "")
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, ":", "")
	return cleaned == ""
}

// extractLabel extracts a human-readable label from a markdown table row
// or returns the trimmed line if not a table.
func extractLabel(line string) string {
	if !strings.Contains(line, "|") {
		return strings.TrimSpace(line)
	}

	// Split table row by | and look for the name column (usually last non-empty).
	cols := strings.Split(line, "|")
	var nonEmpty []string
	for _, col := range cols {
		c := strings.TrimSpace(col)
		if c != "" {
			nonEmpty = append(nonEmpty, c)
		}
	}

	if len(nonEmpty) >= 1 {
		return nonEmpty[len(nonEmpty)-1]
	}
	return strings.TrimSpace(line)
}
