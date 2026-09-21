package brain

// Meetily tag-vocabulary builder (aida task #367). The classifier pass in
// harvest_meetily.go asks the LLM to choose call tags ONLY from a
// candidate vocabulary built at harvest time from what the user is
// actually working on right now -- never a hardcoded project list, since
// that would drift out of date the moment a new client/project shows up.
//
// Three sources feed the vocabulary:
//
//   (a) task tags across every profile and status (see
//       computeMeetilyTagVocabulary / isMechanicalTaskTag for the
//       mechanical-tag exclusion list).
//   (b) brain entity page slugs (partners/tools/people) -- see
//       DB.ListEntitySlugs.
//   (c) library source `entities:` lists (internal/library). Cheap,
//       config/YAML-only reads, no LLM or network calls. internal/library
//       depends only on internal/config, internal/ui, internal/sources,
//       and internal/execx -- none of which import internal/brain -- so
//       importing it here does not create a cycle (verified empirically:
//       `go build ./...` after this file was added).
//
// Deliberately split into a pure function (computeMeetilyTagVocabulary,
// unit-tested directly with literal inputs) and an I/O-performing
// orchestrator (Brain.buildMeetilyTagVocabulary) so the exclusion-list
// logic never needs a real Brain, brain.db, or ~/.aida to test.

import (
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
)

// meetilyMechanicalTagPrefixes are task-tag prefixes that are structural
// bookkeeping, never a topic a call transcript could plausibly be
// classified under.
var meetilyMechanicalTagPrefixes = []string{
	"profile:",
	"due:",
	"source-hash:",
	"tool:",
	"owner:",
	"from-",
}

// meetilyMechanicalTagExact are exact-match task tags with the same
// "structural, not topical" property as meetilyMechanicalTagPrefixes.
var meetilyMechanicalTagExact = map[string]bool{
	"today":        true,
	"tomorrow":     true,
	"jarvis-error": true,
}

// isMechanicalTaskTag reports whether tag is bookkeeping noise that should
// never appear in the classifier's tag vocabulary, as opposed to a real
// project/topic tag (project:<slug>, or a plain topical tag like
// "acme-widgets" or "hoa").
func isMechanicalTaskTag(tag string) bool {
	lower := strings.ToLower(strings.TrimSpace(tag))
	if lower == "" {
		return true
	}
	if meetilyMechanicalTagExact[lower] {
		return true
	}
	for _, prefix := range meetilyMechanicalTagPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// computeMeetilyTagVocabulary merges task tags (mechanical ones filtered
// via isMechanicalTaskTag), entity slugs, and library source entities into
// one deduplicated, sorted vocabulary. Pure -- no I/O -- so it is
// unit-testable with literal input slices; the three inputs are gathered
// by Brain.buildMeetilyTagVocabulary.
func computeMeetilyTagVocabulary(taskTags, entitySlugs, librarySourceEntities []string) []string {
	seen := map[string]bool{}
	var vocab []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		vocab = append(vocab, v)
	}

	for _, t := range taskTags {
		if isMechanicalTaskTag(t) {
			continue
		}
		add(t)
	}
	for _, s := range entitySlugs {
		add(s)
	}
	for _, e := range librarySourceEntities {
		add(e)
	}

	sort.Strings(vocab)
	return vocab
}

// buildMeetilyTagVocabulary gathers task tags (across every profile and
// status -- the goal is "what does the user work on at all", not just
// currently-open/current-profile tasks) and brain entity slugs from this
// Brain, folds in librarySourceEntities (the caller's already-loaded
// library source entities, or nil if unavailable/skipped), and returns the
// combined vocabulary via computeMeetilyTagVocabulary. Each I/O source
// degrades to "contributes nothing" on error rather than failing the
// whole harvest -- vocabulary is a best-effort classifier aid.
func (b *Brain) buildMeetilyTagVocabulary(librarySourceEntities []string) []string {
	var taskTags []string
	if tasks, err := b.ListTasksAllProfiles(true, nil, 0, 0); err == nil {
		for _, t := range tasks {
			taskTags = append(taskTags, t.Tags...)
		}
	}

	var entitySlugs []string
	if b.DB != nil {
		if slugs, err := b.DB.ListEntitySlugs(); err == nil {
			entitySlugs = slugs
		}
	}

	return computeMeetilyTagVocabulary(taskTags, entitySlugs, librarySourceEntities)
}

// meetilyLibrarySourceEntities loads every configured library source's
// `entities:` list. This is the ONLY place that touches the real
// ~/.aida/library.yaml + library/sources/*.yaml -- called exclusively from
// HarvestMeetily (the real entrypoint), never from the injectable
// harvestMeetily core, so tests never hit the live library config.
func meetilyLibrarySourceEntities() ([]string, error) {
	reg, err := library.LoadRegistry(config.Dir())
	if err != nil {
		return nil, err
	}
	srcs, err := reg.LoadSources()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range srcs {
		if s == nil {
			continue
		}
		out = append(out, s.Entities...)
	}
	return out, nil
}
