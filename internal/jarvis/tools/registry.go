// Package tools is the in-process tool registry Jarvis exposes to the LLM.
// Direct Go calls into internal/brain - no MCP, no subprocess.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/calendar"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/mcp"
)

// lastEngineRunID carries the engine run id of the most recent
// aida_query invocation across a side-channel that the LLM client
// drains after each tool call. Used to attach engine_run_id to
// audit.ToolCall so voice feedback tools can target the right aida run.
// Single-writer (the aida_query goroutine) / single-reader (the LLM
// dispatch loop) - atomic.Value is sufficient.
var lastEngineRunID atomic.Value // string

// SetLastEngineRunID stashes the engine run id parsed out of an
// `aida <query>` subprocess. Called from inside aida_query immediately
// before its Run returns. Exported so other tools or tests can reset.
func SetLastEngineRunID(id string) { lastEngineRunID.Store(id) }

// TakeLastEngineRunID reads and clears the stash. The LLM client calls
// this after each tool dispatch; non-empty values get attached to the
// ToolCallStat for whichever tool just ran.
func TakeLastEngineRunID() string {
	v := lastEngineRunID.Swap("")
	if v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// engineRunIDPattern matches the `[aida-run-id:<id>]` sentinel that
// `aida <query>` emits on stderr after recording the lesson.
var engineRunIDPattern = regexp.MustCompile(`\[aida-run-id:([^\]]+)\]`)

// Tool is a single LLM-callable function. Run receives the turn's ctx -
// the same one AskWithHistory was called with, possibly carrying a
// request-scoped value like jarvis.withLocalAudioSuppressed - so a tool
// that itself schedules a deferred audio ack (aida_query's progressAck)
// can thread the turn's origin through to it. Every other tool ignores
// the parameter; it exists on the shared type because Go function types
// must match exactly across the registry, not because most tools need it.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Schema      map[string]interface{} `json:"input_schema"`
	Run         func(ctx context.Context, input json.RawMessage) (string, error)
}

// Registry holds all tools available to Jarvis. Construct one per session.
// Holds an optional MCPDiscovery handle for the MCP find/call dispatcher
// tools; nil when no MCP servers are configured or all failed to connect.
type Registry struct {
	tools     []Tool
	discovery *mcp.MCPDiscovery
}

// New takes a Brain plus the user's home location string. Home is used as
// the default for tools that need a location (weather) when the user
// doesn't say one explicitly.
// domains is the list of aida source names known to be available at
// startup. Injected into the aida_query tool description so the LLM
// router can see what data domains it can reach. Empty/nil falls back
// to a generic description.
//
// discovery is the MCP client used by mcp_find_tool / mcp_call_tool to
// reach external systems (Slack, Gmail, Calendar, Notion, ...). Nil is
// tolerated - the MCP dispatcher tools are skipped in that case so the
// rest of Jarvis still works. day_summary also takes discovery (for its
// calendar section) but stays registered unconditionally even when
// discovery is nil, since its time/weather/tasks sections must not
// disappear just because no MCP client is configured, see
// daySummaryCalendarLine for how it degrades instead of skipping.
//
// jobsStore enables the long-running-agent voice tools (job_start,
// job_status, job_list, …). nil disables that whole class of tools
// - push-to-talk CLI invocations don't pass one and the resulting
// LLM prompt simply omits them.
func New(b *brain.Brain, home string, domains []string, discovery *mcp.MCPDiscovery, jobsStore *jobs.Store, progressAck func(context.Context), lastTurn LastTurnFn, autoSync bool) *Registry {
	r := &Registry{discovery: discovery}
	r.register(tasksListTool(b))
	r.register(taskGetTool(b))
	r.register(tasksAddTool(b, autoSync))
	r.register(taskDoneTool(b))
	r.register(taskStatusTool(b))
	r.register(taskEditTagsTool(b))
	r.register(daySummaryTool(b, home, discovery))
	r.register(currentTimeTool())
	r.register(weatherTool(home))
	r.register(aidaQueryTool(domains, progressAck))
	r.register(ingestNotionTasksTool())
	r.register(minecraftAskTool())
	if b != nil {
		r.register(claudeMemoryRecallTool(b))
		r.register(memorySaveTool(b))
	}
	if discovery != nil {
		r.register(mcpFindTool(discovery))
		r.register(mcpCallTool(discovery))
		r.register(calendarScheduleTool(discovery))
	}
	if jobsStore != nil {
		r.register(jobStartTool(jobsStore))
		r.register(jobStatusTool(jobsStore))
		r.register(jobListTool(jobsStore))
		r.register(jobSendInputTool(jobsStore))
		r.register(jobCancelTool(jobsStore))
		r.register(approveJobTool(jobsStore))
		r.register(rejectJobTool(jobsStore))
	}
	if b != nil && lastTurn != nil {
		r.register(jarvisThumbsUpTool(b, lastTurn))
		r.register(jarvisThumbsDownTool(b, lastTurn))
		r.register(jarvisNoteTool(b, lastTurn))
	}
	// brain_search intentionally not registered for v1 - the demo only
	// exercises tasks_list. Add later when we have voice queries that need
	// freeform brain lookup.
	return r
}

