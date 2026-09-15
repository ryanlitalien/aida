package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newBrainProjectSoulCmd builds `aida brain project-soul` -- the
// projection half of the Claude Code <-> Aida memory bridge (see
// docs/claude-memory-bridge-design.md, open decision 3: soul.yaml is
// the authored source, CLAUDE.md's identity block is a generated
// projection of it, never hand-maintained in both places).
//
// The capture side already exists: brain_capture.go's stripSoulBlock
// strips this exact managed block back out before a CLAUDE.md edit is
// captured into brain memory, so the projection never round-trips into
// itself as a "fact."
func newBrainProjectSoulCmd() *cobra.Command {
	var dryRunFlag bool
	cmd := &cobra.Command{
		Use:   "project-soul",
		Short: "Project soul.yaml identity into ~/.claude/CLAUDE.md",
		Long: "Renders the user's soul.yaml (name, family, people) into a compact\n" +
			"identity digest and splices it as a managed block into\n" +
			"~/.claude/CLAUDE.md, so Claude Code always has this context without\n" +
			"a runtime lookup. Routing preferences and the voice block are\n" +
			"deliberately left out -- those are aida-pipeline-specific, not\n" +
			"identity.\n\n" +
			"Idempotent: re-running with an unchanged soul.yaml is a no-op.\n" +
			"Also runs automatically after a successful `aida brain sync`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProjectSoul(dryRunFlag)
		},
	}
	cmd.Flags().BoolVar(&dryRunFlag, "dry-run", false, "print the managed block without writing it")
	return cmd
}

// runProjectSoul renders soul.yaml and splices it into ~/.claude/CLAUDE.md.
// dryRunFlag is the command's own --dry-run flag; the global --dry-run
// flag (via dryRunGuard) is honored too, so either one triggers a
// preview-only run.
func runProjectSoul(dryRunFlag bool) error {
	soul, err := config.LoadSoul()
	if err != nil {
		return err
	}
	if soul.IsEmpty() {
		fmt.Println("no soul.yaml content to project")
		return nil
	}

	inner := soulDigest(soul)

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	claudeMDPath := filepath.Join(home, ".claude", "CLAUDE.md")

	existing, err := readFileOrEmpty(claudeMDPath)
	if err != nil {
		return err
	}

	newContent, changed, err := spliceManagedBlock(existing, inner)
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("CLAUDE.md soul block already up to date")
		return nil
	}

	if dryRunFlag || dryRunGuard("project soul", claudeMDPath) {
		fmt.Println(renderManagedSoulBlock(inner))
		return nil
	}

	if err := atomicWriteFile(claudeMDPath, newContent); err != nil {
		return err
	}
	fmt.Printf("%s Projected soul into %s\n", ui.SuccessIcon, claudeMDPath)
	return nil
}

// readFileOrEmpty reads path and returns its contents, or "" (no error)
// if the file doesn't exist yet -- a missing CLAUDE.md just means the
// projection will create one.
func readFileOrEmpty(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return string(data), nil
}

