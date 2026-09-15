package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Model.Primary != "claude-haiku-4-5-20251001" {
		t.Errorf("expected primary model claude-haiku-4-5-20251001, got %s", cfg.Model.Primary)
	}
	if cfg.ActiveProfile != "auto" {
		t.Errorf("expected active_profile 'auto', got %s", cfg.ActiveProfile)
	}
	if _, ok := cfg.Profiles["home"]; !ok {
		t.Error("expected 'home' profile to exist")
	}
	if cfg.API.AnthropicKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("expected API key env 'ANTHROPIC_API_KEY', got %s", cfg.API.AnthropicKeyEnv)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	cfg := DefaultConfig()

	// Marshal
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Unmarshal
	var cfg2 Config
	if err := yaml.Unmarshal(data, &cfg2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	// Verify round-trip
	if cfg2.Model.Primary != cfg.Model.Primary {
		t.Errorf("primary model mismatch: %s vs %s", cfg2.Model.Primary, cfg.Model.Primary)
	}
	if cfg2.ActiveProfile != cfg.ActiveProfile {
		t.Errorf("active_profile mismatch: %s vs %s", cfg2.ActiveProfile, cfg.ActiveProfile)
	}
	if len(cfg2.Profiles) != len(cfg.Profiles) {
		t.Errorf("profile count mismatch: %d vs %d", len(cfg2.Profiles), len(cfg.Profiles))
	}
}

func TestConfigLocation(t *testing.T) {
	tests := []struct {
		name     string
		timezone string
		want     *time.Location
	}{
		{"valid IANA zone", "America/New_York", mustLoadLocation(t, "America/New_York")},
		{"empty falls back to local", "", time.Local},
		{"garbage falls back to local", "Not/AZone", time.Local},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Timezone: tt.timezone}
			got := cfg.Location()
			if got.String() != tt.want.String() {
				t.Errorf("Location() = %s, want %s", got.String(), tt.want.String())
			}
		})
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("time.LoadLocation(%q): %v", name, err)
	}
	return loc
}

func TestSourcesRoundTrip(t *testing.T) {
	sources := Sources{
		"sqlite": &Source{
			Path:         "~/dev/sqlite-data",
			Type:         "data-source",
			Context:      "CLAUDE.md",
			Description:  "butterstack sqlite export",
			Capabilities: []string{"sql-query", "partner-lookup"},
			Entities:     []string{"thrive", "viewer-core"},
			Exec: map[string]string{
				"query": "sqlite3 data.db \"{query}\"",
			},
		},
		"plausible": &Source{
			Path:         "~/dev/plausible-cli",
			Type:         "tool",
			Context:      "CLAUDE.md",
			Description:  "Plausible analytics",
			Capabilities: []string{"site-analytics", "traffic-query"},
		},
	}

	// Marshal
	data, err := yaml.Marshal(sources)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	// Unmarshal
	var sources2 Sources
	if err := yaml.Unmarshal(data, &sources2); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(sources2) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(sources2))
	}
	if sources2["sqlite"].Type != "data-source" {
		t.Errorf("expected type 'data-source', got %s", sources2["sqlite"].Type)
	}
	if !sources2["sqlite"].HasCapability("sql-query") {
		t.Error("expected sqlite to have sql-query capability")
	}
	if !sources2["sqlite"].HasEntity("thrive") {
		t.Error("expected sqlite to have thrive entity")
	}
}

