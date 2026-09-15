package roster

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"gopkg.in/yaml.v3"
)

// discoverCacheEntry is one cached directory scan, keyed by the source
// directory's mtime so a long-running `aida serve` doesn't re-parse
// .claude/agents on every dispatch, yet any commit that adds or removes a
// persona invalidates the cache on the very next Load.
type discoverCacheEntry struct {
	mtime   time.Time
	entries []*Entry
}

// discoverCache maps an expanded discovery directory to its cached scan.
// Package-level and safe for concurrent use across dispatches.
var discoverCache sync.Map // map[string]discoverCacheEntry

// callSignPattern matches an org-chart line like "**Pamela** (product-manager)":
// a bold call-sign followed by a parenthesized slug.
var callSignPattern = regexp.MustCompile(`\*\*([A-Z][A-Za-z]+)\*\*\s*\(([a-z][a-z0-9-]*)\)`)

// agentFrontmatter is the subset of a .claude/agents/*.md frontmatter block
// expandDiscovery reads.
type agentFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// expandDiscovery scans parent.Discover for *.md persona files and returns
// one virtual Entry per file, sorted by Name. parent itself is never
// included in the result -- a discovery parent is not a dispatch target.
func expandDiscovery(parent *Entry) ([]*Entry, error) {
	if parent.Subagent == nil || parent.Subagent.Dir == "" {
		return nil, fmt.Errorf("discover entry %q needs a subagent.dir", parent.Name)
	}
	dir := config.ExpandPath(parent.Discover)

	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("discover dir %q missing or empty", dir)
	}

	if cached, ok := discoverCache.Load(dir); ok {
		ce := cached.(discoverCacheEntry)
		if ce.mtime.Equal(info.ModTime()) {
			return cloneEntries(ce.entries), nil
		}
	}

	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("discover dir %q missing or empty: %w", dir, err)
	}

	callSigns := loadCallSigns(parent.CallSignsFrom)

	var entries []*Entry
	for _, f := range files {
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") || !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		slug := strings.ToLower(strings.TrimSuffix(f.Name(), ".md"))
		data, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			continue // best-effort: an unreadable file just produces no entry
		}
		_, desc := parseAgentFrontmatter(data)

		e := &Entry{
			Name:        slug,
			Description: desc,
			Kind:        KindSubagent,
			Profiles:    parent.Profiles,
			Subagent: &SubagentSpec{
				Dir:     parent.Subagent.Dir,
				Agent:   slug,
				Timeout: parent.Subagent.Timeout,
			},
			DiscoveredFrom: dir,
		}
		if callSign, ok := callSigns[slug]; ok {
			e.CallSign = callSign
			e.Aliases = append(e.Aliases, strings.ToLower(callSign))
		}
		entries = append(entries, e)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("discover dir %q missing or empty: no *.md files found", dir)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	discoverCache.Store(dir, discoverCacheEntry{mtime: info.ModTime(), entries: entries})
	return cloneEntries(entries), nil
}

// cloneEntries returns a shallow copy of the slice header so a caller
// appending to the result can never grow into (and corrupt) the cached
// backing array.
func cloneEntries(entries []*Entry) []*Entry {
	out := make([]*Entry, len(entries))
	copy(out, entries)
	return out
}

// loadCallSigns best-effort parses path (an org-chart markdown file) for
// call-sign lines and returns a slug -> call-sign map. An unreadable or
// unparseable file is not an error -- it just yields an empty map, and
// discovered entries fall back to their slug as the display name.
func loadCallSigns(path string) map[string]string {
	result := map[string]string{}
	if path == "" {
		return result
	}
	data, err := os.ReadFile(config.ExpandPath(path))
	if err != nil {
		return result
	}
	for _, m := range callSignPattern.FindAllStringSubmatch(string(data), -1) {
		callSign, slug := m[1], m[2]
		result[strings.ToLower(slug)] = callSign
	}
	return result
}

// parseAgentFrontmatter extracts the leading `---\n...\n---` YAML block
// from a .claude/agents/*.md file. A file with no frontmatter (or a
// malformed block) tolerantly yields empty strings rather than an error,
// so a stub agent file with no metadata still produces a usable entry.
func parseAgentFrontmatter(data []byte) (name, description string) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return "", ""
	}
	var fm agentFrontmatter
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &fm); err != nil {
		return "", ""
	}
	return fm.Name, fm.Description
}
