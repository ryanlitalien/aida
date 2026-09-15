package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/mcp"
	"github.com/ryanlitalien/aida/internal/sources"
)

func fakeSources() config.Sources {
	return config.Sources{
		"first-chair-source": {
			Description:  "First Chair partner data",
			Type:         "claude-project",
			Capabilities: []string{"partner-docs"},
			Entities:     []string{"first-chair"},
		},
		"plausible": {
			Description:  "Plausible log search",
			Type:         "tool",
			Capabilities: []string{"log-search"},
		},
		"sqlite": {
			Description:  "SQLite data warehouse",
			Type:         "sqlite",
			Capabilities: []string{"sql-query", "partner-data"},
			Entities:     []string{"first-chair", "thrive"},
		},
	}
}

func fakeMCPTools() []mcp.DiscoveredTool {
	return []mcp.DiscoveredTool{
		{Server: "notion", Name: "search", Description: "Search Notion pages by query"},
		{Server: "notion", Name: "fetch", Description: "Fetch a Notion page by ID"},
		{Server: "slack", Name: "search_public", Description: "Search public Slack messages"},
		{Server: "gmail", Name: "send", Description: "Send an email", Destructive: true},
	}
}

func TestBuildAgentToolsLazy_RegistersMetaToolsNotEachMCP(t *testing.T) {
	tools := BuildAgentToolsLazy(fakeSources(), fakeMCPTools(), &mcp.MCPDiscovery{}, nil, nil)

	names := map[string]bool{}
	for _, t := range tools {
		names[t.Name] = true
	}
	// Source tools: each present.
	for _, want := range []string{"first-chair-source", "plausible", "sqlite"} {
		if !names[want] {
			t.Errorf("expected source tool %q in lazy build, missing", want)
		}
	}
	// Meta-tools.
	for _, want := range []string{"find_source", "find_mcp_tool", "call_mcp_tool", "delegate_to_claude_code", "ask_user"} {
		if !names[want] {
			t.Errorf("expected meta-tool %q, missing", want)
		}
	}
	// No per-MCP tool registered eagerly.
	for name := range names {
		if strings.HasPrefix(name, "mcp__") {
			t.Errorf("MCP tool %q registered eagerly - should be lazy", name)
		}
	}
}

func TestBuildAgentToolsLazy_NoMCPSkipsMetaTools(t *testing.T) {
	tools := BuildAgentToolsLazy(fakeSources(), nil, nil, nil, nil)
	for _, tl := range tools {
		if tl.Name == "find_mcp_tool" || tl.Name == "call_mcp_tool" {
			t.Errorf("MCP meta-tool %q registered with no MCP servers connected", tl.Name)
		}
	}
}

func TestFindSourceTool_RanksByHits(t *testing.T) {
	tool := buildFindSourceTool(fakeSources())
	// "sqlite sql" - sqlite matches both tokens (2 hits);
	// first-chair-source and plausible match neither (0 hits and dropped).
	in, _ := json.Marshal(map[string]string{"query": "sqlite sql"})
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "sqlite") {
		t.Errorf("expected sqlite in output:\n%s", out)
	}
	if strings.Contains(out, "plausible") {
		t.Errorf("plausible (0 hits) leaked into output:\n%s", out)
	}
}

func TestFindSourceTool_OrdersByHitsThenAlpha(t *testing.T) {
	tool := buildFindSourceTool(fakeSources())
	// "first-chair partner" - both first-chair-source and sqlite have
	// 2 hits each. Tiebreak: alphabetical → first-chair-source first.
	in, _ := json.Marshal(map[string]string{"query": "first-chair partner"})
	out, _ := tool.Execute(context.Background(), in)
	idxSqlite := strings.Index(out, "sqlite")
	idxFC := strings.Index(out, "first-chair-source")
	if idxSqlite < 0 || idxFC < 0 {
		t.Fatalf("expected both sources in output:\n%s", out)
	}
	if idxFC > idxSqlite {
		t.Errorf("alphabetical tiebreak failed: first-chair-source should come before sqlite:\n%s", out)
	}
}

func TestFindSourceTool_NoMatchesIsFriendly(t *testing.T) {
	tool := buildFindSourceTool(fakeSources())
	in, _ := json.Marshal(map[string]string{"query": "no-such-thing"})
	out, _ := tool.Execute(context.Background(), in)
	if !strings.Contains(out, "No sources matched") {
		t.Errorf("expected friendly no-match message, got: %s", out)
	}
}

func TestFindMCPTool_FindsByDescription(t *testing.T) {
	tool := buildFindMCPTool(fakeMCPTools())
	in, _ := json.Marshal(map[string]string{"query": "search public"})
	out, err := tool.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "mcp__slack__search_public") {
		t.Errorf("expected slack search_public in output, got:\n%s", out)
	}
}

func TestFindMCPTool_DescriptionListsServers(t *testing.T) {
	tool := buildFindMCPTool(fakeMCPTools())
	for _, server := range []string{"notion", "slack", "gmail"} {
		if !strings.Contains(tool.Description, server) {
			t.Errorf("tool description should list connected servers, missing %q in: %s", server, tool.Description)
		}
	}
}

func TestCallMCPTool_RejectsUnknownName(t *testing.T) {
	tool := buildCallMCPTool(fakeMCPTools(), &mcp.MCPDiscovery{}, nil, nil)
	in, _ := json.Marshal(map[string]interface{}{
		"name":  "mcp__nonexistent__tool",
		"input": map[string]string{},
	})
	_, err := tool.Execute(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "unknown MCP tool") {
		t.Errorf("expected unknown MCP tool error, got %v", err)
	}
}

func TestCallMCPTool_RejectsMissingName(t *testing.T) {
	tool := buildCallMCPTool(fakeMCPTools(), &mcp.MCPDiscovery{}, nil, nil)
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"input": {}}`))
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("expected name-required error, got %v", err)
	}
}

// timeoutFakeAdapter simulates an adapter that reports a timeout the way
// sqlite/grep/notion/claude-project do today: Status: "timeout" with a nil
// Go error. buildSourceTool must not treat that as ordinary output.
type timeoutFakeAdapter struct{}

func (timeoutFakeAdapter) Execute(ctx context.Context, command string, src config.Source) (sources.SourceResult, error) {
	return sources.SourceResult{Status: "timeout", Summary: "Command timed out after 30s"}, nil
}

func (timeoutFakeAdapter) ParseOutput(raw []byte, src config.Source) ([]sources.Artifact, error) {
	return nil, nil
}

func TestBuildSourceTool_TimeoutStatusSurfacesAsToolError(t *testing.T) {
	sources.RegisterAdapter("test-timeout-adapter", timeoutFakeAdapter{})

	tool := buildSourceTool("timeout-source", &config.Source{Type: "test-timeout-adapter"})
	in, _ := json.Marshal(map[string]string{"query": "anything"})
	_, err := tool.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("expected an error for a timed-out source, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected error to carry the timeout summary, got %v", err)
	}
}
