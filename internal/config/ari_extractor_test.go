package config

import (
	"testing"
)

// fictionalFacilityAccounts is a made-up markdown table in the same shape
// a real source doc might use: a per-environment list of resource IDs
// for named facilities, split into two sections. Standing in for what
// used to be a hardcoded "partner vs merchant" scheme, this is now just
// data driven by the IDPattern under test.
const fictionalFacilityAccounts = `
**Avengers Facility Accounts (6):**
| Environment | ID | Name |
|---|---|---|
| US prod-live | HBRPOIG8F1CBFNO6 | Avengers Compound |
| US prod-live | B9M80O2RAK1VRJNV | [TEST] Avengers Compound |
| US stage-live | GFYGWWQC38HYF9SX | [TEST] Avengers Compound |
| CA prod-live | MECOSFOGYR3XKXWN | Avengers Compound CA |
| CA prod-live | REK8PK3YR9OUDOCU | [TEST] Avengers Compound CA |
| CA stage-live | ZRENUN5Z3JQIP98Q | [TEST] Avengers Compound CA |

**SHIELD Facility Accounts (4):**
| Environment | ID | Name |
|---|---|---|
| US prod-live | 1ZXOI65FDHJK1EYY | SHIELD Helicarrier |
| US prod-live | 37Q9AH8RVHS1K3AQ | [TEST] SHIELD Helicarrier |
| CA prod-live | 6L6GT6MJXK87AU5B | SHIELD Helicarrier CA |
| CA prod-live | HXTPDPFF5E8II49K | [TEST] SHIELD Helicarrier CA |
`

// fictionalFacilityIDPattern is a hand-authored IDPattern equivalent to
// what a source might configure under `id_patterns:` in config.yaml: a
// 16-char alphanumeric ID, categorized "avengers_id" vs "shield_id" by a
// context keyword, with test/stage/region modifiers composed on top.
func fictionalFacilityIDPattern() IDPattern {
	return IDPattern{
		Name:  "avengers_id",
		Regex: `\b[A-Z0-9]{16}\b`,
		Categories: []IDCategory{
			{Key: "shield_id", Match: []string{"shield"}},
		},
		Modifiers: []IDModifier{
			{Suffix: "test", Match: []string{"[test]"}},
			{Suffix: "ca", Match: []string{" ca ", "canada"}},
		},
	}
}

func TestExtractIDsFromText_CategoriesAndModifiers(t *testing.T) {
	existing := map[string]string{
		"avengers_id": "HBRPOIG8F1CBFNO6",
	}

	results, err := ExtractIDsFromText(fictionalFacilityAccounts, existing, []IDPattern{fictionalFacilityIDPattern()})
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}

	// Should find 9 new IDs (the primary avengers_id is already known).
	if len(results) != 9 {
		t.Fatalf("expected 9 new IDs, got %d", len(results))
	}

	byValue := make(map[string]ExtractedID)
	for _, r := range results {
		byValue[r.Value] = r
	}

	tests := []struct {
		value       string
		wantKeyBase string // the base key (before collision resolution)
		wantLabel   string
	}{
		{"B9M80O2RAK1VRJNV", "avengers_id_test", "[TEST] Avengers Compound"},
		{"GFYGWWQC38HYF9SX", "avengers_id_test", "[TEST] Avengers Compound"},
		{"MECOSFOGYR3XKXWN", "avengers_id_ca", "Avengers Compound CA"},
		{"REK8PK3YR9OUDOCU", "avengers_id_test_ca", "[TEST] Avengers Compound CA"},
		{"ZRENUN5Z3JQIP98Q", "avengers_id_test_ca", "[TEST] Avengers Compound CA"},
		{"1ZXOI65FDHJK1EYY", "shield_id", "SHIELD Helicarrier"},
		{"37Q9AH8RVHS1K3AQ", "shield_id_test", "[TEST] SHIELD Helicarrier"},
		{"6L6GT6MJXK87AU5B", "shield_id_ca", "SHIELD Helicarrier CA"},
		{"HXTPDPFF5E8II49K", "shield_id_test_ca", "[TEST] SHIELD Helicarrier CA"},
	}

	for _, tt := range tests {
		r, ok := byValue[tt.value]
		if !ok {
			t.Errorf("ID %s not found in results", tt.value)
			continue
		}
		if r.RawLabel != tt.wantLabel {
			t.Errorf("ID %s: label = %q, want %q", tt.value, r.RawLabel, tt.wantLabel)
		}
		if len(r.InferredKey) < len(tt.wantKeyBase) || r.InferredKey[:len(tt.wantKeyBase)] != tt.wantKeyBase {
			t.Errorf("ID %s: key = %q, want prefix %q", tt.value, r.InferredKey, tt.wantKeyBase)
		}
	}
}

