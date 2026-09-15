package models

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const cannedClaudeUsageJSON = `{
  "limits": [
    {
      "kind": "session",
      "group": "all_models",
      "percent": 15.5,
      "severity": "normal",
      "resets_at": "2026-09-08T18:00:00Z",
      "is_active": true
    },
    {
      "kind": "weekly_all",
      "group": "all_models",
      "percent": 42,
      "severity": "warning",
      "resets_at": "2026-09-12T00:00:00Z",
      "is_active": false
    },
    {
      "kind": "weekly_scoped",
      "group": "fable",
      "percent": 91,
      "severity": "critical",
      "resets_at": "2026-09-12T00:00:00Z",
      "scope": {"model": {"display_name": "Fable"}},
      "is_active": true
    }
  ],
  "five_hour": {"utilization": 0.155, "resets_at": "2026-09-08T18:00:00Z"},
  "seven_day": {"utilization": 0.42, "resets_at": "2026-09-12T00:00:00Z"}
}`

func TestMapClaudeUsage(t *testing.T) {
	var resp claudeUsageResponse
	if err := json.Unmarshal([]byte(cannedClaudeUsageJSON), &resp); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	bars := mapClaudeUsage(&resp)
	if len(bars) != 3 {
		t.Fatalf("len(bars) = %d, want 3", len(bars))
	}

	if bars[0].Label != "5-hour" {
		t.Errorf("bars[0].Label = %q, want 5-hour", bars[0].Label)
	}
	if bars[0].Percent != 15.5 {
		t.Errorf("bars[0].Percent = %v, want 15.5", bars[0].Percent)
	}
	if bars[0].Severity != SeverityNormal {
		t.Errorf("bars[0].Severity = %q, want %q", bars[0].Severity, SeverityNormal)
	}
	if !bars[0].Active {
		t.Error("bars[0].Active = false, want true")
	}
	if bars[0].WindowMins != 300 {
		t.Errorf("bars[0].WindowMins = %v, want 300 (5-hour)", bars[0].WindowMins)
	}
	wantReset, _ := time.Parse(time.RFC3339, "2026-09-08T18:00:00Z")
	if !bars[0].ResetsAt.Equal(wantReset) {
		t.Errorf("bars[0].ResetsAt = %v, want %v", bars[0].ResetsAt, wantReset)
	}

	if bars[1].Label != "7-day" {
		t.Errorf("bars[1].Label = %q, want 7-day", bars[1].Label)
	}
	if bars[1].Severity != SeverityWarning {
		t.Errorf("bars[1].Severity = %q, want %q", bars[1].Severity, SeverityWarning)
	}
	if bars[1].WindowMins != 10080 {
		t.Errorf("bars[1].WindowMins = %v, want 10080 (7-day)", bars[1].WindowMins)
	}

	if bars[2].Label != "7-day Fable" {
		t.Errorf("bars[2].Label = %q, want \"7-day Fable\"", bars[2].Label)
	}
	if bars[2].Severity != SeverityCritical {
		t.Errorf("bars[2].Severity = %q, want %q", bars[2].Severity, SeverityCritical)
	}
	if bars[2].WindowMins != 10080 {
		t.Errorf("bars[2].WindowMins = %v, want 10080 (7-day, model-scoped)", bars[2].WindowMins)
	}
}

func TestMapClaudeUsage_Nil(t *testing.T) {
	if got := mapClaudeUsage(nil); got != nil {
		t.Errorf("mapClaudeUsage(nil) = %v, want nil", got)
	}
}

func TestClaudeBarLabel(t *testing.T) {
	tests := []struct {
		kind, scope, want string
	}{
		{"session", "", "5-hour"},
		{"weekly_all", "", "7-day"},
		{"weekly_scoped", "Fable", "7-day Fable"},
		{"weekly_scoped", "", "7-day"},
		{"some_future_kind", "", "some_future_kind"},
	}
	for _, tt := range tests {
		if got := claudeBarLabel(tt.kind, tt.scope); got != tt.want {
			t.Errorf("claudeBarLabel(%q, %q) = %q, want %q", tt.kind, tt.scope, got, tt.want)
		}
	}
}

func TestClaudeBarWindowMins(t *testing.T) {
	tests := []struct {
		kind string
		want float64
	}{
		{"session", 300},
		{"weekly_all", 10080},
		{"weekly_scoped", 10080},
		{"some_future_kind", 0},
	}
	for _, tt := range tests {
		if got := claudeBarWindowMins(tt.kind); got != tt.want {
			t.Errorf("claudeBarWindowMins(%q) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

func TestReadClaudeCredentialsFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	const fixture = `{"claudeAiOauth":{"accessToken":"sk-test-token","subscriptionType":"pro","rateLimitTier":"tier1"}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	creds, err := readClaudeCredentialsFromFile(path)
	if err != nil {
		t.Fatalf("readClaudeCredentialsFromFile: %v", err)
	}
	if creds.ClaudeAiOauth.AccessToken != "sk-test-token" {
		t.Errorf("AccessToken = %q, want sk-test-token", creds.ClaudeAiOauth.AccessToken)
	}
	if creds.ClaudeAiOauth.SubscriptionType != "pro" {
		t.Errorf("SubscriptionType = %q, want pro", creds.ClaudeAiOauth.SubscriptionType)
	}
	if creds.ClaudeAiOauth.RateLimitTier != "tier1" {
		t.Errorf("RateLimitTier = %q, want tier1", creds.ClaudeAiOauth.RateLimitTier)
	}
}

func TestReadClaudeCredentialsFromFile_Missing(t *testing.T) {
	_, err := readClaudeCredentialsFromFile(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("expected an error for a missing credentials file, got nil")
	}
}

func TestLoadClaudeCredentials_FileOverridesKeychain(t *testing.T) {
	// A ClaudeConfig with CredentialsFile set (and no SSHHost) must read
	// the local file, never touch the keychain -- there's no keychain in
	// the CI/test sandbox, so this would fail loudly if it fell through.
	path := filepath.Join(t.TempDir(), "credentials.json")
	const fixture = `{"claudeAiOauth":{"accessToken":"sk-file-token"}}`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	creds, err := loadClaudeCredentials(context.Background(), &ClaudeConfig{CredentialsFile: path})
	if err != nil {
		t.Fatalf("loadClaudeCredentials: %v", err)
	}
	if creds.ClaudeAiOauth.AccessToken != "sk-file-token" {
		t.Errorf("AccessToken = %q, want sk-file-token", creds.ClaudeAiOauth.AccessToken)
	}
}
