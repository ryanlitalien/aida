package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAgentBoundsConfig writes ~/.aida/config.yaml under the test's HOME
// (see t.Setenv in each test) with the given raw YAML body. Mirrors
// writeCalendarConfig in calendar_test.go.
func writeAgentBoundsConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestAgentMaxWallClock_MissingFileUsesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config.yaml at all

	got := (&Config{}).AgentMaxWallClock()
	if got != defaultAgentMaxWallClock {
		t.Errorf("AgentMaxWallClock() = %v, want default %v", got, defaultAgentMaxWallClock)
	}
}

func TestAgentMaxWallClock_NoAgentBoundsBlockUsesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentBoundsConfig(t, home, "active_profile: auto\n")

	got := (&Config{}).AgentMaxWallClock()
	if got != defaultAgentMaxWallClock {
		t.Errorf("AgentMaxWallClock() = %v, want default %v", got, defaultAgentMaxWallClock)
	}
}

func TestAgentMaxWallClock_ExplicitValueWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentBoundsConfig(t, home, "agent_bounds:\n  max_wall_clock: 45m\n")

	got := (&Config{}).AgentMaxWallClock()
	if got != 45*time.Minute {
		t.Errorf("AgentMaxWallClock() = %v, want 45m", got)
	}
}

// TestAgentMaxWallClock_InvalidValueFallsBackToDefault covers the "a bad
// bound is not a reason to leave the run unbounded" rule: an unparsable
// duration string falls back to the default instead of erroring out.
func TestAgentMaxWallClock_InvalidValueFallsBackToDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentBoundsConfig(t, home, "agent_bounds:\n  max_wall_clock: not-a-duration\n")

	got := (&Config{}).AgentMaxWallClock()
	if got != defaultAgentMaxWallClock {
		t.Errorf("AgentMaxWallClock() = %v, want default %v on an invalid value", got, defaultAgentMaxWallClock)
	}
}

// TestAgentMaxWallClock_ZeroOrNegativeFallsBackToDefault covers a
// configured "0s" or negative duration -- neither is a sane bound, so
// both fall back rather than producing an already-expired deadline.
func TestAgentMaxWallClock_ZeroOrNegativeFallsBackToDefault(t *testing.T) {
	for _, v := range []string{"0s", "-5m"} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeAgentBoundsConfig(t, home, "agent_bounds:\n  max_wall_clock: "+v+"\n")

		got := (&Config{}).AgentMaxWallClock()
		if got != defaultAgentMaxWallClock {
			t.Errorf("AgentMaxWallClock() with max_wall_clock: %s = %v, want default %v", v, got, defaultAgentMaxWallClock)
		}
	}
}

func TestClaudeConfigDirForProfile_NoEntryReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentBoundsConfig(t, home, `agent_bounds:
  claude_config_dir_by_profile:
    work: ~/work-claude-config
`)

	if got := (&Config{}).ClaudeConfigDirForProfile("home"); got != "" {
		t.Errorf("ClaudeConfigDirForProfile(home) = %q, want empty (no entry -> inherit default)", got)
	}
	if got := (&Config{}).ClaudeConfigDirForProfile(""); got != "" {
		t.Errorf("ClaudeConfigDirForProfile(\"\") = %q, want empty", got)
	}
}

func TestClaudeConfigDirForProfile_MatchedProfileExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeAgentBoundsConfig(t, home, `agent_bounds:
  claude_config_dir_by_profile:
    work: ~/work-claude-config
`)

	want := filepath.Join(home, "work-claude-config")
	if got := (&Config{}).ClaudeConfigDirForProfile("work"); got != want {
		t.Errorf("ClaudeConfigDirForProfile(work) = %q, want %q", got, want)
	}
}

func TestClaudeConfigDirForProfile_MissingFileReturnsEmpty(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config.yaml at all

	if got := (&Config{}).ClaudeConfigDirForProfile("work"); got != "" {
		t.Errorf("ClaudeConfigDirForProfile(work) = %q, want empty with no config.yaml", got)
	}
}
