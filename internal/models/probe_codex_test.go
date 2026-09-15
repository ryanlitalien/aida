package models

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cannedRolloutLine is one JSONL event line as it would appear in a
// ~/.codex/sessions/**/rollout-*.jsonl file, with the rate_limits object
// nested inside a "payload" wrapper (matching the "generically walk for
// a rate_limits key at any depth" contract, not a fixed top-level shape).
const cannedRolloutLine = `{"type":"event","timestamp":"2026-09-08T12:00:00Z","payload":{"rate_limits":{"primary":{"used_percent":42.5,"window_minutes":300,"resets_at":1780000000},"secondary":{"used_percent":91.2,"window_minutes":10080,"resets_at":1780500000},"credits":{"balance":0},"plan_type":"plus"}}}`

func TestParseCodexRateLimitsLine(t *testing.T) {
	rl, err := parseCodexRateLimitsLine(cannedRolloutLine)
	if err != nil {
		t.Fatalf("parseCodexRateLimitsLine: %v", err)
	}
	if rl == nil {
		t.Fatal("rl is nil, want a parsed rate_limits object")
	}
	if rl.Primary == nil {
		t.Fatal("rl.Primary is nil")
	}
	if rl.Primary.UsedPercent != 42.5 {
		t.Errorf("Primary.UsedPercent = %v, want 42.5", rl.Primary.UsedPercent)
	}
	if rl.Primary.WindowMinutes != 300 {
		t.Errorf("Primary.WindowMinutes = %v, want 300", rl.Primary.WindowMinutes)
	}
	if rl.Secondary == nil {
		t.Fatal("rl.Secondary is nil")
	}
	if rl.Secondary.UsedPercent != 91.2 {
		t.Errorf("Secondary.UsedPercent = %v, want 91.2", rl.Secondary.UsedPercent)
	}
	if rl.PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", rl.PlanType)
	}
	if rl.Credits == nil {
		t.Fatal("rl.Credits is nil")
	}
}

func TestParseCodexRateLimitsLine_NoKey(t *testing.T) {
	rl, err := parseCodexRateLimitsLine(`{"type":"event","payload":{"other":"stuff"}}`)
	if err != nil {
		t.Fatalf("parseCodexRateLimitsLine: %v", err)
	}
	if rl != nil {
		t.Errorf("rl = %+v, want nil (no rate_limits key on this line)", rl)
	}
}

func TestParseCodexRateLimitsLine_MalformedJSON(t *testing.T) {
	if _, err := parseCodexRateLimitsLine(`not json at all`); err == nil {
		t.Error("expected an error for a malformed line, got nil")
	}
}

func TestMapCodexBars(t *testing.T) {
	rl, err := parseCodexRateLimitsLine(cannedRolloutLine)
	if err != nil {
		t.Fatalf("parseCodexRateLimitsLine: %v", err)
	}
	bars := mapCodexBars(rl)
	if len(bars) != 2 {
		t.Fatalf("len(bars) = %d, want 2", len(bars))
	}
	if bars[0].Label != "5-hour" {
		t.Errorf("bars[0].Label = %q, want 5-hour", bars[0].Label)
	}
	if bars[0].Severity != SeverityNormal {
		t.Errorf("bars[0].Severity = %q, want %q (42.5%%)", bars[0].Severity, SeverityNormal)
	}
	if bars[0].WindowMins != 300 {
		t.Errorf("bars[0].WindowMins = %v, want 300", bars[0].WindowMins)
	}
	if bars[1].Label != "7-day" {
		t.Errorf("bars[1].Label = %q, want 7-day", bars[1].Label)
	}
	if bars[1].Severity != SeverityCritical {
		t.Errorf("bars[1].Severity = %q, want %q (91.2%%)", bars[1].Severity, SeverityCritical)
	}
	if bars[1].WindowMins != 10080 {
		t.Errorf("bars[1].WindowMins = %v, want 10080", bars[1].WindowMins)
	}
	if bars[0].ResetsAt.Unix() != 1780000000 {
		t.Errorf("bars[0].ResetsAt.Unix() = %d, want 1780000000", bars[0].ResetsAt.Unix())
	}
}

