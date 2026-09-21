package library

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
)

// DiscoveredSource is a classification result for one candidate folder.
type DiscoveredSource struct {
	Name         string            // slug used as the key in library.yaml sources:
	Path         string            // absolute folder path
	Type         string            // csv | data-source | docs | codebase
	Kind         string            // "data", "docs", or "code" - used for filtering
	Repo         string            // GitHub owner/repo extracted from git remote (e.g., "ryanlitalien-apps/csv-viewer")
	Description  string            // first non-empty line of CLAUDE.md/README.md, or synthesized
	Capabilities []string          // inferred from filename/foldername
	Entities     []string          // inferred from folder name
	ContextFile  string            // CLAUDE.md or README.md (relative to Path)
	Assets       map[string]string // named CSV/SQL files: asset-name → relative path
	Profiles     []string          // profiles this source belongs to (set at write time)
}

// scanSkipDirs are folder names we never descend into when scanning.
var scanSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
	"bin":          true,
	"target":       true,
	".next":        true,
	".cache":       true,
	".idea":        true,
	".vscode":      true,
	"vendor":       true,
	".terraform":   true,
}

// ScanForSources walks each path one level deep and classifies every child
// directory that looks like a useful source. Returns a slice of discoveries
// sorted by path. Unreadable paths are silently skipped.
//
// In addition, if ANY scanned path contains one or more git repositories,
// a single aggregate `git-repos` source is synthesized that points at the
// scan root and uses the GitAdapter to query every repo under it. This
// gives aida a built-in answer for "what did <author> commit?" style
// questions without the user having to configure anything.
func ScanForSources(paths []string) ([]DiscoveredSource, error) {
	seen := map[string]bool{}
	var out []DiscoveredSource
	var gitCapableRoots []string
	for _, root := range paths {
		abs := expandPath(root)
		entries, err := os.ReadDir(abs)
		if err != nil {
			continue
		}
		foundGitHere := false
		for _, entry := range entries {
			if !entry.IsDir() || scanSkipDirs[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			dir := filepath.Join(abs, entry.Name())
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if d := classifyFolder(dir); d != nil {
				out = append(out, *d)
			}
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				foundGitHere = true
			}
		}
		if foundGitHere {
			gitCapableRoots = append(gitCapableRoots, abs)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	// Emit one aggregate `git-repos` source per scan root that had any
	// git working trees. Using the scan root (not individual repos) lets
	// the GitAdapter iterate -- one source per repo would be N sources
	// of noise.
	for _, root := range gitCapableRoots {
		name := "git-" + slugFolder(filepath.Base(root))
		out = append(out, DiscoveredSource{
			Name:        name,
			Path:        homeRelative(root),
			Kind:        "data",
			Type:        "git",
			Description: fmt.Sprintf("Git history across every repo under %s (commits, authors, branches, PRs)", root),
			Capabilities: []string{
				"git-history", "commit-lookup", "author-lookup",
				"branch-lookup", "pr-history",
			},
			Entities: []string{"git", "commit", "commits", "branch", "branches", "author", "authors", "pr", "pull-request"},
		})
	}
	return out, nil
}

// classifyFolder inspects one folder's direct contents and decides whether
// it's a source candidate, and if so what type/kind/capabilities fit.
// Returns nil if the folder has nothing aida can use.
func classifyFolder(dir string) *DiscoveredSource {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var csvs, sqls, dbs, scripts, mds []string
	var contextFile string
	hasMCPConfig := false
	hasProjectMarker := false // go.mod/package.json/etc -- "this is a code project, not a data wrapper"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case name == "CLAUDE.md":
			contextFile = name
		case name == "AGENT.md" && contextFile == "":
			contextFile = name
		case name == "README.md" && contextFile == "":
			contextFile = name
		case name == ".mcp.json":
			hasMCPConfig = true
		case strings.HasSuffix(lower, ".csv"):
			csvs = append(csvs, name)
		case strings.HasSuffix(lower, ".sql"):
			sqls = append(sqls, name)
		case strings.HasSuffix(lower, ".db"),
			strings.HasSuffix(lower, ".sqlite"),
			strings.HasSuffix(lower, ".sqlite3"):
			dbs = append(dbs, name)
		case strings.HasSuffix(lower, ".py"),
			strings.HasSuffix(lower, ".sh"):
			scripts = append(scripts, name)
		case strings.HasSuffix(lower, ".md"):
			mds = append(mds, name)
		case name == "go.mod",
			name == "package.json",
			name == "Cargo.toml",
			name == "pyproject.toml",
			name == "Package.swift",
			name == "Gemfile",
			name == "build.gradle",
			name == "pom.xml",
			strings.HasSuffix(lower, ".xcodeproj"),
			strings.HasSuffix(lower, ".xcworkspace"),
			strings.HasSuffix(lower, ".go"),
			strings.HasSuffix(lower, ".ts"),
			strings.HasSuffix(lower, ".js"),
			strings.HasSuffix(lower, ".rs"),
			strings.HasSuffix(lower, ".rb"),
			strings.HasSuffix(lower, ".swift"):
			hasProjectMarker = true
		}
	}
	hasCode := hasProjectMarker

	// Decide source kind. Precedence:
	//   1. Folder has BOTH .mcp.json AND CLAUDE.md -- it's a full Claude
	//      Code project with its own MCP servers. Classify as
	//      `claude-project` and delegate questions to a `claude --print`
	//      sub-agent running inside the folder. Example: ~/dev/health
	//      with stdio MCP servers for trainingpeaks/garmin/fatsecret.
	//   2. Folder has BOTH CLAUDE.md AND data files (CSV/SQL) -- a
	//      documented data wrapper. Classify as `tool` so the ExecAdapter
	//      passes LLM-generated shell commands through verbatim.
	//   3. Bare data files with no docs -- use aida's built-in adapter
	//      directly (csv / data-source).
	//   4. Docs / code fallbacks.
	//
	// A CLAUDE.md on a plain code repo does NOT make it a data source --
	// otherwise every repo in ~/dev becomes a fake source.
	d := &DiscoveredSource{
		Name:        slugFolder(filepath.Base(dir)),
		Path:        homeRelative(dir),
		ContextFile: contextFile,
	}

	hasData := len(csvs) > 0 || len(sqls) > 0 || len(dbs) > 0
	_ = scripts // build/dev scripts aren't a data signal on their own
	switch {
	case hasMCPConfig && contextFile == "CLAUDE.md":
		d.Kind = "data"
		d.Type = "claude-project"
	case contextFile == "CLAUDE.md" && len(dbs) > 0 && !hasProjectMarker:
		// CLAUDE.md + sqlite database = use the dedicated sqlite
		// adapter, which runs sqlite3 directly (no shell interpolation
		// of the SQL). This prevents the classic failure where the
		// LLM produces a valid SQL statement containing parens
		// (e.g. `date('now', '-1 month')`) and the ExecAdapter's
		// `sh -c` mis-parses it as a subshell.
		d.Kind = "data"
		d.Type = "sqlite"
		d.Assets = map[string]string{"db": dbs[0]}
	case contextFile == "CLAUDE.md" && hasData && !hasProjectMarker:
		// CLAUDE.md + actual data files (csv/sql/sqlite) + NOT a code
		// project = a documented data wrapper. The user wrote a
		// CLAUDE.md to teach Claude how to query the data, so the LLM
		// has instructions for it. We deliberately do NOT count
		// scripts (.py/.sh) as a data signal because most repos have
		// build scripts; that would re-create the "every CLAUDE.md
		// becomes a source" problem.
		d.Kind = "data"
		d.Type = "tool"
		if len(csvs) > 0 {
			d.Assets = map[string]string{}
			for _, f := range csvs {
				d.Assets[assetKey(f)] = f
			}
		}
	case len(csvs) > 0:
		d.Kind = "data"
		d.Type = "csv"
		d.Assets = map[string]string{}
		for _, f := range csvs {
			d.Assets[assetKey(f)] = f
		}
	case len(sqls) > 0:
		d.Kind = "data"
		d.Type = "data-source"
	case contextFile != "" || len(mds) >= 2:
		if hasCode {
			d.Kind = "code"
			d.Type = "codebase"
		} else {
			d.Kind = "docs"
			d.Type = "docs"
		}
	case hasCode:
		d.Kind = "code"
		d.Type = "codebase"
	default:
		return nil
	}

	d.Description = inferDescription(dir, contextFile, d.Kind, d.Type)
	// Compute name tokens from the original (pre-slug) basename so
	// camelCase folders like "GeminiWatermarkTool" split into useful
	// tokens. The slug ("geminiwatermarktool") loses those boundaries.
	nameTokens := splitNameTokens(filepath.Base(dir))
	d.Capabilities, d.Entities = inferCapabilitiesAndEntities(d.Name, nameTokens, d.Type, csvs)
	// Extract GitHub owner/repo from git remote if available.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		cmd := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url")
		if out, err := cmd.Output(); err == nil {
			d.Repo = config.ParseGitHubRepo(strings.TrimSpace(string(out)))
		}
	}

	return d
}

