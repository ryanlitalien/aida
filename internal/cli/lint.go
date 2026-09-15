package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/ui"
)

// newLintCmd builds the `aida lint` subcommand.
//
// Phase 6.1: walks every library source and flags common misconfigurations.
// Two severity levels:
//
//   - error: the source would crash or shell-inject at runtime (raw-{query}
//     exec, tool with missing binary, path pointing at a deleted folder).
//     Exit code non-zero so CI or pre-commit hooks can gate on errors.
//   - warn: the source is structurally valid but hurts routing quality
//     (empty capabilities, docs type with path but no search.mode, tool
//     with no context doc). No non-zero exit.
//
// Output is a flat list of findings grouped by source, with a per-category
// summary at the bottom.
func newLintCmd() *cobra.Command {
	var warnAsError bool
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Validate library source configs and flag common misconfigurations",
		Long: "Walks every source registered in the library and reports raw-exec\n" +
			"passthroughs, missing capabilities/entities, docs sources without a\n" +
			"search mode, and tools whose command binary isn't on PATH. Exits\n" +
			"non-zero when any error-level finding is present so CI can gate on\n" +
			"it; warn-only findings do not change the exit code unless\n" +
			"--strict is passed.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runLint(warnAsError)
		},
	}
	cmd.Flags().BoolVar(&warnAsError, "strict", false, "treat warnings as errors for exit code")
	return cmd
}

type lintFinding struct {
	source   string
	severity string // "error" | "warn"
	message  string
}

func runLint(strict bool) error {
	reg, err := library.LoadRegistry(config.Dir())
	if err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	sources, err := reg.LoadSources()
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	// Honor the active profile so lint only surfaces findings relevant to
	// the current environment. Otherwise a work-profile run reports 35+
	// missing-path errors for every home-profile source, which is noise.
	if cfg, err := config.LoadConfig(); err == nil && cfg != nil {
		_, profileName := cfg.ActiveProfileConfig()
		if profileName != "" {
			sources = config.FilterByProfile(sources, profileName)
		}
	}

	var findings []lintFinding

	// Library load-time rejections (raw-{query} passthroughs etc.)
	// run inside LoadSources and are remediation-shaped. Surface them
	// here so `aida lint` reports the same issues that already silently
	// dropped sources from the registry - otherwise users have to
	// hunt through verbose logs to find why a source is missing.
	for _, iss := range reg.Issues() {
		findings = append(findings, lintFinding{
			source:   iss.SourceName,
			severity: string(iss.Severity),
			message:  fmt.Sprintf("%s: %s\n      %s", iss.Type, iss.Reason, iss.FormatFix()),
		})
	}

	// soul.yaml: every LoadSoul consumer (engine parser/router/synthesis,
	// jarvis) deliberately degrades to "no user context" on failure, so a
	// syntax error strips personal context from every prompt with zero
	// log lines - lint is the one place that reports it loudly. (Found
	// the hard way: two quoting landmines kept the soul unloaded for
	// weeks and nobody noticed.)
	soulFindings := lintSoul()
	findings = append(findings, soulFindings...)

	names := make([]string, 0, len(sources))
	for n := range sources {
		names = append(names, n)
	}
	// Include rejected-source names so the per-source rendering loop
	// below shows them in their own section, not lumped together.
	rejectedSeen := map[string]bool{}
	for _, iss := range reg.Issues() {
		if !rejectedSeen[iss.SourceName] {
			names = append(names, iss.SourceName)
			rejectedSeen[iss.SourceName] = true
		}
	}
	if len(soulFindings) > 0 {
		names = append(names, "soul.yaml")
	}
	sort.Strings(names)

	for _, name := range names {
		src := sources[name]
		if src == nil {
			// Rejected sources don't appear in the loaded registry -
			// their findings come from reg.Issues() above and don't
			// need lintSource() to add more.
			continue
		}
		findings = append(findings, lintSource(name, src, reg)...)
	}

	errors, warns := 0, 0
	for _, f := range findings {
		switch f.severity {
		case "error":
			errors++
		case "warn":
			warns++
		}
	}

	if len(findings) == 0 {
		fmt.Printf("%s All %d sources clean.\n", ui.SuccessIcon, len(sources))
		return nil
	}

	// Group findings by source for readability.
	bySource := map[string][]lintFinding{}
	for _, f := range findings {
		bySource[f.source] = append(bySource[f.source], f)
	}
	for _, name := range names {
		fs := bySource[name]
		if len(fs) == 0 {
			continue
		}
		fmt.Printf("\n%s:\n", name)
		for _, f := range fs {
			icon := "!"
			if f.severity == "error" {
				icon = "✗"
			}
			fmt.Printf("  %s %s: %s\n", icon, f.severity, f.message)
		}
	}

	fmt.Printf("\n%d source(s), %d error(s), %d warning(s).\n", len(sources), errors, warns)

	if errors > 0 || (strict && warns > 0) {
		return fmt.Errorf("lint found %d error(s), %d warning(s)", errors, warns)
	}
	return nil
}

