package jarvis

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
)

var (
	errsigQuoted = regexp.MustCompile(`"[^"]*"|'[^']*'|` + "`[^`]*`")
	errsigUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	errsigDigits = regexp.MustCompile(`\d+`)
	errsigSpace  = regexp.MustCompile(`\s+`)
)

// normalizeErrorSignature collapses a raw tool-error message down to a
// stable "shape" so near-identical errors compare equal regardless of the
// variable detail embedded in them: durations ("took_ms: 45000" vs
// "took_ms: 47000"), run ids, and quoted user queries. Without this, a
// flaky 45s timeout files a fresh ticket every time because the exact
// duration differs byte for byte from the last one - this is how ten
// near-identical timeout tickets and seven near-identical SSH exit-status
// tickets piled up for what were really two root causes.
func normalizeErrorSignature(errMsg string) string {
	s := strings.ToLower(strings.TrimSpace(errMsg))
	s = errsigUUID.ReplaceAllString(s, "id")
	s = errsigQuoted.ReplaceAllString(s, "text")
	s = errsigDigits.ReplaceAllString(s, "#")
	s = errsigSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// errorSignatureTag hashes a normalized error signature into a short,
// tag-safe token ("errsig:<hex>") that can be attached to a task and
// matched on exactly via the tag filter - avoids re-parsing every
// candidate task's body just to compare signatures.
func errorSignatureTag(errMsg string) string {
	h := fnv.New32a()
	h.Write([]byte(normalizeErrorSignature(errMsg)))
	return fmt.Sprintf("errsig:%08x", h.Sum32())
}
