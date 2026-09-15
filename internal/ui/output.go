package ui

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/muesli/termenv"

	"github.com/ryanlitalien/aida/internal/sources"
)

// ---------------------------------------------------------------------------
// Display types used by the output helpers.
// ---------------------------------------------------------------------------

// ScoredSourceDisplay represents a source with its relevance score for
// routing display.
type ScoredSourceDisplay struct {
	Name  string
	Score int
}

// SourceDisplay describes a data source that contributed to an answer.
type SourceDisplay struct {
	Name   string
	Type   string // "log", "row", "file", etc.
	Detail string
}

// PlanDisplay holds a dry-run execution plan for display.
type PlanDisplay struct {
	Phases []PhaseDisplay
}

// PhaseDisplay describes one phase of a plan (may execute in parallel).
type PhaseDisplay struct {
	Name     string
	Sources  []string
	Parallel bool
}

// Verbose controls whether PrintVerbose produces output. Set this from
// the --verbose / -v CLI flag.
var Verbose bool

// Explain controls whether the planner emits a per-source scoring trace.
// Set this from the --explain CLI flag.
var Explain bool

// ---------------------------------------------------------------------------
// Output helpers
// ---------------------------------------------------------------------------

// PrintParsed prints the parsed intent line.
//
//	"magnifying glass Parsed: action entities (timeframe)"
//
// Suppressed when ui.Quiet is set (--run-dir mode); the events sink
// captures any equivalent signal.
func PrintParsed(action, entities, timeframe string) {
	if Quiet {
		return
	}
	parts := []string{action}
	if entities != "" {
		parts = append(parts, entities)
	}
	detail := strings.Join(parts, " ")
	if timeframe != "" {
		detail += " (" + timeframe + ")"
	}

	fmt.Fprintf(os.Stderr, "%s %s %s\n",
		ParseIcon,
		LabelStyle.Render("Parsed:"),
		ValueStyle.Render(detail),
	)
}

// PrintProfile prints the active profile info line.
//
//	"folder Profile: name (N sources)"
func PrintProfile(name string, sourceCount int) {
	if Quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s %s %s\n",
		ProfileIcon,
		LabelStyle.Render("Profile:"),
		ValueStyle.Render(name),
		DimStyle.Render(fmt.Sprintf("(%d sources)", sourceCount)),
	)
}

// PrintRouting prints which sources were selected and their scores.
//
//	"shuffle Routing to: [src1] (score) [src2] (score)"
func PrintRouting(sources []ScoredSourceDisplay) {
	if Quiet {
		return
	}
	var parts []string
	for _, src := range sources {
		rendered := SourceStyle.Render("["+src.Name+"]") + " " +
			ScoreStyle.Render(fmt.Sprintf("(%d)", src.Score/10))
		parts = append(parts, rendered)
	}

	fmt.Fprintf(os.Stderr, "%s %s %s\n",
		RouteIcon,
		LabelStyle.Render("Routing to:"),
		strings.Join(parts, "  "),
	)
}

// PrintResult prints the final synthesized answer to stdout so it can
// be captured or piped. Markdown `[text](url)` links are converted to
// OSC-8 hyperlinks when stdout is a tty so the text becomes clickable
// in iTerm2/Terminal.app/kitty/alacritty/vscode without showing the
// raw URL - most modern terminals support OSC-8 and the few that don't
// just render the visible text. When stdout is piped or redirected,
// links are left as raw Markdown so logs/grep stay readable.
func PrintResult(answer string) {
	fmt.Println()
	fmt.Println(HeaderStyle.Render("Answer"))
	fmt.Println(strings.Repeat("─", 60))
	fmt.Println(renderMarkdownLinks(answer))
	fmt.Println()
}

// markdownLinkPattern matches `[text](url)`. Greedy on `text` only up to
// the first `]`, lazy on `url` up to the first `)` - covers the
// well-formed links the synthesizer emits without trying to be a full
// CommonMark parser.
var markdownLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

func renderMarkdownLinks(s string) string {
	if !stdoutIsTTY() {
		return s
	}
	return markdownLinkPattern.ReplaceAllStringFunc(s, func(match string) string {
		m := markdownLinkPattern.FindStringSubmatch(match)
		if len(m) != 3 {
			return match
		}
		return termenv.Hyperlink(m[2], m[1])
	})
}

// stdoutIsTTY reports whether os.Stdout is attached to a terminal. Used
// to decide whether to emit escape sequences (OSC-8 hyperlinks) - when
// piped to a file or another process, plain Markdown is preserved.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// PrintSources prints a "Sources:" section listing each contributing
// source with its type and detail.
func PrintSources(sources []SourceDisplay) {
	if len(sources) == 0 {
		return
	}
	fmt.Println(LabelStyle.Render("Sources:"))
	for _, src := range sources {
		tag := DimStyle.Render("[" + src.Type + "]")
		name := SourceStyle.Render(src.Name)
		line := fmt.Sprintf("  %s %s", tag, name)
		if src.Detail != "" {
			line += " " + DimStyle.Render(src.Detail)
		}
		fmt.Println(line)
	}
}