// lintSoul validates ~/.aida/soul.yaml. A parse failure is an ERROR:
// the file exists but no prompt is receiving it, which is silent quality
// loss everywhere. A missing/empty soul is a WARN - legitimate on a
// fresh install, but worth surfacing since routing answers "who is
// asking" questions blind without it.
func lintSoul() []lintFinding {
	s, err := config.LoadSoul()
	if err != nil {
		return []lintFinding{{
			source:   "soul.yaml",
			severity: "error",
			message: fmt.Sprintf("unparseable - every prompt (engine parser/router/synthesis + jarvis) is silently running WITHOUT user context: %v\n"+
				"      fix: single-quote list items that start with a quote or contain \": \"", err),
		}}
	}
	if s.IsEmpty() {
		return []lintFinding{{
			source:   "soul.yaml",
			severity: "warn",
			message:  "missing or empty - prompts run without user context (who you are, your vocabulary, routing preferences); create ~/.aida/soul.yaml",
		}}
	}
	return nil
}

// lintSource inspects one source and returns zero or more findings.
func lintSource(name string, src *config.Source, reg *library.Registry) []lintFinding {
	var out []lintFinding
	add := func(sev, msg string) {
		out = append(out, lintFinding{source: name, severity: sev, message: msg})
	}

	// Path existence (only when the source declares one). A shipped
	// example source (examples/sources/*-example.yaml, or a source
	// explicitly marked `example: true`) points at a placeholder path by
	// design - a fresh `aida init` + `aida lint` shouldn't report errors
	// out of the box just because nobody has edited the two examples yet,
	// so this is a warning there instead of an error. --strict still
	// turns it into a failure like any other warning.
	if src.Path != "" {
		expanded := config.ExpandPath(src.Path)
		if _, err := os.Stat(expanded); err != nil {
			if isExampleSource(name, src, reg) {
				add("warn", fmt.Sprintf("path does not exist: %s (this is an example source; edit examples/sources/%s to point at a real path, or delete it)", src.Path, exampleSourceFileName(name, reg)))
			} else {
				add("error", fmt.Sprintf("path %q does not exist on this machine", src.Path))
			}
		}
	}

	// Exec validation: the library load-time validator already drops
	// sources whose exec.query is raw {query}, so we should never see
	// one here - but if we do, something changed and it's worth
	// re-flagging.
	for key, tmpl := range src.Exec {
		trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(tmpl), "'\""))
		if trimmed == "{query}" || trimmed == "{search}" || trimmed == "{command}" {
			add("error", fmt.Sprintf("exec.%s is a raw {query} passthrough - would shell-inject LLM output", key))
			continue
		}
		if binName := firstBinary(tmpl); binName != "" {
			if _, err := exec.LookPath(binName); err != nil {
				add("warn", fmt.Sprintf("exec.%s references %q which is not on PATH", key, binName))
			}
		}
	}

	// Type-specific checks.
	switch src.Type {
	case "docs":
		if src.Path != "" && (src.Search == nil || src.Search.Mode == "") {
			// docs + path without search.mode still routes to DocsAdapter
			// (read context file only). Fine for narrative-knowledge
			// sources, but suspect for partner repos.
			if src.Context != "" {
				add("warn", "type: docs with path but no search.mode - will only read the context file; flip to search.mode: grep if the directory has partner docs/investigations to search")
			}
		}
	case "tool":
		if len(src.Exec) == 0 {
			add("error", "type: tool has no exec template")
		}
	case "codebase":
		if src.Path == "" {
			add("error", "type: codebase has no path")
		}
	}

	// Routing signal health.
	if len(src.Capabilities) == 0 && len(src.Entities) == 0 {
		add("warn", "no capabilities or entities declared - source can only match by name or description text")
	}

	// Context-doc coverage: tool / sqlite / git / claude-project sources
	// rely on a context doc to teach the executor LLM how to phrase
	// queries (which subcommand, which fields, which tables). Without one,
	// the LLM falls back to the generic system-prompt rule "trust the
	// context doc" with nothing to trust - the original NYT bug. Warn when
	// neither (a) the per-source `context:` field resolves to an existing
	// file, NOR (b) a library layer at `sources/<name>` is registered.
	switch src.Type {
	case "tool", "sqlite", "git", "claude-project":
		hasInline := false
		if src.Context != "" && src.Path != "" {
			full := filepath.Join(config.ExpandPath(src.Path), src.Context)
			if _, err := os.Stat(full); err == nil {
				hasInline = true
			}
		}
		hasLayer := false
		if reg != nil {
			if layer, ok := reg.Layers["sources/"+name]; ok && layer != nil && layer.AbsFile != "" {
				if _, err := os.Stat(layer.AbsFile); err == nil {
					hasLayer = true
				}
			}
		}
		if !hasInline && !hasLayer {
			add("warn", fmt.Sprintf("type: %s has no context doc - add either a `context:` pointing to an existing file under `path:`, OR a library layer at layers/sources/%s.md so the executor knows how to phrase queries (subcommands, table names, fields)", src.Type, name))
		}
	}

	return out
}

