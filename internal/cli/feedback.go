package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/lessons"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newThumbsUpCmd() *cobra.Command {
	var because string
	var more bool
	cmd := &cobra.Command{
		Use:     "thumbs-up [run-id]",
		Aliases: []string{"good", "+1"},
		Short:   "Mark a run as a positive example for future routing (aliases: good, +1)",
		Long: "Records explicit positive feedback for a recorded run. The lesson\n" +
			"file gets a new entry with feedback=thumbs-up that the LLM router\n" +
			"and the planner will weigh strongly when scoring similar future\n" +
			"questions. With no run-id, marks the most recent run.\n\n" +
			"Use --more to open your editor and paste detailed context.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			run, err := loadRunOrLatest(args)
			if err != nil {
				return err
			}
			if more {
				because, err = editFeedbackContent()
				if err != nil {
					return err
				}
			}
			return appendFeedback(run, lessons.FeedbackThumbsUp, because)
		},
	}
	cmd.Flags().StringVar(&because, "because", "", "optional reason for the positive feedback")
	cmd.Flags().BoolVar(&more, "more", false, "open editor to paste detailed feedback")
	return cmd
}

func newThumbsDownCmd() *cobra.Command {
	var because string
	var more bool
	cmd := &cobra.Command{
		Use:     "thumbs-down [run-id]",
		Aliases: []string{"bad", "-1"},
		Short:   "Mark a run as a negative example for future routing (aliases: bad, -1)",
		Long: "Records explicit negative feedback for a recorded run. Future\n" +
			"queries that look similar will AVOID picking the same source(s).\n" +
			"Add --because to give the reason -- the LLM router sees it.\n\n" +
			"Use --more to open your editor and paste detailed context.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			run, err := loadRunOrLatest(args)
			if err != nil {
				return err
			}
			if more {
				var err error
				because, err = editFeedbackContent()
				if err != nil {
					return err
				}
			}
			return appendFeedback(run, lessons.FeedbackThumbsDown, because)
		},
	}
	cmd.Flags().StringVar(&because, "because", "", "reason for the negative feedback (recommended)")
	cmd.Flags().BoolVar(&more, "more", false, "open editor to paste detailed feedback")
	return cmd
}

func newNoteCmd() *cobra.Command {
	var because string
	var more bool
	cmd := &cobra.Command{
		Use:     "note [run-id]",
		Aliases: []string{"ok", "feedback"},
		Short:   "Attach a neutral note to a run without rating it (aliases: ok, feedback)",
		Long: "Records general feedback on a run without marking it positive or\n" +
			"negative. Use this for observations like \"too slow\", \"answer was\n" +
			"verbose\", or \"routing was right but data was stale\". The note is\n" +
			"stored in the lesson file and visible to the LLM router on similar\n" +
			"future questions, but does not trigger PREFER or AVOID behavior.\n\n" +
			"Use --more to open your editor and paste detailed context.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			run, err := loadRunOrLatest(args)
			if err != nil {
				return err
			}
			if more {
				var err error
				because, err = editFeedbackContent()
				if err != nil {
					return err
				}
			}
			if because == "" {
				return fmt.Errorf("--because or --more is required for notes (what do you want to say?)")
			}
			return appendFeedback(run, lessons.FeedbackNote, because)
		},
	}
	cmd.Flags().StringVar(&because, "because", "", "your feedback note (required unless --more)")
	cmd.Flags().BoolVar(&more, "more", false, "open editor to paste detailed feedback")
	return cmd
}