func (r *Registry) register(t Tool) { r.tools = append(r.tools, t) }

// Register adds a tool after construction. It exists so a persona-specific
// registry (Aida's, which gains the roster dispatch tools) can diverge from
// the default set without changing New's signature or the tools it registers
// for every other persona.
func (r *Registry) Register(t Tool) { r.register(t) }

// All returns every tool definition (for sending to the LLM).
func (r *Registry) All() []Tool { return r.tools }

// Run dispatches a tool call by name. ctx is the calling turn's context -
// see Tool.Run's doc comment for why it's threaded through.
func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	for _, t := range r.tools {
		if t.Name == name {
			return t.Run(ctx, input)
		}
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

// ─── tasks_list ──────────────────────────────────────────────────────────────

type tasksListInput struct {
	Tag   string `json:"tag"`
	Limit int    `json:"limit"`
}

func tasksListTool(b *brain.Brain) Tool {
	return Tool{
		Name: "tasks_list",
		Description: "List the user's TODO/project tasks, sorted by priority (p1 > p2 > p3). " +
			"These are tasks the user has explicitly added to their task tracker. " +
			"Filter by tag (e.g. 'acme-widgets', 'work', 'home'). " +
			"Returns task title, status, priority tag, and id. " +
			"NOT for activity/personal-data history (workouts, sleep, fitness, " +
			"health, finances, etc.) - use aida_query for those.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"tag":   map[string]interface{}{"type": "string", "description": "filter by tag, e.g. 'acme-widgets'"},
				"limit": map[string]interface{}{"type": "integer", "description": "max tasks to return (default 10)"},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in tasksListInput
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 10
			}
			var tags []string
			if in.Tag != "" {
				tags = []string{in.Tag}
			}
			// open + in-progress (default visible statuses)
			tasks, err := b.ListTasksByStatus([]string{"open", "in-progress"}, tags, 0, 100)
			if err != nil {
				return "", err
			}
			// sort: p1 first, p2 next, p3 last; within tier keep existing order
			sort.SliceStable(tasks, func(i, j int) bool {
				return priorityRank(tasks[i].PriorityTag()) < priorityRank(tasks[j].PriorityTag())
			})
			if len(tasks) > limit {
				tasks = tasks[:limit]
			}
			if len(tasks) == 0 {
				if in.Tag != "" {
					return fmt.Sprintf("no open tasks tagged '%s'", in.Tag), nil
				}
				return "no open tasks", nil
			}
			var sb strings.Builder
			for i, t := range tasks {
				fmt.Fprintf(&sb, "%d. [%s] %s (%s)\n", i+1, t.PriorityTag(), t.Title, t.Status)
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// ─── task_get ────────────────────────────────────────────────────────────────

type taskGetInput struct {
	Ref string `json:"ref"`
}

func taskGetTool(b *brain.Brain) Tool {
	return Tool{
		Name: "task_get",
		Description: "Return details about a single task by reference. Accepts the " +
			"stable task ID (e.g. '162' or '#162'), an exact slug, or a partial " +
			"slug substring. Use this when the user names a specific task " +
			"('tell me about task 162', 'what's on the blah-blah task'). Returns " +
			"title, status, priority, tags, and the body markdown.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "task ID like '162' or '#162', exact slug, or partial-slug substring",
				},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in taskGetInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Ref) == "" {
				return "", fmt.Errorf("task_get: empty ref")
			}
			rec, err := resolveTaskRef(b, in.Ref)
			if err != nil {
				return "", fmt.Errorf("task_get: %w", err)
			}
			body, _ := readTaskBody(b, rec.Slug)
			var sb strings.Builder
			fmt.Fprintf(&sb, "Task #%d (%s)\n", rec.TaskID, rec.Slug)
			fmt.Fprintf(&sb, "Title: %s\n", rec.Title)
			fmt.Fprintf(&sb, "Status: %s\n", rec.Status)
			fmt.Fprintf(&sb, "Priority: %s\n", rec.PriorityTag())
			if dt := rec.DisplayTags(); dt != "" {
				fmt.Fprintf(&sb, "Tags: %s\n", dt)
			}
			fmt.Fprintf(&sb, "Created: %s\n", rec.Created)
			if body != "" {
				// Cap body at 1500 chars so a giant task doesn't blow
				// the prompt budget. The model can re-fetch if needed.
				if len(body) > 1500 {
					body = body[:1500] + "...[truncated]"
				}
				fmt.Fprintf(&sb, "\nBody:\n%s\n", body)
			}
			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// readTaskBody reads the task's markdown file and strips the YAML
// frontmatter (everything between the first two `---` lines).
func readTaskBody(b *brain.Brain, slug string) (string, error) {
	path := b.TaskFilePath(slug)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(data)
	if !strings.HasPrefix(s, "---\n") {
		return strings.TrimSpace(s), nil
	}
	// Find the closing --- on its own line after the opening one.
	rest := s[4:]
	if i := strings.Index(rest, "\n---"); i >= 0 {
		body := rest[i+4:]
		body = strings.TrimLeft(body, "\n ")
		return strings.TrimSpace(body), nil
	}
	return strings.TrimSpace(s), nil
}

func priorityRank(tag string) int {
	switch tag {
	case "p1":
		return 1
	case "p2":
		return 2
	default:
		return 3
	}
}

// ─── aida_query ────────────────────────────────────────────────────────────

// aidaQueryTool delegates open-ended questions to the broader aida
// engine (`aida <query>`) - weather, web search, partner lookups, codebase
// search, anything in the user's library of sources. Use for anything
// outside the dedicated Jarvis tools (tasks, time).
//
// Implementation note: shells out to `aida` rather than calling the engine
// in-process so that profile resolution, library routing, and source
// execution all use the binary's own logic. See aidaQueryTimeout below for
// why the subprocess deadline is what it is. Memory
// feedback_no_mcp_query_tool prohibits exposing aida_query as an MCP tool
// (sub-agent recursion); this is safe because Jarvis is the top-level
// agent, calling out to aida via subprocess.

// aidaQueryTimeout bounds a single `aida <query>` subprocess call. Raised
// once already from 90s (commit 109efbb) to 180s for multi-source queries
// (workouts, finances) and GitHub fan-outs that occasionally push past 90s
// (audit incident 2026-05-22T00:57Z). That was not enough either: audit
// task #193 (2026-06-24) shows two consecutive aida_query calls in one turn
// hitting the wall at 180004ms and 180012ms, on a request that chained an
// email search, a correlation to a GitHub issue, and drafting that issue,
// plus a second subprocess hop through the claude_fallback connector path.
//
// Composed, multi-step work like that belongs behind job_start, which has
// no wall-clock limit and reports back asynchronously on the next wake --
// see the routing guidance in job_start's and aida_query's descriptions,
// and in the base system prompt (internal/jarvis/llm/client.go). This
// constant is a modest cushion for genuinely simple, single-hop queries,
// not a fix for chained work; raising it again on its own already failed
// once and is not the answer a second time.
const aidaQueryTimeout = 240 * time.Second

type aidaQueryInput struct {
	Query string `json:"query"`
}

func aidaQueryTool(domains []string, progressAck func(context.Context)) Tool {
	// ANSI escape stripper so spinner codes from aida's UI don't pollute
	// the tool result Claude sees.
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*(\x07|\x1b\\)`)
	desc := "Run an open-ended natural-language question through the user's full " +
		"aida engine. USE THIS for ANY personal-data lookup or factual question: " +
		"workouts, sleep, fitness, health, finances, partner data, news, definitions, " +
		"codebase search, AND anything in the user's connected accounts - email/Gmail, " +
		"calendar, Slack, Drive, Airtable, Notion, CRM (aida reaches those through " +
		"the connected systems even though you can't directly). Anything that isn't a " +
		"TODO task or the current time. NEVER tell the user you can't access their " +
		"accounts - use this instead. If unsure between this and tasks_list, prefer " +
		"this. Returns aida's synthesized answer. This tool is for a SINGLE " +
		"self-contained question, even a slow one. For a request that chains " +
		"multiple steps together, such as searching email for an incident, " +
		"correlating it to a GitHub issue, and drafting a new issue, use " +
		"job_start instead of calling aida_query more than once for the same " +
		"request -- aida_query has a hard time limit per call, job_start does not."
	if len(domains) > 0 {
		sorted := append([]string{}, domains...)
		sort.Strings(sorted)
		desc += " Available data domains (registered aida sources): " + strings.Join(sorted, ", ") + "."
	}
	return Tool{
		Name:        "aida_query",
		Description: desc,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "the user's question, in natural language",
				},
			},
			"required": []string{"query"},
		},
		Run: func(turnCtx context.Context, raw json.RawMessage) (string, error) {
			var in aidaQueryInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Query) == "" {
				return "", fmt.Errorf("aida_query: empty query")
			}
			// The subprocess deadline is deliberately built from
			// Background, not turnCtx: it must outlive whatever caller
			// deadline is in play (see aidaQueryTimeout's doc comment for
			// why 240s is the right ceiling regardless of the caller).
			// turnCtx is kept around separately, below, purely so
			// progressAck can see the turn's origin (desk mic vs. LMD/
			// Android) when it fires.
			ctx, cancel := context.WithTimeout(context.Background(), aidaQueryTimeout)
			defer cancel()

			// See aidaQueryTimeout's doc comment above for why this number
			// is what it is. At the 90s mark, roughly a third of the way
			// in, we fire a progress ack so the user knows we're still
			// alive instead of sitting in silence for the rest of the wait.
			// progressAck itself (jarvis.Assistant.progressAck) decides
			// whether that ack may actually play locally - passing
			// turnCtx here is what lets it see whether this turn came in
			// with local audio suppressed (an LMD/Android turn).
			done := make(chan struct{})
			if progressAck != nil {
				go func() {
					select {
					case <-time.After(90 * time.Second):
						progressAck(turnCtx)
					case <-done:
					}
				}()
			}

			cmd := exec.CommandContext(ctx, "aida", in.Query)
			out, err := cmd.CombinedOutput()
			close(done)
			clean := ansi.ReplaceAllString(string(out), "")

			// Parse + stash the engine run id so the LLM dispatch loop
			// can attach it to this tool call's stat. The voice feedback
			// tools later use it to target `aida thumbs-* <run-id>` against
			// the exact run, not the most-recent-aida-run heuristic.
			if m := engineRunIDPattern.FindStringSubmatch(clean); len(m) == 2 {
				SetLastEngineRunID(m[1])
				clean = engineRunIDPattern.ReplaceAllString(clean, "")
			}
			clean = strings.TrimSpace(clean)
			if err != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return clean, aidaQueryTimeoutError()
				}
				return clean, fmt.Errorf("aida subprocess: %w", err)
			}
			return clean, nil
		},
	}
}

// aidaQueryTimeoutError is the error returned when a subprocess call hits
// aidaQueryTimeout. Steers the model toward job_start on retry instead of
// just trying the same aida_query call again with the same hard limit.
// Extracted as its own function so the message text is unit-testable
// without spawning a subprocess.
func aidaQueryTimeoutError() error {
	return fmt.Errorf("aida_query timed out after %s: this looks like composed, "+
		"multi-step work (search, correlate, draft, etc.) rather than a single "+
		"question; rephrase the request as background work via job_start instead "+
		"of retrying aida_query", aidaQueryTimeout)
}

// ─── weather ─────────────────────────────────────────────────────────────────

// weatherTool calls wttr.in (free, no API key) for a one-line current
// conditions summary. Much faster than routing through the full aida
// engine + web-search hop. The system prompt steers Claude to prefer this
// for weather questions instead of aida_query.

type weatherInput struct {
	Location string `json:"location"`
}

func weatherTool(homeDefault string) Tool {
	return Tool{
		Name: "weather",
		Description: "Get current weather conditions for a location. Returns " +
			"a one-line summary including condition, temperature in Fahrenheit, " +
			"humidity, and wind. PREFER THIS over aida_query for weather " +
			"questions - it's much faster.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"location": map[string]interface{}{
					"type": "string",
					"description": "city/state/zip/country, e.g. 'Boston, MA' or 'Seattle'. " +
						"If omitted, uses the user's home location.",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in weatherInput
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
			}
			loc := strings.TrimSpace(in.Location)
			if loc == "" {
				loc = homeDefault
			}
			if loc == "" {
				return "", fmt.Errorf("weather: no location given and no home default configured")
			}
			// 12s - wttr.in can be slow on first hit but usually returns
			// in <2s. Slightly longer than the previous 5s ceiling so a
			// cold cache hit doesn't spuriously fail.
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			// %C: condition, %t: temp (F via u=us), %h: humidity, %w: wind
			u := "https://wttr.in/" + url.PathEscape(loc) + "?format=%C+%t+%h+%w&u"
			req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
			req.Header.Set("User-Agent", "aida-jarvis/1.0")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(os.Stderr, "weather tool: http error: %v\n", err)
				return "", fmt.Errorf("wttr.in transport: %w", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			out := strings.TrimSpace(string(body))
			if resp.StatusCode != http.StatusOK || out == "" {
				fmt.Fprintf(os.Stderr, "weather tool: bad response: status=%d body=%q\n", resp.StatusCode, out)
				return "", fmt.Errorf("wttr.in status=%d body=%q", resp.StatusCode, out)
			}
			// wttr.in occasionally returns a "Sorry, we are running out
			// of queries..." plain-text message with status 200. Detect
			// it so we can surface a clearer error.
			lo := strings.ToLower(out)
			if strings.Contains(lo, "sorry") || strings.Contains(lo, "unknown location") {
				fmt.Fprintf(os.Stderr, "weather tool: wttr.in soft-error: %q\n", out)
				return "", fmt.Errorf("wttr.in soft-error: %s", out)
			}
			// e.g. "Partly cloudy +57°F 64% ↘8mph"
			return fmt.Sprintf("Current conditions in %s: %s", loc, out), nil
		},
	}
}

// ─── minecraft_ask ───────────────────────────────────────────────────────────

// minecraftAskTool shells out to `ssh <host> <remote-script> "<query>"`
// against a Minecraft server host, where host and remote-script come from
// config.MinecraftConfig (the minecraft: block of config.yaml, see
// internal/config/minecraft.go) rather than being hardcoded here. The
// remote script runs Claude on that host with the target repo's own
// CLAUDE.md + LESSONS.md as context. Direct path so voice replies land in
// ~5–10s instead of going through the full aida pipeline (which would
// also work via a library source pointed at the same host, but adds
// parse + plan + synthesize hops on top of the SSH'd Claude call).
//
// Registered unconditionally (see New below); an unconfigured minecraft:
// block degrades at call time to a clear "not configured" error from the
// Run closure, the same pattern calendar_schedule uses in calendar.go for
// its own config-gated block - whether this is configured can only be
// known by reading config.yaml, not from a constructor argument the
// registry already has in hand at registration time.

type minecraftAskInput struct {
	Query string `json:"query"`
}

// isExitCode reports whether err is an *exec.ExitError with the given code.
// `timeout` exits 124 when its deadline fires.
func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == code
	}
	return false
}

func minecraftAskTool() Tool {
	return Tool{
		Name: "minecraft_ask",
		Description: "Run a request against the configured Minecraft " +
			"server host. That host runs Claude with full project " +
			"context AND read+execute access to the `mc` admin wrapper, so " +
			"this tool CAN both answer questions and RUN commands on the " +
			"live server - summon entities, give items, set time/weather, " +
			"manage the allowlist/op list, send chat, toggle the prison " +
			"cell, REPAIR or MAX-ENCHANT the player's held item via " +
			"`mc maxench`, enchant specific items via `mc enchant`, " +
			"teleport the player via `mc tp spawn|bed`, tail logs, run " +
			"backups. Use this for ANYTHING Minecraft-related, whether " +
			"the user is asking a question (\"is the server up?\") OR " +
			"giving an imperative (\"spawn a black cat\", \"give me a " +
			"netherite sword\", \"set time to night\", \"repair my axe\", " +
			"\"max-enchant my pickaxe\"). Do NOT refuse a Minecraft " +
			"request on grounds of lacking authority - this tool IS the " +
			"authority. Prefer this over aida_query for any " +
			"Minecraft-server interaction; it reaches the live game " +
			"state, aida_query does not. If this tool reports it is not " +
			"configured, tell the user plainly instead of guessing at a cause.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "the user's question about the Minecraft server, in natural language",
				},
			},
			"required": []string{"query"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in minecraftAskInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			q := strings.TrimSpace(in.Query)
			if q == "" {
				return "", fmt.Errorf("minecraft_ask: empty query")
			}

			cfg, err := config.LoadConfig()
			if err != nil {
				return "", fmt.Errorf("minecraft_ask: loading config: %w", err)
			}
			mcCfg, err := cfg.MinecraftConfig()
			if err != nil {
				return "", fmt.Errorf("minecraft_ask: %w", err)
			}

			// SSH always evaluates the command argument with /bin/sh on
			// the remote side, so the query has to be single-quoted there.
			// Build the remote command as one shell-quoted string and pass
			// it as a single argv element to ssh - that way the local exec
			// doesn't shell-eval anything either.
			//
			// Wrap it in `timeout` so a runaway agent can't outlive our
			// local deadline: previously a local timeout/cancel killed only
			// the local ssh, leaving the remote `claude -p` running forever
			// (we found month-old orphans). The remote timeout fires just
			// under the local one - see MinecraftConfig.RemoteTimeoutSecs
			// for how that margin is derived/validated - guaranteeing the
			// remote process self-terminates; `-k 10` SIGKILLs if it
			// ignores the SIGTERM. (This kills the claude process; any
			// orphaned tool grandchildren are addressed at the root by the
			// remote repo's own log-tailing fix so they exit on their own.)
			remote := fmt.Sprintf("timeout -k 10 %d %s %s",
				mcCfg.RemoteTimeoutSecs, mcCfg.RemoteScript, shellQuoteSingle(q))
			// The remote `ask` script runs `claude -p` with tool access (mc
			// status/logs/cmd, file reads), which routinely takes 30-90s for
			// tool-heavy questions. A timeout here does NOT mean the server
			// is offline. ServerAliveInterval surfaces a dead network as an
			// ssh exit instead of a silent hang to the local deadline.
			ctx, cancel := context.WithTimeout(context.Background(), mcCfg.LocalTimeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "ssh",
				"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
				mcCfg.SSHHost, remote)
			out, err := cmd.CombinedOutput()
			s := strings.TrimSpace(string(out))
			if err != nil {
				// `timeout` exits 124 when it fired - treat that, and the
				// local deadline, as the same "slow but reachable" case.
				if ctx.Err() == context.DeadlineExceeded || isExitCode(err, 124) {
					return s, fmt.Errorf("minecraft_ask timed out after %ds; the remote claude call is slow but the server is reachable - do NOT tell the user the server is offline", mcCfg.RemoteTimeoutSecs)
				}
				return s, fmt.Errorf("ssh %s ask: %w", mcCfg.SSHHost, err)
			}
			return s, nil
		},
	}
}

// shellQuoteSingle wraps s in single quotes for safe inclusion in a
// /bin/sh command line. Embedded single quotes are escaped via the
// standard `'\”` close-reopen trick.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ─── tasks_add ───────────────────────────────────────────────────────────────

type tasksAddInput struct {
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	Body  string   `json:"body"`
}

func tasksAddTool(b *brain.Brain, autoSync bool) Tool {
	return Tool{
		Name: "tasks_add",
		Description: "Create a new TODO/project task in the user's task tracker. " +
			"Use this when the user says \"add a task to ...\", \"remind me to ...\", " +
			"or otherwise asks Jarvis to record a new task. Returns the created " +
			"task's stable ID and title so the spoken reply can echo it back.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"title": map[string]interface{}{
					"type":        "string",
					"description": "the task title, in natural language",
				},
				"tags": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "optional tags, e.g. ['acme-widgets', 'p1']",
				},
				"body": map[string]interface{}{
					"type":        "string",
					"description": "optional longer-form context paragraph",
				},
			},
			"required": []string{"title"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in tasksAddInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			title := strings.TrimSpace(in.Title)
			if title == "" {
				return "", fmt.Errorf("tasks_add: empty title")
			}
			if autoSync {
				brain.PullAndWait(b.Path)
			}
			rec, err := b.AddTask(title, in.Tags, in.Body)
			if err != nil {
				return "", fmt.Errorf("tasks_add: %w", err)
			}
			return fmt.Sprintf("Added task #%d: %s", rec.TaskID, rec.Title), nil
		},
	}
}

// ─── task_done ───────────────────────────────────────────────────────────────

type taskDoneInput struct {
	Ref string `json:"ref"`
}

func taskDoneTool(b *brain.Brain) Tool {
	return Tool{
		Name: "task_done",
		Description: "Mark a task complete (status: done). Accepts the stable " +
			"task ID (e.g. '162' or '#162'), an exact slug, or a partial slug " +
			"substring. Use this when the user says \"mark task X done\", \"I " +
			"finished X\", or \"complete X\". The reply echoes the task's title " +
			"so the user can verify the right one was completed.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "task ID like '162' or '#162', exact slug, or partial-slug substring",
				},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in taskDoneInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Ref) == "" {
				return "", fmt.Errorf("task_done: empty ref")
			}
			rec, err := resolveTaskRef(b, in.Ref)
			if err != nil {
				return "", fmt.Errorf("task_done: %w", err)
			}
			done, err := b.CompleteTask(rec.Slug)
			if err != nil {
				return "", fmt.Errorf("task_done: %w", err)
			}
			return fmt.Sprintf("Completed task #%d: %s", done.TaskID, done.Title), nil
		},
	}
}

