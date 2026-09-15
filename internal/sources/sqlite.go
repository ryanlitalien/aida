package sources

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
)

const sqliteTimeout = 30 * time.Second

// SQLiteAdapter runs a single SQL statement against a sqlite3 database
// file. The LLM Call #2 output is expected to be ONE SQL statement (no
// wrapping CLI command, no shell quoting). The adapter runs sqlite3
// directly via exec.Command -- NOT `sh -c` -- so SQL syntax like
// `date('now', '-1 month')` passes through without bash misinterpreting
// the parens as a subshell.
//
// The database path comes from one of:
//  1. src.Assets["db"] (preferred; set by the scanner when it sees a
//     top-level *.db / *.sqlite / *.sqlite3 file)
//  2. the first *.db file under src.Path
//  3. src.Path itself if it already ends in .db
type SQLiteAdapter struct{}

// Execute runs the SQL statement against the resolved database file.
func (a *SQLiteAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "sqlite",
		Status: "success",
	}

	sql := strings.TrimSpace(command)
	// Tolerate LLMs that wrap the SQL in a CLI invocation anyway.
	sql = stripSqliteCLIPrefix(sql)
	if sql == "" {
		result.Status = "error"
		result.Summary = "sqlite adapter received empty SQL"
		return result, nil
	}
	// Issue #14 Bug E: when no template matches, the LLM sometimes
	// returns the literal placeholder "NO_MATCHING_TEMPLATE" instead
	// of valid SQL. Treat that as a structured "no template" outcome
	// rather than a SQL parse error so the synthesizer can report it
	// cleanly and the lesson recorder doesn't poison this source.
	if isNoTemplatePlaceholder(sql) {
		result.Status = "empty"
		result.Summary = "no SQL template matched this question (LLM returned NO_MATCHING_TEMPLATE)"
		return result, nil
	}
	if !sqlLooksSafe(sql) {
		result.Status = "error"
		result.Summary = "sqlite adapter refuses non-SELECT statement (got: " + truncateForError(sql, 120) + ")"
		return result, nil
	}

	dbPath := a.resolveDBPath(src)
	if dbPath == "" {
		result.Status = "error"
		result.Summary = "sqlite adapter could not locate a .db file from assets or path"
		return result, nil
	}

	// -header gives us column names on line 1, -csv produces
	// comma-separated output we can parse as rows. execx.Run(path,
	// args...) passes each arg as a single argv entry, so the SQL is
	// never subject to shell parsing.
	res, err := execx.Run(ctx, "sqlite3",
		[]string{"-header", "-csv", dbPath, sql},
		execx.RunOpts{Timeout: sqliteTimeout})
	if res.TimedOut {
		result.Status = "timeout"
		result.Summary = fmt.Sprintf("sqlite query timed out after %s on %s", sqliteTimeout, dbPath)
		return result, nil
	}
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("sqlite3 %s: %s", filepath.Base(dbPath), strings.TrimSpace(string(res.Stderr)))
		return result, nil
	}

	raw := bytes.TrimSpace(res.Stdout)
	if len(raw) == 0 {
		result.Status = "empty"
		result.Summary = fmt.Sprintf("Query returned no rows from %s", filepath.Base(dbPath))
		return result, nil
	}

	artifacts, err := a.parseCSV(raw)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("parsing sqlite output: %s", err)
		return result, nil
	}
	if len(artifacts) == 0 {
		result.Status = "empty"
		result.Summary = fmt.Sprintf("Query returned no rows from %s", filepath.Base(dbPath))
		return result, nil
	}

	result.Artifacts = artifacts
	data, _ := json.Marshal(map[string]any{
		"db":        dbPath,
		"sql":       sql,
		"row_count": len(artifacts),
	})
	result.Data = data
	result.Summary = fmt.Sprintf("sqlite3 %s: %d row(s)", filepath.Base(dbPath), len(artifacts))
	return result, nil
}

// ParseOutput isn't used by the Execute path (which builds artifacts
// inline), but is required by the Adapter interface.
func (a *SQLiteAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return a.parseCSV(raw)
}

