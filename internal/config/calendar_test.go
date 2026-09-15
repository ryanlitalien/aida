package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCalendarConfig writes ~/.aida/config.yaml under the test's HOME
// (see t.Setenv in each test) with the given raw YAML body.
func writeCalendarConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestCalendarConfig_MissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config.yaml at all

	_, err := (&Config{}).CalendarConfig()
	if !errors.Is(err, ErrNoCalendarAccounts) {
		t.Fatalf("CalendarConfig() error = %v, want ErrNoCalendarAccounts", err)
	}
}

func TestCalendarConfig_MissingCalendarBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, "active_profile: auto\n")

	_, err := (&Config{}).CalendarConfig()
	if !errors.Is(err, ErrNoCalendarAccounts) {
		t.Fatalf("CalendarConfig() error = %v, want ErrNoCalendarAccounts", err)
	}
}

func TestCalendarConfig_EmptyAccountsList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, "calendar:\n  accounts: []\n")

	_, err := (&Config{}).CalendarConfig()
	if !errors.Is(err, ErrNoCalendarAccounts) {
		t.Fatalf("CalendarConfig() error = %v, want ErrNoCalendarAccounts", err)
	}
}

func TestCalendarConfig_DefaultsFillIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, `calendar:
  accounts:
    - account: personal
      enabled: true
      calendars:
        - {id: "example@example.com", label: Personal, enabled: true}
`)

	cfg, err := (&Config{}).CalendarConfig()
	if err != nil {
		t.Fatalf("CalendarConfig() error: %v", err)
	}
	if cfg.Timezone != time.Local.String() {
		t.Errorf("Timezone = %q, want system local %q", cfg.Timezone, time.Local.String())
	}
	if cfg.MaxConcurrency != defaultCalendarMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want %d", cfg.MaxConcurrency, defaultCalendarMaxConcurrency)
	}
	if cfg.CallTimeout != defaultCalendarCallTimeout {
		t.Errorf("CallTimeout = %v, want %v", cfg.CallTimeout, defaultCalendarCallTimeout)
	}
	if cfg.MaxResults != defaultCalendarMaxResults {
		t.Errorf("MaxResults = %d, want %d", cfg.MaxResults, defaultCalendarMaxResults)
	}
	if len(cfg.Accounts) != 1 || cfg.Accounts[0].Account != "personal" {
		t.Fatalf("Accounts = %+v, want one 'personal' account", cfg.Accounts)
	}
	if len(cfg.Accounts[0].Calendars) != 1 || cfg.Accounts[0].Calendars[0].ID != "example@example.com" {
		t.Fatalf("Accounts[0].Calendars = %+v", cfg.Accounts[0].Calendars)
	}
}

func TestCalendarConfig_ExplicitValuesWin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, `calendar:
  timezone: America/New_York
  max_concurrency: 2
  call_timeout: 45s
  max_results: 50
  accounts:
    - account: personal
      label: Personal
      enabled: true
      calendars:
        - {id: "example@example.com", label: Personal, enabled: true}
        - {id: "family@group.calendar.google.com", label: Family, enabled: false}
`)

	cfg, err := (&Config{}).CalendarConfig()
	if err != nil {
		t.Fatalf("CalendarConfig() error: %v", err)
	}
	if cfg.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want America/New_York", cfg.Timezone)
	}
	if cfg.MaxConcurrency != 2 {
		t.Errorf("MaxConcurrency = %d, want 2", cfg.MaxConcurrency)
	}
	if cfg.CallTimeout != 45*time.Second {
		t.Errorf("CallTimeout = %v, want 45s", cfg.CallTimeout)
	}
	if cfg.MaxResults != 50 {
		t.Errorf("MaxResults = %d, want 50", cfg.MaxResults)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("Accounts = %+v, want 1", cfg.Accounts)
	}
	acc := cfg.Accounts[0]
	if len(acc.Calendars) != 2 {
		t.Fatalf("Calendars = %+v, want 2", acc.Calendars)
	}
	if acc.Calendars[0].Enabled != true || acc.Calendars[1].Enabled != false {
		t.Errorf("Calendars enabled flags = %v, %v, want true, false", acc.Calendars[0].Enabled, acc.Calendars[1].Enabled)
	}
}

// TestCalendarConfig_MatchTitles covers the per-calendar match_titles list:
// a calendar with the field set parses it verbatim, and a calendar that
// omits it parses to a nil/empty list, i.e. no filtering.
func TestCalendarConfig_MatchTitles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, `calendar:
  accounts:
    - account: personal
      enabled: true
      calendars:
        - {id: "example@example.com", label: Personal, enabled: true}
        - id: "shared@example.com"
          label: "Shared (kids only)"
          enabled: true
          match_titles: ["Alice", "Bob"]
`)

	cfg, err := (&Config{}).CalendarConfig()
	if err != nil {
		t.Fatalf("CalendarConfig() error: %v", err)
	}
	if len(cfg.Accounts) != 1 || len(cfg.Accounts[0].Calendars) != 2 {
		t.Fatalf("Accounts = %+v, want 1 account with 2 calendars", cfg.Accounts)
	}
	cals := cfg.Accounts[0].Calendars
	if len(cals[0].MatchTitles) != 0 {
		t.Errorf("Calendars[0].MatchTitles = %v, want none (field omitted)", cals[0].MatchTitles)
	}
	want := []string{"Alice", "Bob"}
	if len(cals[1].MatchTitles) != len(want) {
		t.Fatalf("Calendars[1].MatchTitles = %v, want %v", cals[1].MatchTitles, want)
	}
	for i, w := range want {
		if cals[1].MatchTitles[i] != w {
			t.Errorf("Calendars[1].MatchTitles[%d] = %q, want %q", i, cals[1].MatchTitles[i], w)
		}
	}
}

func TestCalendarConfig_InvalidCallTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfig(t, home, `calendar:
  call_timeout: not-a-duration
  accounts:
    - account: personal
      enabled: true
      calendars:
        - {id: "example@example.com", enabled: true}
`)

	_, err := (&Config{}).CalendarConfig()
	if err == nil {
		t.Fatal("expected an error for an invalid call_timeout, got nil")
	}
}