// normalizeTaskRef accepts user-spoken forms ("task 162", "162", "#162",
// "task-slug", or a conversational phrase like "water the plants") and
// returns a string ResolveTaskRef will accept.
//
// A conversational phrase gets slugified with brain.SlugifyTaskTitle before
// being handed to ResolveTaskRef, which falls back to a SQL `slug LIKE
// '%partial%'` query for anything that isn't a "#N" id or an exact slug.
// Stored slugs are hyphenated (e.g. "2026-05-12-water-the-plants"); a raw,
// space-separated phrase can never match that LIKE, which is why "water
// the plants" used to come back "no task matching" even when the task
// existed (task #172). brain.SlugifyTaskTitle is the same normalization
// slugifyTask uses to build that stored slug (minus the date prefix, which
// a conversational reference never includes), so the two always agree.
func normalizeTaskRef(raw string) string {
	ref := strings.TrimSpace(raw)
	if ref == "" {
		return ""
	}
	ref = strings.TrimPrefix(strings.ToLower(ref), "task ")
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if strings.HasPrefix(ref, "#") {
		return ref
	}
	if isAllDigits(ref) {
		return "#" + ref
	}
	return brain.SlugifyTaskTitle(ref)
}

// resolveTaskRef normalizes raw (see normalizeTaskRef) and resolves it to a
// task record, falling back to a case-insensitive title-substring search
// when the slug-based lookup misses. The fallback matters when a task's
// title was edited after creation: UpdateTask never renames the task's
// slug/file, so a conversational reference to the CURRENT title can outrun
// what the stored slug still says. Shared by task_get, task_done,
// task_status, and task_edit_tags so all four match the same way.
func resolveTaskRef(b *brain.Brain, raw string) (*brain.TaskRecord, error) {
	ref := normalizeTaskRef(raw)
	if ref == "" {
		return nil, fmt.Errorf("empty task ref")
	}
	rec, err := b.ResolveTaskRef(ref)
	if err == nil {
		return rec, nil
	}
	// "#N" is unambiguous, so a miss there is a real not-found, not a
	// slugification mismatch, and the fallback is skipped.
	if strings.HasPrefix(ref, "#") {
		return nil, err
	}
	if rec, terr := findTaskByTitleSubstring(b, raw); terr == nil {
		return rec, nil
	}
	return nil, err
}

