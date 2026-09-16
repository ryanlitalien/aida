package roster

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"gopkg.in/yaml.v3"
)

// Severity values for LintIssue.
const (
	LintError = "error"
	LintWarn  = "warn"
)

// LintIssue is one problem found in ~/.aida/roster.yaml, attributed to the
// entry that caused it. Severity "error" fails `aida roster lint`
// (non-zero exit); "warn" is advisory only.
type LintIssue struct {
	Entry    string
	Severity string // LintError | LintWarn
	Message  string
}

// HasError reports whether any issue in issues has severity LintError --
// the signal `aida roster lint` uses to decide its exit code.
func HasError(issues []LintIssue) bool {
	for _, i := range issues {
		if i.Severity == LintError {
			return true
		}
	}
	return false
}

// slugPattern is the authoring convention Lint enforces for entry names and
// subagent slugs: lowercase alphanumerics and hyphens, starting with an
// alphanumeric. Load() is more forgiving -- it lowercases keys and would
// even tolerate an underscore as an opaque map key -- but Lint holds
// authors to the stricter, unambiguous slug shape used everywhere else in
// the roster (call-signs, aliases, .claude/agents/*.md filenames).
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validKinds is the set of legal Entry.Kind values.
var validKinds = map[string]bool{
	KindSubagent: true,
	KindSource:   true,
	KindMCP:      true,
	KindJob:      true,
	KindAida:     true,
}

// Lint validates aidaDir/roster.yaml and returns every problem found,
// sorted errors-first then by entry name.
//
// Unlike Load, Lint works over the RAW entries as authored: a discovery
// parent is checked as itself (its dir exists and holds at least one
// *.md file), not expanded into its virtual personas, and problems that
// Load silently drops into r.Issues() (or simply tolerates, like an
// uppercase entry name) are surfaced here as first-class findings.
//
// A missing roster.yaml is not an error -- it returns (nil, nil), the same
// "nothing configured yet" contract as Load.
func Lint(aidaDir string, availableSources []string) ([]LintIssue, error) {
	path := filepath.Join(aidaDir, "roster.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading roster %q: %w", path, err)
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing roster %q: %w", path, err)
	}

	sourceSet := make(map[string]bool, len(availableSources))
	for _, s := range availableSources {
		sourceSet[strings.ToLower(s)] = true
	}

	var issues []LintIssue
	var canonical []*Entry // Name lowercased, for the cross-entry checks below
	aidaCount := 0
	var aidaEntry *Entry

	for rawName, e := range f.Entries {
		name := strings.ToLower(rawName)
		e.Name = name
		canonical = append(canonical, e)

		if !slugPattern.MatchString(rawName) {
			issues = append(issues, LintIssue{Entry: name, Severity: LintError,
				Message: fmt.Sprintf("entry name %q must match %s", rawName, slugPattern.String())})
		}

		if !validKinds[e.Kind] {
			issues = append(issues, LintIssue{Entry: name, Severity: LintError,
				Message: fmt.Sprintf("invalid kind %q", e.Kind)})
			continue // the rest of this loop assumes a recognized kind
		}

		if e.Kind == KindAida {
			aidaCount++
			aidaEntry = e
		}

		issues = append(issues, validateSpecBlock(name, e)...)

		switch {
		case e.Discover != "":
			issues = append(issues, validateDiscoverDir(name, e)...)
		case e.Kind == KindSubagent && e.Subagent != nil:
			issues = append(issues, validateSubagentSlugAndDir(name, e)...)
		case e.Kind == KindSource && e.Source != nil:
			if !sourceSet[strings.ToLower(e.Source.Source)] {
				issues = append(issues, LintIssue{Entry: name, Severity: LintError,
					Message: fmt.Sprintf("source %q is not an available library source", e.Source.Source)})
			}
		}
	}

	issues = append(issues, detectDuplicates(canonical)...)
	issues = append(issues, detectSourceCollisions(canonical, sourceSet)...)

	switch {
	case aidaCount != 1:
		issues = append(issues, LintIssue{Entry: "roster", Severity: LintError,
			Message: fmt.Sprintf("expected exactly one entry with kind %q, found %d", KindAida, aidaCount)})
	case aidaEntry.Name != "aida":
		issues = append(issues, LintIssue{Entry: aidaEntry.Name, Severity: LintError,
			Message: fmt.Sprintf("the kind %q entry must be named %q", KindAida, "aida")})
	}

	sortLintIssues(issues)
	return issues, nil
}

