package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/ui"
)

// harvestCodexHookScript is installed to ~/.aida/hooks/harvest-codex.sh
// and wired into ~/.codex/hooks.json's Stop event by `aida setup`. All
// parsing/watermark/distillation logic lives in Go (`aida brain harvest
// --tool codex`); this script only fires it off, fully detached.
//
//go:embed assets/harvest-codex.sh
var harvestCodexHookScript string

// harvestGeminiHookScript is installed to
// ~/.aida/hooks/harvest-gemini.sh and wired into
// ~/.gemini/settings.json's AfterAgent event by `aida setup`.
//
//go:embed assets/harvest-gemini.sh
var harvestGeminiHookScript string

// codexHarvestHookMarker and geminiHarvestHookMarker are the
// idempotency markers mergeAdditiveHook looks for in an existing
// hook-group's raw JSON -- a substring match on the script path is
// enough to detect "already wired" without needing byte-identical
// JSON.
const (
	codexHarvestHookMarker  = "harvest-codex.sh"
	geminiHarvestHookMarker = "harvest-gemini.sh"
)

// installHookScript writes content to destPath, creating parent
// directories as needed. Idempotent: a no-op (installed=false) if the
// file already holds byte-identical content; otherwise
// creates/overwrites at mode 0755. Dry-run aware. Shared by every
// harvest hook installer (Codex, Gemini) -- the generic sibling of
// installCaptureHook, which is Claude-specific by path.
func installHookScript(destPath, content string) (installed bool, err error) {
	if existing, err := os.ReadFile(destPath); err == nil && string(existing) == content {
		fmt.Printf("%s Harvest hook script already installed at %s\n", ui.SuccessIcon, destPath)
		return false, nil
	}

	if dryRunGuard("install harvest hook script", destPath) {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return false, fmt.Errorf("creating %s: %w", filepath.Dir(destPath), err)
	}
	if err := os.WriteFile(destPath, []byte(content), 0755); err != nil {
		return false, fmt.Errorf("writing %s: %w", destPath, err)
	}

	fmt.Printf("%s Installed harvest hook script at %s\n", ui.SuccessIcon, destPath)
	return true, nil
}

// mergeAdditiveHook merges newGroup into event's hook-group array
// inside data's top-level "hooks" object, preserving every other
// top-level key's raw bytes verbatim and every other event's
// hook-group array completely untouched. Safe to run against a large,
// externally-managed config file that carries hooks this command did
// not install (e.g. Codex's hooks.json and Gemini's settings.json both
// carry supacode-managed entries that must survive byte-for-byte in
// substance, even though the surrounding JSON gets re-serialized).
//
// Idempotent: a no-op (installed=false, data returned unchanged) when a
// hook-group already present under event contains markerSubstring.
// Empty data is treated as an empty object (first-time install).
func mergeAdditiveHook(data []byte, event, markerSubstring string, newGroup json.RawMessage) (out []byte, installed bool, err error) {
	obj := map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, false, fmt.Errorf("parse: %w", err)
		}
	}

	hooks := map[string][]json.RawMessage{}
	if raw, ok := obj["hooks"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, false, fmt.Errorf("parse hooks: %w", err)
		}
	}

	for _, g := range hooks[event] {
		if strings.Contains(string(g), markerSubstring) {
			return data, false, nil
		}
	}
	hooks[event] = append(hooks[event], newGroup)

	hooksJSON, err := json.MarshalIndent(hooks, "", "  ")
	if err != nil {
		return nil, false, err
	}
	obj["hooks"] = hooksJSON

	out, err = json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

// commandHookGroup renders the standard single-command hook-group shape
// both Codex's hooks.json and Gemini's settings.json use:
// {"hooks":[{"type":"command","command":"<path>"}]}.
func commandHookGroup(scriptPath string) json.RawMessage {
	group := map[string]interface{}{
		"hooks": []map[string]interface{}{
			{"type": "command", "command": scriptPath},
		},
	}
	data, _ := json.Marshal(group)
	return data
}

// wireCodexHarvestHook installs ~/.aida/hooks/harvest-codex.sh and
// wires it into ~/.codex/hooks.json's Stop event, additively -- Codex's
// own supacode-managed Stop hooks are preserved untouched. A no-op
// (not an error) when ~/.codex doesn't exist, since Codex may simply
// not be installed on this machine.
func wireCodexHarvestHook(home string) error {
	codexDir := filepath.Join(home, ".codex")
	if _, err := os.Stat(codexDir); err != nil {
		ui.PrintVerbose("setup", "~/.codex not found, skipping Codex harvest hook")
		return nil
	}

	scriptPath := filepath.Join(home, ".aida", "hooks", "harvest-codex.sh")
	if _, err := installHookScript(scriptPath, harvestCodexHookScript); err != nil {
		return fmt.Errorf("codex harvest hook script: %w", err)
	}

	hooksPath := filepath.Join(codexDir, "hooks.json")
	data, _ := os.ReadFile(hooksPath) // missing file == first-time install
	merged, installed, err := mergeAdditiveHook(data, "Stop", codexHarvestHookMarker, commandHookGroup(scriptPath))
	if err != nil {
		return fmt.Errorf("merge %s: %w", hooksPath, err)
	}
	if !installed {
		fmt.Printf("%s Codex harvest hook already wired in %s\n", ui.SuccessIcon, hooksPath)
		return nil
	}
	if dryRunGuard("wire Codex harvest hook into " + hooksPath) {
		return nil
	}
	if err := os.WriteFile(hooksPath, merged, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", hooksPath, err)
	}
	fmt.Printf("%s Wired Codex harvest hook into %s (Stop event)\n", ui.SuccessIcon, hooksPath)
	return nil
}

// wireGeminiHarvestHook installs ~/.aida/hooks/harvest-gemini.sh and
// wires it into ~/.gemini/settings.json's AfterAgent event, additively
// -- Gemini's other top-level settings (security, general, ...) and
// other hook events (BeforeAgent, AfterTool, ...) are preserved
// untouched. A no-op (not an error) when ~/.gemini doesn't exist.
func wireGeminiHarvestHook(home string) error {
	geminiDir := filepath.Join(home, ".gemini")
	if _, err := os.Stat(geminiDir); err != nil {
		ui.PrintVerbose("setup", "~/.gemini not found, skipping Gemini harvest hook")
		return nil
	}

	scriptPath := filepath.Join(home, ".aida", "hooks", "harvest-gemini.sh")
	if _, err := installHookScript(scriptPath, harvestGeminiHookScript); err != nil {
		return fmt.Errorf("gemini harvest hook script: %w", err)
	}

	settingsPath := filepath.Join(geminiDir, "settings.json")
	data, _ := os.ReadFile(settingsPath) // missing file == first-time install
	merged, installed, err := mergeAdditiveHook(data, "AfterAgent", geminiHarvestHookMarker, commandHookGroup(scriptPath))
	if err != nil {
		return fmt.Errorf("merge %s: %w", settingsPath, err)
	}
	if !installed {
		fmt.Printf("%s Gemini harvest hook already wired in %s\n", ui.SuccessIcon, settingsPath)
		return nil
	}
	if dryRunGuard("wire Gemini harvest hook into " + settingsPath) {
		return nil
	}
	if err := os.WriteFile(settingsPath, merged, 0644); err != nil {
		return fmt.Errorf("writing %s: %w", settingsPath, err)
	}
	fmt.Printf("%s Wired Gemini harvest hook into %s (AfterAgent event)\n", ui.SuccessIcon, settingsPath)
	return nil
}
