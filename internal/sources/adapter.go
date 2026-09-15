package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// SourceResult holds the output from executing a source query.
type SourceResult struct {
	Source    string          `json:"source"`
	Status    string          `json:"status"` // "success", "error", "timeout", "empty"
	Data      json.RawMessage `json:"data"`
	Summary   string          `json:"summary"`
	Artifacts []Artifact      `json:"artifacts"`
	Duration  time.Duration   `json:"duration,omitempty"`
	// Command is the exact string passed to the adapter (LLM-generated
	// query for most source types, or the user's raw question for
	// claude-project sources). Recorded so run logs and `aida tune` can
	// show what was actually sent.
	Command string `json:"command,omitempty"`
	// FromContextDoc is true when this result's artifacts came from the
	// source's context file (CLAUDE.md, README.md, etc.) rather than from
	// querying the source itself -- e.g. the executor's "context file
	// fallback" for a grep/codebase source that returned no matches. These
	// artifacts are documentation ABOUT a source, not data FROM it, and
	// must not be treated as having answered a data question.
	FromContextDoc bool `json:"from_context_doc,omitempty"`
}

// Artifact represents a single piece of evidence from a source.
type Artifact struct {
	Type      string `json:"type"` // "log", "row", "file", "page", "code"
	ID        string `json:"id"`
	Timestamp string `json:"timestamp,omitempty"`
	Snippet   string `json:"snippet"`
}

// Adapter defines the interface that all source adapters must implement.
type Adapter interface {
	// Execute runs a command against the source and returns the result.
	Execute(ctx context.Context, command string, src config.Source) (SourceResult, error)

	// ParseOutput converts raw command output into structured artifacts.
	ParseOutput(raw []byte, src config.Source) ([]Artifact, error)
}

// registry holds the mapping of source types to their adapters.
var (
	registry   = make(map[string]Adapter)
	registryMu sync.RWMutex
)

// RegisterAdapter adds an adapter for a given source type.
func RegisterAdapter(sourceType string, adapter Adapter) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[sourceType] = adapter
}

// GetAdapter returns the adapter for a given source type key.
func GetAdapter(sourceType string) Adapter {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[sourceType]
}

// GetAdapterForSource selects the best adapter by inspecting the source's
// exec config, name, and type - not just the generic type string.
func GetAdapterForSource(name string, src config.Source) Adapter {
	// 1. Check by source name (exact match)
	if a := GetAdapter(name); a != nil {
		return a
	}

	// 2. Infer from exec templates
	if src.Exec != nil {
		for _, tmpl := range src.Exec {
			lower := strings.ToLower(tmpl)
			if strings.Contains(lower, "claude ") {
				return GetAdapter("notion")
			}
		}
	}

	// 3. "docs" with a local path but no exec → either grep the path (when
	// search.mode == "grep" is set, opting into full-path search) or read
	// only the context file (the legacy DocsAdapter behavior). The grep
	// path is for partner repos like camp-butz, first-chair, thrive that ship
	// a CLAUDE.md + docs/ + gdrive/ tree the synthesizer needs to search.
	// The legacy path is for narrative-knowledge sources (architecture,
	// aida, claude, ollama) whose CLAUDE.md is read whole.
	if src.Type == "docs" && src.Path != "" && len(src.Exec) == 0 {
		if src.Search != nil && src.Search.Mode == "grep" {
			return GetAdapter("grep")
		}
		return GetAdapter("docs")
	}

	// 4. "codebase" → grep
	if src.Type == "codebase" {
		return GetAdapter("codebase")
	}

	// 5. Fall back to type-based lookup
	if a := GetAdapter(src.Type); a != nil {
		return a
	}

	// 6. Ultimate fallback
	return GetAdapter("exec")
}