func TestMapCodexBars_Nil(t *testing.T) {
	if got := mapCodexBars(nil); got != nil {
		t.Errorf("mapCodexBars(nil) = %v, want nil", got)
	}
}

func TestCodexBarLabel(t *testing.T) {
	tests := []struct {
		minutes   float64
		isPrimary bool
		want      string
	}{
		{300, true, "5-hour"},
		{10080, false, "7-day"},
		{60, true, "1h"},
		{2880, false, "2d"},
		{0, true, "window"},
	}
	for _, tt := range tests {
		if got := codexBarLabel(tt.minutes, tt.isPrimary); got != tt.want {
			t.Errorf("codexBarLabel(%v, %v) = %q, want %q", tt.minutes, tt.isPrimary, got, tt.want)
		}
	}
}

func TestDecodeJWTPayload(t *testing.T) {
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": "plus",
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	seg := base64.RawURLEncoding.EncodeToString(payloadJSON)
	token := "header." + seg + ".signature"

	got, err := decodeJWTPayload(token)
	if err != nil {
		t.Fatalf("decodeJWTPayload: %v", err)
	}
	authClaim, _ := got["https://api.openai.com/auth"].(map[string]any)
	if authClaim == nil {
		t.Fatal("missing auth claim")
	}
	if authClaim["chatgpt_plan_type"] != "plus" {
		t.Errorf("chatgpt_plan_type = %v, want plus", authClaim["chatgpt_plan_type"])
	}
}

func TestReadCodexPlan(t *testing.T) {
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": "pro",
		},
	}
	payloadJSON, _ := json.Marshal(payload)
	seg := base64.RawURLEncoding.EncodeToString(payloadJSON)
	token := "header." + seg + ".signature"

	authPath := filepath.Join(t.TempDir(), "auth.json")
	authFixture, _ := json.Marshal(codexAuthFile{IDToken: token})
	if err := os.WriteFile(authPath, authFixture, 0600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}

	plan, err := readCodexPlan(authPath)
	if err != nil {
		t.Fatalf("readCodexPlan: %v", err)
	}
	if plan != "pro" {
		t.Errorf("plan = %q, want pro", plan)
	}
}

func TestReadCodexPlan_MissingFile(t *testing.T) {
	if _, err := readCodexPlan(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error for a missing auth.json, got nil")
	}
}

func TestReadCodexConfigDetail(t *testing.T) {
	const configToml = `model = "gpt-6-astra"
model_reasoning_effort = "high"

[profiles.astra]
model = "gpt-6-astra"
model_reasoning_effort = "medium"
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(configToml), 0644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	detail := readCodexConfigDetail(path)
	if detail["default_model"] != "gpt-6-astra" {
		t.Errorf("default_model = %q, want gpt-6-astra", detail["default_model"])
	}
	if detail["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %q, want high (top-level, not the [profiles.astra] override)", detail["reasoning_effort"])
	}
}

func TestReadCodexConfigDetail_Missing(t *testing.T) {
	detail := readCodexConfigDetail(filepath.Join(t.TempDir(), "nope.toml"))
	if len(detail) != 0 {
		t.Errorf("detail = %v, want empty for a missing file", detail)
	}
}

func TestUnlistedCodexModels(t *testing.T) {
	cache := codexModelsCache{Models: []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	}{
		{Slug: "gpt-6-astra", DisplayName: "Astra"},
		{Slug: "gpt-7-new-thing", DisplayName: "New Thing"},
	}}
	data, _ := json.Marshal(cache)
	path := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write models_cache.json: %v", err)
	}

	known := map[string]bool{"gpt-6-astra": true}
	got := unlistedCodexModels(path, known)
	if got != "gpt-7-new-thing" {
		t.Errorf("unlistedCodexModels = %q, want gpt-7-new-thing", got)
	}
}

func TestFindKeyAnyDepth(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(`{"a":{"b":[{"c":1},{"target":"found"}]}}`), &v); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	got, ok := findKeyAnyDepth(v, "target")
	if !ok {
		t.Fatal("findKeyAnyDepth did not find the key")
	}
	if got != "found" {
		t.Errorf("found value = %v, want \"found\"", got)
	}

	_, ok = findKeyAnyDepth(v, "does-not-exist")
	if ok {
		t.Error("findKeyAnyDepth found a key that isn't there")
	}
}
