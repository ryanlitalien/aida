package library

// L1 progressive-disclosure index - Action #1b of the harness roadmap.
//
// Three-level pattern from Google ADK Skills:
//
//   L1  name + 1-line description, ALWAYS in the system prompt (~100 tok ea)
//   L2  full body of one layer, fetched on demand via the get_layer tool
//   L3  linked artifacts (source files, brain entity pages), pulled by the
//       existing read/grep/notion-fetch tools
//
// The L1 index lets the agent decide which L2 body is worth pulling instead
// of the harness pre-selecting the bundle. For 80 layers × ~150 chars each
// that's ~3K tokens of always-on context - small enough to stay in the
// stable prompt prefix while letting the agent see every option.

import (
	"bufio"
	"bytes"
	"os"
	"sort"
	"strings"
)

// LayerSummary is the L1 representation of a library layer.
type LayerSummary struct {
	Name        string
	Description string // 1-line, capped at maxDescriptionLen
	Source      string // root name (for citation)
}

// maxDescriptionLen caps each L1 description so the always-on index stays
// roughly bounded in tokens. 200 chars ~= 50 tokens × 80 layers ≈ 4K tok.
const maxDescriptionLen = 200

// LayerSummaries returns L1 summaries for every available layer in the
// registry, alphabetical by name. Layers whose description cannot be read
// from disk still appear, with an empty Description - the agent can still
// see the name and call get_layer.
func (r *Registry) LayerSummaries() []LayerSummary {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.Layers))
	for name, layer := range r.Layers {
		if layer != nil && layer.Available {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]LayerSummary, 0, len(names))
	for _, name := range names {
		layer := r.Layers[name]
		out = append(out, LayerSummary{
			Name:        name,
			Description: extractLayerDescription(layer.AbsFile),
			Source:      layer.Root.Ref.Name,
		})
	}
	return out
}

// extractLayerDescription returns the first non-heading, non-empty
// paragraph of a markdown file, capped at maxDescriptionLen. Returns
// the empty string when the file is missing or has no body.
//
// Convention: existing layer files written by `aida index` are formatted as
//
//	# Source: <name>
//	<blank>
//	<one-line description>      <-- this is what we extract
//	<blank>
//	<body>
//
// Files that don't follow the convention still parse - we just take the
// first non-heading paragraph regardless of position.
func extractLayerDescription(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "<!--") || strings.HasPrefix(line, "---") {
			continue
		}
		return truncateLine(line, maxDescriptionLen)
	}
	return ""
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	// Don't break in the middle of a multi-byte UTF-8 sequence.
	for cut != "" && (cut[len(cut)-1]&0xC0) == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimSpace(cut) + "…"
}

// L1IndexString renders summaries as a compact "name: description" list
// suitable for injection into a system prompt. Empty result when there
// are no available layers.
func L1IndexString(summaries []LayerSummary) string {
	if len(summaries) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, s := range summaries {
		sb.WriteString("- ")
		sb.WriteString(s.Name)
		if s.Description != "" {
			sb.WriteString(": ")
			sb.WriteString(s.Description)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}
