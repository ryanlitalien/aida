package models

import (
	"context"
	"encoding/json"
	"testing"
)

const cannedLiteLLMKeyListJSON = `{
  "keys": [
    {
      "key_alias": "minty-fleet",
      "spend": 32.81,
      "max_budget": 75.0,
      "budget_duration": "30d",
      "budget_reset_at": "2026-10-01T00:00:00Z",
      "models": ["claude-opus-5", "claude-sonnet-5"]
    },
    {
      "key_alias": "butterstack",
      "spend": 0,
      "max_budget": null,
      "budget_reset_at": "",
      "models": []
    }
  ]
}`

func TestMapLiteLLMSpend(t *testing.T) {
	var resp litellmKeyListResponse
	if err := json.Unmarshal([]byte(cannedLiteLLMKeyListJSON), &resp); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	spend := mapLiteLLMSpend(&resp)
	if len(spend) != 2 {
		t.Fatalf("len(spend) = %d, want 2", len(spend))
	}

	if spend[0].Key != "minty-fleet" {
		t.Errorf("spend[0].Key = %q, want minty-fleet", spend[0].Key)
	}
	if spend[0].Spend != 32.81 {
		t.Errorf("spend[0].Spend = %v, want 32.81", spend[0].Spend)
	}
	if spend[0].Budget != 75.0 {
		t.Errorf("spend[0].Budget = %v, want 75.0", spend[0].Budget)
	}
	if spend[0].ResetsAt.IsZero() {
		t.Error("spend[0].ResetsAt is zero, want a parsed time")
	}
	if len(spend[0].Models) != 2 {
		t.Errorf("spend[0].Models = %v, want 2 entries", spend[0].Models)
	}
	if spend[0].WindowMins != 30*24*60 {
		t.Errorf("spend[0].WindowMins = %v, want %v (30d)", spend[0].WindowMins, 30*24*60)
	}

	// A nil max_budget must map to 0, not panic, and an empty
	// budget_reset_at must map to the zero time rather than erroring.
	if spend[1].Budget != 0 {
		t.Errorf("spend[1].Budget = %v, want 0 (nil max_budget)", spend[1].Budget)
	}
	if !spend[1].ResetsAt.IsZero() {
		t.Errorf("spend[1].ResetsAt = %v, want zero", spend[1].ResetsAt)
	}
	// budget_duration is absent on this key -- WindowMins stays 0
	// ("unknown"), never guessed.
	if spend[1].WindowMins != 0 {
		t.Errorf("spend[1].WindowMins = %v, want 0 (no budget_duration)", spend[1].WindowMins)
	}
}

func TestLiteLLMBudgetDurationMins(t *testing.T) {
	tests := []struct {
		in     string
		want   float64
		wantOK bool
	}{
		{"30d", 30 * 24 * 60, true},
		{"7d", 7 * 24 * 60, true},
		{"1mo", 30 * 24 * 60, true},
		{"1y", 365 * 24 * 60, true},
		{"90m", 90, true},
		{"2h", 120, true},
		{"120s", 2, true},
		{"", 0, false},
		{"monthly", 0, false},
		{"mo30", 0, false},
	}
	for _, tt := range tests {
		got, ok := litellmBudgetDurationMins(tt.in)
		if ok != tt.wantOK {
			t.Errorf("litellmBudgetDurationMins(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("litellmBudgetDurationMins(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMapLiteLLMSpend_Nil(t *testing.T) {
	if got := mapLiteLLMSpend(nil); got != nil {
		t.Errorf("mapLiteLLMSpend(nil) = %v, want nil", got)
	}
}

func TestMapLiteLLMSpend_EmptyKeys(t *testing.T) {
	resp := &litellmKeyListResponse{Keys: nil}
	got := mapLiteLLMSpend(resp)
	if len(got) != 0 {
		t.Errorf("mapLiteLLMSpend(empty) = %v, want empty", got)
	}
}

func TestResolveLiteLLMKey_EnvVar(t *testing.T) {
	t.Setenv("AIDA_TEST_LITELLM_KEY", "sk-test-master-key")
	cfg := &LiteLLMConfig{KeyEnv: "AIDA_TEST_LITELLM_KEY"}
	key, err := resolveLiteLLMKey(context.Background(), cfg)
	if err != nil {
		t.Fatalf("resolveLiteLLMKey: %v", err)
	}
	if key != "sk-test-master-key" {
		t.Errorf("key = %q, want sk-test-master-key", key)
	}
}

func TestResolveLiteLLMKey_NoSourceConfigured(t *testing.T) {
	cfg := &LiteLLMConfig{}
	_, err := resolveLiteLLMKey(context.Background(), cfg)
	if err == nil {
		t.Error("expected an error when neither KeyEnv nor an ssh fallback is configured")
	}
}

func TestResolveLiteLLMKey_NilConfig(t *testing.T) {
	_, err := resolveLiteLLMKey(context.Background(), nil)
	if err == nil {
		t.Error("expected an error for a nil LiteLLMConfig")
	}
}
