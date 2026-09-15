package cli

import (
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestSpliceManagedBlock_NeitherMarkerAppends(t *testing.T) {
	existing := "Some prior instructions.\n"
	inner := "## Who I am\n\n- Name: Ryan\n"

	result, changed, err := spliceManagedBlock(existing, inner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if !strings.Contains(result, soulBlockBegin) || !strings.Contains(result, soulBlockEnd) {
		t.Fatalf("result missing markers: %q", result)
	}
	if !strings.Contains(result, inner) {
		t.Fatalf("result missing inner content: %q", result)
	}
	if !strings.HasPrefix(result, "Some prior instructions.\n\n"+soulBlockBegin) {
		t.Fatalf("expected exactly one blank line between prior content and block, got: %q", result)
	}
	if strings.HasSuffix(result, "\n\n") {
		t.Fatalf("expected result to end cleanly, not with a double blank line: %q", result)
	}
}

func TestSpliceManagedBlock_BothMarkersReplacesAndPreservesSurroundings(t *testing.T) {
	before := "# CLAUDE.md\n\nSome preamble.\n\n"
	oldBlock := soulBlockBegin + "\n" + soulNote + "\n\nold inner content\n" + soulBlockEnd
	after := "\n\nMore stuff after.\n"
	existing := before + oldBlock + after

	newInner := "## Who I am\n\n- Name: Ryan\n"
	result, changed, err := spliceManagedBlock(existing, newInner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if !strings.HasPrefix(result, before) {
		t.Fatalf("prior content not preserved, got: %q", result)
	}
	if !strings.HasSuffix(result, after) {
		t.Fatalf("trailing content not preserved, got: %q", result)
	}
	if strings.Contains(result, "old inner content") {
		t.Fatalf("old inner content should have been replaced, got: %q", result)
	}
	if !strings.Contains(result, newInner) {
		t.Fatalf("new inner content missing, got: %q", result)
	}
}

func TestSpliceManagedBlock_MalformedMarkersError(t *testing.T) {
	cases := map[string]string{
		"only begin":       "prefix\n" + soulBlockBegin + "\nsome content\n",
		"only end":         "prefix\n" + "some content\n" + soulBlockEnd + "\n",
		"end before begin": "prefix\n" + soulBlockEnd + "\nmiddle\n" + soulBlockBegin + "\nsuffix\n",
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			result, changed, err := spliceManagedBlock(existing, "inner")
			if err == nil {
				t.Fatalf("expected an error")
			}
			if changed {
				t.Fatalf("expected changed=false")
			}
			if result != existing {
				t.Fatalf("expected result to equal existing unchanged, got: %q, want: %q", result, existing)
			}
		})
	}
}

func TestSpliceManagedBlock_EmptyExisting(t *testing.T) {
	inner := "## Who I am\n\n- Name: Ryan\n"
	result, changed, err := spliceManagedBlock("", inner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	want := renderManagedSoulBlock(inner) + "\n"
	if result != want {
		t.Fatalf("result = %q, want %q", result, want)
	}
}

func TestSpliceManagedBlock_CRLFMarkersMatched(t *testing.T) {
	existing := "prefix\r\n" + soulBlockBegin + "\r\n" + soulNote + "\r\n\r\nold\r\n" + soulBlockEnd + "\r\nsuffix\r\n"
	newInner := "new inner"
	result, changed, err := spliceManagedBlock(existing, newInner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true")
	}
	if !strings.HasPrefix(result, "prefix\r\n") {
		t.Fatalf("expected CRLF prefix preserved, got: %q", result)
	}
	if !strings.HasSuffix(result, "\r\nsuffix\r\n") {
		t.Fatalf("expected CRLF suffix preserved, got: %q", result)
	}
	if strings.Contains(result, "old") {
		t.Fatalf("old inner should have been removed, got: %q", result)
	}
	if !strings.Contains(result, newInner) {
		t.Fatalf("new inner missing, got: %q", result)
	}
}

func TestSpliceManagedBlock_Idempotent(t *testing.T) {
	inner := "## Who I am\n\n- Name: Ryan\n- Role: Engineer\n"

	first, changed, err := spliceManagedBlock("", inner)
	if err != nil {
		t.Fatalf("unexpected error on first splice: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first splice")
	}

	second, changed, err := spliceManagedBlock(first, inner)
	if err != nil {
		t.Fatalf("unexpected error on second splice: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on second splice (idempotent), result: %q", second)
	}
	if second != first {
		t.Fatalf("second splice result differs from first:\nfirst:  %q\nsecond: %q", first, second)
	}

	// Also verify idempotency when the block is embedded in surrounding content.
	withSurroundings := "before text\n\n" + first + "\nafter text\n"
	third, changed, err := spliceManagedBlock(withSurroundings, inner)
	if err != nil {
		t.Fatalf("unexpected error on third splice: %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false when re-splicing identical inner with surroundings, result: %q", third)
	}
}

func TestSoulDigest_NoEmDash(t *testing.T) {
	s := &config.Soul{
		Name:    "Ryan",
		Role:    "Engineer",
		Context: "Works on aida, an agent-of-agents CLI.",
		Family: &config.FamilyInfo{
			Kids: []config.Person{
				{Name: "Roxy", Relation: "daughter", Nickname: "Rox"},
			},
			Spouse: &config.Person{Name: "Jane"},
		},
		People: []config.Contact{
			{Name: "Alex", Aka: []string{"alex@example.com"}, Role: "coworker", Note: "Works on infra."},
		},
	}

	out := soulDigest(s)
	if out == "" {
		t.Fatalf("expected non-empty digest for populated soul")
	}
	if strings.Contains(out, " - ") {
		t.Fatalf("digest must not contain an em dash, got: %q", out)
	}
	for _, want := range []string{"Ryan", "Engineer", "Roxy", "daughter", "Jane", "spouse", "Alex", "coworker"} {
		if !strings.Contains(out, want) {
			t.Errorf("digest missing expected content %q, got: %q", want, out)
		}
	}
}

func TestSoulDigest_Empty(t *testing.T) {
	if got := soulDigest(&config.Soul{}); got != "" {
		t.Fatalf("expected empty digest for empty soul, got: %q", got)
	}
	if got := soulDigest(nil); got != "" {
		t.Fatalf("expected empty digest for nil soul, got: %q", got)
	}
}