// deriveRowID builds a human-readable label for a tabular row by inspecting
// column values. It prefers descriptive columns (category, name, merchant,
// description) then falls back to a date, then to the first few short values.
// The fallback is "row-<index>". The returned ID is lowercased, spaces
// replaced with dashes, and truncated to keep citations compact.
//
// rowMap can have either string or interface{} values (a JSON exec source
// returns map[string]interface{}, CSV returns map[string]string). The
// function handles both via fmt.Sprintf.
// pickIDColumn chooses the best column to use as a row identifier from an
// ordered list of headers and a sample of rows. Among string columns (non-
// numeric, non-date), it prefers the one with the most distinct values so
// that IDs are maximally unique. Falls back to date columns, then any
// non-empty column. Called once per result set.
func pickIDColumn(headers []string, rows []map[string]string) string {
	if len(headers) == 0 || len(rows) == 0 {
		return ""
	}

	// Score each string column by distinct value count; pick the highest.
	bestCol := ""
	bestDistinct := 0
	for _, h := range headers {
		distinct := make(map[string]struct{})
		isString := false
		for _, row := range rows {
			v := strings.TrimSpace(row[h])
			if v == "" {
				continue
			}
			if !looksNumeric(v) && !looksDateOnly(v) {
				isString = true
				distinct[strings.ToLower(v)] = struct{}{}
			}
		}
		if isString && len(distinct) > bestDistinct {
			bestDistinct = len(distinct)
			bestCol = h
		}
	}
	if bestCol != "" {
		return bestCol
	}

	// Fallback: first column with any non-empty value.
	for _, h := range headers {
		for _, row := range rows {
			if strings.TrimSpace(row[h]) != "" {
				return h
			}
		}
	}
	return ""
}

// deriveRowIDFromColumn returns the sanitized value of the chosen ID column
// for a single row, falling back to row-N.
func deriveRowIDFromColumn(index int, row map[string]string, idCol string) string {
	if idCol != "" {
		if v := strings.TrimSpace(row[idCol]); v != "" {
			return sanitizeRowID(v)
		}
	}
	return fmt.Sprintf("row-%d", index)
}

// looksNumeric returns true if s looks like a plain number (int or float),
// optionally negative or with commas. "42", "3.14", "-100", "1,234.56" → true.
func looksNumeric(s string) bool {
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "-")
	if s == "" {
		return false
	}
	dots := 0
	for _, r := range s {
		if r == '.' {
			dots++
			if dots > 1 {
				return false
			}
		} else if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksDateOnly returns true if s looks like a date/timestamp with no
// descriptive text: "2026-04", "2026-03-15", "2026-03-15T10:00:00Z", etc.
func looksDateOnly(s string) bool {
	if len(s) < 7 || len(s) > 30 {
		return false
	}
	// Must start with 4 digits (year)
	if len(s) < 4 {
		return false
	}
	for _, r := range s[:4] {
		if r < '0' || r > '9' {
			return false
		}
	}
	// Only digits, dashes, colons, dots, T, Z, spaces, plus
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '-' || r == ':' || r == '.' || r == 'T' || r == 'Z' || r == ' ' || r == '+':
		default:
			return false
		}
	}
	return true
}

// pickIDColumnFromMaps is pickIDColumn for map[string]interface{} (JSON rows from an exec source).
// sortedKeys returns the keys of a map in sorted order for stable iteration.
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func pickIDColumnFromMaps(keys []string, rows []map[string]interface{}) string {
	if len(keys) == 0 || len(rows) == 0 {
		return ""
	}
	bestCol := ""
	bestDistinct := 0
	for _, h := range keys {
		distinct := make(map[string]struct{})
		isString := false
		for _, row := range rows {
			v := fmt.Sprintf("%v", row[h])
			if v == "<nil>" || v == "" {
				continue
			}
			if !looksNumeric(v) && !looksDateOnly(v) {
				isString = true
				distinct[strings.ToLower(v)] = struct{}{}
			}
		}
		if isString && len(distinct) > bestDistinct {
			bestDistinct = len(distinct)
			bestCol = h
		}
	}
	if bestCol != "" {
		return bestCol
	}
	for _, h := range keys {
		for _, row := range rows {
			v := fmt.Sprintf("%v", row[h])
			if v != "<nil>" && v != "" {
				return h
			}
		}
	}
	return ""
}

