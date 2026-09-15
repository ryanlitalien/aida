package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
)

// newBrainGardenCmd builds the `aida brain garden` subcommand -
// a read-only audit pass that walks the library and brain looking
// for staleness signals: layer docs without matching source yaml,
// lessons referencing sources that no longer exist, etc.
//
// Inspired by the "doc-gardening agent" pattern from OpenAI's
// harness-engineering writeup. v1 reports only - auto-fix is a
// follow-up because some staleness is intentional (a layer doc
// kept around while the source is being rewritten) and should
// not be cleaned up without human approval.
//
// Output is grouped by category with one finding per line. Exit
// code is zero even when findings are present; this is an audit
// tool, not a CI gate.
func newBrainGardenCmd() *cobra.Command {
	var verbose bool
	var maxLayerLines int
	cmd := &cobra.Command{
		Use:   "garden",
		Short: "Audit the library and brain for staleness signals",
		Long: "Walks the active library and brain looking for orphaned layer\n" +
			"docs, lessons referencing dead sources, layer docs that have\n" +
			"grown past the soft cap, and other drift signals. Reports\n" +
			"findings - does not auto-fix.\n\n" +
			"Use --verbose to show the full per-finding context (lesson\n" +
			"ids, file paths). Use --max-layer-lines to tighten or loosen\n" +
			"the long-layer check.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBrainGarden(verbose, maxLayerLines)
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show full per-finding details")
	cmd.Flags().IntVar(&maxLayerLines, "max-layer-lines", defaultMaxLayerLines, "flag layer docs over this line count (0 = disable)")
	return cmd
}

// gardenFinding is one staleness signal found in the audit.
type gardenFinding struct {
	Category string // "orphaned-layer" | "dead-source-ref" | etc.
	Subject  string // affected file / lesson id / etc.
	Detail   string // human-readable explanation
}

func runBrainGarden(verbose bool, maxLayerLines int) error {
	// Load the active library so we know which sources currently
	// exist. Source name → presence map. Names are case-sensitive
	// to match the YAML keys exactly.
	reg, err := library.LoadRegistry(config.Dir())
	if err != nil {
		return fmt.Errorf("load library: %w", err)
	}
	sources, err := reg.LoadSources()
	if err != nil {
		return fmt.Errorf("load sources: %w", err)
	}
	knownSource := map[string]bool{}
	for name := range sources {
		knownSource[name] = true
	}

	allLessons, err := lessons.LoadAll()
	if err != nil {
		return fmt.Errorf("load lessons: %w", err)
	}

	// Read line counts for every layer file so the long-layer
	// check has the data it needs. Errors per-file are
	// non-fatal - a single unreadable layer must not abort the
	// audit. A nil entry signals "skip this file."
	linesByPath := map[string]int{}
	for _, layer := range reg.Layers {
		if layer == nil || layer.AbsFile == "" {
			continue
		}
		if n, err := countFileLines(layer.AbsFile); err == nil {
			linesByPath[layer.AbsFile] = n
		}
	}

	// I/O is done; analysis is a pure function over the loaded
	// data so it can be tested without touching disk.
	findings := auditGarden(knownSource, reg.Layers, allLessons, linesByPath, maxLayerLines)

	// ----- Render report --------------------------------------------
	// Group by category for skim-readability.
	byCategory := map[string][]gardenFinding{}
	for _, f := range findings {
		byCategory[f.Category] = append(byCategory[f.Category], f)
	}
	categories := []string{"orphaned-layer", "dead-source-ref", "long-layer"}

	totalFindings := len(findings)
	fmt.Printf("Doc-gardening audit: %d source(s) loaded, %d lesson(s) scanned\n",
		len(sources), len(allLessons))
	if totalFindings == 0 {
		fmt.Println("No staleness signals found. Brain looks clean.")
		return nil
	}
	fmt.Printf("\n%d finding(s) total\n", totalFindings)

	for _, cat := range categories {
		fs := byCategory[cat]
		if len(fs) == 0 {
			continue
		}
		fmt.Printf("\n%s (%d):\n", cat, len(fs))
		const previewLimit = 5
		for i, f := range fs {
			if !verbose && i >= previewLimit {
				fmt.Printf("  … %d more (use --verbose to see all)\n", len(fs)-previewLimit)
				break
			}
			subj := f.Subject
			if subj == "" {
				subj = "(no subject)"
			}
			fmt.Printf("  - %s\n", f.Detail)
			if verbose {
				fmt.Printf("    subject: %s\n", subj)
			}
		}
	}
	return nil
}

// countFileLines reads a file and returns its line count. A
// trailing newline at end-of-file does NOT contribute an extra
// line - matches the wc -l convention for non-newline-terminated
// files but is more forgiving for the markdown docs we audit.
// Errors propagate; callers decide whether to skip silently.
func countFileLines(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, nil
	}
	count := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		count++
	}
	return count, nil
}