func newLessonsCmd() *cobra.Command {
	var forQuestion string
	var limit int
	cmd := &cobra.Command{
		Use:   "lessons",
		Short: "List recent learned lessons or preview matches for a hypothetical question",
		Long: "Without --for, lists the most recent lessons in ~/.aida/lessons.jsonl.\n" +
			"With --for \"<question>\", shows which past lessons would be matched as\n" +
			"similar -- the same data the LLM router sees when picking sources.",
		RunE: func(_ *cobra.Command, _ []string) error {
			if limit == 0 {
				limit = 20
			}

			cfg, _ := config.LoadConfig()
			_, profileName := cfg.ActiveProfileConfig()
			brn, brainErr := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if brainErr == nil {
				defer brn.Close()
			}

			if forQuestion != "" {
				// Use brain semantic search if available, else Jaccard
				if brn != nil {
					sc, err := brn.Search(context.Background(), forQuestion, nil, limit)
					if err != nil {
						return err
					}
					if len(sc.SimilarLessons) == 0 {
						fmt.Printf("%s No similar past lessons found for %q\n", ui.WarnIcon, forQuestion)
						return nil
					}
					fmt.Printf("Top %d lessons matching %q:\n\n", len(sc.SimilarLessons), forQuestion)
					for _, s := range sc.SimilarLessons {
						l := s.Lesson
						srcs := strings.Join(l.Sources, ",")
						feedback := ""
						if l.Feedback != "" {
							feedback = fmt.Sprintf(" (%s)", l.Feedback)
						}
						fmt.Printf("  [sim=%.2f] %s [%s → q%d]%s  %q\n",
							s.Similarity, l.Timestamp, srcs, l.Quality, feedback, l.Question)
					}
					return nil
				}
				// Fallback to Jaccard if brain unavailable
				similar, err := lessons.FindSimilar(forQuestion, limit)
				if err != nil {
					return err
				}
				if len(similar) == 0 {
					fmt.Printf("%s No similar past lessons found for %q\n", ui.WarnIcon, forQuestion)
					return nil
				}
				fmt.Printf("Top %d lessons matching %q:\n\n", len(similar), forQuestion)
				for _, s := range similar {
					fmt.Printf("  [score=%.2f] %s\n", s.Score, s.Lesson.Summary())
				}
				return nil
			}

			// List recent lessons from brain.db if available
			if brn != nil {
				stats := brn.GetStats()
				if stats.LessonCount == 0 {
					fmt.Printf("%s No lessons recorded yet. Run a query first.\n", ui.WarnIcon)
					return nil
				}
				fmt.Printf("Brain: %d lessons, %d entities\n", stats.LessonCount, stats.EntityCount)
				fmt.Printf("Path:  %s\n\n", stats.BrainPath)

				// Use brain search with empty query to list recent
				sc, _ := brn.Search(context.Background(), "", nil, 0)
				_ = sc // brain search requires a query; just show stats
				fmt.Println("Use 'aida brain search \"query\"' to find specific lessons.")
				fmt.Println("Use 'aida lessons --for \"query\"' to preview matches for a question.")
				return nil
			}

			// Fallback to legacy file
			all, err := lessons.LoadAll()
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Printf("%s No lessons recorded yet. Run a query first.\n", ui.WarnIcon)
				return nil
			}
			start := 0
			if len(all) > limit {
				start = len(all) - limit
			}
			fmt.Printf("Showing %d of %d total lessons (newest last):\n\n", len(all)-start, len(all))
			for _, l := range all[start:] {
				fmt.Printf("  %s\n", l.Summary())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&forQuestion, "for", "", "show lessons that would match this hypothetical question")
	cmd.Flags().IntVar(&limit, "limit", 20, "max number of lessons to show")
	return cmd
}

// appendFeedback writes a NEW lesson record carrying the explicit user
// verdict. We append rather than mutate so the lessons.jsonl file stays
// strictly append-only and a feedback record always points back to its
// original run via RunID. The router and planner read the latest lesson
// per RunID, so a thumbs-down lesson written later overrides the silent
// auto-recorded one.
func appendFeedback(run *runs.Run, verdict lessons.Feedback, reason string) error {
	if dryRunGuard("record feedback", string(verdict)+" on run "+run.ID) {
		return nil
	}
	// Try to find the auto-recorded lesson for this run so the new
	// feedback record carries forward the same source list and outcome.
	prior, _ := lessons.FindByRunID(run.ID)
	feedback := &lessons.Lesson{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		RunID:          run.ID,
		Question:       strings.ToLower(strings.TrimSpace(run.Question)),
		Cwd:            run.Cwd,
		Action:         run.Action,
		Strategy:       run.Strategy,
		Feedback:       verdict,
		FeedbackReason: reason,
	}
	if prior != nil {
		feedback.Sources = prior.Sources
		feedback.PerSourceStatus = prior.PerSourceStatus
		feedback.ArtifactCount = prior.ArtifactCount
		feedback.AnswerSnippet = prior.AnswerSnippet
	} else {
		// No prior auto-lesson; reconstruct what we can from the run log.
		var picked []string
		statuses := map[string]lessons.Status{}
		artifacts := 0
		for _, p := range run.Phases {
			for _, s := range p.Sources {
				picked = append(picked, s.Name)
				statuses[s.Name] = lessons.Status(s.Status)
				artifacts += s.ArtifactCount
			}
		}
		feedback.Sources = picked
		feedback.PerSourceStatus = statuses
		feedback.ArtifactCount = artifacts
		feedback.AnswerSnippet = truncateRunSummary(run.Answer, 500)
	}
	// Directive extraction: parse the --because text to find which source
	// the user intended and what type of failure occurred. This gives
	// the router a much stronger signal than raw text alone.
	if reason != "" {
		// Output directives (sentence count, "include the URL", etc.) are
		// extracted for ALL verdicts - note, thumbs-up, thumbs-down - because
		// they describe forward-looking style preferences regardless of the
		// verdict, and the synthesizer needs to apply them on future similar
		// questions. (Without this, "aida ok --because '3 sentences'" stored
		// only as soft FeedbackReason gets ignored on re-runs - the original
		// bug that prompted Part D.)
		feedback.FeedbackOutputDirectives = extractOutputDirectives(reason)

		// Routing directives (intended/excluded sources, failure type) are
		// only meaningful for thumbs-down - those are the corrections.
		if verdict == lessons.FeedbackThumbsDown {
			// Load sources from the library registry (where the 40 real
			// sources live), not the legacy config.LoadSources() which is
			// typically empty. This was the bug: "website" in the user's
			// feedback was never matched because config had 0 sources.
			var allSources config.Sources
			if reg, err := library.LoadRegistry(config.Dir()); err == nil {
				allSources, _ = reg.LoadSources()
			}
			if len(allSources) == 0 {
				allSources, _ = config.LoadSources() // legacy fallback
			}
			intendedSources, excludedSources, failType := extractDirective(reason, allSources)
			feedback.FeedbackIntendedSources = intendedSources
			feedback.FeedbackExcludedSources = excludedSources
			if len(intendedSources) > 0 {
				feedback.FeedbackIntendedSource = intendedSources[0]
			}
			feedback.FeedbackFailureType = failType
		}
	}

	// Write to brain (dual-write: brain + legacy lessons.jsonl)
	cfg, _ := config.LoadConfig()
	if cfg != nil {
		_, profileName := cfg.ActiveProfileConfig()
		brn, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
		if err == nil {
			defer brn.Close()
			if err := brn.RecordLesson(context.Background(), feedback); err != nil {
				ui.PrintVerbose("Brain write", "feedback error: "+err.Error())
			} else if verdict == lessons.FeedbackThumbsDown {
				// Retract any auto-recorded lesson(s) for this same run so
				// they stop competing with the user's explicit correction in
				// recall forever (the real incident: a quality>=4 auto
				// routing_hint and a thumbs-down for the same run both
				// persisting). feedbackID is computed rather than returned
				// from RecordLesson so the just-written lesson isn't
				// retracted by its own supersede pass.
				feedbackID := brain.LessonID(feedback.Timestamp, feedback.Question)
				n, serr := brn.SupersedeLessonsByRunID(context.Background(), run.ID, feedbackID)
				if serr != nil {
					ui.PrintVerbose("Brain supersede", "error: "+serr.Error())
				} else if n > 0 {
					ui.PrintVerbose("Brain supersede", fmt.Sprintf("retracted %d prior lesson(s) for run %s", n, run.ID))
				}
			}
			if cfg.Brain.AutoSync {
				brain.CommitAndPush(cfg.BrainPath(), profileName)
			}
		}
	}

	icon := "👍"
	switch verdict {
	case lessons.FeedbackThumbsDown:
		icon = "👎"
	case lessons.FeedbackNote:
		icon = "📝"
	}
	fmt.Printf("%s %s %s on run %s\n", ui.SuccessIcon, icon, verdict, run.ID)
	if reason != "" {
		fmt.Printf("    reason: %s\n", reason)
	}
	if len(feedback.FeedbackIntendedSources) > 0 {
		fmt.Printf("    intended sources: %s\n", strings.Join(feedback.FeedbackIntendedSources, ", "))
	}
	if len(feedback.FeedbackExcludedSources) > 0 {
		fmt.Printf("    excluded sources: %s\n", strings.Join(feedback.FeedbackExcludedSources, ", "))
	}
	if len(feedback.Sources) > 0 {
		fmt.Printf("    sources on this run: %s\n", strings.Join(feedback.Sources, ", "))
	}
	// Phase 3.4: tell the user what the next similar question's routing will
	// look like so they can see the directive extractor worked. Only show
	// when feedback is thumbs-down AND we extracted a directive - noisy
	// otherwise.
	if verdict == lessons.FeedbackThumbsDown && (len(feedback.FeedbackIntendedSources) > 0 || len(feedback.FeedbackExcludedSources) > 0) {
		fmt.Printf("    on next similar question:")
		if len(feedback.FeedbackIntendedSources) > 0 {
			fmt.Printf(" PREFER [%s]", strings.Join(feedback.FeedbackIntendedSources, ", "))
		}
		if len(feedback.FeedbackExcludedSources) > 0 {
			fmt.Printf(" AVOID [%s]", strings.Join(feedback.FeedbackExcludedSources, ", "))
		}
		fmt.Println()
	}
	return nil
}

// editFeedbackContent opens $EDITOR for pasting detailed feedback.
// Returns the content after stripping comment lines.
func editFeedbackContent() (string, error) {
	cfg, _ := config.LoadConfig()
	editor := "vim"
	if cfg != nil {
		editor = cfg.GetEditor()
	}

	tmpFile := filepath.Join(os.TempDir(), "aida-feedback.md")
	prompt := `<!-- Paste your feedback below this line. Save and close when done. -->
<!-- Lines starting with <!-- will be stripped. -->

`
	if err := os.WriteFile(tmpFile, []byte(prompt), 0644); err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}
	defer os.Remove(tmpFile)

	editorCmd := exec.Command(editor, tmpFile)
	editorCmd.Stdin = os.Stdin
	editorCmd.Stdout = os.Stdout
	editorCmd.Stderr = os.Stderr
	if err := editorCmd.Run(); err != nil {
		return "", fmt.Errorf("editor failed: %w", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return "", fmt.Errorf("reading temp file: %w", err)
	}

	// Strip HTML comment lines
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!--") && strings.HasSuffix(trimmed, "-->") {
			continue
		}
		lines = append(lines, line)
	}

	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}