// findTaskByTitleSubstring is resolveTaskRef's last-resort fallback: a
// case-insensitive substring match against each task's current title,
// across every status. Scoped to the brain's active profile like every
// other task lookup.
func findTaskByTitleSubstring(b *brain.Brain, raw string) (*brain.TaskRecord, error) {
	needle := strings.ToLower(strings.TrimSpace(raw))
	if needle == "" {
		return nil, fmt.Errorf("empty title reference")
	}
	tasks, err := b.ListTasksByStatus(brain.AllStatuses(), nil, 0, 500)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if strings.Contains(strings.ToLower(tasks[i].Title), needle) {
			return &tasks[i], nil
		}
	}
	return nil, fmt.Errorf("no task matching %q", raw)
}

// ─── task_status ─────────────────────────────────────────────────────────────

type taskStatusInput struct {
	Ref    string `json:"ref"`
	Status string `json:"status"`
}

func taskStatusTool(b *brain.Brain) Tool {
	statuses := brain.AllStatuses()
	return Tool{
		Name: "task_status",
		Description: "Change a task's status to one of: " + strings.Join(statuses, ", ") +
			". Use this when the user wants to put a task on hold (\"put X on " +
			"hold\"), defer it (\"push X off\"), reopen it, close it as no-longer-" +
			"relevant, or start work (\"I'm starting on X\" -> in-progress). For " +
			"plain completion (\"mark X done\"), prefer task_done - it has a " +
			"simpler signature. Accepts the same ref formats as task_done.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "task ID like '162' or '#162', exact slug, or partial-slug substring",
				},
				"status": map[string]interface{}{
					"type":        "string",
					"enum":        statuses,
					"description": "new status",
				},
			},
			"required": []string{"ref", "status"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in taskStatusInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Ref) == "" {
				return "", fmt.Errorf("task_status: empty ref")
			}
			status := strings.TrimSpace(in.Status)
			if !brain.IsValidStatus(status) {
				return "", fmt.Errorf("task_status: invalid status %q (want one of: %s)",
					status, strings.Join(statuses, ", "))
			}
			rec, err := resolveTaskRef(b, in.Ref)
			if err != nil {
				return "", fmt.Errorf("task_status: %w", err)
			}
			updated, err := b.SetTaskStatus(rec.Slug, status)
			if err != nil {
				return "", fmt.Errorf("task_status: %w", err)
			}
			return fmt.Sprintf("Task #%d is now %s: %s", updated.TaskID, updated.Status, updated.Title), nil
		},
	}
}

