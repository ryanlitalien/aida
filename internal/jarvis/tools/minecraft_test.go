package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestIsExitCode(t *testing.T) {
	err124 := exec.Command("sh", "-c", "exit 124").Run()
	if !isExitCode(err124, 124) {
		t.Errorf("expected exit 124 to be detected")
	}
	if isExitCode(err124, 1) {
		t.Errorf("exit 124 should not match code 1")
	}

	err1 := exec.Command("sh", "-c", "exit 1").Run()
	if !isExitCode(err1, 1) {
		t.Errorf("expected exit 1 to be detected")
	}

	if isExitCode(nil, 124) {
		t.Errorf("nil error should never match")
	}
}

// writeMinecraftToolConfig writes ~/.aida/config.yaml under the test's
// HOME with the given raw YAML body. Mirrors writeCalendarConfigForTest
// in registry_test.go.
func writeMinecraftToolConfig(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, config.ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// TestMinecraftAskTool_NotConfigured covers the degrade path this task
// requires: with no minecraft: block at all, the registered tool must
// still be callable (it's always registered - see minecraftAskTool's doc
// comment) and must return a clear, non-panicking "not configured" error
// rather than attempting an ssh call with empty host/script arguments.
func TestMinecraftAskTool_NotConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.aida/config.yaml at all

	tool := minecraftAskTool()
	input, _ := json.Marshal(minecraftAskInput{Query: "is the server up?"})
	out, err := tool.Run(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error when minecraft: is not configured, got nil")
	}
	if out != "" {
		t.Errorf("Run() output = %q, want empty on a config error", out)
	}
	if !strings.Contains(err.Error(), "minecraft_ask") {
		t.Errorf("error = %v, want it to identify the minecraft_ask tool", err)
	}
	// The error must not silently look like a generic ssh failure - it
	// should be traceable back to ErrMinecraftNotConfigured so an agent
	// (or a person reading logs) can tell "not configured" apart from
	// "configured but unreachable".
	if !strings.Contains(err.Error(), "not configured") && !strings.Contains(err.Error(), "no minecraft") {
		t.Errorf("error = %v, want it to clearly say the tool is not configured", err)
	}
}

// TestMinecraftAskTool_EmptyQuery covers the pre-existing empty-query
// guard, which must still run before any config lookup.
func TestMinecraftAskTool_EmptyQuery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeMinecraftToolConfig(t, home, `minecraft:
  ssh_host: example-host
  remote_script: /opt/example/scripts/ask
`)

	tool := minecraftAskTool()
	input, _ := json.Marshal(minecraftAskInput{Query: "   "})
	_, err := tool.Run(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error for an empty/whitespace query, got nil")
	}
	if !strings.Contains(err.Error(), "empty query") {
		t.Errorf("error = %v, want it to mention the empty query", err)
	}
}
