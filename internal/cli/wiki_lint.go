package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
)

// newWikiLintCmd builds `aida wiki lint` -- a read-only audit pass over the
// wiki repo, cloned from newBrainGardenCmd's shape (brain_garden.go):
// pure analysis function over already-loaded data, findings grouped by
// category, report tool rather than a gate. Unlike garden, `--strict`
// flips the exit code non-zero when findings are present, matching
// cli/lint.go's convention -- the wiki is expected to reach "clean" as
// pages get synthesized, so CI/pre-commit callers want an opt-in gate.
func newWikiLintCmd() *cobra.Command {
	var strict bool
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Audit the wiki repo for drift (dead links, orphans, missing aliases, ...)",
		Long: "Walks wiki/{projects,entities,concepts} plus wiki_status:\n" +
			"triaged_keep source notes under evernotes/ and reports six\n" +
			"categories of drift: dead-link, orphan-page, empty-page,\n" +
			"missing-aliases, dup-slug, frontmatter. Exit 0 by default\n" +
			"(report, not gate) -- pass --strict to fail CI/pre-commit on\n" +
			"any finding.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWikiLint(strict)
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "exit non-zero when any finding is present")
	return cmd
}

// wikiFinding is one drift signal found in the audit.
type wikiFinding struct {
	Category string // "dead-link" | "orphan-page" | "empty-page" | "missing-aliases" | "dup-slug" | "frontmatter"
	Subject  string // affected page/note path
	Detail   string // human-readable explanation
}

// wikiCategories is the fixed report order -- matches the plan's listed
// order and the reference Python implementation (wiki_tools.py).
var wikiCategories = []string{"dead-link", "orphan-page", "empty-page", "missing-aliases", "dup-slug", "frontmatter"}

func runWikiLint(strict bool) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}
	wikiRoot := cfg.WikiPath()

	pages, err := loadWikiPages(wikiRoot)
	if err != nil {
		return fmt.Errorf("load wiki pages: %w", err)
	}
	notes, err := loadKeptSourceNotes(wikiRoot)
	if err != nil {
		return fmt.Errorf("load kept source notes: %w", err)
	}
	// Dead-link resolution needs to know about every real file in the
	// repo, not just the pages/notes we fully parsed -- pages legitimately
	// link to things outside wiki/ and evernotes/ (e.g. _meta/*.md), and
	// those links are perfectly valid.
	existingFiles, err := listWikiRepoFiles(wikiRoot)
	if err != nil {
		return fmt.Errorf("list wiki repo files: %w", err)
	}
	var indexText string
	if data, err := os.ReadFile(filepath.Join(wikiRoot, "wiki", "index.md")); err == nil {
		indexText = string(data)
	}

	findings := auditWiki(pages, notes, indexText, existingFiles)

	fmt.Printf("Wiki lint: %d page(s), %d kept source note(s) scanned\n", len(pages), len(notes))
	if len(findings) == 0 {
		fmt.Println("No findings. Wiki looks clean.")
		return nil
	}
	fmt.Printf("\n%d finding(s) total\n", len(findings))

	byCategory := map[string][]wikiFinding{}
	for _, f := range findings {
		byCategory[f.Category] = append(byCategory[f.Category], f)
	}
	for _, cat := range wikiCategories {
		fs := byCategory[cat]
		if len(fs) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d):\n", cat, len(fs))
		for _, f := range fs {
			fmt.Printf("  - %s: %s\n", f.Subject, f.Detail)
		}
	}

	if strict {
		return fmt.Errorf("wiki lint found %d finding(s)", len(findings))
	}
	return nil
}

// ----- Fixture types (I/O side) -------------------------------------------

// wikiPage is one parsed page under wiki/{projects,entities,concepts}.
type wikiPage struct {
	RelPath    string // repo-root-relative, e.g. "wiki/projects/fighterbrands.md"
	HasFM      bool   // false = missing/unparseable frontmatter
	Type       string
	Title      string
	Aliases    []string
	Sources    []string
	WikiStatus string
	Body       string // content after the frontmatter block
}

// wikiSourceNote is one kept (wiki_status: triaged_keep) source note under
// evernotes/. Only kept notes are loaded -- lint's scope over source
// material follows `aida wiki index`'s scope (see wiki_index.go).
type wikiSourceNote struct {
	RelPath    string // repo-root-relative, e.g. "evernotes/StartWire-SQL/Tyler email messages.md"
	HasFM      bool
	Type       string
	Title      string
	WikiStatus string
}