func slugFolder(name string) string {
	s := strings.ToLower(name)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '_', r == '-':
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// splitNameTokens turns a folder basename into a list of lowercase
// tokens. It splits on hyphens, underscores, spaces, and camelCase
// boundaries - so "csv-viewer" → ["csv", "viewer"], "mcp_perforce" →
// ["mcp", "perforce"], "GeminiWatermarkTool" → ["gemini", "watermark",
// "tool"]. Tokens shorter than 4 characters are dropped to avoid noise
// from generic suffixes like "cli", "api", "mcp", "app".
//
// The returned tokens are used by inferCapabilitiesAndEntities to
// register the source's name as an entity so the planner's topic-match
// path fires when the question literally names the source.
func splitNameTokens(basename string) []string {
	if basename == "" {
		return nil
	}
	// First split on hyphens, underscores, spaces, and camelCase
	// boundaries (lowercase → uppercase transition).
	var tokens []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			tokens = append(tokens, strings.ToLower(string(current)))
			current = nil
		}
	}
	prev := rune(0)
	for _, r := range basename {
		// Delimiters: hyphen, underscore, space, dot.
		if r == '-' || r == '_' || r == ' ' || r == '.' {
			flush()
			prev = r
			continue
		}
		// CamelCase: lowercase followed by uppercase starts a new token.
		if r >= 'A' && r <= 'Z' && prev >= 'a' && prev <= 'z' {
			flush()
		}
		current = append(current, r)
		prev = r
	}
	flush()
	// Drop short tokens that would cause false positives.
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if len(t) >= 4 {
			out = append(out, t)
		}
	}
	return out
}

