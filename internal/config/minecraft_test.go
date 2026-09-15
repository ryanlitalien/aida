package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeMinecraftConfig writes ~/.aida/config.yaml under the test's HOME
// (see t.Setenv in each test) with the given raw YAML body. Mirrors
// writeCalendarConfig in calendar_test.go.
func writeMinecraftConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

func TestMinecraftConfig_MissingFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no config.yaml at all

	_, err := (&Config{}).MinecraftConfig()
	if !errors.Is(err, ErrMinecraftNotConfigured) {
		t.Fatalf("MinecraftConfig() error = %v, want ErrMinecraftNotConfigured", err)
	}
}

func TestMinecraftConfig_MissingBlock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, "active_profile: auto\n")

	_, err := (&Config{}).MinecraftConfig()
	if !errors.Is(err, ErrMinecraftNotConfigured) {
		t.Fatalf("MinecraftConfig() error = %v, want ErrMinecraftNotConfigured", err)
	}
}

// TestMinecraftConfig_PartiallyConfigured covers each half of the pair
// missing on its own -- the tool needs BOTH ssh_host and remote_script,
// so either alone must still degrade to "not configured".
func TestMinecraftConfig_PartiallyConfigured(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"host only", "minecraft:\n  ssh_host: example-host\n"},
		{"script only", "minecraft:\n  remote_script: /opt/example/ask\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeMinecraftConfig(t, home, tc.body)

			_, err := (&Config{}).MinecraftConfig()
			if !errors.Is(err, ErrMinecraftNotConfigured) {
				t.Fatalf("MinecraftConfig() error = %v, want ErrMinecraftNotConfigured", err)
			}
		})
	}
}

func TestMinecraftConfig_DefaultsFillIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
`)

	cfg, err := (&Config{}).MinecraftConfig()
	if err != nil {
		t.Fatalf("MinecraftConfig() error: %v", err)
	}
	if cfg.SSHHost != "example-host" {
		t.Errorf("SSHHost = %q, want example-host", cfg.SSHHost)
	}
	if cfg.RemoteScript != "/opt/example/scripts/ask" {
		t.Errorf("RemoteScript = %q, want /opt/example/scripts/ask", cfg.RemoteScript)
	}
	if cfg.LocalTimeout != defaultMinecraftLocalTimeout {
		t.Errorf("LocalTimeout = %v, want %v", cfg.LocalTimeout, defaultMinecraftLocalTimeout)
	}
	// The original hardcoded pair was 120s local / 115s remote -- a 5s
	// margin. Defaults must reproduce that exact relationship.
	wantRemoteSecs := int((defaultMinecraftLocalTimeout - minecraftRemoteMargin).Seconds())
	if cfg.RemoteTimeoutSecs != wantRemoteSecs {
		t.Errorf("RemoteTimeoutSecs = %d, want %d", cfg.RemoteTimeoutSecs, wantRemoteSecs)
	}
	if cfg.RemoteTimeoutSecs >= int(cfg.LocalTimeout.Seconds()) {
		t.Errorf("invariant broken: RemoteTimeoutSecs (%d) must be less than LocalTimeout (%s)", cfg.RemoteTimeoutSecs, cfg.LocalTimeout)
	}
}

// TestMinecraftConfig_ExplicitTimeoutsWin covers a caller overriding both
// timeouts, and the derived-vs-explicit remote value holding the same
// invariant either way.
func TestMinecraftConfig_ExplicitTimeoutsWin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
  local_timeout: 60s
  remote_timeout_secs: 50
`)

	cfg, err := (&Config{}).MinecraftConfig()
	if err != nil {
		t.Fatalf("MinecraftConfig() error: %v", err)
	}
	if cfg.LocalTimeout != 60*time.Second {
		t.Errorf("LocalTimeout = %v, want 60s", cfg.LocalTimeout)
	}
	if cfg.RemoteTimeoutSecs != 50 {
		t.Errorf("RemoteTimeoutSecs = %d, want 50", cfg.RemoteTimeoutSecs)
	}
}

// TestMinecraftConfig_TimeoutInvariantEnforced proves an explicit
// remote_timeout_secs that would violate "remote fires before local" is
// rejected outright rather than silently accepted.
func TestMinecraftConfig_TimeoutInvariantEnforced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
  local_timeout: 30s
  remote_timeout_secs: 30
`)

	_, err := (&Config{}).MinecraftConfig()
	if err == nil {
		t.Fatal("expected an error when remote_timeout_secs >= local_timeout, got nil")
	}
}

// TestMinecraftConfig_LocalTimeoutTooShortForDefaultMargin covers a
// local_timeout so small the derived default remote margin would be
// zero or negative -- this must fail loudly rather than produce a
// nonsensical (or negative) remote deadline.
func TestMinecraftConfig_LocalTimeoutTooShortForDefaultMargin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
  local_timeout: 3s
`)

	_, err := (&Config{}).MinecraftConfig()
	if err == nil {
		t.Fatal("expected an error when local_timeout leaves no room for the default remote margin, got nil")
	}
}

func TestMinecraftConfig_InvalidLocalTimeout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
  local_timeout: not-a-duration
`)

	_, err := (&Config{}).MinecraftConfig()
	if err == nil {
		t.Fatal("expected an error for an invalid local_timeout, got nil")
	}
}