// ─── task_edit_tags ──────────────────────────────────────────────────────────

type taskEditTagsInput struct {
	Ref        string   `json:"ref"`
	AddTags    []string `json:"add_tags"`
	RemoveTags []string `json:"remove_tags"`
}

func taskEditTagsTool(b *brain.Brain) Tool {
	return Tool{
		Name: "task_edit_tags",
		Description: "Add or remove tags on an existing task without opening an " +
			"editor. Use when the user says \"tag X as p1\", \"add the home tag " +
			"to X\", \"remove the work tag from X\". Both add_tags and " +
			"remove_tags are optional arrays; at least one must be non-empty. " +
			"Accepts the same ref formats as task_done.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"ref": map[string]interface{}{
					"type":        "string",
					"description": "task ID like '162' or '#162', exact slug, or partial-slug substring",
				},
				"add_tags": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "tags to add",
				},
				"remove_tags": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "tags to remove",
				},
			},
			"required": []string{"ref"},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			var in taskEditTagsInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			if strings.TrimSpace(in.Ref) == "" {
				return "", fmt.Errorf("task_edit_tags: empty ref")
			}
			if len(in.AddTags) == 0 && len(in.RemoveTags) == 0 {
				return "", fmt.Errorf("task_edit_tags: add_tags and remove_tags both empty")
			}
			rec, err := resolveTaskRef(b, in.Ref)
			if err != nil {
				return "", fmt.Errorf("task_edit_tags: %w", err)
			}
			patch := brain.TaskPatch{
				AddTags:    in.AddTags,
				RemoveTags: in.RemoveTags,
			}
			updated, err := b.UpdateTask(rec.Slug, patch)
			if err != nil {
				return "", fmt.Errorf("task_edit_tags: %w", err)
			}
			tags := updated.DisplayTags()
			if tags == "" {
				tags = "(none)"
			}
			return fmt.Sprintf("Updated tags on task #%d (%s). Now: %s", updated.TaskID, updated.Title, tags), nil
		},
	}
}