// validateSpecBlock enforces "exactly one backend spec block, matching
// kind": e.g. kind subagent must have subagent: populated and none of
// source/mcp/job. KindAida carries no spec block at all and is exempt.
// A discovery parent is a shape of its own: kind subagent, a subagent:
// block (for Dir only), and nothing else -- it is not itself a dispatch
// target, so it doesn't go through the generic per-kind check below.
func validateSpecBlock(name string, e *Entry) []LintIssue {
	if e.Kind == KindAida {
		return nil
	}

	populated := map[string]bool{
		KindSubagent: e.Subagent != nil,
		KindSource:   e.Source != nil,
		KindMCP:      e.MCP != nil,
		KindJob:      e.Job != nil,
	}
	count := 0
	for _, ok := range populated {
		if ok {
			count++
		}
	}

	if e.Discover != "" {
		if e.Kind != KindSubagent || !populated[KindSubagent] || count != 1 {
			return []LintIssue{{Entry: name, Severity: LintError,
				Message: "discover entry must have kind subagent and a subagent: block (for dir), and no other backend spec"}}
		}
		return nil
	}

	if !populated[e.Kind] {
		return []LintIssue{{Entry: name, Severity: LintError,
			Message: fmt.Sprintf("kind %q needs a %s: block", e.Kind, e.Kind)}}
	}
	if count != 1 {
		return []LintIssue{{Entry: name, Severity: LintError,
			Message: fmt.Sprintf("entry has more than one backend spec block populated; kind %q needs only %s:", e.Kind, e.Kind)}}
	}
	return nil
}

// validateDiscoverDir checks that a discovery parent's dir exists and
// holds at least one *.md file -- the same requirement expandDiscovery
// enforces at Load time, checked here directly so a broken discovery dir
// shows up as a lint error rather than only a runtime r.Issues() entry.
func validateDiscoverDir(name string, e *Entry) []LintIssue {
	dir := config.ExpandPath(e.Discover)
	n, err := countMarkdownFiles(dir)
	if err != nil || n == 0 {
		return []LintIssue{{Entry: name, Severity: LintError,
			Message: fmt.Sprintf("discover dir %q missing or has no *.md files", e.Discover)}}
	}
	return nil
}

// countMarkdownFiles counts the non-hidden *.md files directly inside dir.
func countMarkdownFiles(dir string) (int, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
			continue
		}
		if strings.HasSuffix(f.Name(), ".md") {
			n++
		}
	}
	return n, nil
}