// wikiFrontmatterRaw is the subset of OKF frontmatter fields lint checks.
// Deliberately duplicated (rather than shared) with
// internal/brain/wiki_index.go's equivalent struct -- lint is a pure
// filesystem audit with zero brain/DB dependency by design (mirrors
// brain_garden.go, which also doesn't reach into the brain package for
// its analysis), and that's worth a little duplication.
type wikiFrontmatterRaw struct {
	Type       string   `yaml:"type"`
	Title      string   `yaml:"title"`
	Aliases    []string `yaml:"aliases"`
	Sources    []string `yaml:"sources"`
	WikiStatus string   `yaml:"wiki_status"`
}

var wikiPageDirs = []string{"projects", "entities", "concepts"}

var frontmatterPattern = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

// splitFrontmatter separates a leading `---\n...\n---` YAML block from the
// rest of the file. ok is false when no frontmatter block is present.
func splitFrontmatter(data []byte) (fm string, body string, ok bool) {
	m := frontmatterPattern.FindSubmatch(data)
	if m == nil {
		return "", string(data), false
	}
	return string(m[1]), string(m[2]), true
}

// loadWikiPages walks wiki/{projects,entities,concepts}/*.md, matching the
// reference Python's pages() -- deliberately excludes wiki/index.md and
// wiki/log.md, which are OKF-reserved, not pages themselves.
func loadWikiPages(wikiRoot string) ([]wikiPage, error) {
	var out []wikiPage
	wikiDir := filepath.Join(wikiRoot, "wiki")
	for _, sub := range wikiPageDirs {
		dir := filepath.Join(wikiDir, sub)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // missing subdir is fine on a partial/fresh repo
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			abs := filepath.Join(dir, name)
			data, err := os.ReadFile(abs)
			if err != nil {
				continue
			}
			p := wikiPage{RelPath: filepath.ToSlash(filepath.Join("wiki", sub, name))}
			fm, body, ok := splitFrontmatter(data)
			p.Body = body
			if ok {
				var raw wikiFrontmatterRaw
				if yaml.Unmarshal([]byte(fm), &raw) == nil {
					p.HasFM = true
					p.Type = raw.Type
					p.Title = raw.Title
					p.Aliases = raw.Aliases
					p.Sources = raw.Sources
					p.WikiStatus = raw.WikiStatus
				}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// loadKeptSourceNotes walks evernotes/ recursively for notes whose
// frontmatter has wiki_status: triaged_keep. Notes without frontmatter,
// unparseable frontmatter, or any other status (triaged_skip,
// raw_converted, ...) are silently skipped -- they're out of lint's scope
// exactly as they're out of `aida wiki index`'s.
func loadKeptSourceNotes(wikiRoot string) ([]wikiSourceNote, error) {
	var out []wikiSourceNote
	root := filepath.Join(wikiRoot, "evernotes")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		fm, _, ok := splitFrontmatter(data)
		if !ok {
			return nil
		}
		var raw wikiFrontmatterRaw
		if yaml.Unmarshal([]byte(fm), &raw) != nil {
			return nil
		}
		if raw.WikiStatus != "triaged_keep" {
			return nil
		}
		rel, err := filepath.Rel(wikiRoot, path)
		if err != nil {
			rel = path
		}
		out = append(out, wikiSourceNote{
			RelPath:    filepath.ToSlash(rel),
			HasFM:      true,
			Type:       raw.Type,
			Title:      raw.Title,
			WikiStatus: raw.WikiStatus,
		})
		return nil
	})
	return out, nil
}

// listWikiRepoFiles walks the entire wiki repo (skipping .git) and
// returns every regular file's repo-root-relative path, casefolded. Dead-
// link resolution checks against this in addition to the loaded pages
// and kept source notes -- a page can legitimately link to something
// this tool never fully parses (e.g. _meta/*.md, an image), and that's
// not a dead link.
func listWikiRepoFiles(wikiRoot string) (map[string]bool, error) {
	out := make(map[string]bool)
	err := filepath.WalkDir(wikiRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(wikiRoot, path)
		if err != nil {
			return nil
		}
		out[strings.ToLower(filepath.ToSlash(rel))] = true
		return nil
	})
	return out, err
}

// ----- Pure analysis -------------------------------------------------------

// minWikiPageBody is the byte threshold below which a wiki page's body
// (post-frontmatter) is flagged empty-page. Ported from wiki_tools.py's
// 120-char threshold.
const minWikiPageBody = 120

