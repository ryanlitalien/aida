// MCP find-and-call dispatcher tools for Jarvis. Two-tool design (see
// the parent plan): mcp_find_tool returns up to 10 candidate tools that
// match a free-form query against name+description; mcp_call_tool then
// invokes a chosen tool by server+name with a JSON arg blob. This is
// deliberately a generic dispatcher rather than per-server wrappers -
// one implementation covers all 13+ servers in the user's .mcp.json.
//
// Confirmation policy: locked to echo-confirm. The system prompt
// instructs Haiku to phrase the spoken reply to repeat back what was
// sent ("Sent to #foo on Slack: '...'") so the user can verify; the
// audit log at ~/.aida/brain/jarvis/audit.ndjson is the undo trail.
// There is intentionally no _confirmed gate.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ryanlitalien/aida/internal/mcp"
)

// maxFindResults caps the JSON blob returned to Haiku. Ten candidates
// is generous - when the query is reasonably specific the right tool is
// usually in the top three. Bigger lists just burn prompt tokens.
const maxFindResults = 10

// ─── mcp_find_tool ───────────────────────────────────────────────────────────

type mcpFindInput struct {
	Query string `json:"query"`
}

// mcpFindResult is the per-candidate shape returned to the LLM. Kept
// flat and minimal so Haiku can easily pick a server+name pair to
// feed into mcp_call_tool without re-parsing prose.
type mcpFindResult struct {
	Server      string                 `json:"server"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema,omitempty"`
}

func mcpFindTool(d *mcp.MCPDiscovery) Tool {
	return Tool{
		Name: "mcp_find_tool",
		Description: "Search across all connected external systems (Slack, " +
			"Gmail, Google Calendar, Notion, Airtable, Drive, etc.) for a tool " +
			"that can perform the user's request. Returns up to ten candidate " +
			"tools with their server, name, description, and input schema. " +
			"Always call this FIRST when the user wants to send a message, " +
			"create or look up an event, draft an email, query Notion or " +
			"Airtable, or do anything that touches an external system. Then " +
			"feed the chosen server+name into mcp_call_tool.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type": "string",
					"description": "free-form description of what you want to do, " +
						"e.g. 'send a slack message to a channel' or " +
						"'create a calendar event tomorrow'",
				},
			},
			"required": []string{"query"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in mcpFindInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			q := strings.TrimSpace(in.Query)
			if q == "" {
				return "", fmt.Errorf("mcp_find_tool: empty query")
			}
			results := rankMCPTools(d.Tools(), q)
			if len(results) > maxFindResults {
				results = results[:maxFindResults]
			}
			if len(results) == 0 {
				return "[] (no matching MCP tools - try a different query, or use aida_query)", nil
			}
			out, err := json.Marshal(results)
			if err != nil {
				return "", err
			}
			return string(out), nil
		},
	}
}

// rankMCPTools scores every discovered tool against the query and
// returns the matches sorted best-first. The scoring is intentionally
// simple - token-overlap against name+description+server - so the LLM,
// not us, makes the final pick. Anything with a positive score is
// returned; empty input returns the full list capped by the caller.
func rankMCPTools(all []mcp.DiscoveredTool, query string) []mcpFindResult {
	terms := tokenizeMCPQuery(strings.ToLower(query))
	if len(terms) == 0 {
		// No tokens after stop-word stripping - return everything, the
		// caller will trim. Better than returning empty for a vague query.
		out := make([]mcpFindResult, 0, len(all))
		for _, t := range all {
			out = append(out, toFindResult(t))
		}
		return out
	}
	type scored struct {
		r     mcpFindResult
		score int
	}
	var hits []scored
	for _, t := range all {
		haystack := strings.ToLower(t.Server + " " + t.Name + " " + t.Description)
		s := 0
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				s++
			}
		}
		if s > 0 {
			hits = append(hits, scored{r: toFindResult(t), score: s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]mcpFindResult, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.r)
	}
	return out
}

func toFindResult(t mcp.DiscoveredTool) mcpFindResult {
	return mcpFindResult{
		Server:      t.Server,
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
	}
}

// tokenizeMCPQuery splits on non-word chars and drops obvious stop
// tokens. Tiny stop-word list - we don't need a full IR pipeline, just
// enough to keep "send a slack message" from matching every tool because
// of "a".
func tokenizeMCPQuery(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		t := cur.String()
		cur.Reset()
		if len(t) < 2 {
			return
		}
		switch t {
		case "the", "and", "for", "with", "from", "into", "that", "this", "any":
			return
		}
		out = append(out, t)
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// ─── mcp_call_tool ───────────────────────────────────────────────────────────

type mcpCallInput struct {
	Server string                 `json:"server"`
	Name   string                 `json:"name"`
	Args   map[string]interface{} `json:"args"`
}

func mcpCallTool(d *mcp.MCPDiscovery) Tool {
	return Tool{
		Name: "mcp_call_tool",
		Description: "Invoke a specific MCP tool on a specific server. Use the " +
			"server+name pair from mcp_find_tool's results. Pass the tool's " +
			"arguments as the args object - its shape comes from the chosen " +
			"tool's input_schema. The call runs IMMEDIATELY; do not ask the " +
			"user to confirm. After the call returns, phrase your spoken " +
			"reply to ECHO BACK what was sent or created so the user can " +
			"verify by ear: \"Sent to #general on Slack: 'running " +
			"late, sir'.\" or \"Created calendar event 'Lunch with Mike' " +
			"for twelve thirty tomorrow.\" The audit log is the undo trail.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"server": map[string]interface{}{
					"type":        "string",
					"description": "MCP server name from mcp_find_tool",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "tool name from mcp_find_tool",
				},
				"args": map[string]interface{}{
					"type":        "object",
					"description": "arguments object matching the tool's input_schema",
				},
			},
			"required": []string{"server", "name"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in mcpCallInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Server) == "" || strings.TrimSpace(in.Name) == "" {
				return "", fmt.Errorf("mcp_call_tool: server and name are required")
			}
			args := in.Args
			if args == nil {
				args = map[string]interface{}{}
			}
			// Use Background so a slow MCP tool doesn't get cancelled by
			// the outer LLM round-trip ctx; discovery.CallTool already
			// imposes a per-call timeout (toolCallTimeout, 30s) inside.
			out, err := d.CallTool(context.Background(), in.Server, in.Name, args)
			if err != nil {
				return "", fmt.Errorf("mcp_call_tool: %w", err)
			}
			return out, nil
		},
	}
}
