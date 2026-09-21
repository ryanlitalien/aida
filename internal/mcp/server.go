// Package mcp implements a Model Context Protocol (MCP) server for Aida.
//
// The server exposes Aida operations as MCP tools over stdio transport,
// allowing Claude Code and other MCP clients to query Aida, search the
// brain, manage tasks, and access entity knowledge programmatically.
//
// Start with: aida serve
// Connect from Claude Code via .mcp.json:
//
//	{
//	  "mcpServers": {
//	    "aida": {
//	      "command": "aida",
//	      "args": ["serve"]
//	    }
//	  }
//	}
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
)

// Server is the MCP tool server. It runs in one of two shapes:
//
//   - stdio proxy (per Claude Code session): NewServer with a daemonURL. Tool
//     calls are forwarded to the always-on HTTP daemon so a single, current
//     aida instance does the work; the brain is opened lazily only if the
//     daemon is unreachable and we must fall back to in-process.
//   - in-process dispatcher (inside the daemon): NewDispatcher over the
//     daemon's own brain handle, exposed via HTTPHandler at /mcp/call.
type Server struct {
	brain     *brain.Brain
	ownsBrain bool // we opened brain and must Close it (vs. an injected handle)
	cfg       *config.Config
	profile   string

	daemonURL string // non-empty ⇒ proxy tool calls here (http://host:port)
	httpc     *http.Client
	proxy     bool
	mu        sync.Mutex // guards lazy brain open on fallback
}

// NewServer builds the stdio MCP server. When daemonURL is non-empty it runs in
// proxy mode (forwarding tool calls to the daemon, brain opened lazily only on
// fallback); otherwise it opens the brain and serves in-process.
func NewServer(cfg *config.Config, profile, daemonURL string) (*Server, error) {
	s := &Server{
		cfg:       cfg,
		profile:   profile,
		daemonURL: strings.TrimRight(daemonURL, "/"),
		httpc:     &http.Client{Timeout: 30 * time.Second},
	}
	if s.daemonURL != "" {
		s.proxy = true
		return s, nil
	}
	if err := s.openBrain(); err != nil {
		return nil, err
	}
	return s, nil
}

// NewDispatcher builds an in-process tool dispatcher over a brain handle owned
// by the caller (the HTTP daemon). It never opens or closes that brain.
func NewDispatcher(b *brain.Brain, cfg *config.Config, profile string) *Server {
	return &Server{brain: b, cfg: cfg, profile: profile}
}

// Proxying reports whether tool calls are forwarded to the daemon.
func (s *Server) Proxying() bool { return s.proxy }

// openBrain opens and takes ownership of the brain handle.
func (s *Server) openBrain() error {
	brn, err := brain.Open(s.cfg.BrainPath(), s.profile, s.cfg.VoyageKeyEnv(), s.cfg.Brain.GitHubRepo())
	if err != nil {
		return err
	}
	s.brain = brn
	s.ownsBrain = true
	return nil
}

// ensureBrain lazily opens the brain for in-process fallback.
func (s *Server) ensureBrain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.brain != nil {
		return nil
	}
	return s.openBrain()
}

// Close closes the brain only if this server opened it.
func (s *Server) Close() error {
	if s.ownsBrain && s.brain != nil {
		return s.brain.Close()
	}
	return nil
}

// JSON-RPC types for MCP protocol
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP protocol types
type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
}

type capabilities struct {
	Tools *toolsCap `json:"tools,omitempty"`
}

type toolsCap struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

type inputSchema struct {
	Type       string             `json:"type"`
	Properties map[string]propDef `json:"properties,omitempty"`
	Required   []string           `json:"required,omitempty"`
}

type propDef struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Serve runs the MCP server on stdio.
func (s *Server) Serve() error {
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		resp := s.handleRequest(&req)
		if resp != nil {
			encoder.Encode(resp)
		}
	}
}

