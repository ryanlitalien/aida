package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMCPConfigs_GlobalOnly(t *testing.T) {
	// Create a temporary directory structure to simulate ~/.claude/
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	if err := os.MkdirAll(claudeDir, 0755); err != nil {
		t.Fatal(err)
	}

	mcpConfig := MCPFileConfig{
		MCPServers: map[string]MCPServerConfig{
			"gmail": {
				Command: "npx",
				Args:    []string{"gmail-mcp-server"},
			},
			"slack": {
				Command: "npx",
				Args:    []string{"slack-mcp-server", "--token", "xoxb-123"},
			},
		},
	}

	data, err := json.Marshal(mcpConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, ".mcp.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// Use loadMCPFile directly since LoadMCPConfigs reads from real paths.
	configs := make(map[string]MCPServerConfig)
	if err := loadMCPFile(filepath.Join(claudeDir, ".mcp.json"), configs); err != nil {
		t.Fatal(err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
	if configs["gmail"].Command != "npx" {
		t.Errorf("expected gmail command 'npx', got %q", configs["gmail"].Command)
	}
	if len(configs["slack"].Args) != 3 {
		t.Errorf("expected 3 slack args, got %d", len(configs["slack"].Args))
	}
}

func TestLoadMCPConfigs_ProjectOverridesGlobal(t *testing.T) {
	tmpDir := t.TempDir()

	// Global config
	globalConfig := MCPFileConfig{
		MCPServers: map[string]MCPServerConfig{
			"gmail": {
				Command: "npx",
				Args:    []string{"gmail-mcp-server"},
			},
			"calendar": {
				Command: "npx",
				Args:    []string{"gcal-mcp-server"},
			},
		},
	}
	globalData, _ := json.Marshal(globalConfig)
	globalPath := filepath.Join(tmpDir, "global.mcp.json")
	os.WriteFile(globalPath, globalData, 0644)

	// Project config overrides gmail, adds notion
	projectConfig := MCPFileConfig{
		MCPServers: map[string]MCPServerConfig{
			"gmail": {
				Command: "custom-gmail",
				Args:    []string{"--custom"},
			},
			"notion": {
				Command: "npx",
				Args:    []string{"notion-mcp-server"},
			},
		},
	}
	projectData, _ := json.Marshal(projectConfig)
	projectPath := filepath.Join(tmpDir, "project.mcp.json")
	os.WriteFile(projectPath, projectData, 0644)

	// Load global, then project (project overrides)
	configs := make(map[string]MCPServerConfig)
	if err := loadMCPFile(globalPath, configs); err != nil {
		t.Fatal(err)
	}
	if err := loadMCPFile(projectPath, configs); err != nil {
		t.Fatal(err)
	}

	if len(configs) != 3 {
		t.Fatalf("expected 3 configs (gmail, calendar, notion), got %d", len(configs))
	}
	// Gmail should be overridden by project
	if configs["gmail"].Command != "custom-gmail" {
		t.Errorf("expected gmail command 'custom-gmail', got %q", configs["gmail"].Command)
	}
	// Calendar should come from global
	if configs["calendar"].Command != "npx" {
		t.Errorf("expected calendar command 'npx', got %q", configs["calendar"].Command)
	}
	// Notion should come from project
	if _, ok := configs["notion"]; !ok {
		t.Error("expected notion config from project")
	}
}

func TestLoadMCPConfigs_MissingFile(t *testing.T) {
	configs := make(map[string]MCPServerConfig)
	err := loadMCPFile("/nonexistent/path/.mcp.json", configs)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if len(configs) != 0 {
		t.Fatalf("expected empty configs for missing file, got %d", len(configs))
	}
}

func TestLoadMCPConfigs_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, ".mcp.json")
	os.WriteFile(path, []byte("not json"), 0644)

	configs := make(map[string]MCPServerConfig)
	err := loadMCPFile(path, configs)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestLoadClaudeUserMCP_RoundTrip(t *testing.T) {
	// Simulate the shape of ~/.claude.json - many unrelated top-level
	// keys plus a mcpServers map mixing URL and command entries.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, ".claude.json")
	payload := []byte(`{
  "anonymousId": "abc-123",
  "claudeMaxTier": "pro",
  "projects": {"/some/path": {"unrelated": true}},
  "mcpServers": {
    "claude.ai Slack": {"url": "https://mcp.slack.com/mcp"},
    "nytimes": {
      "type": "stdio",
      "command": "/usr/local/bin/nyt-mcp",
      "args": [],
      "env": {"NYT_KEY": "test"}
    }
  },
  "otherKey": [1, 2, 3]
}`)
	if err := os.WriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}

	configs := make(map[string]MCPServerConfig)
	if err := loadClaudeUserMCP(path, configs); err != nil {
		t.Fatalf("loadClaudeUserMCP: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d: %+v", len(configs), configs)
	}

	slack, ok := configs["claude.ai Slack"]
	if !ok {
		t.Fatal("missing URL-based server claude.ai Slack")
	}
	if slack.URL != "https://mcp.slack.com/mcp" {
		t.Errorf("expected slack URL https://mcp.slack.com/mcp, got %q", slack.URL)
	}
	if slack.Command != "" {
		t.Errorf("expected empty command for URL server, got %q", slack.Command)
	}

	nyt, ok := configs["nytimes"]
	if !ok {
		t.Fatal("missing command-based server nytimes")
	}
	if nyt.Command != "/usr/local/bin/nyt-mcp" {
		t.Errorf("expected nyt command, got %q", nyt.Command)
	}
	if nyt.URL != "" {
		t.Errorf("expected empty URL for command server, got %q", nyt.URL)
	}
	if nyt.Env["NYT_KEY"] != "test" {
		t.Errorf("expected env NYT_KEY=test, got %q", nyt.Env["NYT_KEY"])
	}
}

func TestLoadClaudeUserMCP_MissingFile(t *testing.T) {
	configs := make(map[string]MCPServerConfig)
	if err := loadClaudeUserMCP("/nonexistent/path/.claude.json", configs); err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if len(configs) != 0 {
		t.Errorf("expected empty configs, got %d", len(configs))
	}
}

func TestLoadClaudeUserMCP_MalformedJSON(t *testing.T) {
	// Malformed ~/.claude.json must not break MCP discovery.
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, ".claude.json")
	if err := os.WriteFile(path, []byte("not valid json"), 0644); err != nil {
		t.Fatal(err)
	}
	configs := make(map[string]MCPServerConfig)
	if err := loadClaudeUserMCP(path, configs); err != nil {
		t.Errorf("expected nil error for malformed json (graceful degradation), got %v", err)
	}
}