// ─── day_summary ─────────────────────────────────────────────────────────────

func daySummaryTool(b *brain.Brain, home string, discovery *mcp.MCPDiscovery) Tool {
	return Tool{
		Name: "day_summary",
		Description: "Compose a morning briefing for the user: top five open or " +
			"in-progress tasks (priority-sorted), the current local time, current " +
			"home weather, and what's on the calendar for today. Use when the user " +
			"asks \"what's my day look like\", \"morning briefing\", \"give me the " +
			"rundown\". Returns a plain-text dossier; phrase it naturally in the " +
			"spoken reply. The calendar line is honest about its own coverage -- if " +
			"it says the schedule couldn't be fully checked, or that no calendar " +
			"connection is available, say that plainly rather than implying the day " +
			"is clear.",
		Schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			var sb strings.Builder

			// now is captured once and reused for both the Time line and the
			// calendar lookup below, so the summary can never describe two
			// different days.
			now := time.Now()
			fmt.Fprintf(&sb, "Time: %s, %s\n",
				now.Format("3:04 PM"),
				now.Format("Monday, January 2, 2006"),
			)

			// Weather, tasks, and calendar are independent sections: a
			// failure in one must never suppress or alter another.
			if home != "" {
				if w, err := fetchWeatherShort(home); err == nil && w != "" {
					fmt.Fprintf(&sb, "Weather (%s): %s\n", home, w)
				}
			}

			tasks, err := b.ListTasksByStatus([]string{"open", "in-progress"}, nil, 0, 100)
			if err == nil {
				sort.SliceStable(tasks, func(i, j int) bool {
					return priorityRank(tasks[i].PriorityTag()) < priorityRank(tasks[j].PriorityTag())
				})
				if len(tasks) > 5 {
					tasks = tasks[:5]
				}
				if len(tasks) == 0 {
					sb.WriteString("Tasks: no open tasks.\n")
				} else {
					sb.WriteString("Top tasks:\n")
					for i, t := range tasks {
						fmt.Fprintf(&sb, "%d. [%s] %s (%s)\n", i+1, t.PriorityTag(), t.Title, t.Status)
					}
				}
			}

			fmt.Fprintf(&sb, "Calendar: %s\n", daySummaryCalendarLine(ctx, now, discovery))

			return strings.TrimRight(sb.String(), "\n"), nil
		},
	}
}

