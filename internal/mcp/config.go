package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
)

// MCPServerConfig mirrors a single entry in .mcp.json or
// ~/.claude.json mcpServers. An entry has either a Command (stdio
// transport) or a URL (streamable HTTP transport), not both.
type MCPServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// MCPFileConfig mirrors the .mcp.json file structure.
type MCPFileConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// claudeUserConfig is a permissive shape for ~/.claude.json. We only
// care about the top-level mcpServers map; every other key is ignored.
type claudeUserConfig struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// LoadMCPConfigs reads MCP server definitions from the user's config
// files and merges them into a single map. Precedence (lowest to
// highest - later entries win on name collisions):
//
//  1. ~/.claude.json (Claude Code user-level - broadest)
//  2. ~/.claude/.mcp.json (legacy/aida-specific user config)
//  3. ./.mcp.json (project-level overrides)
//
// Any server whose name is "aida" or starts with "aida" is dropped
// to prevent recursive subprocesses (aida serve invoking aida serve).
func LoadMCPConfigs() (map[string]MCPServerConfig, error) {
	merged := make(map[string]MCPServerConfig)

	// 1. Claude Code user-level config: ~/.claude.json mcpServers
	claudeJSON := config.ExpandPath("~/.claude.json")
	if err := loadClaudeUserMCP(claudeJSON, merged); err != nil {
		return nil, err
	}

	// 2. Legacy user config: ~/.claude/.mcp.json
	globalPath := config.ExpandPath("~/.claude/.mcp.json")
	if err := loadMCPFile(globalPath, merged); err != nil {
		return nil, err
	}

	// 3. Project-level config: .mcp.json in current directory
	cwd, err := os.Getwd()
	if err == nil {
		projectPath := filepath.Join(cwd, ".mcp.json")
		if err := loadMCPFile(projectPath, merged); err != nil {
			return nil, err
		}
	}

	// Drop aida* servers defensively - sub-agent recursion.
	for name := range merged {
		if isAidaServer(name) {
			delete(merged, name)
		}
	}

	return merged, nil
}

// isAidaServer reports whether the server name should be skipped to
// prevent aida-serve-invoking-aida-serve recursion.
func isAidaServer(name string) bool {
	lower := strings.ToLower(name)
	return lower == "aida" || strings.HasPrefix(lower, "aida")
}

// loadClaudeUserMCP parses ~/.claude.json, extracts the top-level
// mcpServers map, and merges entries into dst. The file is large
// (~100KB+) and contains many unrelated keys; we use a permissive
// decoder that ignores everything except mcpServers.
func loadClaudeUserMCP(path string, dst map[string]MCPServerConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var cfg claudeUserConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		// Malformed ~/.claude.json shouldn't break MCP discovery -
		// log-and-continue is the contract everywhere else in the
		// pipeline.
		return nil
	}

	for name, entry := range cfg.MCPServers {
		dst[name] = entry
	}
	return nil
}

// loadMCPFile reads a single .mcp.json file and merges entries into dst.
// Missing files are silently ignored.
func loadMCPFile(path string, dst map[string]MCPServerConfig) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var fc MCPFileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return err
	}

	for name, cfg := range fc.MCPServers {
		dst[name] = cfg
	}
	return nil
}
