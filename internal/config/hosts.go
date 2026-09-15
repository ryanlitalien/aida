package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// HostEntry describes one machine in the user's fleet for the
// assistant's own background knowledge. Distinct from DeviceConfig in
// devices.go, which drives the /dashboard page's liveness probing --
// this is prose context injected into the Jarvis/Aida system prompt so
// the assistant knows what a machine is for and how it's reached BEFORE
// it tries something and fails, rather than burning a turn discovering
// blind that it has no credentials for a host it didn't know existed.
type HostEntry struct {
	// Name is how the assistant should refer to the machine when
	// talking about it.
	Name string `yaml:"name"`
	// Description is a short, free-form sentence on what the machine
	// is for (e.g. "runs the home Minecraft server and long-running
	// background agents").
	Description string `yaml:"description"`
	// Reach says how the machine is contacted, e.g. "ssh alias: foo",
	// "not directly reachable, ask <other host> instead". Optional --
	// a host worth mentioning for context doesn't always have to be
	// something the assistant can reach itself.
	Reach string `yaml:"reach,omitempty"`
}

// HostsConfig is the parsed hosts: block of config.yaml.
type HostsConfig struct {
	Hosts []HostEntry `yaml:"hosts,omitempty"`
}

type hostsConfigFile struct {
	Hosts []HostEntry `yaml:"hosts,omitempty"`
}

// LoadHosts reads the hosts: block from config.yaml. Returns a zero
// HostsConfig (not an error) when config.yaml or the block is missing --
// hosts: is entirely optional, following LoadSoul's pattern in soul.go,
// so a fresh install or a user who has never listed any hosts sees no
// hosts block rather than an error.
func LoadHosts() (*HostsConfig, error) {
	path := filepath.Join(Dir(), ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &HostsConfig{}, nil
		}
		return nil, fmt.Errorf("reading config for hosts: %w", err)
	}
	var file hostsConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parsing hosts config: %w", err)
	}
	return &HostsConfig{Hosts: file.Hosts}, nil
}

// IsEmpty returns true when no hosts are configured.
func (h *HostsConfig) IsEmpty() bool {
	return h == nil || len(h.Hosts) == 0
}

// ForPrompt renders the hosts list as a compact prose block for
// injection into the Jarvis/Aida system prompt, mirroring
// Soul.ForPrompt in soul.go. Returns "" when there are no hosts
// configured, so an unconfigured hosts: block adds nothing to the
// prompt.
func (h *HostsConfig) ForPrompt() string {
	if h.IsEmpty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Known machines:\n")
	for _, host := range h.Hosts {
		if host.Name == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s", host.Name)
		if host.Description != "" {
			fmt.Fprintf(&b, ": %s", strings.TrimSpace(host.Description))
		}
		if host.Reach != "" {
			fmt.Fprintf(&b, " (reach: %s)", strings.TrimSpace(host.Reach))
		}
		b.WriteString("\n")
	}
	return b.String()
}
