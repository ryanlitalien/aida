package models

import "testing"

// cannedAgyQuotaText is the exact output captured from
// `agy --print /quota --output-format text` on 2026-09-08.
const cannedAgyQuotaText = `Gemini Models	Weekly Limit Remaining	96%	2026-09-10T20:57:08Z
Gemini Models	Five Hour Limit Remaining	86%	2026-09-08T18:20:12Z
Claude and GPT models	Weekly Limit Remaining	100%	2026-09-15T14:48:17Z
Claude and GPT models	Five Hour Limit Remaining	100%	2026-09-08T19:48:17Z
`

// cannedAgyModelsText is the exact output captured from `agy models` on
// 2026-09-08.
const cannedAgyModelsText = `Fetching available models...
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
gemini-3.7-flash-high	Gemini 3.7 Flash (High)
gemini-3.7-flash-medium	Gemini 3.7 Flash (Medium)
gemini-3.7-flash-low	Gemini 3.7 Flash (Low)
gemini-3.6-flash-high	Gemini 3.6 Flash (High)
gemini-3.6-flash-medium	Gemini 3.6 Flash (Medium)
gemini-3.6-flash-low	Gemini 3.6 Flash (Low)
gemini-3.1-pro-high	Gemini 3.1 Pro (High)
gemini-3.1-pro-low	Gemini 3.1 Pro (Low)
claude-sonnet-4-6	Claude Sonnet 4.6 (Thinking)
claude-opus-4-6-thinking	Claude Opus 4.6 (Thinking)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)
`

// agyRosterKnownIDs mirrors the google provider's models[].id list this
// task installs into ~/.aida/models.yaml -- kept here as a literal so
// TestAgyModelsUnlisted_RealRosterHasNoUnlisted doesn't depend on that
// external file.
func agyRosterKnownIDs() map[string]bool {
	return map[string]bool{
		"gemini-3.8-flash":         true,
		"gemini-3.7-flash":         true,
		"gemini-3.6-flash":         true,
		"gemini-3.1-pro":           true,
		"claude-sonnet-4-6":        true,
		"claude-opus-4-6-thinking": true,
		"gpt-oss-120b-medium":      true,
		"gemini-image":             true,
	}
}

func TestParseAgyQuotaLine(t *testing.T) {
	row, ok := parseAgyQuotaLine("Gemini Models\tFive Hour Limit Remaining\t86%\t2026-09-08T18:20:12Z")
	if !ok {
		t.Fatal("parseAgyQuotaLine returned ok=false, want true")
	}
	if row.Group != "Gemini Models" {
		t.Errorf("Group = %q, want %q", row.Group, "Gemini Models")
	}
	if row.Window != "Five Hour Limit Remaining" {
		t.Errorf("Window = %q, want %q", row.Window, "Five Hour Limit Remaining")
	}
	if row.Remaining != 86 {
		t.Errorf("Remaining = %v, want 86", row.Remaining)
	}
	if row.ResetsAt.IsZero() {
		t.Error("ResetsAt is zero, want a parsed time")
	}
}

func TestParseAgyQuotaLine_Malformed(t *testing.T) {
	tests := []string{
		"",
		"only\tthree\tfields",
		"Gemini Models\tFive Hour Limit Remaining\tnot-a-percent\t2026-09-08T18:20:12Z",
	}
	for _, line := range tests {
		if _, ok := parseAgyQuotaLine(line); ok {
			t.Errorf("parseAgyQuotaLine(%q) ok=true, want false", line)
		}
	}
}