func (s *Server) handleRequest(req *jsonrpcRequest) *jsonrpcResponse {
	switch req.Method {
	case "initialize":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: initializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities: capabilities{
					Tools: &toolsCap{},
				},
				ServerInfo: serverInfo{
					Name:    "aida",
					Version: "1.0.0",
				},
			},
		}

	case "notifications/initialized":
		return nil // notification, no response

	case "tools/list":
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  s.toolsList(),
		}

	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return &jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid params: " + err.Error()},
			}
		}
		result := s.callTool(params)
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  result,
		}

	default:
		return &jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + req.Method},
		}
	}
}

func (s *Server) toolsList() toolsListResult {
	return toolsListResult{
		Tools: []toolDef{
			{
				Name:        "brain_search",
				Description: "Semantic search against the Aida brain knowledge base. Returns similar past lessons, entity pages, curated knowledge pages (brain/knowledge/domains: source profiles and hand-written domain notes), and routing wisdom.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"query": {Type: "string", Description: "The search query"},
						"limit": {Type: "number", Description: "Max results (default 5)"},
					},
					Required: []string{"query"},
				},
			},
			{
				Name:        "brain_recall",
				Description: "Recall durable memories captured from coding-agent sessions and call recordings (facts, preferences, and project notes the user's coding assistants and calls saved). Four profiles: \"claude\" (Claude Code, the default -- memory files captured live), \"codex\" (OpenAI Codex, LLM-distilled from session transcripts), \"gemini\" (Gemini CLI/Antigravity, LLM-distilled plus mirrored walkthrough/plan artifacts and GEMINI.md), and \"meetily\" (LLM-distilled from exported Meetily call recordings, each also classified with tags -- see tag param). Two modes: semantic (default -- embeds the query and ranks by similarity) and recency (set recent=true, or ask for the 'last/latest' memory -- returns the newest). Optionally scope-filter with scope=\"global\" or scope=\"project:<slug>\" (a project scope also includes global memories), and/or tag-filter with tag=\"<tag>\" (e.g. a meetily classifier tag like \"cta\" to recall only that project's calls). Distinct from brain_search, which covers lessons/entities, not these memories.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"query":   {Type: "string", Description: "What to recall, in natural language. Optional when recent=true."},
						"scope":   {Type: "string", Description: "Optional scope filter: 'global' or 'project:<slug>'."},
						"tag":     {Type: "string", Description: "Optional tag filter: only records carrying this tag (case-insensitive), e.g. a meetily classifier tag like 'cta'."},
						"limit":   {Type: "number", Description: "Max results (default 5)"},
						"recent":  {Type: "boolean", Description: "List the newest memories instead of semantic search."},
						"profile": {Type: "string", Description: "Memory profile to search: 'claude' (default), 'codex', 'gemini', 'meetily', or 'all' to search every profile."},
					},
				},
			},
			{
				Name:        "brain_get_page",
				Description: "Read a brain entity page (partner, tool, or person).",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"type": {Type: "string", Description: "Entity type: partners, tools, or people"},
						"slug": {Type: "string", Description: "Entity slug (filename without .md)"},
					},
					Required: []string{"type", "slug"},
				},
			},
			{
				Name:        "tasks_list",
				Description: "List brain tasks with optional tag filtering. Filtered to the active profile by default. Default status filter is open + in-progress; pass include_done or status to broaden.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"tag":          {Type: "string", Description: "Filter by tag (substring match)"},
						"include_done": {Type: "boolean", Description: "Include all statuses (overrides default open + in-progress filter)"},
						"all_profiles": {Type: "boolean", Description: "Bypass profile isolation and list every profile's tasks"},
						"status":       {Type: "string", Description: "Comma-separated status filter (open, in-progress, hold, deferred, done, closed). Overrides include_done."},
					},
				},
			},
			{
				Name:        "tasks_add",
				Description: "Create a new brain task.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"title": {Type: "string", Description: "Task title"},
						"tags":  {Type: "string", Description: "Comma-separated tags (e.g. 'project:aida,p1')"},
					},
					Required: []string{"title"},
				},
			},
			{
				Name:        "tasks_done",
				Description: "Mark a brain task as completed.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"slug": {Type: "string", Description: "Task slug, partial match, or '#N' stable task ID"},
					},
					Required: []string{"slug"},
				},
			},
			{
				Name:        "tasks_reopen",
				Description: "Reopen a completed brain task.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"slug": {Type: "string", Description: "Task slug, partial match, or '#N' stable task ID"},
					},
					Required: []string{"slug"},
				},
			},
			{
				Name:        "tasks_edit",
				Description: "Edit a brain task's title, body, tags, or status without opening an editor.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"slug":        {Type: "string", Description: "Task slug, partial match, or '#N' stable task ID"},
						"title":       {Type: "string", Description: "New title (optional)"},
						"body":        {Type: "string", Description: "Replace body text; pass empty string to clear (optional)"},
						"tags":        {Type: "string", Description: "Comma-separated tags, replaces the full tag list; mutually exclusive with add_tags/remove_tags (optional)"},
						"add_tags":    {Type: "string", Description: "Comma-separated tags to add (optional)"},
						"remove_tags": {Type: "string", Description: "Comma-separated tags to remove (optional)"},
						"status":      {Type: "string", Description: "New status: open, in-progress, hold, deferred, done, or closed (optional)"},
					},
					Required: []string{"slug"},
				},
			},
			{
				Name:        "tasks_show",
				Description: "Read a brain task's full markdown body (title + notes, frontmatter stripped).",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"slug": {Type: "string", Description: "Task slug, partial match, or '#N' stable task ID"},
					},
					Required: []string{"slug"},
				},
			},
			{
				Name:        "tasks_get",
				Description: "Get a brain task's structured metadata (slug, title, status, tags, task_id, issue_number, completed_at) as JSON. completed_at is the RFC3339 timestamp of when the task entered a terminal status (done/closed); empty for non-terminal tasks.",
				InputSchema: inputSchema{
					Type: "object",
					Properties: map[string]propDef{
						"slug": {Type: "string", Description: "Task slug, partial match, or '#N' stable task ID"},
					},
					Required: []string{"slug"},
				},
			},
			{
				Name:        "brain_stats",
				Description: "Get brain statistics: lesson count, entity count, task counts, DB size.",
				InputSchema: inputSchema{
					Type: "object",
				},
			},
		},
	}
}

