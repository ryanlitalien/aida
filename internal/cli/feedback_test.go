package cli

import (
	"reflect"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// TestExtractDirective captures the Phase 3.1 rewrite. Prior behavior
// iterated allSources as a Go map with randomized order and substring
// matched, so a reason that mentioned github + first-chair + spell-checker
// could return any of the three as "intendedSource" depending on map seed.
// The new parser is deterministic and handles negation.
func TestExtractDirective(t *testing.T) {
	sources := config.Sources{
		"github":         {},
		"first-chair":    {},
		"spell-checker":  {},
		"all-the-things": {},
		"pine-hollow":    {},
	}

	tests := []struct {
		name        string
		reason      string
		intended    []string
		excluded    []string
		failureType string
	}{
		{
			name:        "real run B thumbs-down text",
			reason:      "It should have routed to the first-chair agent, as that agent has context around First Chair/FC. Also, no need for github or spell-checker.",
			intended:    []string{"first-chair"},
			excluded:    []string{"github", "spell-checker"},
			failureType: "wrong_source",
		},
		{
			name:        "simple positive intent",
			reason:      "should have used all-the-things",
			intended:    []string{"all-the-things"},
			excluded:    nil,
			failureType: "wrong_source",
		},
		{
			name:        "simple negation",
			reason:      "do not use github for this, prefer first-chair instead",
			intended:    []string{"first-chair"},
			excluded:    []string{"github"},
			failureType: "wrong_source",
		},
		{
			name:        "multi-source intended",
			reason:      "should route to pine-hollow and all-the-things",
			intended:    []string{"pine-hollow", "all-the-things"},
			excluded:    nil,
			failureType: "wrong_source",
		},
		{
			name:        "no need for phrasing",
			reason:      "no need for spell-checker here",
			intended:    nil,
			excluded:    []string{"spell-checker"},
			failureType: "wrong_source",
		},
		{
			name:        "avoid keyword",
			reason:      "avoid github, route to first-chair",
			intended:    []string{"first-chair"},
			excluded:    []string{"github"},
			failureType: "wrong_source",
		},
		{
			name:        "no source mentioned",
			reason:      "the answer was wrong",
			intended:    nil,
			excluded:    nil,
			failureType: "wrong_answer",
		},
		{
			name:        "too slow classification",
			reason:      "this run was too slow",
			intended:    nil,
			excluded:    nil,
			failureType: "too_slow",
		},
		{
			name:        "empty reason",
			reason:      "",
			intended:    nil,
			excluded:    nil,
			failureType: "",
		},
		{
			name:        "deduplicate intended",
			reason:      "first-chair is the right source, first-chair has context, route to first-chair",
			intended:    []string{"first-chair"},
			excluded:    nil,
			failureType: "wrong_source",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			intended, excluded, failureType := extractDirective(tc.reason, sources)
			if !reflect.DeepEqual(intended, tc.intended) {
				t.Errorf("intended: got %v, want %v", intended, tc.intended)
			}
			if !reflect.DeepEqual(excluded, tc.excluded) {
				t.Errorf("excluded: got %v, want %v", excluded, tc.excluded)
			}
			if failureType != tc.failureType {
				t.Errorf("failureType: got %q, want %q", failureType, tc.failureType)
			}
		})
	}
}

// TestExtractDirective_Deterministic runs the same reason 20 times with
// the same allSources map. Pre-fix, map iteration randomization would
// flip intendedSource between runs. Post-fix, output must be stable.
func TestExtractDirective_Deterministic(t *testing.T) {
	sources := config.Sources{
		"github":         {},
		"first-chair":    {},
		"spell-checker":  {},
		"all-the-things": {},
		"pine-hollow":    {},
		"slack":          {},
		"notion":         {},
		"sqlite":         {},
	}
	reason := "should have routed to the first-chair agent, no need for github or spell-checker"
	firstIntended, firstExcluded, _ := extractDirective(reason, sources)
	for i := 0; i < 20; i++ {
		intended, excluded, _ := extractDirective(reason, sources)
		if !reflect.DeepEqual(intended, firstIntended) {
			t.Fatalf("iteration %d: intended drift. first=%v now=%v", i, firstIntended, intended)
		}
		if !reflect.DeepEqual(excluded, firstExcluded) {
			t.Fatalf("iteration %d: excluded drift. first=%v now=%v", i, firstExcluded, excluded)
		}
	}
}
