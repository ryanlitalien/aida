package cli

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// captureHookScript is the memory-bridge capture hook installed to
// ~/.claude/hooks/capture-memory.sh by `aida setup`. All path/scope/parse
// logic lives in Go (`aida brain capture-hook`); this script only reads
// the PostToolUse hook JSON and hands it off, fully detached.
//
//go:embed assets/capture-memory.sh
var captureHookScript string

func newSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Register aida MCP server and memory-capture hook in Claude Code",
		Long: `Adds aida as an MCP tool server in ~/.claude/.mcp.json so brain
and task tools are available in every Claude Code session, and installs
the memory-capture hook (~/.claude/hooks/capture-memory.sh) that mirrors
Claude Code memory writes into Aida's brain. Wiring that hook into
~/.claude/settings.json is manual -- settings.json is large and
externally managed, so this command detects whether it's already wired
and, if not, prints the exact JSON snippet to merge in.`,
		Args: cobra.NoArgs,
		RunE: runSetup,
	}
}

// mcpConfig mirrors the .mcp.json structure.
type mcpConfig struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

type mcpServerEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func runSetup(_ *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}

	claudeDir := filepath.Join(home, ".claude")
	mcpPath := filepath.Join(claudeDir, ".mcp.json")

	if _, err := os.Stat(claudeDir); os.IsNotExist(err) {
		return fmt.Errorf("~/.claude/ not found - install Claude Code first")
	}

	// Read existing config or start fresh.
	var cfg mcpConfig
	if data, err := os.ReadFile(mcpPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return fmt.Errorf("parsing existing %s: %w", mcpPath, err)
		}
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]mcpServerEntry)
	}

	// Check if already configured. Note: unlike the original version of
	// this command, neither branch below returns early -- both the
	// already-configured case and the dry-run preview case fall through
	// to the memory-capture hook section, so `aida setup` re-run (or run
	// with --dry-run) always reports the full picture instead of bailing
	// out after the MCP step alone.
	if existing, ok := cfg.MCPServers["aida"]; ok && existing.Command == "aida" && len(existing.Args) == 1 && existing.Args[0] == "serve" {
		fmt.Printf("%s aida MCP server already configured in %s\n", ui.SuccessIcon, mcpPath)
	} else {
		// Add/update aida entry.
		cfg.MCPServers["aida"] = mcpServerEntry{
			Command: "aida",
			Args:    []string{"serve"},
		}

		data, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling config: %w", err)
		}
		data = append(data, '\n')

		if !dryRunGuard("write MCP config", mcpPath) {
			if err := os.WriteFile(mcpPath, data, 0644); err != nil {
				return fmt.Errorf("writing %s: %w", mcpPath, err)
			}
			fmt.Printf("%s Registered aida MCP server in %s\n", ui.SuccessIcon, mcpPath)
			fmt.Println("  Restart Claude Code to pick up the change.")
		}
	}

	// Memory-capture hook: install the script (idempotent, dry-run aware),
	// then detect + instruct on wiring it into settings.json.
	if _, err := installCaptureHook(claudeDir); err != nil {
		return err
	}
	printCaptureHookSettingsNote(claudeDir)

	// Multi-agent memory bridge: Codex and Gemini harvest hooks. Unlike
	// the Claude capture hook above, these are written directly (not
	// detect-and-instruct) since ~/.codex/hooks.json and
	// ~/.gemini/settings.json are structured, additively mergeable JSON
	// rather than the large, freeform settings.json Claude Code owns.
	// Each is a no-op, not an error, when its tool isn't installed.
	if err := wireCodexHarvestHook(home); err != nil {
		fmt.Printf("%s Codex harvest hook: %v\n", ui.WarnIcon, err)
	}
	if err := wireGeminiHarvestHook(home); err != nil {
		fmt.Printf("%s Gemini harvest hook: %v\n", ui.WarnIcon, err)
	}

	return nil
}

// installCaptureHook writes the embedded capture-memory.sh script to
// ~/.claude/hooks/capture-memory.sh, creating the hooks directory as
// needed. It is idempotent: if the file already exists with content
// identical to the embedded script, it prints an "already installed"
// note and returns installed=false without touching the file. Otherwise
// it writes the file at mode 0755 (guarded by the standard --dry-run
// preview) and returns installed=true.
func installCaptureHook(claudeDir string) (installed bool, err error) {
	hooksDir := filepath.Join(claudeDir, "hooks")
	hookPath := filepath.Join(hooksDir, "capture-memory.sh")

	if existing, err := os.ReadFile(hookPath); err == nil && string(existing) == captureHookScript {
		fmt.Printf("%s Memory-capture hook already installed at %s\n", ui.SuccessIcon, hookPath)
		return false, nil
	}

	if dryRunGuard("install capture hook", hookPath) {
		return false, nil
	}

	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return false, fmt.Errorf("creating %s: %w", hooksDir, err)
	}
	if err := os.WriteFile(hookPath, []byte(captureHookScript), 0755); err != nil {
		return false, fmt.Errorf("writing %s: %w", hookPath, err)
	}

	fmt.Printf("%s Installed memory-capture hook at %s\n", ui.SuccessIcon, hookPath)
	return true, nil
}

// captureHookSettingsSnippet is the exact PostToolUse entry a user needs to
// merge into ~/.claude/settings.json's "hooks" object to wire the capture
// hook in. Printed verbatim, not templated, so it stays copy-pasteable.
const captureHookSettingsSnippet = `{
  "matcher": "Write|Edit",
  "hooks": [
    { "type": "command", "command": "$HOME/.claude/hooks/capture-memory.sh" }
  ]
}`

// printCaptureHookSettingsNote reports whether ~/.claude/settings.json
// already wires up the capture hook and, if not, prints the JSON snippet
// to merge in. settings.json is large and externally managed,
// so aida never writes to it -- this is detect-and-instruct only, and is
// deliberately best-effort: an absent or unparseable file just gets the
// snippet printed, never an error.
func printCaptureHookSettingsNote(claudeDir string) {
	settingsPath := filepath.Join(claudeDir, "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err == nil {
		var parsed map[string]any
		_ = json.Unmarshal(data, &parsed) // read-only; parse errors don't block the raw-text check below
		if strings.Contains(string(data), "capture-memory.sh") {
			fmt.Printf("%s Capture hook already wired in %s\n", ui.SuccessIcon, settingsPath)
			return
		}
	}

	fmt.Println()
	fmt.Printf("%s Capture hook not wired into %s yet.\n", ui.WarnIcon, settingsPath)
	fmt.Println("  Add this under \"hooks\" -> \"PostToolUse\" (merge into the existing")
	fmt.Println("  hooks object -- do not replace it):")
	fmt.Println()
	fmt.Println("  ----------------------------------------------------------------")
	for _, line := range strings.Split(captureHookSettingsSnippet, "\n") {
		fmt.Println("  " + line)
	}
	fmt.Println("  ----------------------------------------------------------------")
	fmt.Println()
}