var validWikiPageTypes = map[string]bool{"project": true, "entity": true, "concept": true}
var validWikiPageStatus = map[string]bool{"stub": true, "synthesized": true, "reviewed": true}

// auditWiki is the pure-data half of runWikiLint -- given already-loaded
// pages + kept source notes + index.md's raw text + a casefolded set of
// every real file in the repo (for dead-link resolution against targets
// this tool doesn't otherwise parse, e.g. _meta/*.md or images), returns
// every finding. existingFiles may be nil in tests that don't exercise
// links to non-page/non-note files -- dead-link resolution then falls
// back to just pages+notes+the two OKF-reserved filenames. Split out so
// unit tests can exercise the analysis without touching disk (mirrors
// auditGarden's split in brain_garden.go).
func auditWiki(pages []wikiPage, notes []wikiSourceNote, indexText string, existingFiles map[string]bool) []wikiFinding {
	var findings []wikiFinding

	// Existence index for dead-link resolution: every page + kept source
	// note + every other real repo file, keyed casefolded so a link that
	// differs only in case still resolves on a case-sensitive filesystem
	// the same way it silently would on APFS -- the whole point of
	// "casefold-aware". wiki/index.md and wiki/log.md are OKF-reserved
	// filenames (conventions.md #4-5) a conformant bundle always has,
	// even though they're not "pages" this audit loads -- treat links to
	// either as always resolving so a page's "back to index" link isn't a
	// false dead-link even when existingFiles is nil.
	exists := make(map[string]bool, len(pages)+len(notes)+len(existingFiles)+2)
	exists["wiki/index.md"] = true
	exists["wiki/log.md"] = true
	for k := range existingFiles {
		exists[k] = true
	}
	for _, p := range pages {
		exists[strings.ToLower(p.RelPath)] = true
	}
	for _, n := range notes {
		exists[strings.ToLower(n.RelPath)] = true
	}

	findings = append(findings, auditDupSlugs(pages, notes)...)

	linked := make(map[string]bool) // casefold(relpath) of every resolved link target seen

	for _, p := range pages {
		findings = append(findings, auditPageFrontmatter(p)...)

		if len(strings.TrimSpace(p.Body)) < minWikiPageBody {
			findings = append(findings, wikiFinding{
				Category: "empty-page",
				Subject:  p.RelPath,
				Detail:   fmt.Sprintf("body is %d chars (< %d threshold)", len(strings.TrimSpace(p.Body)), minWikiPageBody),
			})
		}

		for _, target := range extractLinks(p.Body) {
			resolved, ok := resolveWikiLink(p.RelPath, target)
			if !ok {
				continue // external URL, mailto:, or in-page anchor -- not a file target
			}
			linked[strings.ToLower(resolved)] = true
			if !exists[strings.ToLower(resolved)] {
				findings = append(findings, wikiFinding{
					Category: "dead-link",
					Subject:  p.RelPath,
					Detail:   fmt.Sprintf("-> %s (resolved %s, not found)", target, resolved),
				})
			}
		}
	}

	for _, n := range notes {
		if !n.HasFM {
			findings = append(findings, wikiFinding{Category: "frontmatter", Subject: n.RelPath, Detail: "no (or unparseable) frontmatter"})
			continue
		}
		if n.Type != "source_note" {
			findings = append(findings, wikiFinding{Category: "frontmatter", Subject: n.RelPath, Detail: fmt.Sprintf("invalid type %q (want source_note)", n.Type)})
		}
		if strings.TrimSpace(n.Title) == "" {
			findings = append(findings, wikiFinding{Category: "frontmatter", Subject: n.RelPath, Detail: "missing title"})
		}
	}

	// orphan-page: no inbound link from any page, and not referenced from
	// index.md's raw text (matching a page in either its full path or bare
	// filename -- index.md links use URL-encoded relative paths).
	for _, p := range pages {
		if linked[strings.ToLower(p.RelPath)] {
			continue
		}
		base := filepath.Base(p.RelPath)
		if strings.Contains(indexText, base) {
			continue
		}
		findings = append(findings, wikiFinding{
			Category: "orphan-page",
			Subject:  p.RelPath,
			Detail:   "no inbound links and not referenced from index.md",
		})
	}

	return findings
}