// isExampleSource reports whether src is a shipped example (never a real
// user source): either explicitly marked `example: true`, or its source
// YAML file name ends in "-example.yaml" (the examples/sources/ naming
// convention). The filename suffix is a fallback for sources that predate
// the `example:` field or were copied without it.
func isExampleSource(name string, src *config.Source, reg *library.Registry) bool {
	if src.Example {
		return true
	}
	return strings.HasSuffix(exampleSourceFileName(name, reg), "-example.yaml")
}

// exampleSourceFileName returns the base file name (e.g.
// "codebase-example.yaml") of the source's YAML file, or name+".yaml" if
// the registry has no resolved entry for it.
func exampleSourceFileName(name string, reg *library.Registry) string {
	if reg != nil {
		if resolved, ok := reg.Sources[name]; ok && resolved != nil && resolved.AbsFile != "" {
			return filepath.Base(resolved.AbsFile)
		}
	}
	return name + ".yaml"
}

// firstBinary returns the first shell token of a command template (before
// any flag or {query} placeholder), so we can LookPath it. Returns "" for
// exec templates that are shell pipelines or sh -c invocations; those are
// too freeform to validate here.
func firstBinary(tmpl string) string {
	t := strings.TrimSpace(tmpl)
	if t == "" {
		return ""
	}
	// Bail if the template starts with a shell construct we can't parse.
	if strings.HasPrefix(t, "sh ") || strings.HasPrefix(t, "bash ") ||
		strings.HasPrefix(t, "/bin/") {
		return ""
	}
	// Extract the first whitespace-delimited token, stripping any trailing
	// backtick or quote.
	if idx := strings.IndexAny(t, " \t"); idx >= 0 {
		t = t[:idx]
	}
	t = strings.Trim(t, "'\"`")
	// Ignore things that look like placeholders.
	if strings.Contains(t, "{") || strings.ContainsAny(t, "|><") {
		return ""
	}
	// Only return bare command names - if the caller uses absolute paths
	// LookPath can't help; Stat would, but that's a different check.
	if filepath.IsAbs(t) {
		return ""
	}
	return t
}