func TestConfigSaveLoad(t *testing.T) {
	// Use a temp directory
	tmpDir := t.TempDir()
	origDir := os.Getenv("HOME")

	// Create a temporary aida dir
	aidaDir := filepath.Join(tmpDir, ConfigDir)
	os.MkdirAll(aidaDir, 0755)

	// Write config directly to temp dir
	cfg := DefaultConfig()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	configPath := filepath.Join(aidaDir, ConfigFile)
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Read it back
	readData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var loaded Config
	if err := yaml.Unmarshal(readData, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if loaded.Model.Primary != cfg.Model.Primary {
		t.Errorf("model mismatch after save/load")
	}

	_ = origDir // silence unused warning
}

func TestActiveProfile_EnvOverride(t *testing.T) {
	cfg := DefaultConfig()

	t.Run("env var picks valid profile", func(t *testing.T) {
		t.Setenv("AIDA_PROFILE", "home")
		_, name := cfg.ActiveProfileConfig()
		if name != "home" {
			t.Errorf("expected 'home', got %q", name)
		}
	})

	t.Run("env var with unknown profile falls through", func(t *testing.T) {
		t.Setenv("AIDA_PROFILE", "does-not-exist")
		// Force yaml-level selection so fallthrough target is deterministic.
		// Adds a second profile explicitly -- proves explicit `profiles:`
		// config still works even though the shipped default is a single
		// undetected "home" profile.
		cfg := DefaultConfig()
		cfg.Profiles["work"] = Profile{}
		cfg.ActiveProfile = "work"
		_, name := cfg.ActiveProfileConfig()
		if name != "work" {
			t.Errorf("expected fallthrough to 'work', got %q", name)
		}
	})

	t.Run("env var unset uses existing logic", func(t *testing.T) {
		t.Setenv("AIDA_PROFILE", "")
		cfg := DefaultConfig()
		cfg.ActiveProfile = "home"
		_, name := cfg.ActiveProfileConfig()
		if name != "home" {
			t.Errorf("expected 'home' from yaml, got %q", name)
		}
	})
}

func TestLoadDotEnv(t *testing.T) {
	// Dir() resolves ~/.aida via os.UserHomeDir(), which reads $HOME on
	// darwin/linux -- point HOME at a throwaway dir so these tests never
	// touch the real ~/.aida.
	t.Run("export-prefixed line sets the var", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		aidaDir := filepath.Join(tmpHome, ConfigDir)
		if err := os.MkdirAll(aidaDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(aidaDir, ".env"), []byte("export AIDA_TEST_EXPORT_FOO=bar\n"), 0600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		os.Unsetenv("AIDA_TEST_EXPORT_FOO")
		defer os.Unsetenv("AIDA_TEST_EXPORT_FOO")

		LoadDotEnv()

		if got := os.Getenv("AIDA_TEST_EXPORT_FOO"); got != "bar" {
			t.Errorf("AIDA_TEST_EXPORT_FOO = %q, want %q", got, "bar")
		}
	})

	t.Run("op.env is loaded when present", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		aidaDir := filepath.Join(tmpHome, ConfigDir)
		if err := os.MkdirAll(aidaDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(aidaDir, "op.env"), []byte("AIDA_TEST_OP_TOKEN=ops_abc123\n"), 0600); err != nil {
			t.Fatalf("write op.env: %v", err)
		}
		os.Unsetenv("AIDA_TEST_OP_TOKEN")
		defer os.Unsetenv("AIDA_TEST_OP_TOKEN")

		LoadDotEnv()

		if got := os.Getenv("AIDA_TEST_OP_TOKEN"); got != "ops_abc123" {
			t.Errorf("AIDA_TEST_OP_TOKEN = %q, want %q", got, "ops_abc123")
		}
	})

	t.Run("already-set env var is not overwritten", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		aidaDir := filepath.Join(tmpHome, ConfigDir)
		if err := os.MkdirAll(aidaDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(aidaDir, ".env"), []byte("AIDA_TEST_ALREADY_SET=fromfile\n"), 0600); err != nil {
			t.Fatalf("write .env: %v", err)
		}
		t.Setenv("AIDA_TEST_ALREADY_SET", "original")

		LoadDotEnv()

		if got := os.Getenv("AIDA_TEST_ALREADY_SET"); got != "original" {
			t.Errorf("AIDA_TEST_ALREADY_SET = %q, want %q (should not be overwritten)", got, "original")
		}
	})
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()

	tests := []struct {
		input    string
		expected string
	}{
		{"~/dev", filepath.Join(home, "dev")},
		{"/absolute/path", "/absolute/path"},
		{"relative/path", "relative/path"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := ExpandPath(tt.input)
			if result != tt.expected {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestModelsPath(t *testing.T) {
	t.Run("default falls back to ~/.aida/models.yaml", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cfg := &Config{}
		want := filepath.Join(tmpHome, ConfigDir, "models.yaml")
		if got := cfg.ModelsPath(); got != want {
			t.Errorf("ModelsPath() = %q, want %q", got, want)
		}
	})

	t.Run("explicit config value wins and is tilde-expanded", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cfg := &Config{Models: ModelsConfig{Path: "~/custom-models.yaml"}}
		want := filepath.Join(tmpHome, "custom-models.yaml")
		if got := cfg.ModelsPath(); got != want {
			t.Errorf("ModelsPath() = %q, want %q", got, want)
		}
	})
}

func TestBurndownPath(t *testing.T) {
	t.Run("default falls back to ~/.aida/burndown.yaml", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cfg := &Config{}
		want := filepath.Join(tmpHome, ConfigDir, "burndown.yaml")
		if got := cfg.BurndownPath(); got != want {
			t.Errorf("BurndownPath() = %q, want %q", got, want)
		}
	})

	t.Run("explicit config value wins and is tilde-expanded", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		cfg := &Config{Burndown: BurndownConfig{Path: "~/custom-burndown.yaml"}}
		want := filepath.Join(tmpHome, "custom-burndown.yaml")
		if got := cfg.BurndownPath(); got != want {
			t.Errorf("BurndownPath() = %q, want %q", got, want)
		}
	})
}