// fallbackPath returns "no path recorded" when path is empty so
// rendered findings are never confusingly truncated.
func fallbackPath(path string) string {
	if path == "" {
		return "(no path recorded)"
	}
	return path
}

// defaultMaxLayerLines is the soft cap for a layer doc's line
// count before garden flags it as "too long". Per OpenAI's
// harness writeup, a tight, scannable AGENTS.md / layer doc
// outperforms a sprawling one - context is a scarce resource
// and "too much guidance becomes non-guidance." 200 is generous;
// the OpenAI team uses ~100 for AGENTS.md. Surfaced as a flag
// so users can tighten or loosen per project.
const defaultMaxLayerLines = 200

// auditGarden is the pure-data half of runBrainGarden - given the
// loaded library + lessons + per-file line counts, return the
// staleness findings. Split out so unit tests can exercise the
// analysis without touching disk or needing a real ~/.aida.
//
// linesByPath maps a layer's AbsFile to its line count. Callers
// that don't want the long-layer check pass nil and the layer
// length analysis is skipped.
func auditGarden(
	knownSource map[string]bool,
	layers map[string]*library.ResolvedLayer,
	allLessons []lessons.Lesson,
	linesByPath map[string]int,
	maxLayerLines int,
) []gardenFinding {
	var findings []gardenFinding

	// Category 1: orphaned layer docs.
	const layerPrefix = "sources/"
	for layerName, layer := range layers {
		if !strings.HasPrefix(layerName, layerPrefix) {
			continue
		}
		srcName := strings.TrimPrefix(layerName, layerPrefix)
		if knownSource[srcName] {
			continue
		}
		path := ""
		if layer != nil {
			path = layer.AbsFile
		}
		findings = append(findings, gardenFinding{
			Category: "orphaned-layer",
			Subject:  layerName,
			Detail: fmt.Sprintf("layer %s has no matching library source - %s",
				layerName, fallbackPath(path)),
		})
	}

	// Category 2: lessons referencing dead sources.
	for _, l := range allLessons {
		dead := map[string]bool{}
		for _, n := range l.FeedbackIntendedSources {
			if n != "" && !knownSource[n] {
				dead[n] = true
			}
		}
		if l.FeedbackIntendedSource != "" && !knownSource[l.FeedbackIntendedSource] {
			dead[l.FeedbackIntendedSource] = true
		}
		for _, n := range l.FeedbackExcludedSources {
			if n != "" && !knownSource[n] {
				dead[n] = true
			}
		}
		if len(dead) == 0 {
			continue
		}
		var names []string
		for n := range dead {
			names = append(names, n)
		}
		sort.Strings(names)
		findings = append(findings, gardenFinding{
			Category: "dead-source-ref",
			Subject:  l.RunID,
			Detail: fmt.Sprintf("lesson references source(s) not in current library: %s",
				strings.Join(names, ", ")),
		})
	}

	// Category 3: layer docs that have grown past the soft cap.
	// Long layer docs crowd context and pattern-match locally
	// rather than steering the agent intentionally - exactly the
	// failure mode OpenAI calls out in their harness writeup.
	if linesByPath != nil && maxLayerLines > 0 {
		for layerName, layer := range layers {
			if layer == nil || layer.AbsFile == "" {
				continue
			}
			n, ok := linesByPath[layer.AbsFile]
			if !ok {
				continue
			}
			if n <= maxLayerLines {
				continue
			}
			findings = append(findings, gardenFinding{
				Category: "long-layer",
				Subject:  layerName,
				Detail: fmt.Sprintf("layer %s is %d lines (cap %d) - split into smaller pieces or move detail into a referenced doc",
					layerName, n, maxLayerLines),
			})
		}
	}

	return findings
}