func TestParseAgyQuotaText(t *testing.T) {
	rows := parseAgyQuotaText(cannedAgyQuotaText)
	if len(rows) != 4 {
		t.Fatalf("len(rows) = %d, want 4", len(rows))
	}
	if rows[0].Group != "Gemini Models" || rows[0].Window != "Weekly Limit Remaining" || rows[0].Remaining != 96 {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[3].Group != "Claude and GPT models" || rows[3].Remaining != 100 {
		t.Errorf("rows[3] = %+v", rows[3])
	}
}

func TestMapAgyQuotaBars(t *testing.T) {
	rows := parseAgyQuotaText(cannedAgyQuotaText)
	bars := mapAgyQuotaBars(rows)
	if len(bars) != 4 {
		t.Fatalf("len(bars) = %d, want 4", len(bars))
	}

	byLabel := map[string]Bar{}
	for _, b := range bars {
		byLabel[b.Label] = b
	}

	want := map[string]float64{
		"5-hour Gemini":     14, // 100 - 86
		"7-day Gemini":      4,  // 100 - 96
		"5-hour Claude/GPT": 0,  // 100 - 100
		"7-day Claude/GPT":  0,  // 100 - 100
	}
	wantWindowMins := map[string]float64{
		"5-hour Gemini":     300,
		"7-day Gemini":      10080,
		"5-hour Claude/GPT": 300,
		"7-day Claude/GPT":  10080,
	}
	for label, wantPct := range want {
		b, ok := byLabel[label]
		if !ok {
			t.Errorf("no bar labeled %q; got labels %v", label, labelsOf(bars))
			continue
		}
		if b.Percent != wantPct {
			t.Errorf("bar %q Percent = %v, want %v (used = 100 - remaining)", label, b.Percent, wantPct)
		}
		if b.WindowMins != wantWindowMins[label] {
			t.Errorf("bar %q WindowMins = %v, want %v", label, b.WindowMins, wantWindowMins[label])
		}
	}

	// 5-hour must sort before 7-day within a group.
	gem5h, gem7d := -1, -1
	for i, b := range bars {
		switch b.Label {
		case "5-hour Gemini":
			gem5h = i
		case "7-day Gemini":
			gem7d = i
		}
	}
	if gem5h < 0 || gem7d < 0 || gem5h > gem7d {
		t.Errorf("expected 5-hour Gemini before 7-day Gemini, got order %v", labelsOf(bars))
	}
}

func labelsOf(bars []Bar) []string {
	out := make([]string, len(bars))
	for i, b := range bars {
		out[i] = b.Label
	}
	return out
}

func TestMapAgyQuotaBars_Empty(t *testing.T) {
	if got := mapAgyQuotaBars(nil); got != nil {
		t.Errorf("mapAgyQuotaBars(nil) = %v, want nil", got)
	}
}

func TestAgyGroupLabel(t *testing.T) {
	tests := map[string]string{
		"Gemini Models":         "Gemini",
		"Claude and GPT models": "Claude/GPT",
		"Something New Models":  "Something New Models",
	}
	for in, want := range tests {
		if got := agyGroupLabel(in); got != want {
			t.Errorf("agyGroupLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgyWindowLabel(t *testing.T) {
	tests := map[string]string{
		"Five Hour Limit Remaining": "5-hour",
		"Weekly Limit Remaining":    "7-day",
		"Something Else":            "Something Else",
	}
	for in, want := range tests {
		if got := agyWindowLabel(in); got != want {
			t.Errorf("agyWindowLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgyWindowMins(t *testing.T) {
	tests := map[string]float64{
		"Five Hour Limit Remaining": 300,
		"Weekly Limit Remaining":    10080,
		"Something Else":            0,
	}
	for in, want := range tests {
		if got := agyWindowMins(in); got != want {
			t.Errorf("agyWindowMins(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseAgyModelsOutput(t *testing.T) {
	entries := parseAgyModelsOutput(cannedAgyModelsText)
	if len(entries) != 14 {
		t.Fatalf("len(entries) = %d, want 14", len(entries))
	}
	if entries[0].ID != "gemini-3.8-flash-high" || entries[0].DisplayName != "Gemini 3.8 Flash (High)" {
		t.Errorf("entries[0] = %+v", entries[0])
	}
	last := entries[len(entries)-1]
	if last.ID != "gpt-oss-120b-medium" || last.DisplayName != "GPT-OSS 120B (Medium)" {
		t.Errorf("last entry = %+v", last)
	}
}

func TestAgyModelBaseID(t *testing.T) {
	tests := map[string]string{
		"gemini-3.8-flash-high":    "gemini-3.8-flash",
		"gemini-3.8-flash-medium":  "gemini-3.8-flash",
		"gemini-3.1-pro-low":       "gemini-3.1-pro",
		"claude-sonnet-4-6":        "claude-sonnet-4-6",
		"claude-opus-4-6-thinking": "claude-opus-4-6-thinking",
		"gpt-oss-120b-medium":      "gpt-oss-120b",
	}
	for in, want := range tests {
		if got := agyModelBaseID(in); got != want {
			t.Errorf("agyModelBaseID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAgyModelsUnlisted_RealRosterHasNoUnlisted pins the exact
// requirement from the roster edit this task makes: with the google
// provider's models[] storing base ids for the gemini flash/pro family
// (effort variants folded into the note) and literal ids for the three
// "via Antigravity" Claude/GPT models, a real `agy models` listing must
// diff clean.
func TestAgyModelsUnlisted_RealRosterHasNoUnlisted(t *testing.T) {
	got := agyModelsUnlisted(cannedAgyModelsText, agyRosterKnownIDs())
	if got != "" {
		t.Errorf("agyModelsUnlisted = %q, want empty (every agy model id should resolve to a known base or literal id)", got)
	}
}

func TestAgyModelsUnlisted_FlagsARealNewModel(t *testing.T) {
	text := cannedAgyModelsText + "gemini-4.0-ultra-high\tGemini 4.0 Ultra (High)\n"
	got := agyModelsUnlisted(text, agyRosterKnownIDs())
	if got != "gemini-4.0-ultra-high" {
		t.Errorf("agyModelsUnlisted = %q, want gemini-4.0-ultra-high", got)
	}
}

func TestAgyModelsUnlisted_Empty(t *testing.T) {
	if got := agyModelsUnlisted("", agyRosterKnownIDs()); got != "" {
		t.Errorf("agyModelsUnlisted(\"\", ...) = %q, want empty", got)
	}
}
