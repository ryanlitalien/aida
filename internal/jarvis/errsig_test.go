package jarvis

import "testing"

// TestNormalizeErrorSignature covers the variable detail that made near-
// identical errors compare unequal before normalization: durations, run
// ids/UUIDs, and quoted text. Same root cause should normalize identically
// regardless of the exact number or quoted detail involved.
func TestNormalizeErrorSignature(t *testing.T) {
	tests := []struct {
		name string
		a, b string
	}{
		{
			name: "duration in ms differs",
			a:    "context deadline exceeded (took_ms: 45000)",
			b:    "context deadline exceeded (took_ms: 47231)",
		},
		{
			name: "timeout seconds differs",
			a:    "operation timed out after 45s",
			b:    "operation timed out after 60s",
		},
		{
			name: "exit status same but framed differently in whitespace",
			a:    "ssh: exit status 1",
			b:    "ssh:   exit status 1",
		},
		{
			name: "quoted user query differs",
			a:    `failed to run command "build the nether portal"`,
			b:    `failed to run command "mine some diamonds"`,
		},
		{
			name: "run id UUID differs",
			a:    "run 123e4567-e89b-12d3-a456-426614174000 failed: signal: killed",
			b:    "run 9f8e7d6c-5b4a-3210-9876-fedcba098765 failed: signal: killed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			na, nb := normalizeErrorSignature(tt.a), normalizeErrorSignature(tt.b)
			if na != nb {
				t.Errorf("normalizeErrorSignature(%q) = %q, normalizeErrorSignature(%q) = %q; want equal", tt.a, na, tt.b, nb)
			}
		})
	}
}

// TestNormalizeErrorSignature_DistinctErrorsStayDistinct guards against the
// normalization being so aggressive that unrelated failures collapse
// together and get silently merged.
func TestNormalizeErrorSignature_DistinctErrorsStayDistinct(t *testing.T) {
	a := normalizeErrorSignature("ssh: exit status 1")
	b := normalizeErrorSignature("signal: killed")
	if a == b {
		t.Errorf("unrelated error messages normalized to the same signature: %q", a)
	}
}

func TestErrorSignatureTag(t *testing.T) {
	tag1 := errorSignatureTag("context deadline exceeded (took_ms: 45000)")
	tag2 := errorSignatureTag("context deadline exceeded (took_ms: 47231)")
	if tag1 != tag2 {
		t.Errorf("errorSignatureTag differs for near-identical timeouts: %q vs %q", tag1, tag2)
	}
	if len(tag1) == 0 || tag1[:len("errsig:")] != "errsig:" {
		t.Errorf("errorSignatureTag(%q) = %q, want errsig: prefix", "took_ms", tag1)
	}

	tag3 := errorSignatureTag("signal: killed")
	if tag1 == tag3 {
		t.Errorf("errorSignatureTag collapsed two unrelated errors to the same tag: %q", tag1)
	}
}