func TestExtractIDsFromText_SkipsExisting(t *testing.T) {
	text := "The ID is HBRPOIG8F1CBFNO6 and also B9M80O2RAK1VRJNV."
	existing := map[string]string{
		"avengers_id":      "HBRPOIG8F1CBFNO6",
		"avengers_id_test": "B9M80O2RAK1VRJNV",
	}

	results, err := ExtractIDsFromText(text, existing, []IDPattern{fictionalFacilityIDPattern()})
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 new IDs (all already known), got %d", len(results))
	}
}

func TestExtractIDsFromText_InlineText(t *testing.T) {
	// When IDs are on separate lines, context is unambiguous.
	text := "The Avengers ID is A1B2C3D4E5F6G7H8.\nThe SHIELD ID for CA is I9J0K1L2M3N4O5P6."
	results, err := ExtractIDsFromText(text, nil, []IDPattern{fictionalFacilityIDPattern()})
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(results))
	}

	byValue := make(map[string]ExtractedID)
	for _, r := range results {
		byValue[r.Value] = r
	}

	r1 := byValue["A1B2C3D4E5F6G7H8"]
	if r1.InferredKey != "avengers_id" {
		t.Errorf("expected avengers_id, got %q", r1.InferredKey)
	}

	r2 := byValue["I9J0K1L2M3N4O5P6"]
	if r2.InferredKey != "shield_id_ca" {
		t.Errorf("expected shield_id_ca, got %q", r2.InferredKey)
	}
}

func TestExtractIDsFromText_CollisionResolution(t *testing.T) {
	text := `
| US prod-live | AAAA1111BBBB2222 | Facility A |
| US prod-live | CCCC3333DDDD4444 | Facility B |
`
	results, err := ExtractIDsFromText(text, nil, []IDPattern{fictionalFacilityIDPattern()})
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(results))
	}

	// Both would infer avengers_id, but collision resolution should differentiate them.
	keys := make(map[string]bool)
	for _, r := range results {
		if keys[r.InferredKey] {
			t.Errorf("duplicate key %q - collision resolution failed", r.InferredKey)
		}
		keys[r.InferredKey] = true
	}
}

func TestExtractIDsFromText_EmptyInput(t *testing.T) {
	results, err := ExtractIDsFromText("", nil, nil)
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 IDs from empty input, got %d", len(results))
	}
}

func TestExtractIDsFromText_NoIDs(t *testing.T) {
	results, err := ExtractIDsFromText("This is just some text with no identifiers.", nil, nil)
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 IDs, got %d", len(results))
	}
}

func TestExtractIDsFromText_DefaultPatternUsedWhenNoneConfigured(t *testing.T) {
	// No patterns passed -> falls back to DefaultIDPatterns, which infers
	// the plain key "id" (no category/modifier scheme) for every match.
	text := "Two ids here: AAAA1111BBBB2222 and CCCC3333DDDD4444."
	results, err := ExtractIDsFromText(text, nil, nil)
	if err != nil {
		t.Fatalf("ExtractIDsFromText: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(results))
	}
	for _, r := range results {
		if r.InferredKey != "id" && r.InferredKey != "id_2" {
			t.Errorf("unexpected inferred key %q for default pattern", r.InferredKey)
		}
	}
}

func TestExtractIDsFromText_InvalidRegexErrors(t *testing.T) {
	_, err := ExtractIDsFromText("anything", nil, []IDPattern{{Name: "bad", Regex: "(["}})
	if err == nil {
		t.Fatal("expected an error for an invalid regex, got nil")
	}
}