// auditPageFrontmatter checks one page's frontmatter shape: type, title,
// wiki_status, sources-on-synthesized, and (separately categorized)
// aliases.
func auditPageFrontmatter(p wikiPage) []wikiFinding {
	var out []wikiFinding
	if !p.HasFM {
		return []wikiFinding{{Category: "frontmatter", Subject: p.RelPath, Detail: "no (or unparseable) frontmatter"}}
	}
	if !validWikiPageTypes[p.Type] {
		out = append(out, wikiFinding{Category: "frontmatter", Subject: p.RelPath, Detail: fmt.Sprintf("invalid type %q (want project/entity/concept)", p.Type)})
	}
	if strings.TrimSpace(p.Title) == "" {
		out = append(out, wikiFinding{Category: "frontmatter", Subject: p.RelPath, Detail: "missing title"})
	}
	if !validWikiPageStatus[p.WikiStatus] {
		out = append(out, wikiFinding{Category: "frontmatter", Subject: p.RelPath, Detail: fmt.Sprintf("invalid wiki_status %q (want stub/synthesized/reviewed)", p.WikiStatus)})
	}
	if p.WikiStatus != "stub" && len(p.Sources) == 0 {
		out = append(out, wikiFinding{Category: "frontmatter", Subject: p.RelPath, Detail: "synthesized page has no sources citations"})
	}
	if len(p.Aliases) == 0 {
		out = append(out, wikiFinding{Category: "missing-aliases", Subject: p.RelPath, Detail: "no aliases -- duplicate detection and cross-linking depend on at least one"})
	}
	return out
}

// auditDupSlugs finds casefold-duplicate slugs: APFS is case-insensitive,
// so two files differing only in case silently overwrite each other there
// -- the bug that motivated this category (found during the evernotes
// collision repair). Wiki pages are checked globally (OKF's "casefold-
// unique across the whole bundle" rule, conventions.md #1); source notes
// are checked per-directory, since only a same-directory collision is a
// real on-disk conflict -- two different notebooks legitimately sharing a
// note title is not.
func auditDupSlugs(pages []wikiPage, notes []wikiSourceNote) []wikiFinding {
	var out []wikiFinding

	pageSlugs := map[string][]string{}
	for _, p := range pages {
		slug := strings.ToLower(strings.TrimSuffix(filepath.Base(p.RelPath), ".md"))
		pageSlugs[slug] = append(pageSlugs[slug], p.RelPath)
	}
	for _, slug := range sortedKeys(pageSlugs) {
		paths := pageSlugs[slug]
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		out = append(out, wikiFinding{
			Category: "dup-slug",
			Subject:  slug,
			Detail:   fmt.Sprintf("slug %q used by %d pages (casefold-duplicate): %s", slug, len(paths), strings.Join(paths, ", ")),
		})
	}

	noteSlugs := map[string][]string{}
	for _, n := range notes {
		key := filepath.Dir(n.RelPath) + "/" + strings.ToLower(strings.TrimSuffix(filepath.Base(n.RelPath), ".md"))
		noteSlugs[key] = append(noteSlugs[key], n.RelPath)
	}
	for _, key := range sortedKeys(noteSlugs) {
		paths := noteSlugs[key]
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		out = append(out, wikiFinding{
			Category: "dup-slug",
			Subject:  key,
			Detail:   fmt.Sprintf("%d source notes casefold-collide in the same directory: %s", len(paths), strings.Join(paths, ", ")),
		})
	}

	return out
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var (
	mdLinkAngleRe = regexp.MustCompile(`\]\(<([^>]+)>\)`)
	mdLinkPlainRe = regexp.MustCompile(`\]\(([^()<>\s]+)\)`)
	wikiLinkRe    = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
)

// extractLinks pulls every markdown link target (`[text](target)` and the
// angle-bracket form `[text](<target>)`) plus Obsidian-style `[[target]]`
// wikilinks out of a page body.
func extractLinks(body string) []string {
	var out []string
	for _, m := range mdLinkAngleRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	for _, m := range mdLinkPlainRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	for _, m := range wikiLinkRe.FindAllStringSubmatch(body, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// resolveWikiLink resolves a link target found in the page at relPath
// (itself repo-root-relative) to another repo-root-relative path. ok is
// false for external URLs, mailto: links, and pure in-page anchors --
// none of these have a file to check for existence.
func resolveWikiLink(relPath, target string) (string, bool) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", false
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
		return "", false
	}
	if idx := strings.Index(target, "#"); idx == 0 {
		return "", false // in-page anchor only, no file target
	} else if idx > 0 {
		target = target[:idx]
	}
	if unescaped, err := url.PathUnescape(target); err == nil {
		target = unescaped
	}
	dir := filepath.Dir(relPath)
	joined := filepath.ToSlash(filepath.Join(dir, target))
	return joined, true
}
