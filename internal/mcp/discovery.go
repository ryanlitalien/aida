package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	connectTimeout  = 10 * time.Second
	toolCallTimeout = 30 * time.Second
)

// DiscoveredTool represents an MCP tool found via tool discovery.
type DiscoveredTool struct {
	Server      string                 // server name from .mcp.json
	Name        string                 // tool name
	Description string                 // human-readable description
	InputSchema map[string]interface{} // JSON schema for arguments
	ReadOnly    bool
	Destructive bool
}

// ConnectStatus records the outcome of a per-server connect attempt.
// Used for the startup banner so users can see at a glance which
// servers are reachable.
type ConnectStatus struct {
	Server    string
	Transport string // "stdio" | "http"
	Connected bool
	NumTools  int
	Err       error
	// AuthSkipped is true when the connect attempt failed because the
	// server requires OAuth/auth Aida can't supply (the user's tokens
	// live in Claude Code's keychain, not reachable from here). These are
	// expected skips, not errors - the banner renders them calmly.
	AuthSkipped bool
}

// MCPDiscovery manages connections to MCP servers and discovers their tools.
type MCPDiscovery struct {
	sessions map[string]*mcpsdk.ClientSession
	tools    []DiscoveredTool
	statuses []ConnectStatus
	verbose  bool
	mu       sync.RWMutex
}

// NewMCPDiscovery creates a new MCPDiscovery instance.
// When verbose is true, connection and discovery warnings are logged;
// otherwise they are silently skipped.
func NewMCPDiscovery(verbose bool) *MCPDiscovery {
	return &MCPDiscovery{
		sessions: make(map[string]*mcpsdk.ClientSession),
		verbose:  verbose,
	}
}

// ConnectAll connects to all configured MCP servers. If a server fails to
// connect, a warning is logged and the remaining servers are still attempted.
// The "aida" server is always skipped to prevent recursion.
//
// Each entry's transport is selected by its config shape: a URL means
// streamable HTTP; a Command means stdio. Per-server outcomes are
// recorded for the caller to render a status banner.
func (d *MCPDiscovery) ConnectAll(ctx context.Context, configs map[string]MCPServerConfig) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "aida",
		Version: "1.0.0",
	}, nil)

	// Deterministic order - keeps the banner stable across launches.
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cfg := configs[name]

		// Skip the aida server to prevent recursion.
		if isAidaServer(name) {
			continue
		}

		transport := transportLabel(cfg)
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		session, err := connectServer(connectCtx, client, cfg)
		cancel()
		if err != nil {
			if d.verbose {
				log.Printf("warning: MCP server %q failed to connect: %v", name, err)
			}
			d.statuses = append(d.statuses, ConnectStatus{
				Server:      name,
				Transport:   transport,
				Connected:   false,
				Err:         err,
				AuthSkipped: isAuthError(err),
			})
			continue
		}
		d.sessions[name] = session
		d.statuses = append(d.statuses, ConnectStatus{
			Server:    name,
			Transport: transport,
			Connected: true,
		})
	}

	return nil
}

// transportLabel returns a human-readable transport name based on the
// config shape.
func transportLabel(cfg MCPServerConfig) string {
	if cfg.URL != "" {
		return "http"
	}
	return "stdio"
}

// isAuthError reports whether a connect error is an authentication failure
// (the server needs OAuth/credentials Aida can't supply) rather than a real
// fault. These are expected for remote URL servers shared with Claude Code -
// their bearer tokens live in Claude Code's keychain, not reachable here - so
// the banner treats them as quiet skips instead of errors.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"unauthorized", "forbidden", "401", "403"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// connectServer picks the appropriate transport (stdio CommandTransport
// or StreamableClientTransport) and connects a client session.
//
// For URL-based servers we do NOT attach an OAuth handler - the user's
// bearer tokens live in Claude Code's keychain and aren't reachable
// from here. If the server requires auth, Connect returns a 401/403
// wrapped error, which the caller logs and treats as "reachable but
// not usable" rather than fatal.
func connectServer(ctx context.Context, client *mcpsdk.Client, cfg MCPServerConfig) (*mcpsdk.ClientSession, error) {
	transport, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to MCP server: %w", err)
	}
	return session, nil
}