func TestIsAidaServer(t *testing.T) {
	cases := map[string]bool{
		"aida":           true,
		"Aida":           true,
		"aida-dev":       true,
		"aidainator":     true, // defensive: anything aida* is skipped
		"chrome-devtool": false,
		"gmail":          false,
		"":               false,
	}
	for name, want := range cases {
		if got := isAidaServer(name); got != want {
			t.Errorf("isAidaServer(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLoadMCPConfigs_EnvField(t *testing.T) {
	tmpDir := t.TempDir()

	mcpConfig := MCPFileConfig{
		MCPServers: map[string]MCPServerConfig{
			"slack": {
				Command: "npx",
				Args:    []string{"slack-mcp"},
				Env: map[string]string{
					"SLACK_TOKEN": "xoxb-test",
				},
			},
		},
	}
	data, _ := json.Marshal(mcpConfig)
	path := filepath.Join(tmpDir, ".mcp.json")
	os.WriteFile(path, data, 0644)

	configs := make(map[string]MCPServerConfig)
	if err := loadMCPFile(path, configs); err != nil {
		t.Fatal(err)
	}
	if configs["slack"].Env["SLACK_TOKEN"] != "xoxb-test" {
		t.Errorf("expected env SLACK_TOKEN=xoxb-test, got %q", configs["slack"].Env["SLACK_TOKEN"])
	}
}

func TestNewMCPDiscovery(t *testing.T) {
	d := NewMCPDiscovery(false)
	if d == nil {
		t.Fatal("expected non-nil MCPDiscovery")
	}
	if d.sessions == nil {
		t.Fatal("expected non-nil sessions map")
	}
	if len(d.tools) != 0 {
		t.Fatalf("expected empty tools, got %d", len(d.tools))
	}
}

func TestConnectAll_SkipsAida(t *testing.T) {
	d := NewMCPDiscovery(false)

	configs := map[string]MCPServerConfig{
		"aida": {
			Command: "aida",
			Args:    []string{"serve"},
		},
	}

	// ConnectAll should skip "aida" and not error.
	err := d.ConnectAll(context.Background(), configs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(d.sessions) != 0 {
		t.Fatalf("expected 0 sessions (aida skipped), got %d", len(d.sessions))
	}
}

func TestConnectAll_SkipsAidaAmongOthers(t *testing.T) {
	d := NewMCPDiscovery(false)

	configs := map[string]MCPServerConfig{
		"aida": {
			Command: "aida",
			Args:    []string{"serve"},
		},
		"nonexistent-server": {
			Command: "/nonexistent/binary/that/does/not/exist",
			Args:    []string{},
		},
	}

	// aida is skipped; nonexistent-server will fail to connect but that's
	// logged as a warning, not an error.
	err := d.ConnectAll(context.Background(), configs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := d.sessions["aida"]; ok {
		t.Error("aida session should not exist")
	}
}

func TestCallTool_UnknownServer(t *testing.T) {
	d := NewMCPDiscovery(false)

	_, err := d.CallTool(context.Background(), "nonexistent", "some-tool", nil)
	if err == nil {
		t.Fatal("expected error for unknown server")
	}
}

func TestDiscoveredTool_Fields(t *testing.T) {
	dt := DiscoveredTool{
		Server:      "gmail",
		Name:        "search_emails",
		Description: "Search emails by query",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query",
				},
			},
		},
		ReadOnly:    true,
		Destructive: false,
	}

	if dt.Server != "gmail" {
		t.Errorf("expected server 'gmail', got %q", dt.Server)
	}
	if dt.Name != "search_emails" {
		t.Errorf("expected name 'search_emails', got %q", dt.Name)
	}
	if !dt.ReadOnly {
		t.Error("expected ReadOnly to be true")
	}
	if dt.Destructive {
		t.Error("expected Destructive to be false")
	}
}

func TestExtractTextContent(t *testing.T) {
	// Test with nil result.
	if got := extractTextContent(nil); got != "" {
		t.Errorf("expected empty string for nil result, got %q", got)
	}
}

func TestTools_ReturnsDiscovered(t *testing.T) {
	d := NewMCPDiscovery(false)
	d.tools = []DiscoveredTool{
		{Server: "test", Name: "tool1"},
		{Server: "test", Name: "tool2"},
	}

	tools := d.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
}

func TestBuildTransport_URLPicksStreamable(t *testing.T) {
	cfg := MCPServerConfig{URL: "https://example.com/mcp"}
	transport, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	// We can't assert on the concrete type without importing the SDK
	// in the test (already imported transitively); a nil check plus the
	// transportLabel round-trip is enough to confirm the URL branch.
	if transport == nil {
		t.Fatal("expected non-nil transport for URL config")
	}
	if got := transportLabel(cfg); got != "http" {
		t.Errorf("transportLabel(url-only) = %q, want %q", got, "http")
	}
}

func TestBuildTransport_CommandPicksStdio(t *testing.T) {
	cfg := MCPServerConfig{Command: "/bin/echo", Args: []string{"hi"}}
	transport, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("buildTransport: %v", err)
	}
	if transport == nil {
		t.Fatal("expected non-nil transport for command config")
	}
	if got := transportLabel(cfg); got != "stdio" {
		t.Errorf("transportLabel(command-only) = %q, want %q", got, "stdio")
	}
}

func TestBuildTransport_EmptyConfigErrors(t *testing.T) {
	if _, err := buildTransport(MCPServerConfig{}); err == nil {
		t.Fatal("expected error for config with neither url nor command")
	}
}

func TestConnectAll_RecordsStatuses(t *testing.T) {
	d := NewMCPDiscovery(false)

	// One nonexistent stdio binary, one URL that cannot resolve. Both
	// should produce a ConnectStatus entry with Connected=false.
	configs := map[string]MCPServerConfig{
		"bad-stdio": {Command: "/nonexistent/binary/that/does/not/exist"},
		"bad-http":  {URL: "http://127.0.0.1:1/does-not-exist"},
	}

	if err := d.ConnectAll(context.Background(), configs); err != nil {
		t.Fatalf("ConnectAll: %v", err)
	}

	statuses := d.Statuses()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}

	byName := make(map[string]ConnectStatus, len(statuses))
	for _, s := range statuses {
		byName[s.Server] = s
	}

	if byName["bad-stdio"].Transport != "stdio" {
		t.Errorf("bad-stdio transport = %q, want stdio", byName["bad-stdio"].Transport)
	}
	if byName["bad-http"].Transport != "http" {
		t.Errorf("bad-http transport = %q, want http", byName["bad-http"].Transport)
	}
	for _, s := range statuses {
		if s.Connected {
			t.Errorf("%s unexpectedly Connected=true", s.Server)
		}
	}
}

func TestClose_EmptyDiscovery(t *testing.T) {
	d := NewMCPDiscovery(false)
	d.Close() // should not panic
	if len(d.sessions) != 0 {
		t.Error("expected empty sessions after close")
	}
}

// TestFormatToolError verifies the error CallTool returns when a tool
// response sets IsError includes both the server/tool name and the tool's
// own reported text, so a failing MCP call is never mistaken for success.
func TestFormatToolError(t *testing.T) {
	err := formatToolError("gmail", "send", "rate limited")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "gmail/send") {
		t.Errorf("expected error to name the server/tool, got %v", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected error to include the tool's reported text, got %v", err)
	}
}

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"connecting to MCP server: calling \"initialize\": sending \"initialize\": Unauthorized", true},
		{"server returned 401", true},
		{"403 Forbidden", true},
		{"FORBIDDEN", true},
		{"connection refused", false},
		{"context deadline exceeded", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = errors.New(c.msg)
		}
		if got := isAuthError(err); got != c.want {
			t.Errorf("isAuthError(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
