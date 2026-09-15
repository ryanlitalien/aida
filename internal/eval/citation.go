package eval

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// citationPattern matches inline citations of the form
// "(source_name: artifact_id)". Per the synthesizer prompt
// (internal/llm/prompts.go SynthesisSystemPrompt), every factual
// claim must be followed by such a citation, and the source_name
// is verbatim from a "--- name ---" header. The artifact_id is
// taken verbatim from the Artifacts list of that source.
//
// Source names follow aida's YAML-key convention: lowercase
// letters, digits, dashes, underscores. Artifact ids are more
// permissive (commonly contain dots, slashes, mixed case from
// e.g. owner/repo/pull/N or row IDs).
//
// Markdown links of the form [text](url) collide with this
// pattern because `(scheme:rest)` looks like `(source:id)`.
// We disambiguate at match time by checking the character
// immediately preceding the match - if it's `]`, it's a
// markdown link, not a citation.
var citationPattern = regexp.MustCompile(`\(([a-z0-9_-]+)\s*:\s*([A-Za-z0-9_./-]+)\)`)

// genericCitationPrefixes are non-specific lead words the synthesizer
// sometimes emits instead of a canonical source name, e.g.
// "(source: timeanddate.com)" rather than "(web-search: result-3)".
// When group1 matches one of these, group2 is the CLAIMED SOURCE NAME
// itself, not an artifact id - it must be checked against the set of
// queried source names, not against one source's artifact list. This
// keeps the missing-source finding readable (it names the claimed
// source, e.g. "timeanddate.com", instead of the generic word
// "source").
var genericCitationPrefixes = map[string]struct{}{
	"source":  {},
	"sources": {},
	"src":     {},
	"via":     {},
	"from":    {},
}

// CitationReviewer is a deterministic Reviewer that checks every
// inline (source: artifact_id) citation in the synthesized answer
// resolves to an actual artifact in the source results.
//
// What it catches:
//   - Citations to source names that weren't actually queried.
//   - Citations to artifact ids that don't appear in any source's
//     Artifacts list.
//   - The "placeholder citation" anti-pattern (source_name: source_name)
//     called out explicitly by the synthesizer prompt.
//
// What it does NOT catch (deferred to inferential reviewers):
//   - Citations whose artifact exists but doesn't actually support
//     the surrounding claim semantically.
//   - Hallucinated facts that happen to be cited correctly.
type CitationReviewer struct{}

// NewCitationReviewer returns a stateless reviewer instance.
func NewCitationReviewer() *CitationReviewer { return &CitationReviewer{} }

// Name implements Reviewer.
func (CitationReviewer) Name() string { return "citation" }

// Review implements Reviewer. Returns Pass when every citation
// resolves; Fail when at least one is broken. Warn when a citation
// references a source that exists but used a non-canonical
// artifact id format (currently unused; reserved for future
// fuzzy-match heuristics).
func (CitationReviewer) Review(ctx context.Context, in ReviewInput) (*ReviewRecord, error) {
	// Build a quick lookup: source name → set of artifact IDs.
	known := make(map[string]map[string]struct{}, len(in.Results))
	for _, r := range in.Results {
		ids := make(map[string]struct{}, len(r.Artifacts))
		for _, a := range r.Artifacts {
			ids[a.ID] = struct{}{}
		}
		known[r.Source] = ids
	}

	// Use index-based matching so we can peek at the character
	// preceding each match - needed to skip markdown links
	// "[text](url)" which collide with the citation pattern.
	idxMatches := citationPattern.FindAllStringSubmatchIndex(in.Answer, -1)

	// Filter out markdown links.
	type match struct {
		full, src, id string
	}
	var matches []match
	for _, m := range idxMatches {
		start := m[0]
		if start > 0 && in.Answer[start-1] == ']' {
			continue
		}
		matches = append(matches, match{
			full: in.Answer[m[0]:m[1]],
			src:  in.Answer[m[2]:m[3]],
			id:   in.Answer[m[4]:m[5]],
		})
	}

	// Empty-answer or no-citations short-circuit. We don't make a
	// claim either way - that's the completeness reviewer's job.
	if len(matches) == 0 {
		return &ReviewRecord{
			Reviewer:  "citation",
			Verdict:   VerdictPass,
			Rationale: "no inline citations to validate",
		}, nil
	}

	var issues []Issue
	for _, m := range matches {
		full := m.full
		src := strings.TrimSpace(m.src)
		id := strings.TrimSpace(m.id)

		// Anti-pattern explicitly called out in the synth prompt:
		// (source_name: source_name) used as a placeholder.
		if src == id {
			issues = append(issues, Issue{
				Type:     "placeholder-citation",
				Severity: "error",
				Message: fmt.Sprintf("citation %q reuses the source name as the artifact id; "+
					"this is the placeholder anti-pattern called out in the synth prompt", full),
				Anchor: full,
			})
			continue
		}

		// Generic-prefix form: "(source: <name>)", "(via: <name>)", etc.
		// Here group2 is the claimed source name, not an artifact id.
		if _, generic := genericCitationPrefixes[src]; generic {
			if _, queried := known[id]; !queried {
				issues = append(issues, Issue{
					Type:     "missing-source",
					Severity: "error",
					Message: fmt.Sprintf("citation %q references source %q which was not queried in this run",
						full, id),
					Anchor: full,
				})
			}
			// id names a source that WAS queried (e.g. "(source: web-search)").
			// This is a benign, non-canonical citation form - the claim is
			// anchored to a real queried source, just without a specific
			// artifact id pinned. Not a fabrication signal, so no issue.
			continue
		}

		artifacts, sourceKnown := known[src]
		if !sourceKnown {
			issues = append(issues, Issue{
				Type:     "missing-source",
				Severity: "error",
				Message: fmt.Sprintf("citation %q references source %q which was not queried in this run",
					full, src),
				Anchor: full,
			})
			continue
		}
		if _, ok := artifacts[id]; !ok {
			issues = append(issues, Issue{
				Type:     "missing-citation",
				Severity: "error",
				Message: fmt.Sprintf("citation %q references artifact %q which is not present in source %q's results",
					full, id, src),
				Anchor: full,
			})
		}
	}

	if len(issues) == 0 {
		return &ReviewRecord{
			Reviewer:  "citation",
			Verdict:   VerdictPass,
			Score:     1.0,
			Rationale: fmt.Sprintf("%d citation(s) all resolved cleanly", len(matches)),
		}, nil
	}

	return &ReviewRecord{
		Reviewer:  "citation",
		Verdict:   VerdictFail,
		Score:     float64(len(matches)-len(issues)) / float64(len(matches)),
		Issues:    issues,
		Rationale: fmt.Sprintf("%d of %d citations failed validation", len(issues), len(matches)),
	}, nil
}
