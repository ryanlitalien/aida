package roster

import (
	"context"
	"errors"
	"testing"

	"github.com/ryanlitalien/aida/internal/mcp"
)

// fakeToolCaller is a stand-in for *mcp.MCPDiscovery: tests never connect
// to a real MCP server.
type fakeToolCaller struct {
	tools     []mcp.DiscoveredTool
	callErr   error
	callText  string
	lastArgs  map[string]any
	lastTool  string
	lastCalls int
}

func (f *fakeToolCaller) Tools() []mcp.DiscoveredTool { return f.tools }

func (f *fakeToolCaller) CallTool(_ context.Context, _, name string, args map[string]any) (string, error) {
	f.lastTool = name
	f.lastArgs = args
	f.lastCalls++
	if f.callErr != nil {
		return "", f.callErr
	}
	return f.callText, nil
}

func TestSelectMCPTool(t *testing.T) {
	tools := []mcp.DiscoveredTool{
		{Server: "other", Name: "unrelated_tool", Description: "on a different server"},
		{Server: "slack", Name: "slack_search_public", Description: "Search public channels for messages"},
		{Server: "slack", Name: "slack_send_message", Description: "Send a message to a channel", ReadOnly: false},
		{Server: "slack", Name: "slack_read_channel", Description: "Read messages from a channel", ReadOnly: true},
	}

	t.Run("word match wins", func(t *testing.T) {
		got, ok := selectMCPTool(tools, "slack", "search for messages about the outage")
		if !ok {
			t.Fatal("expected a match")
		}
		if got.Name != "slack_search_public" {
			t.Errorf("got %q, want slack_search_public", got.Name)
		}
	})

	t.Run("falls back to read-only when no word matches", func(t *testing.T) {
		got, ok := selectMCPTool(tools, "slack", "xyzzy plugh quux")
		if !ok {
			t.Fatal("expected a match")
		}
		if got.Name != "slack_read_channel" {
			t.Errorf("got %q, want slack_read_channel (the read-only tool)", got.Name)
		}
	})

	t.Run("falls back to first tool when nothing else matches", func(t *testing.T) {
		noReadOnly := []mcp.DiscoveredTool{
			{Server: "x", Name: "alpha", Description: "does a thing"},
			{Server: "x", Name: "beta", Description: "does another thing"},
		}
		got, ok := selectMCPTool(noReadOnly, "x", "xyzzy plugh quux")
		if !ok {
			t.Fatal("expected a match")
		}
		if got.Name != "alpha" {
			t.Errorf("got %q, want alpha (the first tool)", got.Name)
		}
	})

	t.Run("no tools on server", func(t *testing.T) {
		_, ok := selectMCPTool(tools, "nonexistent-server", "anything")
		if ok {
			t.Fatal("expected no match")
		}
	})
}

func TestTaskWords(t *testing.T) {
	got := taskWords("Search for messages about the outage!")
	want := map[string]bool{"search": true, "for": false, "messages": true, "about": true, "the": false, "outage": true}
	gotSet := map[string]bool{}
	for _, w := range got {
		gotSet[w] = true
	}
	for w, wantPresent := range want {
		if gotSet[w] != wantPresent {
			t.Errorf("taskWords() presence of %q = %v, want %v (got %v)", w, gotSet[w], wantPresent, got)
		}
	}
}

