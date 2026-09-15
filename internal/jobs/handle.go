package jobs

import (
	"crypto/sha1"
	"strings"
)

// A run-id is a long timestamp-slug ("20260622-022838-…") that the LLM
// mangles when reading aloud and the user can't easily say back. DeriveHandle
// maps it to a short, pronounceable adjective-noun handle ("amber-otter") that
// Jarvis speaks and the web runs UI displays - the shared spoken/visual
// identifier for a background job. Derived on demand, so nothing extra is
// stored and any consumer (voice tools, web UI) computes the same handle.
//
// Both lists hold exactly 64 entries: one hash byte indexes each list, and
// 256 % 64 == 0 keeps the pick uniform (no modulo bias). 64×64 = 4096 combos
// puts the birthday-collision 50% point near 75 jobs - comfortably past the
// 200-row window resolveJobRef scans. Entries are TTS-clean (lowercase
// letters only, no digits) and chosen to avoid near-homophones Whisper
// would swap.

var handleAdjectives = []string{
	"amber", "bold", "bright", "brisk", "calm", "candid", "cheerful", "clever",
	"cozy", "crisp", "dapper", "daring", "deft", "eager", "earnest", "fearless",
	"festive", "fleet", "frosty", "gallant", "gentle", "golden", "graceful", "hardy",
	"hearty", "honest", "humble", "jaunty", "jolly", "keen", "lively", "loyal",
	"lucky", "mellow", "merry", "modest", "nimble", "noble", "patient", "peppy",
	"perky", "plucky", "polished", "proud", "quiet", "rapid", "rosy", "rugged",
	"silver", "sleek", "snappy", "steady", "sturdy", "sunny", "swift", "tidy",
	"trusty", "upbeat", "valiant", "vivid", "warm", "witty", "zesty", "zippy",
}

var handleNouns = []string{
	"acorn", "anchor", "aspen", "badger", "beacon", "birch", "brook", "canyon",
	"cedar", "clover", "comet", "compass", "coral", "crane", "cricket", "dolphin",
	"eagle", "ember", "falcon", "fern", "finch", "garnet", "glacier", "grove",
	"harbor", "hawk", "hazel", "heron", "island", "jasper", "kestrel", "lagoon",
	"lantern", "lark", "linden", "lotus", "magpie", "maple", "marble", "marten",
	"meadow", "mesa", "moss", "opal", "orchard", "osprey", "otter", "pebble",
	"pine", "plover", "prairie", "quartz", "raven", "reef", "ridge", "river",
	"robin", "saddle", "sparrow", "summit", "thistle", "tulip", "willow", "wren",
}

// DeriveHandle maps a run-id to a stable adjective-noun handle. Deterministic
// (same run-id always yields the same handle) and TTS-clean (plain English,
// no digits).
func DeriveHandle(runID string) string {
	h := sha1.Sum([]byte(runID))
	adj := handleAdjectives[int(h[0])%len(handleAdjectives)]
	noun := handleNouns[int(h[1])%len(handleNouns)]
	return adj + "-" + noun
}

// NormalizeRef lowercases s and strips it to letters+digits, so a spoken
// "amber otter" / "Amber-Otter" / "amber, otter" all compare equal to a
// derived handle's normalized form ("amberotter").
func NormalizeRef(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// RefMatchesHandleWord reports whether any word of ref equals the adjective
// or the noun of runID's derived handle. This is the mishear-tolerant tier
// of handle resolution: Whisper renders "amber-otter" as "amber other" or
// the user says just "amber", and either surviving word still identifies
// the job. Callers should try full-handle and substring matches first -
// a single common word is weaker evidence than either.
func RefMatchesHandleWord(runID, ref string) bool {
	parts := strings.SplitN(DeriveHandle(runID), "-", 2)
	for _, tok := range strings.FieldsFunc(strings.ToLower(ref), func(r rune) bool {
		return r < 'a' || r > 'z'
	}) {
		if tok == parts[0] || tok == parts[1] {
			return true
		}
	}
	return false
}