// daySummaryCalendarLine resolves today's schedule for day_summary and
// always returns a non-empty, honest sentence about it -- never silently
// omitted, and never phrased as "nothing on the calendar" unless
// calendar.FetchSchedule's own ClearAllowed/Speech actually says so. now
// must be the same instant day_summary used for its Time line, so the
// calendar lookup can't land on a different day than the rest of the
// briefing.
//
// A nil discovery (no MCP client configured for this session) and a
// calendar: config error are both reported directly here, before
// FetchSchedule is ever called -- FetchSchedule needs a live MCPCaller and
// a resolved config to run at all, so there is no ScheduleResult to read
// Speech from in either case. Once FetchSchedule does run, its
// deterministic Speech field is used verbatim rather than re-describing
// events here, so the calendar line stays exactly as trustworthy as the
// structured result it summarizes.
func daySummaryCalendarLine(ctx context.Context, now time.Time, discovery *mcp.MCPDiscovery) string {
	if discovery == nil {
		return "no calendar connection is available right now, so I can't check today's schedule."
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Sprintf("I couldn't check today's schedule: %v.", err)
	}
	calCfg, err := cfg.CalendarConfig()
	if err != nil {
		return fmt.Sprintf("I couldn't check today's schedule: %v.", err)
	}

	req := calendar.ScheduleRequest{
		When:   calendar.When{Kind: calendar.WhenDay, Offset: 0},
		Period: calendar.PeriodDay,
	}
	result := calendar.FetchSchedule(ctx, now, req, calCfg, discovery)
	return result.Speech
}