func (a *SQLiteAdapter) parseCSV(raw []byte) ([]Artifact, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	headers := records[0]

	// Build all row maps first so we can pick the best ID column.
	rowMaps := make([]map[string]string, 0, len(records)-1)
	for _, row := range records[1:] {
		m := make(map[string]string, len(headers))
		for j, v := range row {
			if j < len(headers) {
				m[headers[j]] = v
			}
		}
		rowMaps = append(rowMaps, m)
	}

	idCol := pickIDColumn(headers, rowMaps)

	artifacts := make([]Artifact, 0, len(rowMaps))
	for i, m := range rowMaps {
		snippet, _ := json.Marshal(m)
		artifacts = append(artifacts, Artifact{
			Type:    "row",
			ID:      deriveRowIDFromColumn(i, m, idCol),
			Snippet: string(snippet),
		})
	}
	ensureUniqueIDs(artifacts)
	return artifacts, nil
}

// resolveDBPath finds the .db file for a source. Assets map wins;
// otherwise globs src.Path for *.db / *.sqlite / *.sqlite3.
func (a *SQLiteAdapter) resolveDBPath(src config.Source) string {
	if p, ok := src.Assets["db"]; ok {
		return resolveCSVFile(src.Path, p) // shared helper
	}
	if p, ok := src.Assets["sqlite"]; ok {
		return resolveCSVFile(src.Path, p)
	}
	if src.Path == "" {
		return ""
	}
	base := config.ExpandPath(src.Path)
	if strings.HasSuffix(strings.ToLower(base), ".db") ||
		strings.HasSuffix(strings.ToLower(base), ".sqlite") ||
		strings.HasSuffix(strings.ToLower(base), ".sqlite3") {
		return base
	}
	var found []string
	for _, ext := range []string{"*.db", "*.sqlite", "*.sqlite3"} {
		matches, _ := filepath.Glob(filepath.Join(base, ext))
		found = append(found, matches...)
	}
	if len(found) == 0 {
		return ""
	}
	sort.Strings(found)
	return found[0]
}

// stripSqliteCLIPrefix removes a leading `sqlite3 <db> "..."` wrapper if
// the LLM produced one, leaving just the inner SQL.
func stripSqliteCLIPrefix(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	if !strings.HasPrefix(lower, "sqlite3 ") && !strings.HasPrefix(lower, "sqlite ") {
		return s
	}
	// Find the first quote and unwrap.
	if i := strings.Index(s, "\""); i >= 0 {
		j := strings.LastIndex(s, "\"")
		if j > i {
			return strings.TrimSpace(s[i+1 : j])
		}
	}
	if i := strings.Index(s, "'"); i >= 0 {
		j := strings.LastIndex(s, "'")
		if j > i {
			return strings.TrimSpace(s[i+1 : j])
		}
	}
	return s
}

// sqlLooksSafe returns true only for SELECT/WITH/PRAGMA/EXPLAIN
// statements. The adapter is read-only by design to keep a careless LLM
// from issuing UPDATE/DELETE/DROP against the user's data.
func sqlLooksSafe(sql string) bool {
	trimmed := strings.TrimSpace(sql)
	trimmed = strings.TrimSuffix(trimmed, ";")
	upper := strings.ToUpper(trimmed)
	for _, prefix := range []string{"SELECT", "WITH", "PRAGMA", "EXPLAIN"} {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

func truncateForError(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

// isNoTemplatePlaceholder returns true when the LLM returned the literal
// "NO_MATCHING_TEMPLATE" placeholder string instead of valid SQL/shell.
// Issue #14 Bug E: this is the LLM's way of saying "no template fit your
// question" - adapters should treat it as an empty result, not a parse
// error. Used by the sqlite adapter (here) and the exec adapter.
func isNoTemplatePlaceholder(s string) bool {
	t := strings.TrimSpace(s)
	t = strings.TrimSuffix(t, ";")
	t = strings.TrimSpace(t)
	return strings.EqualFold(t, "NO_MATCHING_TEMPLATE") ||
		strings.HasPrefix(strings.ToUpper(t), "NO_MATCHING_TEMPLATE")
}