// buildTransport picks the right SDK transport for the config shape.
// URL wins over Command if both are somehow set.
//
// Subprocess lifetime is intentionally NOT bound to a context: the
// caller's connect-timeout ctx flows through `client.Connect` for the
// initialize handshake, and `session.Close()` handles subprocess
// shutdown via stdin close. Binding the subprocess to the connect ctx
// (the prior behavior) killed the server the instant the per-call
// timeout was cancelled, so tools/list calls landed on a closed pipe
// for any server slower than the random map-iteration winner.
func buildTransport(cfg MCPServerConfig) (mcpsdk.Transport, error) {
	if cfg.URL != "" {
		return &mcpsdk.StreamableClientTransport{
			Endpoint: cfg.URL,
			HTTPClient: &http.Client{
				Timeout: connectTimeout,
			},
			// We don't maintain a persistent stream - Aida
			// only makes request/response tool calls, never
			// receives server-initiated notifications.
			DisableStandaloneSSE: true,
		}, nil
	}

	if cfg.Command == "" {
		return nil, fmt.Errorf("MCP config has neither url nor command")
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	// Merge the inherited environment with any per-server overrides so
	// subprocesses still see PATH / HOME / language toolchain vars.
	// `cmd.Env = nil` would inherit, but as soon as we append to it Go
	// treats it as an explicit replacement - the bug that previously
	// stripped everything except the user-declared keys.
	if len(cfg.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return &mcpsdk.CommandTransport{Command: cmd}, nil
}

// DiscoverTools queries all connected MCP servers for their available tools
// and returns a merged list. Previously discovered tools are refreshed.
// Per-server tool counts are folded back into ConnectStatus.
func (d *MCPDiscovery) DiscoverTools(ctx context.Context) ([]DiscoveredTool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var all []DiscoveredTool
	counts := make(map[string]int, len(d.sessions))

	for serverName, session := range d.sessions {
		tools, err := discoverServerTools(ctx, serverName, session)
		if err != nil {
			if d.verbose {
				log.Printf("warning: failed to discover tools from %q: %v", serverName, err)
			}
			continue
		}
		counts[serverName] = len(tools)
		all = append(all, tools...)
	}

	for i := range d.statuses {
		if n, ok := counts[d.statuses[i].Server]; ok {
			d.statuses[i].NumTools = n
		}
	}

	d.tools = all
	return all, nil
}

// discoverServerTools lists tools from a single server session.
func discoverServerTools(ctx context.Context, serverName string, session *mcpsdk.ClientSession) ([]DiscoveredTool, error) {
	var tools []DiscoveredTool

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return tools, fmt.Errorf("listing tools: %w", err)
		}

		dt := DiscoveredTool{
			Server:      serverName,
			Name:        tool.Name,
			Description: tool.Description,
		}

		// Extract input schema as map[string]interface{}.
		if tool.InputSchema != nil {
			schemaBytes, err := json.Marshal(tool.InputSchema)
			if err == nil {
				var schemaMap map[string]interface{}
				if json.Unmarshal(schemaBytes, &schemaMap) == nil {
					dt.InputSchema = schemaMap
				}
			}
		}

		// Extract annotation hints.
		if tool.Annotations != nil {
			dt.ReadOnly = tool.Annotations.ReadOnlyHint
			if tool.Annotations.DestructiveHint != nil {
				dt.Destructive = *tool.Annotations.DestructiveHint
			}
		}

		tools = append(tools, dt)
	}

	return tools, nil
}

// CallTool invokes a tool on the specified MCP server and returns the text
// result. The args map is passed as the tool's JSON arguments.
func (d *MCPDiscovery) CallTool(ctx context.Context, server, name string, args map[string]any) (string, error) {
	d.mu.RLock()
	session, ok := d.sessions[server]
	d.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("MCP server %q not connected", server)
	}

	callCtx, cancel := context.WithTimeout(ctx, toolCallTimeout)
	defer cancel()

	result, err := session.CallTool(callCtx, &mcpsdk.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", fmt.Errorf("calling tool %s/%s: %w", server, name, err)
	}

	// The MCP protocol signals tool-level failure via IsError on an
	// otherwise-successful response, not via a transport error -- aida's
	// own server sets it the same way (see server.go's toolResult). Miss
	// this and a failing MCP tool call is recorded as a success.
	if result != nil && result.IsError {
		return "", formatToolError(server, name, extractTextContent(result))
	}

	return extractTextContent(result), nil
}

// formatToolError builds the Go error for a tool call that reported failure
// via the MCP protocol's IsError flag rather than a transport-level error.
func formatToolError(server, name, text string) error {
	return fmt.Errorf("tool %s/%s reported an error: %s", server, name, text)
}

// extractTextContent concatenates all TextContent blocks from a CallToolResult.
func extractTextContent(result *mcpsdk.CallToolResult) string {
	if result == nil {
		return ""
	}

	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// Tools returns the most recently discovered tools. Call DiscoverTools first.
func (d *MCPDiscovery) Tools() []DiscoveredTool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.tools
}

// Statuses returns the per-server connect outcomes captured during the
// last ConnectAll, in deterministic name order. Each entry's NumTools
// is populated by DiscoverTools.
func (d *MCPDiscovery) Statuses() []ConnectStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]ConnectStatus, len(d.statuses))
	copy(out, d.statuses)
	return out
}

// Close closes all MCP server sessions.
func (d *MCPDiscovery) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for name, session := range d.sessions {
		if err := session.Close(); err != nil {
			if d.verbose {
				log.Printf("warning: error closing MCP session %q: %v", name, err)
			}
		}
	}
	d.sessions = make(map[string]*mcpsdk.ClientSession)
	d.tools = nil
}