// PrintDryRun prints an execution plan without actually running it.
func PrintDryRun(plan PlanDisplay) {
	fmt.Println(HeaderStyle.Render("Execution Plan (dry run)"))
	fmt.Println(strings.Repeat("─", 60))
	for i, phase := range plan.Phases {
		mode := "sequential"
		if phase.Parallel {
			mode = "parallel"
		}
		fmt.Printf("  Phase %d: %s [%s]\n",
			i+1,
			ValueStyle.Render(phase.Name),
			DimStyle.Render(mode),
		)
		for _, src := range phase.Sources {
			fmt.Printf("    - %s\n", SourceStyle.Render(src))
		}
	}
	fmt.Println()
}

// PrintError prints a formatted error message to stderr.
func PrintError(err error) {
	fmt.Fprintf(os.Stderr, "%s %s\n",
		ErrorIcon,
		ErrorStyle.Render(err.Error()),
	)
}

// PrintVerbose prints debug-level info only when the Verbose flag is set.
func PrintVerbose(label, detail string) {
	if !Verbose {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s\n",
		LabelStyle.Render(label+":"),
		DimStyle.Render(detail),
	)
}

// PrintTiming prints query duration breakdown.
func PrintTiming(parse, exec, synth, total time.Duration, results []sources.SourceResult) {
	fmt.Println(DimStyle.Render(strings.Repeat("─", 60)))
	fmt.Printf("%s %s\n", LabelStyle.Render("Timing:"), DimStyle.Render(fmt.Sprintf("total %s", total.Round(time.Millisecond))))
	fmt.Printf("  parse: %s  execute: %s  synthesize: %s\n",
		DimStyle.Render(parse.Round(time.Millisecond).String()),
		DimStyle.Render(exec.Round(time.Millisecond).String()),
		DimStyle.Render(synth.Round(time.Millisecond).String()),
	)

	// Per-source timing. Entries with zero duration are side-channel
	// injections (e.g. brain feedback surfaced as synthetic source
	// results for citation), not real executed sources - render them
	// with an "injected" label so they aren't confused with a 0ms query.
	for _, r := range results {
		status := SuccessStyle.Render(r.Status)
		if r.Status == "error" || r.Status == "timeout" {
			status = ErrorStyle.Render(r.Status)
		} else if r.Status == "empty" {
			status = WarnStyle.Render(r.Status)
		}
		detail := r.Duration.Round(time.Millisecond).String()
		if r.Duration == 0 {
			detail = "injected"
		}
		fmt.Printf("  %s %s %s\n",
			SourceStyle.Render("["+r.Source+"]"),
			status,
			DimStyle.Render(detail),
		)
	}
	fmt.Println()
}

// PrintShareable prints a clean, plain-text summary for sharing via Slack/email.
func PrintShareable(question, answer string) {
	summary := extractShareableSummary(answer)
	fmt.Println(DimStyle.Render("📋 Shareable"))
	fmt.Println(DimStyle.Render(strings.Repeat("─", 60)))
	fmt.Println(summary)
	fmt.Println()
}

// extractShareableSummary pulls the core answer, strips markdown and citations,
// drops boilerplate sections, and returns clean plain text.
func extractShareableSummary(answer string) string {
	lines := strings.Split(strings.TrimSpace(answer), "\n")
	var summary []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)

		// Stop at boilerplate sections
		if strings.HasPrefix(lower, "**source") ||
			strings.HasPrefix(lower, "**recommended") ||
			strings.HasPrefix(lower, "**next step") ||
			strings.HasPrefix(lower, "**note:") ||
			strings.HasPrefix(lower, "sources:") ||
			strings.HasPrefix(lower, "---") ||
			strings.HasPrefix(lower, "if you need") ||
			strings.HasPrefix(lower, "to get this") ||
			strings.HasPrefix(lower, "to proceed") ||
			strings.HasPrefix(lower, "would you like") ||
			strings.HasPrefix(lower, "recommendation") {
			break
		}

		// Skip empty lines
		if trimmed == "" {
			if len(summary) > 0 {
				summary = append(summary, "")
			}
			continue
		}

		// Strip markdown formatting
		cleaned := stripMarkdown(trimmed)

		// Strip inline source citations like (source: snowflake)
		for strings.Contains(cleaned, "(source:") {
			start := strings.Index(cleaned, "(source:")
			end := strings.Index(cleaned[start:], ")")
			if end >= 0 {
				cleaned = strings.TrimSpace(cleaned[:start] + cleaned[start+end+1:])
			} else {
				break
			}
		}

		if cleaned != "" {
			summary = append(summary, cleaned)
		}
	}

	// Trim trailing blank lines
	for len(summary) > 0 && summary[len(summary)-1] == "" {
		summary = summary[:len(summary)-1]
	}

	if len(summary) == 0 {
		return stripMarkdown(strings.Split(strings.TrimSpace(answer), "\n")[0])
	}

	return strings.Join(summary, "\n")
}

// stripMarkdown removes common markdown formatting from a string.
func stripMarkdown(s string) string {
	// Remove bold
	s = strings.ReplaceAll(s, "**", "")
	// Remove italic underscores (but not mid-word)
	if strings.HasPrefix(s, "_") && strings.HasSuffix(s, "_") {
		s = s[1 : len(s)-1]
	}
	// Remove backtick code spans
	s = strings.ReplaceAll(s, "`", "")
	// Remove heading markers
	for strings.HasPrefix(s, "# ") {
		s = s[2:]
	}
	// Clean up bullet markers to plain dash
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") {
		s = "- " + s[2:]
	}
	return s
}