func assetKey(filename string) string {
	base := strings.TrimSuffix(strings.ToLower(filename), filepath.Ext(filename))
	base = strings.TrimPrefix(base, "consolidated_")
	base = strings.ReplaceAll(base, "_", "-")
	return base
}

// inferDescription returns the first non-empty non-heading line of the
// context doc if present, otherwise a canned fallback based on type.
func inferDescription(dir, contextFile, kind, typ string) string {
	if contextFile != "" {
		if body, err := os.ReadFile(filepath.Join(dir, contextFile)); err == nil {
			for _, line := range strings.Split(string(body), "\n") {
				t := strings.TrimSpace(line)
				if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "<!--") {
					continue
				}
				if len(t) > 200 {
					t = t[:200] + "..."
				}
				return t
			}
		}
	}
	base := filepath.Base(dir)
	switch kind {
	case "data":
		return fmt.Sprintf("%s data (%s) discovered at %s", strings.Title(base), typ, dir)
	case "docs":
		return fmt.Sprintf("%s docs discovered at %s", strings.Title(base), dir)
	default:
		return fmt.Sprintf("%s codebase discovered at %s", strings.Title(base), dir)
	}
}

// inferCapabilitiesAndEntities turns a folder name + type into a best-guess
// set of capabilities and entity labels that the planner can match against.
// The router uses these to pick sources for a query, so the rules here
// directly determine whether "latest workout" finds your workouts folder.
//
// nameTokens are the lowercase tokens extracted from the original (pre-slug)
// folder basename - see splitNameTokens. They are appended to the entity
// list so the planner's topic-match path fires when the question literally
// names the source. The full slug name is also always added.
func inferCapabilitiesAndEntities(name string, nameTokens []string, typ string, csvFiles []string) ([]string, []string) {
	caps := []string{}
	ents := []string{}
	n := strings.ToLower(name)

	switch typ {
	case "claude-project":
		caps = append(caps, "subagent-delegation", "mcp-backed", "documented-interface")
	case "sqlite":
		caps = append(caps, "sql-query", "data-lookup", "expense-tracking", "csv-query")
	case "tool":
		caps = append(caps, "tool-invocation", "documented-interface")
	case "csv":
		caps = append(caps, "csv-query", "flat-file-lookup")
	case "data-source":
		caps = append(caps, "sql-query", "data-lookup")
	case "docs":
		caps = append(caps, "partner-docs", "doc-search")
	case "codebase":
		caps = append(caps, "code-reference")
	}

	// Keyword-based enrichment. Keep the list short and high-signal.
	keywordMap := map[string][]string{
		"workout":  {"fitness-tracking", "workout-lookup", "health-metrics"},
		"fitness":  {"fitness-tracking", "health-metrics"},
		"run":      {"fitness-tracking"},
		"weight":   {"fitness-tracking", "health-metrics"},
		"sleep":    {"health-metrics"},
		"finance":  {"expense-tracking", "budget-lookup"},
		"budget":   {"expense-tracking", "budget-lookup"},
		"expense":  {"expense-tracking"},
		"spending": {"expense-tracking"},
		"note":     {"note-search"},
		"journal":  {"note-search", "journal-lookup"},
	}
	entityMap := map[string][]string{
		"workout": {"workouts", "fitness", "health"},
		"health":  {"health", "workouts", "fitness", "nutrition"},
		"fitness": {"fitness"},
		"finance": {"finances"},
		"budget":  {"budget"},
		"expense": {"expenses"},
	}
	for kw, extra := range keywordMap {
		if strings.Contains(n, kw) {
			caps = append(caps, extra...)
		}
	}
	for kw, extra := range entityMap {
		if strings.Contains(n, kw) {
			ents = append(ents, extra...)
		}
	}

	// Also pull entity hints from CSV filenames (e.g. consolidated_sleep.csv → "sleep").
	for _, f := range csvFiles {
		key := assetKey(f)
		if key != "" && !contains(ents, key) {
			ents = append(ents, key)
		}
	}

	// Register the source's own slug name and its component tokens as
	// entities so the planner's topic-match path fires when the question
	// literally names the source. Always include the full slug; only
	// include tokens that survived the length gate (>= 4) and are not
	// on the generic blocklist (tool/data/docs/etc).
	if name != "" && !contains(ents, name) {
		ents = append(ents, name)
	}
	for _, tok := range nameTokens {
		if isGenericLibraryToken(tok) {
			continue
		}
		if !contains(ents, tok) {
			ents = append(ents, tok)
		}
	}

	// Dedup capabilities.
	seen := map[string]bool{}
	var out []string
	for _, c := range caps {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, ents
}

// genericLibraryTokens are name fragments common across many folders that
// would cause false-positive matches if registered as entities. Kept here
// (separate from internal/engine's blocklist) so the library package
// doesn't import engine. Update both lists if either changes.
var genericLibraryTokens = map[string]bool{
	"main":  true,
	"core":  true,
	"base":  true,
	"util":  true,
	"utils": true,
	"test":  true,
	"tests": true,
	"data":  true,
	"docs":  true,
	"tool":  true,
	"tools": true,
	"lib":   true,
	"libs":  true,
	"app":   true,
	"apps":  true,
	"src":   true,
}

func isGenericLibraryToken(tok string) bool {
	return genericLibraryTokens[strings.ToLower(tok)]
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// WriteDiscovered persists the discovered sources into a library root:
//   - writes <root>/sources/<name>.yaml for each discovery
//   - writes <root>/layers/sources/<name>.md with the context doc body
//   - adds manifest entries (sources: and layers:)
//   - appends a route: `match_cwd: "<path>/**"` → [source, layer]
//
// WriteAction describes what WriteDiscovered did with a single source file.
type WriteAction string

const (
	// WriteActionCreate means the source YAML did not exist and was written fresh.
	WriteActionCreate WriteAction = "create"
	// WriteActionUpdate means the source YAML existed; some fields were merged in.
	WriteActionUpdate WriteAction = "update"
	// WriteActionSkip means the source YAML existed and no fields changed.
	WriteActionSkip WriteAction = "skip"
)

// WriteReport is the per-source summary returned by WriteDiscovered. Callers
// render it as the end-of-scan change table so Ryan can eyeball what was
// touched before any manual edits get overwritten.
type WriteReport struct {
	Name          string
	File          string // relative path under rootPath
	Action        WriteAction
	FieldsChanged []string // only populated when Action is create|update
}

// WriteDiscovered merges each DiscoveredSource into the library under
// rootPath. By default it is a MERGE: existing YAMLs keep their manually
// set entities/capabilities/search/description; only missing fields get
// filled in from the discovery. Passing force=true restores the old
// overwrite-everything behavior (useful for mass re-scans from scratch).
//
// Returns a WriteReport per discovery so the caller can print a summary.
// The second return is the count of sources written for backwards compat.
func WriteDiscovered(rootPath string, discoveries []DiscoveredSource) (int, error) {
	_, n, err := writeDiscovered(rootPath, discoveries, false)
	return n, err
}

// WriteDiscoveredMerge is the explicit merge-aware entry point. Returns a
// report plus the count of sources processed. Callers that want to show
// the scan summary should use this.
func WriteDiscoveredMerge(rootPath string, discoveries []DiscoveredSource, force bool) ([]WriteReport, int, error) {
	return writeDiscovered(rootPath, discoveries, force)
}

func writeDiscovered(rootPath string, discoveries []DiscoveredSource, force bool) ([]WriteReport, int, error) {
	reports := make([]WriteReport, 0, len(discoveries))
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return nil, 0, err
	}
	for _, sub := range []string{
		"sources",
		filepath.Join("layers", "sources"),
	} {
		if err := os.MkdirAll(filepath.Join(rootPath, sub), 0755); err != nil {
			return nil, 0, err
		}
	}

	manifest, err := LoadManifest(rootPath)
	if err != nil {
		return nil, 0, err
	}
	if manifest.Sources == nil {
		manifest.Sources = map[string]SourceEntry{}
	}
	if manifest.Layers == nil {
		manifest.Layers = map[string]LayerEntry{}
	}

	// Prune orphaned manifest entries whose backing files no longer
	// exist. Without this, a re-scan (e.g. after rm sources/*.yaml)
	// leaves phantom references that crash LoadSources.
	// Use resolveUnderRoot so absolute-path layers (like the `personal`
	// layer pointing at ~/.claude/CLAUDE.md) are checked at their real
	// location, not a bogus rootPath+abs join.
	for name, entry := range manifest.Sources {
		if _, err := os.Stat(resolveUnderRoot(rootPath, entry.File)); err != nil {
			delete(manifest.Sources, name)
		}
	}
	protectedLayers := map[string]bool{"global": true, "personal": true}
	for name, entry := range manifest.Layers {
		if _, err := os.Stat(resolveUnderRoot(rootPath, entry.File)); err != nil {
			// Never prune the baseline layers -- init is responsible
			// for them and scan should treat them as authoritative.
			if protectedLayers[name] {
				continue
			}
			delete(manifest.Layers, name)
		}
	}

	// Build a lookup of (expanded path) → existing source name so the
	// scanner can SKIP discoveries whose folder is already registered
	// under a different slug - preventing spurious dupes like
	// `acme_widgets` getting created when `acme-widgets` already points
	// at the same `~/dev/acme_widgets` directory.
	existingByPath := map[string]string{}
	for name, entry := range manifest.Sources {
		var src config.Source
		data, readErr := os.ReadFile(resolveUnderRoot(rootPath, entry.File))
		if readErr != nil {
			continue
		}
		if yaml.Unmarshal(data, &src) != nil || src.Path == "" {
			continue
		}
		existingByPath[expandPath(src.Path)] = name
	}

	existingRoutes, err := LoadRoutes(rootPath)
	if err != nil {
		return nil, 0, err
	}
	// Also drop routes whose match_cwd points at a folder that no
	// longer exists on disk. (Self-healing for re-scans after
	// re-organizing ~/dev.)
	pruned := existingRoutes[:0]
	for _, r := range existingRoutes {
		if r.MatchCwd != "" {
			candidate := strings.TrimSuffix(expandPath(r.MatchCwd), "/**")
			if _, err := os.Stat(candidate); err != nil {
				continue
			}
		}
		pruned = append(pruned, r)
	}
	existingRoutes = pruned

	written := 0
	for _, d := range discoveries {
		// 0. Path-collision skip: if some other slug already points at
		//    the same folder, do nothing - re-creating the registration
		//    under the discovered slug would just clutter the registry
		//    with `acme_widgets` vs `acme-widgets` style dupes.
		if existing, ok := existingByPath[expandPath(d.Path)]; ok && existing != d.Name {
			reports = append(reports, WriteReport{
				Name:          d.Name,
				File:          filepath.Join("sources", d.Name+".yaml"),
				Action:        WriteActionSkip,
				FieldsChanged: []string{"already registered as " + existing},
			})
			continue
		}
		// 1. Build the source from the discovery, then merge with any
		//    existing YAML on disk so manually-tuned fields survive.
		//    homeRelative-collapse the path here (in addition to the
		//    classifier-side normalization) so discoveries coming
		//    from the LLM-classifier code path also land as
		//    ~/-relative - no caller can accidentally write absolute
		//    paths into the registry.
		src := config.Source{
			Path:         homeRelative(d.Path),
			Type:         d.Type,
			Repo:         d.Repo,
			Description:  d.Description,
			Capabilities: d.Capabilities,
			Entities:     d.Entities,
			Assets:       d.Assets,
			Profiles:     d.Profiles,
		}
		if d.ContextFile != "" {
			src.Context = d.ContextFile
		}
		// Note: we deliberately do NOT auto-populate exec for type:tool
		// sources. A bare `query: '{query}'` template would feed the
		// LLM's raw output directly to `sh -c`, and the load-time
		// validator drops sources with that shape - so the
		// auto-generated stub silently disabled the source. Leaving
		// exec empty surfaces the gap via `aida lint` ("type: tool with
		// no exec template") so the user adds a real command prefix
		// like `mytool {query}`.
		cfgRel := filepath.Join("sources", d.Name+".yaml")
		cfgAbs := filepath.Join(rootPath, cfgRel)

		action := WriteActionCreate
		var changedFields []string
		if existingBytes, err := os.ReadFile(cfgAbs); err == nil {
			// File exists - merge discovery into it unless force=true.
			if force {
				action = WriteActionUpdate
				changedFields = []string{"all fields (--force)"}
			} else {
				var existing config.Source
				if yaml.Unmarshal(existingBytes, &existing) == nil {
					merged, diff := mergeSourceConfig(existing, src)
					src = merged
					if len(diff) == 0 {
						action = WriteActionSkip
					} else {
						action = WriteActionUpdate
						changedFields = diff
					}
				}
			}
		}

		if action != WriteActionSkip {
			data, err := yaml.Marshal(src)
			if err != nil {
				return reports, written, fmt.Errorf("marshaling source %q: %w", d.Name, err)
			}
			if err := os.WriteFile(cfgAbs, data, 0644); err != nil {
				return reports, written, fmt.Errorf("writing %s: %w", cfgRel, err)
			}
		}
		manifest.Sources[d.Name] = SourceEntry{File: cfgRel}
		reports = append(reports, WriteReport{
			Name:          d.Name,
			File:          cfgRel,
			Action:        action,
			FieldsChanged: changedFields,
		})

		// 2. Copy the context doc into a layer file (so read-only source
		//    folders still work after the folder is deleted/moved).
		//
		//    Existing layer files are preserved unless force=true. Once
		//    a layer doc has been written, hand-tuning it (e.g. the NYT
		//    subcommand routing table) should survive subsequent scans -
		//    otherwise re-running `aida index --generate` silently
		//    obliterates curated executor guidance with whatever the
		//    upstream README happens to say. force=true still wins for
		//    intentional resyncs.
		layerRel := ""
		if d.ContextFile != "" {
			layerRel = filepath.Join("layers", "sources", d.Name+".md")
			layerAbs := filepath.Join(rootPath, layerRel)
			_, statErr := os.Stat(layerAbs)
			if statErr != nil || force {
				if body, err := os.ReadFile(filepath.Join(expandPath(d.Path), d.ContextFile)); err == nil {
					header := fmt.Sprintf("# Source: %s\n\n%s\n\n", d.Name, d.Description)
					layerBody := append([]byte(header), body...)
					if d.Repo != "" {
						ghSection := fmt.Sprintf("\n\n## GitHub\n\nRepository: %s\nIssues: `gh issue list --repo %s`\nPRs: `gh pr list --repo %s`\n", d.Repo, d.Repo, d.Repo)
						layerBody = append(layerBody, []byte(ghSection)...)
					}
					if err := os.WriteFile(layerAbs, layerBody, 0644); err != nil {
						return reports, written, fmt.Errorf("writing layer %s: %w", layerRel, err)
					}
				}
			}
			manifest.Layers["sources/"+d.Name] = LayerEntry{File: layerRel}
		}

		// 3. Add a route that activates this source when cwd is under the
		//    discovered folder. Dedupe against existing routes by match_cwd.
		//
		// Skip cwd-routes for `git` sources: they aggregate over an
		// entire scan-root (e.g. ~/dev), so a cwd-route would fire for
		// EVERY query run from anywhere under ~/dev -- including non-git
		// questions like "how much did I spend last month?". Git sources
		// instead rely on entity/keyword topic matching in the planner.
		if d.Type != "git" {
			routeCwd := homeRelative(d.Path) + "/**"
			if !hasCwdRoute(existingRoutes, routeCwd) {
				route := Route{
					MatchCwd: routeCwd,
					Sources:  []string{d.Name},
				}
				if layerRel != "" {
					route.Layers = []string{"sources/" + d.Name}
				}
				existingRoutes = append(existingRoutes, route)
			}
		}

		written++
	}

	// Ensure a baseline `always: true` route exists so the global +
	// personal layers are activated for every query even after a scan
	// has added match_cwd routes. Without this, a user who rm's their
	// routes.yaml and re-scans would lose the baseline.
	hasAlways := false
	for _, r := range existingRoutes {
		if r.Always {
			hasAlways = true
			break
		}
	}
	if !hasAlways {
		existingRoutes = append([]Route{{Always: true, Layers: []string{"global", "personal"}}}, existingRoutes...)
	}

	if err := SaveManifest(rootPath, manifest); err != nil {
		return reports, written, err
	}
	if err := saveRoutes(rootPath, existingRoutes); err != nil {
		return reports, written, err
	}
	return reports, written, nil
}

// mergeSourceConfig returns a merged Source plus a list of field names whose
// values were replaced (from discovery overriding existing) or filled in
// (existing was empty). Manually-set fields in `existing` win over discovery
// for the lists/descriptions the user typically tunes by hand. Path/Type/
// Repo/Context are refreshed from discovery because those reflect on-disk
// reality - if the repo moved or was renamed, scans should reflect that.
func mergeSourceConfig(existing, discovered config.Source) (config.Source, []string) {
	merged := existing
	var diff []string

	// Always refresh on-disk facts. Compare expanded paths so an existing
	// `~/dev/X` is not flagged as different from a discovery's
	// `/Users/<me>/dev/X` - that re-expansion was breaking the dual-machine
	// setup by spuriously rewriting every source's path on every scan.
	expE, expD := expandPath(existing.Path), expandPath(discovered.Path)
	switch {
	case expE != expD:
		merged.Path = discovered.Path
		diff = append(diff, "path")
	case existing.Path != discovered.Path && strings.HasPrefix(discovered.Path, "~"):
		// Same logical location but existing is absolute and discovered
		// is ~-relative (cleanup pass after the prior absolute-path
		// regression). Collapse to ~-relative for portability across
		// the home/work dual-machine setup.
		merged.Path = discovered.Path
		diff = append(diff, "path-normalized")
	}
	if existing.Type == "" && discovered.Type != "" {
		merged.Type = discovered.Type
		diff = append(diff, "type")
	}
	if existing.Repo != discovered.Repo && discovered.Repo != "" {
		merged.Repo = discovered.Repo
		diff = append(diff, "repo")
	}
	if existing.Context == "" && discovered.Context != "" {
		merged.Context = discovered.Context
		diff = append(diff, "context")
	}

	// Preserve manual tuning for description/entities/capabilities/search.
	if existing.Description == "" && discovered.Description != "" {
		merged.Description = discovered.Description
		diff = append(diff, "description")
	}
	if len(existing.Entities) == 0 && len(discovered.Entities) > 0 {
		merged.Entities = discovered.Entities
		diff = append(diff, "entities")
	}
	if len(existing.Capabilities) == 0 && len(discovered.Capabilities) > 0 {
		merged.Capabilities = discovered.Capabilities
		diff = append(diff, "capabilities")
	}
	if existing.Search == nil && discovered.Search != nil {
		merged.Search = discovered.Search
		diff = append(diff, "search")
	}
	// Exec: only fill if missing. Existing exec (even the raw {query}
	// shape) is treated as deliberate so users can opt into wrappers.
	if len(existing.Exec) == 0 && len(discovered.Exec) > 0 {
		merged.Exec = discovered.Exec
		diff = append(diff, "exec")
	}
	if len(existing.Assets) == 0 && len(discovered.Assets) > 0 {
		merged.Assets = discovered.Assets
		diff = append(diff, "assets")
	}

	return merged, diff
}

func hasCwdRoute(routes []Route, cwd string) bool {
	target := expandPath(strings.TrimSuffix(cwd, "/**"))
	for _, r := range routes {
		if r.MatchCwd == "" {
			continue
		}
		existing := expandPath(strings.TrimSuffix(r.MatchCwd, "/**"))
		if existing == target {
			return true
		}
	}
	return false
}

// saveRoutes writes routes.yaml, preserving the stub header when possible.
func saveRoutes(rootPath string, routes []Route) error {
	doc := routesDoc{Routes: routes}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(rootPath, RoutesFile), data, 0644)
}