// validateSubagentSlugAndDir checks a non-discovery subagent entry's
// agent slug shape and (best-effort, warn-only) that its dir and agent
// markdown file actually exist on disk.
func validateSubagentSlugAndDir(name string, e *Entry) []LintIssue {
	var issues []LintIssue

	if e.Subagent.Agent != "" && !slugPattern.MatchString(e.Subagent.Agent) {
		issues = append(issues, LintIssue{Entry: name, Severity: LintError,
			Message: fmt.Sprintf("subagent.agent %q must match %s", e.Subagent.Agent, slugPattern.String())})
	}

	// subagent.Model is passed verbatim as a single argv element to
	// `claude --model <value>` (see subagent.go) -- there is no shell to
	// inject into, so this is a flag-injection guard, not a model
	// registry. New model names/aliases appear constantly, so we never
	// allowlist them: we only reject shapes claude would misparse, a
	// leading '-' (read as another flag) or embedded whitespace (not a
	// single argv token).
	if m := e.Subagent.Model; m != "" {
		if strings.HasPrefix(m, "-") || strings.ContainsAny(m, " \t\r\n") {
			issues = append(issues, LintIssue{Entry: name, Severity: LintError,
				Message: fmt.Sprintf("subagent.model %q must be a single model name or alias (no leading '-', no whitespace); it is passed verbatim to claude --model", m)})
		}
	}

	if e.Subagent.Dir == "" {
		return issues
	}
	dir := config.ExpandPath(e.Subagent.Dir)
	if _, err := os.Stat(dir); err != nil {
		issues = append(issues, LintIssue{Entry: name, Severity: LintWarn,
			Message: fmt.Sprintf("subagent.dir %q does not exist", e.Subagent.Dir)})
	}

	if e.Subagent.Agent != "" {
		inDir := filepath.Join(dir, ".claude", "agents", e.Subagent.Agent+".md")
		home, _ := os.UserHomeDir()
		inHome := filepath.Join(home, ".claude", "agents", e.Subagent.Agent+".md")
		if !isFile(inDir) && !isFile(inHome) {
			issues = append(issues, LintIssue{Entry: name, Severity: LintWarn,
				Message: fmt.Sprintf("no agent file found for %q (checked %s and %s)", e.Subagent.Agent, inDir, inHome)})
		}
	}
	return issues
}

// isFile reports whether path exists and is a regular file (not a dir).
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// detectDuplicates errors on any Name/CallSign/Alias that, after
// normalizeName, is claimed by more than one distinct entry -- a spoken
// reference or `aida ask <who>` couldn't tell them apart.
func detectDuplicates(entries []*Entry) []LintIssue {
	owners := map[string]map[string]bool{} // normalized identifier -> owning entry names
	for _, e := range entries {
		ids := []string{e.Name}
		if e.CallSign != "" {
			ids = append(ids, e.CallSign)
		}
		ids = append(ids, e.Aliases...)

		for _, id := range ids {
			norm := normalizeName(id)
			if norm == "" {
				continue
			}
			if owners[norm] == nil {
				owners[norm] = map[string]bool{}
			}
			owners[norm][e.Name] = true
		}
	}

	var norms []string
	for norm := range owners {
		norms = append(norms, norm)
	}
	sort.Strings(norms)

	var issues []LintIssue
	for _, norm := range norms {
		ownerSet := owners[norm]
		if len(ownerSet) < 2 {
			continue
		}
		names := make([]string, 0, len(ownerSet))
		for n := range ownerSet {
			names = append(names, n)
		}
		sort.Strings(names)
		issues = append(issues, LintIssue{
			Entry:    names[0],
			Severity: LintError,
			Message:  fmt.Sprintf("name/call-sign/alias %q collides across entries: %s", norm, strings.Join(names, ", ")),
		})
	}
	return issues
}

// detectSourceCollisions warns when an entry's call-sign, after
// normalizeName, matches the name of a real library source -- confusing
// for anyone trying to tell "ask the roster entry" from "pin the source"
// apart, even though nothing is actually broken.
func detectSourceCollisions(entries []*Entry, sourceSet map[string]bool) []LintIssue {
	var issues []LintIssue
	for _, e := range entries {
		if e.CallSign == "" {
			continue
		}
		if sourceSet[strings.ToLower(e.CallSign)] {
			issues = append(issues, LintIssue{Entry: e.Name, Severity: LintWarn,
				Message: fmt.Sprintf("call-sign %q collides with a library source of the same name", e.CallSign)})
		}
	}
	return issues
}

// sortLintIssues sorts errors before warnings, then by Entry.
func sortLintIssues(issues []LintIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		if (issues[i].Severity == LintError) != (issues[j].Severity == LintError) {
			return issues[i].Severity == LintError
		}
		return issues[i].Entry < issues[j].Entry
	})
}