// extractOutputDirectives splits a --because reason into clauses and keeps
// the ones that look like format/output guidance: sentence/word/paragraph
// caps, "include the URL/link", "be concise", "shorter", "more detail",
// etc. The kept clauses are surfaced to the synthesizer as MUST-FOLLOW
// rules on similar future questions, so the user's "aida ok --because
// 'max 3 sentences and include the link'" actually changes behavior next
// time instead of being filed as soft prose.
//
// The matcher is intentionally permissive: false positives only cost a
// few extra prompt tokens, while false negatives mean the user has to
// repeat themselves.
func extractOutputDirectives(reason string) []string {
	if strings.TrimSpace(reason) == "" {
		return nil
	}
	clauses := splitIntoClauses(reason)
	keywords := []string{
		// Length/structure
		"sentence", "paragraph", "word", "bullet", "line",
		"max", "min", "no more than", "at most", "at least",
		"concise", "brief", "short", "shorter", "long", "longer",
		"verbose", "less", "more detail", "more context",
		// Format / required fields
		"include", "with the", "show the", "return the",
		"link", "url", "clickable", "click",
		"summary", "summarize", "summarise", "tl;dr", "tldr",
		"format", "table", "list as",
		// Tone
		"no commentary", "just the answer", "don't pad", "skip the",
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range clauses {
		l := strings.ToLower(c)
		for _, kw := range keywords {
			if strings.Contains(l, kw) {
				trim := strings.TrimSpace(c)
				if trim != "" && !seen[strings.ToLower(trim)] {
					seen[strings.ToLower(trim)] = true
					out = append(out, trim)
				}
				break
			}
		}
	}
	return out
}

// splitIntoClauses breaks reason into sentence- and clause-sized pieces on
// `.`, `!`, `?`, `;`, `,`, " and ", " but ", " also ", " plus ".
// Empty pieces are dropped.
func splitIntoClauses(s string) []string {
	// Replace conjunction phrases with a sentinel before splitting on it.
	for _, conj := range []string{" and ", " but ", " also ", " plus "} {
		s = strings.ReplaceAll(s, conj, "\x00")
	}
	// Replace punctuation with the same sentinel.
	for _, p := range []string{".", "!", "?", ";", ","} {
		s = strings.ReplaceAll(s, p, "\x00")
	}
	parts := strings.Split(s, "\x00")
	var out []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// extractDirective parses a --because reason to find:
//  1. the sources the user said SHOULD have been picked (intendedSources)
//  2. the sources the user said should NOT have been picked (excludedSources)
//  3. a failure type classification ("wrong_source", "wrong_answer",
//     "too_slow", or "")
//
// Phase 3.1 rewrite: the prior implementation iterated allSources (a Go
// map) and substring-matched the reason - map iteration is randomized, so
// a reason like "no need for github or spell-checker, should route to
// first-chair" could return any of the three as "intendedSource" depending
// on map order.
//
// The new parser tokenizes the reason once and walks it left-to-right,
// carrying a polarity (positive / negative) that flips on negation or
// positive cue phrases. Each source-name token gets bucketized into the
// current polarity. Polarity resets on sentence boundaries so
// "We used github. Should have used first-chair." correctly buckets only
// first-chair as intended.
func extractDirective(reason string, allSources config.Sources) (intendedSources, excludedSources []string, failureType string) {
	lower := strings.ToLower(reason)
	failureType = classifyFailureType(lower)

	tokens := directiveTokenizeWithBoundaries(lower)

	canon := make(map[string]string, len(allSources))
	for name := range allSources {
		canon[strings.ToLower(name)] = name
	}

	intendedSeen := map[string]bool{}
	excludedSeen := map[string]bool{}

	// Polarity starts positive ("should use X" is the common case). It
	// flips on negation cues, flips back on positive cues, and resets on
	// sentence boundaries (empty token from splitter).
	polarity := polarityPositive

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "" {
			// Sentence boundary marker emitted by the tokenizer.
			polarity = polarityPositive
			continue
		}
		// Multi-word phrase detection first - "no need for", "do not use",
		// "should have", etc. Phrases set polarity for following tokens.
		if adv, p, matched := matchPhrase(tokens, i); matched {
			polarity = p
			i += adv - 1 // -1 because outer loop also advances
			continue
		}
		if isNegationToken(tok) {
			polarity = polarityNegative
			continue
		}
		if isPositiveCue(tok) {
			polarity = polarityPositive
			continue
		}
		if name, ok := canon[tok]; ok {
			if polarity == polarityNegative {
				if !excludedSeen[name] {
					excludedSeen[name] = true
					excludedSources = append(excludedSources, name)
				}
			} else {
				if !intendedSeen[name] {
					intendedSeen[name] = true
					intendedSources = append(intendedSources, name)
				}
			}
		}
	}

	return intendedSources, excludedSources, failureType
}

type directivePolarity int

const (
	polarityPositive directivePolarity = iota
	polarityNegative
)

func classifyFailureType(lower string) string {
	switch {
	case strings.Contains(lower, "wrong source") || strings.Contains(lower, "should have") ||
		strings.Contains(lower, "should go to") || strings.Contains(lower, "should route") ||
		strings.Contains(lower, "should use") || strings.Contains(lower, "wanted") ||
		strings.Contains(lower, "instead of") || strings.Contains(lower, "no need for") ||
		strings.Contains(lower, "should not") || strings.Contains(lower, "avoid ") ||
		strings.Contains(lower, "right source") || strings.Contains(lower, "do not use") ||
		strings.Contains(lower, "don't use") || strings.Contains(lower, "route to"):
		return "wrong_source"
	case strings.Contains(lower, "wrong answer") || strings.Contains(lower, "incorrect") ||
		strings.Contains(lower, "inaccurate") || strings.Contains(lower, "answer was wrong") ||
		strings.Contains(lower, "the answer is wrong"):
		return "wrong_answer"
	case strings.Contains(lower, "slow") || strings.Contains(lower, "timeout"):
		return "too_slow"
	}
	return ""
}

// matchPhrase checks whether tokens[i..] begins with a known polarity-setting
// phrase. Returns (advance, polarity, true) on match. Longer phrases are
// tried before shorter so "do not use" wins over "not".
func matchPhrase(tokens []string, i int) (int, directivePolarity, bool) {
	type phrase struct {
		words    []string
		polarity directivePolarity
	}
	negPhrases := []phrase{
		{[]string{"no", "need", "for"}, polarityNegative},
		{[]string{"do", "not", "use"}, polarityNegative},
		{[]string{"do", "not"}, polarityNegative},
		{[]string{"dont", "use"}, polarityNegative},
		{[]string{"instead", "of"}, polarityNegative},
		{[]string{"rather", "than"}, polarityNegative},
		{[]string{"should", "not"}, polarityNegative},
	}
	posPhrases := []phrase{
		{[]string{"should", "have", "routed"}, polarityPositive},
		{[]string{"should", "have", "used"}, polarityPositive},
		{[]string{"should", "route", "to"}, polarityPositive},
		{[]string{"should", "use"}, polarityPositive},
		{[]string{"route", "to"}, polarityPositive},
		{[]string{"routed", "to"}, polarityPositive},
		{[]string{"right", "source"}, polarityPositive},
	}
	tryMatch := func(p phrase) bool {
		if i+len(p.words) > len(tokens) {
			return false
		}
		for k, w := range p.words {
			if tokens[i+k] != w {
				return false
			}
		}
		return true
	}
	for _, p := range negPhrases {
		if tryMatch(p) {
			return len(p.words), p.polarity, true
		}
	}
	for _, p := range posPhrases {
		if tryMatch(p) {
			return len(p.words), p.polarity, true
		}
	}
	return 0, polarityPositive, false
}

// directiveTokenizeWithBoundaries splits text into lowercase word tokens,
// preserving internal hyphens so multi-word source names like
// "all-the-things" stay intact. Sentence-ending punctuation (.!?) emits
// an empty token so the caller can reset polarity on each sentence.
func directiveTokenizeWithBoundaries(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r == '.' || r == '!' || r == '?':
			flush()
			out = append(out, "") // sentence boundary
		default:
			flush()
		}
	}
	flush()
	return out
}

func isNegationToken(tok string) bool {
	switch tok {
	case "not", "without", "avoid", "drop", "skip", "exclude", "dont", "don-t":
		return true
	}
	return false
}

func isPositiveCue(tok string) bool {
	switch tok {
	case "use", "prefer", "want", "pick", "picked", "choose":
		return true
	}
	return false
}
