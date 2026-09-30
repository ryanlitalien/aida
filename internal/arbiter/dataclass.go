package arbiter

import (
	"fmt"
	"strings"
)

// personalTags, butterstackTags, and gamesTags are the task-tag
// vocabularies docs/arbiter-plan.md section 3 maps to each data class.
// Matching is case-insensitive (ClassifyTags lowercases before lookup).
// A tag that matches none of these, and isn't exactly "public", falls
// through ClassifyTags's default case (personal) -- see its doc comment.
var (
	personalTags = map[string]bool{
		"personal":         true,
		"project:finances": true,
		"project:life-log": true,
		"finances":         true,
		"family":           true,
		"career":           true,
	}
	butterstackTags = map[string]bool{
		"project:butterstack":  true,
		"project:butter_stack": true, // retired variant still on older tasks
		"butterstack":          true,
		"project:cta":          true,
		"cta":                  true,
		"its":                  true,
	}
	gamesTags = map[string]bool{
		"games":                 true,
		"game":                  true,
		"project:plt":           true,
		"project:tb":            true,
		"project:bu":            true,
		"project:butter-up":     true,
		"project:pilot-light":   true,
		"pilot-light":           true,
		"project:butterup":      true,
		"project:bsg":           true, // Butter Smooth Games, the studio banner
		"bsg":                   true,
		"project:tracer-bullet": true,
	}
)

// ClassifyTags maps a task's tags to one DataClass, per
// docs/arbiter-plan.md section 3:
//
//   - any tag in personalTags -> DataClassPersonal
//   - any tag in butterstackTags -> DataClassButterstack
//   - any tag in gamesTags -> DataClassGames
//   - the tag "public" -> DataClassPublic
//   - a tag matching more than one of the above: the most restrictive
//     wins, in order personal > butterstack > games > public
//   - personal and butterstack matched together is a config error (ADR:
//     no lane may carry both, so a task that claims to be both can never
//     be routed -- better to fail loudly here than silently pick one)
//   - no tag matches anything above (including no tags at all, e.g. a
//     bare "project:aida"): DataClassPersonal, the safest default
//     (ADR-0003) -- an untagged or unrecognized-tag task is treated as
//     though it might be personal data until proven otherwise.
func ClassifyTags(tags []string) (DataClass, error) {
	matched := make(map[DataClass]bool, 4)
	for _, t := range tags {
		tl := strings.ToLower(strings.TrimSpace(t))
		switch {
		case personalTags[tl]:
			matched[DataClassPersonal] = true
		case butterstackTags[tl]:
			matched[DataClassButterstack] = true
		case gamesTags[tl]:
			matched[DataClassGames] = true
		case tl == "public":
			matched[DataClassPublic] = true
		}
	}

	if matched[DataClassPersonal] && matched[DataClassButterstack] {
		return "", fmt.Errorf("arbiter: tags %v map to both %s and %s data classes; no lane may carry both", tags, DataClassPersonal, DataClassButterstack)
	}

	switch {
	case matched[DataClassPersonal]:
		return DataClassPersonal, nil
	case matched[DataClassButterstack]:
		return DataClassButterstack, nil
	case matched[DataClassGames]:
		return DataClassGames, nil
	case matched[DataClassPublic]:
		return DataClassPublic, nil
	default:
		return DataClassPersonal, nil
	}
}
