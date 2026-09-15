package config

import (
	"os/exec"
	"sort"
)

// BuiltinToolSources is the registry of tools aida knows how to use.
// Each entry maps a CLI binary name to a Source definition. At runtime,
// aida checks which of these are on PATH and auto-injects sources for
// the ones found - no manual config needed.
var BuiltinToolSources = map[string]*Source{
	"gh": {
		Type:        "tool",
		Description: "GitHub CLI for querying pull requests, issues, repos, and code reviews",
		Capabilities: []string{
			"pr-lookup", "pr-review", "pr-diff",
			"issue-lookup", "github-api",
		},
		Entities: []string{
			"github", "pr", "pull-request",
			"issue", "review", "gh",
		},
		Exec: map[string]string{
			"query": "gh {query}",
		},
	},
	"claude": {
		Type:        "tool",
		Description: "Claude Code CLI for delegating questions to a Claude sub-agent",
		Capabilities: []string{
			"code-reference", "subagent-delegation",
		},
		Entities: []string{
			"claude", "code",
		},
		Exec: map[string]string{
			"query": "claude --print {query}",
		},
	},
	"ollama": {
		Type:        "tool",
		Description: "Ollama local LLM runner for offline queries and model interaction",
		Capabilities: []string{
			"llm-query", "offline-query",
		},
		Entities: []string{
			"ollama", "local-llm",
		},
		Exec: map[string]string{
			"query": "ollama run {query}",
		},
	},
	"current-time": {
		Type:        "current-time",
		Description: "Local system clock - current time, date, day of week, timezone",
		Capabilities: []string{
			"current-time", "date-lookup",
		},
		Entities: []string{
			"time", "date", "today", "clock", "timezone",
		},
		// No Exec: this is a built-in Go adapter (CurrentTimeAdapter), not
		// a shelled-out CLI tool. SelfContained tells InjectAvailableTools
		// to skip the PATH check entirely -- there's no "current-time"
		// binary to find.
		SelfContained: true,
	},
}

// KnownToolNames returns a sorted list of all tool names in the registry.
// Used by aida init and aida profile scan for display.
func KnownToolNames() []string {
	names := make([]string, 0, len(BuiltinToolSources))
	for name := range BuiltinToolSources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// InjectToolSources adds built-in source definitions for any tool declared
// in profile.Tools that doesn't already have an explicit source. This
// handles explicit overrides where a user forces a tool into a profile.
func InjectToolSources(sources Sources, profile *Profile) {
	if profile == nil || len(profile.Tools) == 0 {
		return
	}
	for toolName := range profile.Tools {
		if _, exists := sources[toolName]; exists {
			continue
		}
		if builtin, ok := BuiltinToolSources[toolName]; ok {
			sources[toolName] = builtin
		}
	}
}

// InjectAvailableTools checks PATH for every tool in the registry and
// injects sources for those found. No profile.Tools declaration needed -
// if the binary is on PATH and no explicit source overrides it, the
// built-in source is activated automatically.
//
// Builtins marked SelfContained (e.g. "current-time") aren't CLI wrappers
// at all - they're pure Go adapters with nothing to find on PATH - so
// they're injected unconditionally, skipping the exec.LookPath gate
// entirely. Everything else still requires its binary on PATH as before.
func InjectAvailableTools(sources Sources) {
	for toolName, src := range BuiltinToolSources {
		if _, exists := sources[toolName]; exists {
			continue // explicit source takes precedence
		}
		if src.SelfContained {
			sources[toolName] = src
			continue
		}
		if _, err := exec.LookPath(toolName); err == nil {
			sources[toolName] = src
		}
	}
}