// callTool routes a tool call: proxy to the daemon when configured, else run
// it in-process. On a daemon failure it falls back to in-process for that call
// (opening the brain lazily) and keeps proxy on so the next call retries the
// daemon - a daemon restart recovers transparently.
func (s *Server) callTool(params toolCallParams) toolResult {
	if s.proxy {
		res, err := s.forwardToDaemon(params)
		if err == nil {
			return res
		}
		if berr := s.ensureBrain(); berr != nil {
			return errorResult(fmt.Sprintf("daemon at %s unreachable (%v) and local fallback failed: %v", s.daemonURL, err, berr))
		}
		fmt.Fprintf(os.Stderr, "Aida MCP: daemon unreachable (%v); serving this call in-process\n", err)
	}
	return s.dispatchLocal(params)
}

// forwardToDaemon POSTs the tool call to the running HTTP daemon's /mcp/call
// and returns the toolResult it produced. A tool-level error comes back as a
// normal toolResult{IsError:true} with HTTP 200; a non-200 or transport error
// is returned as an error so callTool can fall back to in-process.
func (s *Server) forwardToDaemon(params toolCallParams) (toolResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return toolResult{}, err
	}
	req, err := http.NewRequest(http.MethodPost, s.daemonURL+"/mcp/call", bytes.NewReader(body))
	if err != nil {
		return toolResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpc.Do(req)
	if err != nil {
		return toolResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return toolResult{}, fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out toolResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolResult{}, err
	}
	return out, nil
}

