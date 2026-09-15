package cli

import (
	"os/exec"

	"github.com/ryanlitalien/aida/internal/config"
)

// toolIntegration maps tool names to their integration type for
// profile.Tools values. Most tools use their binary name, but some
// (like notion) use a different adapter type.
var toolIntegration = map[string]string{
	"notion": "mcp",
}

// detectToolsOnPath probes PATH for every tool in the built-in registry
// and returns a map of tool name → resolved path for those found.
func detectToolsOnPath() map[string]string {
	found := make(map[string]string)
	for _, tool := range config.KnownToolNames() {
		if path, err := exec.LookPath(tool); err == nil {
			found[tool] = path
		}
	}
	return found
}

// writeDetectedTools merges discovered tools into the active profile and
// saves the config. Returns the count of newly added tools (existing
// entries are never overwritten).
func writeDetectedTools(cfg *config.Config, detected map[string]string) int {
	profile, profileName := cfg.ActiveProfileConfig()
	if profile == nil || profileName == "" {
		return 0
	}
	if profile.Tools == nil {
		profile.Tools = make(map[string]string)
	}
	added := 0
	for tool := range detected {
		if _, exists := profile.Tools[tool]; exists {
			continue
		}
		value := tool
		if override, ok := toolIntegration[tool]; ok {
			value = override
		}
		profile.Tools[tool] = value
		added++
	}
	if added > 0 {
		cfg.Profiles[profileName] = *profile
		_ = config.SaveConfig(cfg)
	}
	return added
}
