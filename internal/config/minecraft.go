package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// MinecraftConfig holds settings for the minecraft_ask voice tool
// (internal/jarvis/tools), which SSHes into a remote host and runs a
// fixed "ask" script there to reach a Minecraft server's admin agent.
// Read from the minecraft: block of config.yaml -- see CalendarConfig's
// doc comment in calendar.go for why this reads straight off disk rather
// than adding a field to the shared Config struct in config.go, which
// sees heavy concurrent churn on another branch and shouldn't grow by
// one field per unrelated feature.
type MinecraftConfig struct {
	// SSHHost is the ssh destination for the machine running the
	// Minecraft server -- an alias from ~/.ssh/config, or a bare
	// hostname/IP. No default: an empty value means the tool is not
	// configured.
	SSHHost string
	// RemoteScript is the path to the "ask" script on that host,
	// invoked as `<RemoteScript> "<query>"`. Tilde-expansion, if any,
	// happens on the remote shell, not here. No default: an empty
	// value means the tool is not configured.
	RemoteScript string
	// LocalTimeout bounds the local ssh call.
	LocalTimeout time.Duration
	// RemoteTimeoutSecs is the deadline passed to the remote `timeout`
	// wrapper. Always kept a few seconds under LocalTimeout -- see
	// resolveMinecraftConfig -- so the remote process self-terminates
	// before the local ssh is killed, rather than leaving an orphaned
	// process running on the remote host.
	RemoteTimeoutSecs int
}

const (
	// defaultMinecraftLocalTimeout matches the tool's original hardcoded
	// local deadline.
	defaultMinecraftLocalTimeout = 120 * time.Second
	// minecraftRemoteMargin is how far under LocalTimeout the remote
	// deadline sits when remote_timeout_secs isn't set explicitly --
	// the "few seconds" of the invariant described on
	// MinecraftConfig.RemoteTimeoutSecs. Matches the tool's original
	// hardcoded 120s/115s pair.
	minecraftRemoteMargin = 5 * time.Second
)

// minecraftConfigYAML is the on-disk shape of the minecraft: block. Kept
// separate from MinecraftConfig so LocalTimeout can be authored as a
// human-friendly Go duration string ("120s") in YAML -- same split as
// calendarConfigYAML in calendar.go.
type minecraftConfigYAML struct {
	SSHHost           string `yaml:"ssh_host,omitempty"`
	RemoteScript      string `yaml:"remote_script,omitempty"`
	LocalTimeout      string `yaml:"local_timeout,omitempty"`
	RemoteTimeoutSecs int    `yaml:"remote_timeout_secs,omitempty"`
}

type minecraftConfigFile struct {
	Minecraft minecraftConfigYAML `yaml:"minecraft"`
}

// ErrMinecraftNotConfigured is returned by MinecraftConfig when
// config.yaml is missing, has no minecraft: block, or that block omits
// ssh_host or remote_script. Callers must treat this as "the tool has
// nothing to reach" and degrade accordingly, never as a generic failure.
var ErrMinecraftNotConfigured = fmt.Errorf("minecraft: config.yaml has no minecraft.ssh_host / minecraft.remote_script configured")

// MinecraftConfig returns settings for the minecraft_ask voice tool,
// following the CalendarConfig/AgentBounds pattern: explicit config wins,
// sensible defaults fill in the timeouts, and a missing ssh_host or
// remote_script is reported as ErrMinecraftNotConfigured rather than
// silently handing back a zero-value config a caller might try to use.
func (c *Config) MinecraftConfig() (MinecraftConfig, error) {
	path := filepath.Join(Dir(), ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return MinecraftConfig{}, ErrMinecraftNotConfigured
		}
		return MinecraftConfig{}, fmt.Errorf("reading config for minecraft settings: %w", err)
	}

	var file minecraftConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return MinecraftConfig{}, fmt.Errorf("parsing minecraft config: %w", err)
	}
	return resolveMinecraftConfig(file.Minecraft)
}

// resolveMinecraftConfig applies defaults to a parsed minecraft: block and
// enforces the remote-before-local timeout invariant described on
// MinecraftConfig.RemoteTimeoutSecs: an unset remote_timeout_secs derives
// the remote deadline from LocalTimeout minus minecraftRemoteMargin, while
// an explicit value is validated against whatever LocalTimeout resolves
// to and rejected outright if it wouldn't leave the remote process time
// to self-terminate before the local deadline fires.
func resolveMinecraftConfig(raw minecraftConfigYAML) (MinecraftConfig, error) {
	if raw.SSHHost == "" || raw.RemoteScript == "" {
		return MinecraftConfig{}, ErrMinecraftNotConfigured
	}

	cfg := MinecraftConfig{
		SSHHost:      raw.SSHHost,
		RemoteScript: raw.RemoteScript,
		LocalTimeout: defaultMinecraftLocalTimeout,
	}
	if raw.LocalTimeout != "" {
		d, err := time.ParseDuration(raw.LocalTimeout)
		if err != nil {
			return MinecraftConfig{}, fmt.Errorf("minecraft.local_timeout %q: %w", raw.LocalTimeout, err)
		}
		cfg.LocalTimeout = d
	}

	if raw.RemoteTimeoutSecs > 0 {
		if time.Duration(raw.RemoteTimeoutSecs)*time.Second >= cfg.LocalTimeout {
			return MinecraftConfig{}, fmt.Errorf(
				"minecraft.remote_timeout_secs (%ds) must be less than the local timeout (%s) so the remote process self-terminates first",
				raw.RemoteTimeoutSecs, cfg.LocalTimeout)
		}
		cfg.RemoteTimeoutSecs = raw.RemoteTimeoutSecs
		return cfg, nil
	}

	remote := cfg.LocalTimeout - minecraftRemoteMargin
	if remote <= 0 {
		return MinecraftConfig{}, fmt.Errorf("minecraft.local_timeout %s is too short to leave a remote margin", cfg.LocalTimeout)
	}
	cfg.RemoteTimeoutSecs = int(remote.Seconds())
	return cfg, nil
}
