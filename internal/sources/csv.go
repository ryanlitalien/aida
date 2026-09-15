package sources

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

const csvTimeout = 30 * time.Second

// CSVAdapter handles reading and appending to CSV files.
type CSVAdapter struct{}

// Execute reads or appends to a CSV file based on the command.
//
// Supported command grammars:
//
//	append: <comma,separated,values>           -- append a row
//	latest [N]                                 -- last N rows of the primary file (default 20)
//	<asset-name> [latest [N]]                  -- switch to a named asset, optionally get last N
//	<asset-name> filter: <substring>           -- filter rows on a substring
//	filter: <substring>                        -- substring filter against the primary file
//	<anything else>                            -- treated as a substring filter (legacy)
//
// The "primary file" is the first asset alphabetically, or the first CSV
// in src.Path if no assets are declared, or src.Path itself if it already
// ends in .csv.
func (a *CSVAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "csv",
		Status: "success",
	}

	// Check for context cancellation.
	select {
	case <-ctx.Done():
		result.Status = "timeout"
		result.Summary = "Operation cancelled"
		return result, nil
	default:
	}

	// Append path (old behavior).
	if strings.HasPrefix(command, "append:") {
		csvPath := a.primaryCSVPath(src)
		if csvPath == "" {
			result.Status = "error"
			result.Summary = "No CSV file path configured for source"
			return result, nil
		}
		return a.appendRow(command[7:], csvPath, result)
	}

	// Parse read commands: pick an asset, extract "latest N", extract filter.
	req := parseCSVCommand(command, src)
	csvPath, assetName := a.resolveAssetPath(src, req.asset)
	if csvPath == "" {
		result.Status = "error"
		result.Summary = fmt.Sprintf("No CSV file found for source (asset=%q, path=%q)", req.asset, src.Path)
		return result, nil
	}
	result.Source = assetName
	return a.readCSV(req, csvPath, result, src)
}

// csvRequest is the parsed form of an incoming read command.
type csvRequest struct {
	asset  string // asset name the caller wants, or "" for primary
	latest int    // N: return last N rows; 0 = return all
	filter string // substring filter; "" = no filter
}

var latestRE = regexp.MustCompile(`(?i)\blatest(?:\s+(\d+))?\b`)

// parseCSVCommand is a small, tolerant parser that accepts either a
// structured grammar or a raw substring filter.
func parseCSVCommand(cmd string, src config.Source) csvRequest {
	cmd = strings.TrimSpace(cmd)
	req := csvRequest{}

	// Empty command = latest 20 rows of the primary file.
	if cmd == "" {
		req.latest = 20
		return req
	}

	// If the first word matches an asset name, consume it.
	lower := strings.ToLower(cmd)
	if len(src.Assets) > 0 {
		for name := range src.Assets {
			lname := strings.ToLower(name)
			if lower == lname || strings.HasPrefix(lower, lname+" ") || strings.HasPrefix(lower, lname+":") {
				req.asset = name
				cmd = strings.TrimSpace(cmd[len(name):])
				cmd = strings.TrimPrefix(cmd, ":")
				cmd = strings.TrimSpace(cmd)
				break
			}
		}
	}

	// Extract latest [N] if present.
	if m := latestRE.FindStringSubmatch(cmd); m != nil {
		n := 20
		if m[1] != "" {
			if parsed, err := strconv.Atoi(m[1]); err == nil && parsed > 0 {
				n = parsed
			}
		}
		req.latest = n
		cmd = strings.TrimSpace(latestRE.ReplaceAllString(cmd, ""))
	}

	// Explicit filter: prefix wins. Otherwise any remaining text is a filter.
	if rest, ok := strings.CutPrefix(cmd, "filter:"); ok {
		req.filter = strings.TrimSpace(rest)
	} else if cmd != "" && req.latest == 0 {
		req.filter = cmd
	}
	// If both filter and latest were asked for, filter then take last N.
	return req
}

// primaryCSVPath returns the single best CSV path for this source.
func (a *CSVAdapter) primaryCSVPath(src config.Source) string {
	path, _ := a.resolveAssetPath(src, "")
	return path
}

// resolveAssetPath returns (absolute csv path, asset display name) for the
// requested asset. If asset is empty, picks the alphabetically-first asset,
// or falls back to src.Path behaviors.
func (a *CSVAdapter) resolveAssetPath(src config.Source, asset string) (string, string) {
	// 1. Named asset lookup.
	if asset != "" {
		if file, ok := src.Assets[asset]; ok {
			return resolveCSVFile(src.Path, file), asset
		}
	}
	// 2. Legacy single-file asset keys.
	if file, ok := src.Assets["csv"]; ok {
		return resolveCSVFile(src.Path, file), "csv"
	}
	if file, ok := src.Assets["file"]; ok {
		return resolveCSVFile(src.Path, file), "file"
	}
	// 3. Multi-asset map: pick the alphabetically-first (stable default).
	if len(src.Assets) > 0 {
		names := make([]string, 0, len(src.Assets))
		for k := range src.Assets {
			names = append(names, k)
		}
		sort.Strings(names)
		return resolveCSVFile(src.Path, src.Assets[names[0]]), names[0]
	}
	// 4. Path fallback.
	if src.Path != "" {
		expanded := config.ExpandPath(src.Path)
		if strings.HasSuffix(expanded, ".csv") {
			return expanded, filepath.Base(expanded)
		}
		matches, err := filepath.Glob(filepath.Join(expanded, "*.csv"))
		if err == nil && len(matches) > 0 {
			sort.Strings(matches)
			return matches[0], filepath.Base(matches[0])
		}
	}
	return "", ""
}

