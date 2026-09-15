package roster

import (
	"sort"
	"strings"
)

// normalizeName strips common separators and lowercases for fuzzy matching,
// e.g. "Product Manager", "product-manager", and "product_manager" all
// become "productmanager". Replicated (not imported) from the identical
// helper in internal/cli/tasks.go -- the roster package must not import cli.
func normalizeName(s string) string {
	s = strings.ToLower(s)
	for _, sep := range []string{"-", "_", ".", " "} {
		s = strings.ReplaceAll(s, sep, "")
	}
	return s
}

// Resolve maps a spoken or typed reference to a single roster entry.
//
// It searches the active profile's entries first, in precedence order:
// exact name, exact call-sign, exact alias, normalizeName equality, then
// normalizeName substring. The first tier that yields any match wins --
// later, looser tiers are never consulted once an earlier tier hits.
//
// If nothing in the active profile matches, Resolve searches every entry
// across every profile. A hit there means the reference is real but scoped
// elsewhere: it returns ErrOtherProfile carrying the match and its owning
// profile so the caller can offer to cross over. No match anywhere returns
// ErrNotFound.
func (r *Roster) Resolve(ref string) (*Entry, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, ErrNotFound
	}

	if hits := matchIn(r.entries, trimmed); len(hits) > 0 {
		if len(hits) > 1 {
			return nil, ErrAmbiguous
		}
		return hits[0], nil
	}

	if hits := matchIn(r.all, trimmed); len(hits) > 0 {
		sortEntriesByName(hits)
		match := hits[0]
		return nil, &ErrOtherProfile{Ref: trimmed, Entry: match, Profile: firstProfileOf(match)}
	}

	return nil, ErrNotFound
}

// firstProfileOf returns the first profile that owns e, or "other" when e
// carries no explicit profile (shouldn't happen for anything reaching the
// ErrOtherProfile path, since profile-less entries match every profile).
func firstProfileOf(e *Entry) string {
	if len(e.Profiles) > 0 {
		return e.Profiles[0]
	}
	return "other"
}

// matchIn searches entries for ref, trying each precedence tier in order
// and stopping at the first tier that yields any hit. Hits within a tier
// are deduplicated by Name.
func matchIn(entries []*Entry, ref string) []*Entry {
	normRef := normalizeName(ref)

	tiers := []func(*Entry) bool{
		// 1. exact case-insensitive Name.
		func(e *Entry) bool { return strings.EqualFold(e.Name, ref) },
		// 2. exact case-insensitive CallSign.
		func(e *Entry) bool { return e.CallSign != "" && strings.EqualFold(e.CallSign, ref) },
		// 3. exact case-insensitive alias.
		func(e *Entry) bool {
			for _, a := range e.Aliases {
				if strings.EqualFold(a, ref) {
					return true
				}
			}
			return false
		},
		// 4. normalizeName equality against Name/CallSign/aliases.
		func(e *Entry) bool {
			if normalizeName(e.Name) == normRef {
				return true
			}
			if e.CallSign != "" && normalizeName(e.CallSign) == normRef {
				return true
			}
			for _, a := range e.Aliases {
				if normalizeName(a) == normRef {
					return true
				}
			}
			return false
		},
		// 5. normalizeName substring: ref is contained in a candidate's
		// normalized Name or CallSign.
		func(e *Entry) bool {
			if strings.Contains(normalizeName(e.Name), normRef) {
				return true
			}
			if e.CallSign != "" && strings.Contains(normalizeName(e.CallSign), normRef) {
				return true
			}
			return false
		},
	}

	for _, match := range tiers {
		var hits []*Entry
		for _, e := range entries {
			if match(e) {
				hits = append(hits, e)
			}
		}
		hits = dedupeByName(hits)
		if len(hits) > 0 {
			return hits
		}
	}
	return nil
}

// dedupeByName drops repeat entries (by Name) while preserving order.
func dedupeByName(entries []*Entry) []*Entry {
	seen := map[string]bool{}
	var out []*Entry
	for _, e := range entries {
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		out = append(out, e)
	}
	return out
}

// sortEntriesByName sorts entries in place by Name, used to make the
// "match in another profile" pick deterministic when more than one exists.
func sortEntriesByName(entries []*Entry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}