func TestBuildMCPArgs(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]interface{}
		task   string
		want   map[string]any
	}{
		{
			name:   "nil schema",
			schema: nil,
			task:   "hello",
			want:   map[string]any{},
		},
		{
			name: "no properties",
			schema: map[string]interface{}{
				"type": "object",
			},
			task: "hello",
			want: map[string]any{},
		},
		{
			name: "query property present",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"query": map[string]interface{}{"type": "string"},
					"limit": map[string]interface{}{"type": "number"},
				},
			},
			task: "find the thing",
			want: map[string]any{"query": "find the thing"},
		},
		{
			name: "prompt property preferred key present",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"prompt": map[string]interface{}{"type": "string"},
				},
			},
			task: "do the thing",
			want: map[string]any{"prompt": "do the thing"},
		},
		{
			name: "single required string property, no known key",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"message": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"message"},
			},
			task: "hello there",
			want: map[string]any{"message": "hello there"},
		},
		{
			name: "single required string property via []string (test-fixture shape)",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"message": map[string]interface{}{"type": "string"},
				},
				"required": []string{"message"},
			},
			task: "hello there",
			want: map[string]any{"message": "hello there"},
		},
		{
			name: "single required non-string property: no guess",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"count": map[string]interface{}{"type": "number"},
				},
				"required": []interface{}{"count"},
			},
			task: "hello",
			want: map[string]any{},
		},
		{
			name: "multiple required properties: no guess",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{
					"a": map[string]interface{}{"type": "string"},
					"b": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"a", "b"},
			},
			task: "hello",
			want: map[string]any{},
		},
		{
			name: "no-arg tool: empty properties",
			schema: map[string]interface{}{
				"properties": map[string]interface{}{},
			},
			task: "hello",
			want: map[string]any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildMCPArgs(tc.schema, tc.task)
			if len(got) != len(tc.want) {
				t.Fatalf("buildMCPArgs() = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("buildMCPArgs()[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestNewMCPBackend_RequiresSpecAndDiscovery(t *testing.T) {
	if _, err := newMCPBackend(&Entry{Name: "x", Kind: KindMCP}, Deps{Discovery: nil}); err == nil {
		t.Fatal("expected an error when MCP spec is nil")
	}
	e := &Entry{Name: "x", Kind: KindMCP, MCP: &MCPSpec{Server: "slack"}}
	if _, err := newMCPBackend(e, Deps{}); err == nil {
		t.Fatal("expected an error when deps.Discovery is nil")
	}
}

// newTestMCPBackend builds an mcpBackend directly (bypassing newMCPBackend's
// Deps.Discovery type requirement) so tests can inject a fakeToolCaller.
func newTestMCPBackend(server, tool string, caller toolCaller) *mcpBackend {
	return &mcpBackend{name: "test-entry", server: server, tool: tool, caller: caller}
}

func TestMCPBackend_Ask_Success(t *testing.T) {
	fake := &fakeToolCaller{
		tools: []mcp.DiscoveredTool{
			{Server: "slack", Name: "slack_send_message", InputSchema: map[string]interface{}{
				"properties": map[string]interface{}{"text": map[string]interface{}{"type": "string"}},
			}},
		},
		callText: "message sent",
	}
	b := newTestMCPBackend("slack", "slack_send_message", fake)
	res, err := b.Ask(context.Background(), Request{Task: "tell the team we shipped"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Status != StatusSuccess {
		t.Errorf("Status = %q, want %q", res.Status, StatusSuccess)
	}
	if res.Text != "message sent" {
		t.Errorf("Text = %q, want %q", res.Text, "message sent")
	}
	if fake.lastArgs["text"] != "tell the team we shipped" {
		t.Errorf("CallTool args = %v, want text=<task>", fake.lastArgs)
	}
	if res.Entry != "test-entry" {
		t.Errorf("Entry = %q, want test-entry", res.Entry)
	}
}

func TestMCPBackend_Ask_AutoSelectsTool(t *testing.T) {
	fake := &fakeToolCaller{
		tools: []mcp.DiscoveredTool{
			{Server: "slack", Name: "slack_search_public", Description: "Search public channels"},
		},
		callText: "found 3 messages",
	}
	b := newTestMCPBackend("slack", "", fake)
	res, err := b.Ask(context.Background(), Request{Task: "search for the outage thread"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Status != StatusSuccess {
		t.Fatalf("Status = %q, want %q", res.Status, StatusSuccess)
	}
	if fake.lastTool != "slack_search_public" {
		t.Errorf("CallTool tool = %q, want slack_search_public", fake.lastTool)
	}
}

func TestMCPBackend_Ask_NoToolFound(t *testing.T) {
	fake := &fakeToolCaller{}
	b := newTestMCPBackend("slack", "", fake)
	res, err := b.Ask(context.Background(), Request{Task: "anything"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Status != StatusError {
		t.Errorf("Status = %q, want %q", res.Status, StatusError)
	}
	if fake.lastCalls != 0 {
		t.Error("CallTool should not have been called")
	}
}

func TestMCPBackend_Ask_Empty(t *testing.T) {
	fake := &fakeToolCaller{
		tools:    []mcp.DiscoveredTool{{Server: "slack", Name: "noop"}},
		callText: "   ",
	}
	b := newTestMCPBackend("slack", "noop", fake)
	res, err := b.Ask(context.Background(), Request{Task: "anything"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Status != StatusEmpty {
		t.Errorf("Status = %q, want %q", res.Status, StatusEmpty)
	}
}

func TestMCPBackend_Ask_Error(t *testing.T) {
	fake := &fakeToolCaller{
		tools:   []mcp.DiscoveredTool{{Server: "slack", Name: "noop"}},
		callErr: errors.New("server unreachable"),
	}
	b := newTestMCPBackend("slack", "noop", fake)
	res, err := b.Ask(context.Background(), Request{Task: "anything"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.Status != StatusError {
		t.Errorf("Status = %q, want %q", res.Status, StatusError)
	}
	if res.Text != "server unreachable" {
		t.Errorf("Text = %q, want %q", res.Text, "server unreachable")
	}
}