func TestSourceHasCapability(t *testing.T) {
	src := &Source{
		Capabilities: []string{"sql-query", "partner-lookup", "gmv-analysis"},
	}

	if !src.HasCapability("sql-query") {
		t.Error("expected HasCapability('sql-query') = true")
	}
	if src.HasCapability("nonexistent") {
		t.Error("expected HasCapability('nonexistent') = false")
	}
}

func TestSourceHasEntity(t *testing.T) {
	src := &Source{
		Entities: []string{"thrive", "viewer-core"},
	}

	if !src.HasEntity("thrive") {
		t.Error("expected HasEntity('thrive') = true")
	}
	if src.HasEntity("unknown") {
		t.Error("expected HasEntity('unknown') = false")
	}
}

func TestScrubAnthropicCreds(t *testing.T) {
	// Every variable that must be stripped: the original two, plus the
	// widened set (setup-token, cloud-provider enable switches, per-backend
	// credential/endpoint vars, WIF, base URL / custom headers, and the
	// Bedrock-only bearer token).
	stripped := []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY",
		"CLAUDE_CODE_USE_MANTLE",
		"ANTHROPIC_AWS_API_KEY",
		"ANTHROPIC_AWS_BASE_URL",
		"ANTHROPIC_AWS_WORKSPACE_ID",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_BEDROCK_MANTLE_BASE_URL",
		"ANTHROPIC_BEDROCK_SERVICE_TIER",
		"ANTHROPIC_VERTEX_BASE_URL",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"ANTHROPIC_FOUNDRY_API_KEY",
		"ANTHROPIC_FOUNDRY_AUTH_TOKEN",
		"ANTHROPIC_FOUNDRY_BASE_URL",
		"ANTHROPIC_FOUNDRY_RESOURCE",
		"ANTHROPIC_WORKSPACE_ID",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_CUSTOM_HEADERS",
		"AWS_BEARER_TOKEN_BEDROCK",
	}

	// Variables that must survive: unrelated env, plus vars that look
	// similar but are deliberately out of scope (model selection, not
	// backend/account routing; and generic cloud SDK vars other tools need).
	survivors := []string{
		"PATH",
		"HOME",
		"ANTHROPIC_MODEL",
		"ANTHROPIC_DEFAULT_OPUS_MODEL",
		"ANTHROPIC_DEFAULT_SONNET_MODEL",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"ANTHROPIC_SMALL_FAST_MODEL",
		"ANTHROPIC_BETAS",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		"AWS_PROFILE",
		"AWS_REGION",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GCLOUD_PROJECT",
	}

	env := make([]string, 0, len(stripped)+len(survivors))
	for _, k := range stripped {
		env = append(env, k+"=leaked-value")
	}
	for _, k := range survivors {
		env = append(env, k+"=keep-me")
	}

	out := ScrubAnthropicCreds(env)

	present := make(map[string]bool, len(out))
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		present[k] = true
	}

	for _, k := range stripped {
		if present[k] {
			t.Errorf("%q should have been scrubbed, but survived", k)
		}
	}
	for _, k := range survivors {
		if !present[k] {
			t.Errorf("%q should have survived scrubbing, but was removed", k)
		}
	}

	if len(out) != len(survivors) {
		t.Errorf("expected exactly %d surviving vars, got %d: %v", len(survivors), len(out), out)
	}
}