// atomicWriteFile writes content to path via a temp-file-plus-rename so
// a reader never observes a half-written CLAUDE.md. The temp file lives
// in the same directory as path so the rename is same-filesystem (and
// therefore atomic). The existing file's mode is preserved when present;
// a brand-new file gets 0644.
func atomicWriteFile(path, content string) error {
	mode := os.FileMode(0644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".aida-soul-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup; no-op once the rename below succeeds.
	defer os.Remove(tmpPath)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("setting mode on temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	return nil
}

// soulNote is the generated-content notice inside the managed block.
const soulNote = "<!-- generated from ~/.aida/soul.yaml - do not edit by hand -->"

// renderManagedSoulBlock wraps inner in the BEGIN/END markers plus the
// generated-content note, per the managed-block contract used by both
// this command and brain_capture.go's stripSoulBlock. The result has no
// trailing newline -- callers decide how it's stitched into a larger
// document.
func renderManagedSoulBlock(inner string) string {
	trimmedInner := strings.TrimRight(inner, "\n")
	return soulBlockBegin + "\n" + soulNote + "\n\n" + trimmedInner + "\n" + soulBlockEnd
}

// findMarkerLine scans s for a line whose content -- after stripping an
// optional trailing \r, so CRLF files match too -- exactly equals marker.
// It returns the byte offset where that line begins (lineStart) and the
// byte offset immediately after the marker text itself, before any \r or
// \n terminator (markerEnd). Slicing on these two offsets lets callers
// replace or preserve surrounding content without caring whether the
// file uses LF or CRLF line endings.
func findMarkerLine(s, marker string) (lineStart, markerEnd int, found bool) {
	pos := 0
	for {
		nl := strings.IndexByte(s[pos:], '\n')
		var line string
		if nl == -1 {
			line = s[pos:]
		} else {
			line = s[pos : pos+nl]
		}
		if strings.TrimSuffix(line, "\r") == marker {
			return pos, pos + len(marker), true
		}
		if nl == -1 {
			return 0, 0, false
		}
		pos += nl + 1
	}
}

// spliceManagedBlock splices the freshly-rendered managed block (BEGIN
// marker, generated-content note, inner, END marker) into existing:
//
//   - Both markers present, BEGIN before END: replace the BEGIN-through-END
//     span in place, leaving everything before and after untouched.
//   - Neither marker present: append the block to the end of existing,
//     separated from any prior content by exactly one blank line.
//   - Exactly one marker present, or END before BEGIN: the file has been
//     hand-mangled. Return existing unchanged with changed=false and an
//     error describing the problem -- never guess and risk corrupting the
//     user's CLAUDE.md.
//
// changed is true only when the returned string differs from existing,
// so callers can skip a write (and skip printing a "wrote it" message)
// when the projection is already up to date.
func spliceManagedBlock(existing, inner string) (result string, changed bool, err error) {
	block := renderManagedSoulBlock(inner)

	beginLineStart, _, hasBegin := findMarkerLine(existing, soulBlockBegin)
	endLineStart, endMarkerEnd, hasEnd := findMarkerLine(existing, soulBlockEnd)

	switch {
	case hasBegin && hasEnd:
		if beginLineStart > endLineStart {
			return existing, false, fmt.Errorf(
				"aida-soul markers in CLAUDE.md are malformed: END marker appears before BEGIN marker")
		}
		result = existing[:beginLineStart] + block + existing[endMarkerEnd:]
		return result, result != existing, nil

	case hasBegin && !hasEnd:
		return existing, false, fmt.Errorf(
			"aida-soul markers in CLAUDE.md are malformed: found a BEGIN marker with no matching END marker")

	case !hasBegin && hasEnd:
		return existing, false, fmt.Errorf(
			"aida-soul markers in CLAUDE.md are malformed: found an END marker with no matching BEGIN marker")

	default: // neither marker present -- append
		trimmed := strings.TrimRight(existing, "\r\n")
		if trimmed == "" {
			result = block + "\n"
		} else {
			result = trimmed + "\n\n" + block + "\n"
		}
		return result, result != existing, nil
	}
}

// soulDigest renders a compact, CLAUDE.md-friendly identity block from
// s: name, role, background, family, and people. Routing preferences
// (s.Preferences) and the voice/drafting block are deliberately left
// out -- those drive aida's own pipeline, not Claude Code's sense of
// who it's talking to. Returns "" when s is empty.
//
// Every line here uses a plain hyphen, never an em dash -- this text is
// spliced verbatim into the user's global CLAUDE.md.
func soulDigest(s *config.Soul) string {
	if s == nil || s.IsEmpty() {
		return ""
	}

	var sections []string

	var top strings.Builder
	if s.Name != "" {
		fmt.Fprintf(&top, "- Name: %s\n", s.Name)
	}
	if s.Role != "" {
		fmt.Fprintf(&top, "- Role: %s\n", s.Role)
	}
	if s.Context != "" {
		fmt.Fprintf(&top, "- Background: %s\n", strings.TrimSpace(s.Context))
	}
	if top.Len() > 0 {
		sections = append(sections, strings.TrimRight(top.String(), "\n"))
	}

	if fam := familyDigest(s.Family); fam != "" {
		sections = append(sections, "### Family\n\n"+strings.TrimRight(fam, "\n"))
	}
	if ppl := peopleDigest(s.People); ppl != "" {
		sections = append(sections, "### People\n\n"+strings.TrimRight(ppl, "\n"))
	}

	if len(sections) == 0 {
		return ""
	}
	digest := "## Who I am\n\n" + strings.Join(sections, "\n\n") + "\n"

	// soul.yaml is free-text the user authored for themselves and may
	// contain em dashes. This block gets spliced into CLAUDE.md as
	// generated output, so normalize every field in one pass rather than
	// relying on each call site to remember to do it.
	return plainHyphens(digest)
}

// plainHyphens replaces every em dash with a plain hyphen. Source
// text in soul.yaml consistently spaces em dashes ("word -- word"),
// so a direct swap reads cleanly without reflowing surrounding text.
func plainHyphens(s string) string {
	return strings.ReplaceAll(s, " - ", "-")
}

// familyDigest renders the family block as compact bullet lines, or ""
// if there's no family info. Mirrors FamilyInfo.forPrompt in
// internal/config/soul.go but with plain hyphens instead of em dashes.
func familyDigest(f *config.FamilyInfo) string {
	if f == nil {
		return ""
	}
	var lines []string
	add := func(p *config.Person, rel string) {
		if p == nil || p.Name == "" {
			return
		}
		line := "- " + p.Name
		var extras []string
		if rel != "" {
			extras = append(extras, rel)
		}
		if p.Nickname != "" {
			extras = append(extras, "aka "+p.Nickname)
		}
		if len(extras) > 0 {
			line += " (" + strings.Join(extras, "; ") + ")"
		}
		lines = append(lines, line)
	}
	for i := range f.Kids {
		add(&f.Kids[i], f.Kids[i].Relation)
	}
	add(f.Spouse, "spouse")
	add(f.ExWife, "ex-wife")
	for i := range f.Parents {
		rel := f.Parents[i].Relation
		if rel == "" {
			rel = "parent"
		}
		add(&f.Parents[i], rel)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// peopleDigest renders the people roster as compact bullet lines, or ""
// if there are none. Mirrors peopleForPrompt in internal/config/soul.go
// but with plain hyphens instead of em dashes.
func peopleDigest(people []config.Contact) string {
	if len(people) == 0 {
		return ""
	}
	var b strings.Builder
	for _, p := range people {
		if p.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s", p.Name)
		if len(p.Aka) > 0 {
			fmt.Fprintf(&b, " (aka %s)", strings.Join(p.Aka, ", "))
		}
		if p.Role != "" {
			fmt.Fprintf(&b, " - %s", p.Role)
		}
		b.WriteString("\n")
		if p.Note != "" {
			fmt.Fprintf(&b, "  %s\n", strings.TrimSpace(p.Note))
		}
	}
	return b.String()
}