// resolveCSVFile turns an asset value into an absolute path, relative to
// the source's Path if the value is not already absolute.
func resolveCSVFile(basePath, file string) string {
	file = config.ExpandPath(file)
	if filepath.IsAbs(file) {
		return file
	}
	if basePath == "" {
		return file
	}
	return filepath.Join(config.ExpandPath(basePath), file)
}

// readCSV reads the CSV file and returns rows matching the parsed request.
func (a *CSVAdapter) readCSV(req csvRequest, csvPath string, result SourceResult, src config.Source) (SourceResult, error) {
	data, err := os.ReadFile(csvPath)
	if err != nil {
		if os.IsNotExist(err) {
			result.Status = "empty"
			result.Summary = fmt.Sprintf("CSV file not found: %s", csvPath)
			return result, nil
		}
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to read CSV: %s", err.Error())
		return result, nil
	}

	artifacts, err := a.ParseOutput(data, src)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to parse CSV: %s", err.Error())
		return result, nil
	}
	totalRows := len(artifacts)

	// Apply substring filter first, if any.
	if req.filter != "" {
		q := strings.ToLower(req.filter)
		filtered := make([]Artifact, 0, len(artifacts))
		for _, art := range artifacts {
			if strings.Contains(strings.ToLower(art.Snippet), q) {
				filtered = append(filtered, art)
			}
		}
		artifacts = filtered
	}

	// If "latest N" was requested, sort by timestamp desc (when available)
	// and trim. This is what turns "latest workout" into useful data.
	if req.latest > 0 {
		if anyHaveTimestamps(artifacts) {
			sort.SliceStable(artifacts, func(i, j int) bool {
				return artifacts[i].Timestamp > artifacts[j].Timestamp
			})
		} else {
			// No timestamps: assume the file is already chronological
			// (common for append-only logs). Reverse so newest is first.
			for i, j := 0, len(artifacts)-1; i < j; i, j = i+1, j-1 {
				artifacts[i], artifacts[j] = artifacts[j], artifacts[i]
			}
		}
		if len(artifacts) > req.latest {
			artifacts = artifacts[:req.latest]
		}
	}

	if len(artifacts) == 0 {
		result.Status = "empty"
		if req.filter != "" {
			result.Summary = fmt.Sprintf("No rows matching %q in %s (%d total rows)", req.filter, filepath.Base(csvPath), totalRows)
		} else {
			result.Summary = fmt.Sprintf("CSV is empty: %s", filepath.Base(csvPath))
		}
		return result, nil
	}

	result.Artifacts = artifacts
	result.Data = json.RawMessage(fmt.Sprintf(`{"row_count":%d,"file":"%s","total_rows":%d}`, len(artifacts), csvPath, totalRows))
	summary := fmt.Sprintf("Returned %d row(s) from %s (%d total)", len(artifacts), filepath.Base(csvPath), totalRows)
	if req.latest > 0 {
		summary = fmt.Sprintf("Latest %d of %d row(s) from %s", len(artifacts), totalRows, filepath.Base(csvPath))
	}
	result.Summary = summary
	return result, nil
}

func anyHaveTimestamps(arts []Artifact) bool {
	for _, a := range arts {
		if a.Timestamp != "" {
			return true
		}
	}
	return false
}

// appendRow adds a new row to the CSV file.
func (a *CSVAdapter) appendRow(rowData, csvPath string, result SourceResult) (SourceResult, error) {
	// Parse the row data (comma-separated values)
	reader := csv.NewReader(strings.NewReader(rowData))
	fields, err := reader.Read()
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Invalid row data: %s", err.Error())
		return result, nil
	}

	// Open file in append mode, create if not exists
	f, err := os.OpenFile(csvPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to open CSV for writing: %s", err.Error())
		return result, nil
	}
	defer f.Close()

	writer := csv.NewWriter(f)
	// Prepend timestamp as first field
	row := append([]string{time.Now().Format(time.RFC3339)}, fields...)
	if err := writer.Write(row); err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to write row: %s", err.Error())
		return result, nil
	}
	writer.Flush()

	if err := writer.Error(); err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("Failed to flush CSV: %s", err.Error())
		return result, nil
	}

	result.Summary = fmt.Sprintf("Appended row to %s", filepath.Base(csvPath))
	result.Artifacts = []Artifact{
		{
			Type:    "row",
			ID:      fmt.Sprintf("row-%d", time.Now().UnixMilli()),
			Snippet: strings.Join(row, ","),
		},
	}
	return result, nil
}

// ParseOutput parses CSV file contents into row artifacts.
func (a *CSVAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing CSV: %w", err)
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("empty CSV file")
	}

	// First row is headers
	headers := records[0]

	// Build all row maps first so we can pick the best ID column.
	rowMaps := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		rowMap := make(map[string]string)
		for j, val := range record {
			if j < len(headers) {
				rowMap[headers[j]] = val
			}
		}
		rowMaps = append(rowMaps, rowMap)
	}

	idCol := pickIDColumn(headers, rowMaps)

	artifacts := make([]Artifact, 0, len(rowMaps))
	for i, rowMap := range rowMaps {
		snippet, _ := json.Marshal(rowMap)

		artifact := Artifact{
			Type:    "row",
			ID:      deriveRowIDFromColumn(i, rowMap, idCol),
			Snippet: string(snippet),
		}

		// Extract timestamp from common column names (case-insensitive,
		// covering the columns aida's CSV sources actually contain).
		for header, val := range rowMap {
			if val == "" {
				continue
			}
			lh := strings.ToLower(header)
			switch lh {
			case "date", "timestamp", "created_at", "workoutday", "day", "datetime", "time":
				artifact.Timestamp = val
			}
			if artifact.Timestamp != "" {
				break
			}
		}

		artifacts = append(artifacts, artifact)
	}
	ensureUniqueIDs(artifacts)
	return artifacts, nil
}