// HTTPHandler serves POST /mcp/call on the daemon: decode a tool call, run it
// in-process against this dispatcher's brain, and return the toolResult JSON.
// This is the endpoint the per-session stdio proxies forward to.
func (s *Server) HTTPHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var params toolCallParams
		if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.dispatchLocal(params))
	}
}

// dispatchLocal executes a tool call against the in-process brain.
func (s *Server) dispatchLocal(params toolCallParams) toolResult {
	ctx := context.Background()

	switch params.Name {
	case "brain_search":
		query, _ := params.Arguments["query"].(string)
		limit := 5
		if l, ok := params.Arguments["limit"].(float64); ok {
			limit = int(l)
		}
		sc, err := s.brain.Search(ctx, query, nil, limit)
		if err != nil {
			return errorResult(err.Error())
		}
		var out strings.Builder
		if len(sc.SimilarLessons) > 0 {
			fmt.Fprintf(&out, "## Similar Lessons (%d)\n\n", len(sc.SimilarLessons))
			for _, sl := range sc.SimilarLessons {
				fmt.Fprintf(&out, "- (sim=%.2f) %q → %s [quality %d/5]\n",
					sl.Similarity, sl.Lesson.Question, strings.Join(sl.Lesson.Sources, ","), sl.Lesson.Quality)
			}
		}
		if len(sc.EntityPages) > 0 {
			fmt.Fprintf(&out, "\n## Entity Pages (%d)\n\n", len(sc.EntityPages))
			for _, ep := range sc.EntityPages {
				fmt.Fprintf(&out, "### %s\n%s\n\n", ep.Entity.Name, ep.Content)
			}
		}
		if len(sc.KnowledgePages) > 0 {
			fmt.Fprintf(&out, "\n## Knowledge Pages (%d)\n\n", len(sc.KnowledgePages))
			for _, kp := range sc.KnowledgePages {
				fmt.Fprintf(&out, "### %s (%s)\n%s\n\n", kp.Title, kp.Path, strings.TrimSpace(kp.Body))
			}
		}
		if sc.RoutingWisdom != "" {
			fmt.Fprintf(&out, "\n## Routing Wisdom\n\n%s\n", sc.RoutingWisdom)
		}
		if len(sc.OpenTasks) > 0 {
			fmt.Fprintf(&out, "\n## Open Tasks (%d)\n\n", len(sc.OpenTasks))
			for _, t := range sc.OpenTasks {
				fmt.Fprintf(&out, "- [%s] %s (%s)\n", t.PriorityTag(), t.Title, t.DisplayTags())
			}
		}
		if out.Len() == 0 {
			return textResult("No results found.")
		}
		return textResult(out.String())

	case "brain_recall":
		query, _ := params.Arguments["query"].(string)
		limit := 5
		if l, ok := params.Arguments["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		scope, _ := params.Arguments["scope"].(string)
		tag, _ := params.Arguments["tag"].(string)
		recent := false
		if r, ok := params.Arguments["recent"].(bool); ok {
			recent = r
		}
		profile := brain.ClaudeMemoryProfile
		if p, ok := params.Arguments["profile"].(string); ok && p != "" {
			profile = p
		}
		res, err := s.brain.RecallMemories(ctx, query, limit, profile, scope, tag, recent)
		if err != nil {
			return errorResult(err.Error())
		}
		if len(res.Memories) == 0 {
			return textResult("No memories found.")
		}
		var out strings.Builder
		mode := "most recent"
		if res.BySimilarity {
			mode = "semantic"
		}
		fmt.Fprintf(&out, "## Recalled Memories (%d, %s)\n\n", len(res.Memories), mode)
		for _, m := range res.Memories {
			r := m.Record
			created := r.Created
			if len(created) >= 10 {
				created = created[:10]
			}
			if res.BySimilarity {
				fmt.Fprintf(&out, "- (sim=%.2f) [%s] %s - %s\n", m.Similarity, r.Type, r.Key, created)
			} else {
				fmt.Fprintf(&out, "- [%s] %s - %s\n", r.Type, r.Key, created)
			}
			body := strings.ReplaceAll(strings.TrimSpace(r.Body), "\n", " ")
			if len(body) > 300 {
				body = body[:300] + "..."
			}
			fmt.Fprintf(&out, "  %s\n", body)
		}
		return textResult(out.String())

	case "brain_get_page":
		entityType, _ := params.Arguments["type"].(string)
		slug, _ := params.Arguments["slug"].(string)
		path := fmt.Sprintf("%s/entities/%s/%s.md", s.brain.Path, entityType, slug)
		data, err := os.ReadFile(path)
		if err != nil {
			return errorResult("Page not found: " + err.Error())
		}
		return textResult(string(data))

	case "tasks_list":
		var tags []string
		if tag, ok := params.Arguments["tag"].(string); ok && tag != "" {
			tags = []string{tag}
		}
		showDone := false
		if d, ok := params.Arguments["include_done"].(bool); ok {
			showDone = d
		}
		// status param wins over include_done. Default (no status, no
		// include_done) shows the active set (open + in-progress).
		var statuses []string
		if statusStr, ok := params.Arguments["status"].(string); ok && statusStr != "" {
			for _, s := range strings.Split(statusStr, ",") {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				if !brain.IsValidStatus(s) {
					return errorResult(fmt.Sprintf("invalid status %q", s))
				}
				statuses = append(statuses, s)
			}
		} else if !showDone {
			statuses = brain.ActiveStatuses()
		}
		// Profile isolation is enforced inside brain.ListTasks. Callers
		// that genuinely need cross-profile data must pass all_profiles=true.
		allProfiles := false
		if v, ok := params.Arguments["all_profiles"].(bool); ok {
			allProfiles = v
		}
		var (
			tasks []brain.TaskRecord
			err   error
		)
		switch {
		case allProfiles && len(statuses) > 0:
			tasks, err = s.brain.ListTasksAllProfilesByStatus(statuses, tags, 0, 0)
		case allProfiles:
			tasks, err = s.brain.ListTasksAllProfiles(showDone, tags, 0, 0)
		case len(statuses) > 0:
			tasks, err = s.brain.ListTasksByStatus(statuses, tags, 0, 0)
		default:
			tasks, err = s.brain.ListTasks(showDone, tags, 0, 0)
		}
		if err != nil {
			return errorResult(err.Error())
		}
		if len(tasks) == 0 {
			return textResult("No tasks found.")
		}
		var out strings.Builder
		for _, t := range tasks {
			fmt.Fprintf(&out, "#%d [%s] %s %s %s\n", t.TaskID, t.Status, t.PriorityTag(), t.Title, t.DisplayTags())
		}
		return textResult(out.String())

	case "tasks_add":
		title, _ := params.Arguments["title"].(string)
		if title == "" {
			return errorResult("title is required")
		}
		var tags []string
		if tagStr, ok := params.Arguments["tags"].(string); ok && tagStr != "" {
			tags = strings.Split(tagStr, ",")
			for i := range tags {
				tags[i] = strings.TrimSpace(tags[i])
			}
		}
		if s.cfg.Brain.AutoSync {
			brain.PullAndWait(s.cfg.BrainPath())
		}
		task, err := s.brain.AddTask(title, tags, "")
		if err != nil {
			return errorResult(err.Error())
		}
		if s.cfg.Brain.AutoSync {
			brain.CommitAndPush(s.cfg.BrainPath(), s.profile)
		}
		return textResult(fmt.Sprintf("Task added: %s", task.Slug))

	case "tasks_done":
		slug, _ := params.Arguments["slug"].(string)
		if slug == "" {
			return errorResult("slug is required")
		}
		resolved, err := s.brain.ResolveTaskRef(slug)
		if err != nil {
			return errorResult(err.Error())
		}
		task, err := s.brain.CompleteTask(resolved.Slug)
		if err != nil {
			return errorResult(err.Error())
		}
		if s.cfg.Brain.AutoSync {
			brain.CommitAndPush(s.cfg.BrainPath(), s.profile)
		}
		return textResult(fmt.Sprintf("Done: #%d %s", task.TaskID, task.Slug))

	case "tasks_reopen":
		slug, _ := params.Arguments["slug"].(string)
		if slug == "" {
			return errorResult("slug is required")
		}
		resolved, err := s.brain.ResolveTaskRef(slug)
		if err != nil {
			return errorResult(err.Error())
		}
		task, err := s.brain.ReopenTask(resolved.Slug)
		if err != nil {
			return errorResult(err.Error())
		}
		if s.cfg.Brain.AutoSync {
			brain.CommitAndPush(s.cfg.BrainPath(), s.profile)
		}
		return textResult(fmt.Sprintf("Reopened: #%d %s", task.TaskID, task.Slug))

	case "tasks_edit":
		slug, _ := params.Arguments["slug"].(string)
		if slug == "" {
			return errorResult("slug is required")
		}
		resolved, err := s.brain.ResolveTaskRef(slug)
		if err != nil {
			return errorResult(err.Error())
		}

		patch := brain.TaskPatch{}
		if v, ok := params.Arguments["title"].(string); ok {
			patch.Title = &v
		}
		if v, ok := params.Arguments["body"].(string); ok {
			patch.Body = &v
		}
		if v, ok := params.Arguments["tags"].(string); ok && v != "" {
			t := splitCSV(v)
			patch.Tags = &t
		}
		if v, ok := params.Arguments["add_tags"].(string); ok && v != "" {
			patch.AddTags = splitCSV(v)
		}
		if v, ok := params.Arguments["remove_tags"].(string); ok && v != "" {
			patch.RemoveTags = splitCSV(v)
		}
		if v, ok := params.Arguments["status"].(string); ok && v != "" {
			patch.Status = &v
		}

		task, err := s.brain.UpdateTask(resolved.Slug, patch)
		if err != nil {
			return errorResult(err.Error())
		}
		if s.cfg.Brain.AutoSync {
			brain.CommitAndPush(s.cfg.BrainPath(), s.profile)
		}
		return textResult(fmt.Sprintf("Task updated: #%d %s - %s", task.TaskID, task.Slug, task.Title))

	case "tasks_show":
		slug, _ := params.Arguments["slug"].(string)
		if slug == "" {
			return errorResult("slug is required")
		}
		resolved, err := s.brain.ResolveTaskRef(slug)
		if err != nil {
			return errorResult(err.Error())
		}
		data, err := os.ReadFile(s.brain.TaskFilePath(resolved.Slug))
		if err != nil {
			return errorResult("task file not found: " + err.Error())
		}
		return textResult(stripFrontmatter(string(data)))

	case "tasks_get":
		slug, _ := params.Arguments["slug"].(string)
		if slug == "" {
			return errorResult("slug is required")
		}
		resolved, err := s.brain.ResolveTaskRef(slug)
		if err != nil {
			return errorResult(err.Error())
		}
		data, err := json.MarshalIndent(resolved, "", "  ")
		if err != nil {
			return errorResult(err.Error())
		}
		return textResult(string(data))

	case "brain_stats":
		stats := s.brain.GetStats()
		taskLine := brain.FormatTaskCounts(stats.TasksByStatus)
		if taskLine == "" {
			taskLine = "(none)"
		}
		return textResult(fmt.Sprintf(
			"Lessons: %d\nEntities: %d\nTasks: %s\nDB size: %d bytes\nEmbeddings: %v\nLast lesson: %s",
			stats.LessonCount, stats.EntityCount, taskLine,
			stats.DBSize, stats.HasEmbeddings, stats.LastLesson))

	default:
		return errorResult("unknown tool: " + params.Name)
	}
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

func errorResult(msg string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: "Error: " + msg}}, IsError: true}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stripFrontmatter drops a leading "---\n...\n---\n" YAML block.
// Returns the original string if no frontmatter is present.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return s
	}
	return strings.TrimLeft(rest[end+5:], "\n")
}
