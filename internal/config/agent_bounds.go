package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultAgentMaxWallClock is the wall-clock deadline applied to an
// `aida --agent` run when config.yaml sets no explicit override. It
// replaces the old 15-turn cap on orchestrator.Agent.MaxTurns, which
// the Runner no longer honors (a turn count says nothing about wall
// time -- a single turn's tool call, e.g. delegate_to_claude_code or a
// slow MCP call, can itself run for minutes).
//
// 30 minutes is a deliberate choice, not a placeholder: long enough
// for a multi-step investigation with several delegate_to_claude_code
// round-trips to actually finish thinking, short enough that an
// unattended run (voice job_start, a web "Prepare draft" click, an
// ingest auto-solve job) with nobody watching it cannot run forever.
// A caller that wants a tighter bound for one class of run already has
// one: `aida loop` wraps its spawned agent in its own, shorter
// --per-task-timeout (default 7m30s). This is the floor underneath
// everything else, including the previously-unbounded voice/web paths
// that had only the turn cap and now would otherwise have nothing.
const defaultAgentMaxWallClock = 30 * time.Minute

// agentBoundsYAML is the on-disk shape of the agent_bounds: block.
// Kept separate from the exported accessors below so max_wall_clock
// can be authored as a human-friendly Go duration string ("45m") in
// YAML while callers get a real time.Duration back -- yaml.v3 has no
// built-in support for parsing a duration string into a
// time.Duration field. Mirrors calendarConfigYAML's split in
// calendar.go.
type agentBoundsYAML struct {
	MaxWallClock string `yaml:"max_wall_clock,omitempty"`

	// ClaudeConfigDirByProfile maps an aida profile name (the same
	// name resolved by cfg.ActiveProfileConfig / AIDA_PROFILE, e.g.
	// "work") to the CLAUDE_CONFIG_DIR a delegate_to_claude_code
	// subprocess spawned from an agent run under that profile should
	// use. A profile with no entry here -- including the default
	// profile -- inherits today's behavior: no override, the claude
	// CLI falls back to its own default config directory.
	//
	// Keying on aida's own profile concept (already how this codebase
	// distinguishes "which organization's context this run belongs
	// to" -- see the `profiles:` scoping used elsewhere in config.yaml)
	// keeps the actual org name and directory path entirely in
	// config.yaml; no Go source here knows or cares what any profile
	// is named or which organization it belongs to.
	ClaudeConfigDirByProfile map[string]string `yaml:"claude_config_dir_by_profile,omitempty"`
}

type agentBoundsConfigFile struct {
	AgentBounds agentBoundsYAML `yaml:"agent_bounds"`
}

// readAgentBoundsYAML reads and parses the agent_bounds: block
// directly off config.yaml, mirroring CalendarConfig's
// read-straight-off-disk pattern in calendar.go. That pattern lets
// this file add configuration surface for the agent loop's execution
// bounds without adding a field to the shared Config struct in
// config.go, which sees heavy concurrent churn elsewhere and
// shouldn't grow by one field per unrelated feature. A missing
// config.yaml is not an error here -- it just means every accessor
// below falls back to its default.
func readAgentBoundsYAML() (agentBoundsYAML, error) {
	path := filepath.Join(Dir(), ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agentBoundsYAML{}, nil
		}
		return agentBoundsYAML{}, fmt.Errorf("reading config for agent bounds: %w", err)
	}
	var file agentBoundsConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return agentBoundsYAML{}, fmt.Errorf("parsing agent_bounds config: %w", err)
	}
	return file.AgentBounds, nil
}

// AgentMaxWallClock returns the wall-clock deadline to apply to one
// `aida --agent` run (see orchestrator.WithDeadline). An explicit
// agent_bounds.max_wall_clock in config.yaml wins, parsed as a Go
// duration string (e.g. "45m"); a missing or unparsable value falls
// back to defaultAgentMaxWallClock rather than failing the run
// outright -- a malformed bound is a reason to fall back to a safe
// default, not a reason to leave the run completely unbounded.
func (c *Config) AgentMaxWallClock() time.Duration {
	raw, err := readAgentBoundsYAML()
	if err != nil || raw.MaxWallClock == "" {
		return defaultAgentMaxWallClock
	}
	d, err := time.ParseDuration(raw.MaxWallClock)
	if err != nil || d <= 0 {
		return defaultAgentMaxWallClock
	}
	return d
}

// ClaudeConfigDirForProfile returns the CLAUDE_CONFIG_DIR a
// delegate_to_claude_code subprocess spawned from an agent run under
// the given aida profile should use, tilde-expanded. Empty means
// "inherit today's behavior" -- no override -- which is what every
// profile gets unless config.yaml's agent_bounds.claude_config_dir_by_profile
// names one explicitly. See ClaudeConfigDirByProfile's doc comment for
// why profile is the routing key and why no organization name or path
// lives in this file.
func (c *Config) ClaudeConfigDirForProfile(profile string) string {
	if profile == "" {
		return ""
	}
	raw, err := readAgentBoundsYAML()
	if err != nil {
		return ""
	}
	dir := raw.ClaudeConfigDirByProfile[profile]
	if dir == "" {
		return ""
	}
	return expandPath(dir)
}