// jsonRowID builds a compact, human-readable artifact ID from a JSON object
// row. Callers are ExecAdapter and MCPToolAdapter, which both historically
// returned opaque "item-N" IDs - useless for citations, since the synthesizer
// and the user cannot tell which PR / issue / record an id refers to.
//
// Resolution order:
//  1. GitHub PR/issue shape: nested "repository" object + top-level "number"
//     → "{repo}-{number}" (e.g. "butter-stack-3476"). Same shape with an
//     "owner/name" variant covers `gh search` output.
//  2. Well-known scalar id fields in priority order: id, ID, Id, number,
//     name, Name, title, Title, url, URL.
//  3. Fall back to the column chosen by pickIDColumnFromMaps (the picker
//     handed in as fallbackCol).
//  4. Last resort: "item-N".
//
// IDs are sanitized (lowercased, non-alphanumerics → dashes, 40-char cap)
// and callers should run ensureUniqueIDs over the full artifact slice to
// disambiguate collisions.
func jsonRowID(row map[string]interface{}, fallbackCol string, index int) string {
	if repo := extractRepoName(row["repository"]); repo != "" {
		if num := stringFromJSON(row["number"]); num != "" {
			return sanitizeRowID(repo + "-" + num)
		}
	}
	for _, key := range []string{"id", "ID", "Id", "number", "name", "Name", "title", "Title", "url", "URL"} {
		if v := stringFromJSON(row[key]); v != "" {
			return sanitizeRowID(v)
		}
	}
	if fallbackCol != "" {
		if v := stringFromJSON(row[fallbackCol]); v != "" {
			return sanitizeRowID(v)
		}
	}
	return fmt.Sprintf("item-%d", index)
}

// extractRepoName pulls a repository name out of common GitHub API shapes.
// `gh pr list --json repository` returns {"name": "butter_stack"};
// `gh search prs --json repository` returns {"nameWithOwner": "org/repo"};
// `gh api` returns {"full_name": "org/repo"}. Also accepts a bare string.
func extractRepoName(v interface{}) string {
	switch repo := v.(type) {
	case string:
		return repo
	case map[string]interface{}:
		for _, key := range []string{"nameWithOwner", "full_name", "name"} {
			if s := stringFromJSON(repo[key]); s != "" {
				return s
			}
		}
	}
	return ""
}

// stringFromJSON normalizes a JSON value to a trimmed non-empty string, or "".
// Handles strings, numbers (encoded as float64 by encoding/json), and bools.
// Returns "" for nil, empty string, and everything else.
func stringFromJSON(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case float64:
		// JSON numbers round-trip as float64. Emit as int when it is one.
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	case bool:
		return fmt.Sprintf("%v", t)
	default:
		return ""
	}
}

// sanitizeRowID turns a raw value into a compact, citation-friendly ID:
// lowercase, spaces/special chars to dashes, max 40 chars.
func sanitizeRowID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	// Replace whitespace and special chars with dashes.
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
		out = strings.TrimRight(out, "-")
	}
	if out == "" {
		return "row"
	}
	return out
}

// ensureUniqueIDs deduplicates artifact IDs by appending a numeric suffix
// when collisions occur. Call this after building the full artifact slice.
func ensureUniqueIDs(artifacts []Artifact) {
	seen := make(map[string]int, len(artifacts))
	for i := range artifacts {
		id := artifacts[i].ID
		if count, exists := seen[id]; exists {
			seen[id] = count + 1
			artifacts[i].ID = fmt.Sprintf("%s-%d", id, count+1)
		} else {
			seen[id] = 1
		}
	}
}

// init registers all built-in adapters.
func init() {
	// "data-source" is the generic type for anything queried via a CLI
	// tool (a SQL warehouse, an observability backend, etc.) - the exec
	// adapter shells out to whatever `exec:` template the source YAML
	// declares, so there is no dedicated per-vendor adapter to write.
	RegisterAdapter("data-source", &ExecAdapter{})
	RegisterAdapter("codebase", &GrepAdapter{})
	RegisterAdapter("grep", &GrepAdapter{})
	RegisterAdapter("notion", &NotionAdapter{})
	RegisterAdapter("exec", &ExecAdapter{})
	RegisterAdapter("tool", &ExecAdapter{})
	RegisterAdapter("csv", &CSVAdapter{})
	RegisterAdapter("app", &ExecAdapter{})
	RegisterAdapter("claude-project", &ClaudeProjectAdapter{})
	RegisterAdapter("git", &GitAdapter{})
	RegisterAdapter("sqlite", &SQLiteAdapter{})
	RegisterAdapter("docs", &DocsAdapter{})
}