// fetchWeatherShort returns a one-line wttr.in summary for the given
// location. Shared with weatherTool so day_summary doesn't duplicate the
// transport logic.
func fetchWeatherShort(loc string) (string, error) {
	if strings.TrimSpace(loc) == "" {
		return "", fmt.Errorf("empty location")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	u := "https://wttr.in/" + url.PathEscape(loc) + "?format=%C+%t+%h+%w&u"
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", "aida-jarvis/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	out := strings.TrimSpace(string(body))
	if resp.StatusCode != http.StatusOK || out == "" {
		return "", fmt.Errorf("wttr.in status=%d", resp.StatusCode)
	}
	lo := strings.ToLower(out)
	if strings.Contains(lo, "sorry") || strings.Contains(lo, "unknown location") {
		return "", fmt.Errorf("wttr.in soft-error")
	}
	return out, nil
}

// ─── current_time ────────────────────────────────────────────────────────────

func currentTimeTool() Tool {
	return Tool{
		Name: "current_time",
		Description: "Return the current local date and time. Use this whenever the user asks " +
			"the time, the date, the day of the week, or anything time-relative " +
			"like \"what's on my schedule today\".",
		Schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		Run: func(_ context.Context, _ json.RawMessage) (string, error) {
			now := time.Now()
			return fmt.Sprintf("%s (%s, %s)",
				now.Format("3:04 PM"),
				now.Format("Monday, January 2, 2006"),
				now.Format("MST"),
			), nil
		},
	}
}
