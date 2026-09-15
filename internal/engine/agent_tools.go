package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine/orchestrator"
	"github.com/ryanlitalien/aida/internal/execx"
	"github.com/ryanlitalien/aida/internal/mcp"
	"github.com/ryanlitalien/aida/internal/sources"
)

// AgentTool is a tool available to the agent / orchestrator loop.
type AgentTool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	// Execute runs the tool and returns a text result or error string.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// BuildAgentTools constructs the set of tools available to the agent loop
// from four categories:
//  1. Aida source adapters (sqlite, grep, exec, etc.)
//  2. MCP tools discovered from connected servers
//  3. Meta-tools (delegate_to_claude_code, ask_user)
//  4. The submit_result terminal tool (see runner.SubmitResultToolName)
//
// confirmFn is called before executing destructive MCP tools. If nil,
// destructive tools are blocked.
//
// claudeConfigDir is a trailing variadic so existing call sites keep
// compiling unchanged; pass at most one value, the Claude config
// directory (CLAUDE_CONFIG_DIR) delegate_to_claude_code should set on
// its child process. Which directory to route to which kind of work is
// a decision for the caller (driven by aida's own config), never
// hardcoded here -- see buildDelegateToClaudeCodeTool.
func BuildAgentTools(
	srcs config.Sources,
	mcpTools []mcp.DiscoveredTool,
	mcpDiscovery *mcp.MCPDiscovery,
	confirmFn func(action string) bool,
	confirmPatterns []string,
	claudeConfigDir ...string,
) []AgentTool {
	var tools []AgentTool

	// --- 1. Aida source adapters ---
	for name, src := range srcs {
		tools = append(tools, buildSourceTool(name, src))
	}

	// --- 2. MCP tools ---
	for _, mt := range mcpTools {
		tools = append(tools, buildMCPTool(mt, mcpDiscovery, confirmFn, confirmPatterns))
	}

	// --- 3. Meta-tools ---
	tools = append(tools, buildDelegateToClaudeCodeTool(firstOrEmpty(claudeConfigDir)))
	tools = append(tools, buildAskUserTool())

	// --- 4. Terminal tool ---
	tools = append(tools, buildSubmitResultTool())

	return tools
}

// firstOrEmpty returns vs[0] if present, else "". Backs the trailing
// variadic config-dir parameter on BuildAgentTools / BuildAgentToolsLazy.
func firstOrEmpty(vs []string) string {
	if len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// queryHintForType returns type-specific guidance for how to query a source.
func queryHintForType(sourceType string) string {
	switch sourceType {
	case "sqlite", "data-source":
		return "Provide a SQL SELECT statement (or the query payload the source's exec template expects). Use table and column names from the source's documentation."
	case "codebase", "grep":
		return "Provide a search pattern (regex supported). Example: 'handleCheckout|processPayment'"
	case "git":
		return "Provide git log arguments. Example: '--since=\"1 week\" --oneline' or '--author=\"Name\" -5'"
	case "claude-project":
		return "Provide a natural language question for the sub-agent. Be specific - include identifiers, dates, and context."
	case "csv":
		return "Provide one of: 'latest N', '<asset> latest N', '<asset>', 'filter: <substring>'"
	case "web-search":
		return "Provide a concise search query, like what you'd type into Google."
	case "tool", "exec":
		return "Provide the exact CLI command to execute."
	case "docs":
		return "Provide a search term to find in documentation."
	default:
		return "Provide the query appropriate for this source type."
	}
}

// buildSourceTool wraps a aida source adapter as an AgentTool.
func buildSourceTool(name string, src *config.Source) AgentTool {
	hint := queryHintForType(src.Type)
	description := fmt.Sprintf("Query the %q source (%s). %s", name, src.Type, hint)
	if src.Description != "" {
		description = fmt.Sprintf("%s %s", src.Description, hint)
	}

	return AgentTool{
		Name:        name,
		Description: description,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": hint,
				},
			},
			"required": []interface{}{"query"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if args.Query == "" {
				return "", fmt.Errorf("query is required")
			}

			adapter := sources.GetAdapterForSource(name, *src)
			if adapter == nil {
				return "", fmt.Errorf("no adapter found for source %q (type %s)", name, src.Type)
			}

			result, err := adapter.Execute(ctx, args.Query, *src)
			if err != nil {
				return "", err
			}

			// A timed-out source must surface as a tool error, not ordinary
			// output -- otherwise the agent loop (and the job's
			// events.ndjson) records "Command timed out" as a successful
			// result.
			if result.Status == "error" || result.Status == "timeout" {
				return "", fmt.Errorf("%s", result.Summary)
			}

			// Build a text summary from the result.
			var out strings.Builder
			if result.Summary != "" {
				out.WriteString(result.Summary)
			}
			if len(result.Artifacts) > 0 {
				if out.Len() > 0 {
					out.WriteString("\n\nArtifacts:\n")
				}
				for _, a := range result.Artifacts {
					fmt.Fprintf(&out, "- [%s] %s: %s\n", a.Type, a.ID, a.Snippet)
				}
			}
			if out.Len() == 0 {
				return "(no results)", nil
			}
			return out.String(), nil
		},
	}
}

// buildMCPTool wraps an MCP discovered tool as an AgentTool.
func buildMCPTool(
	mt mcp.DiscoveredTool,
	discovery *mcp.MCPDiscovery,
	confirmFn func(action string) bool,
	confirmPatterns []string,
) AgentTool {
	// Namespace the tool name to avoid collisions: mcp__{server}__{name}
	fullName := fmt.Sprintf("mcp__%s__%s", mt.Server, mt.Name)

	return AgentTool{
		Name:        fullName,
		Description: mt.Description,
		InputSchema: mt.InputSchema,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			// Determine whether this tool needs confirmation: either it is
			// marked destructive by the MCP server, or it matches a
			// profile-level guardrail pattern.
			needsConfirm := mt.Destructive
			for _, pattern := range confirmPatterns {
				if strings.Contains(mt.Name, pattern) || strings.Contains(fullName, pattern) {
					needsConfirm = true
					break
				}
			}

			if needsConfirm {
				if confirmFn == nil {
					return "", fmt.Errorf("tool %q requires confirmation but no confirmation function is available", mt.Name)
				}
				if !confirmFn(fmt.Sprintf("Allow MCP tool %s/%s?", mt.Server, mt.Name)) {
					return "", fmt.Errorf("user declined MCP tool %s/%s", mt.Server, mt.Name)
				}
			}

			// Parse input into a map for the MCP caller.
			var args map[string]interface{}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid MCP tool input: %w", err)
			}

			result, err := discovery.CallTool(ctx, mt.Server, mt.Name, args)
			if err != nil {
				return "", err
			}
			if result == "" {
				return "(no output)", nil
			}
			return result, nil
		},
	}
}

// delegateToClaudeCodeTimeout bounds how long a delegated Claude Code
// sub-agent may run before it is killed. Without a real timeout here,
// a hung `claude` subprocess hangs the enclosing job forever -- the
// wall-clock ceiling mirrors the philosophy of aida loop's own
// per-task timeout (see the "--per-task-timeout" flag), sized larger
// because a delegated task is often open-ended code work rather than
// one bounded loop iteration.
const delegateToClaudeCodeTimeout = 15 * time.Minute

// buildDelegateToClaudeCodeTool creates a meta-tool that delegates a task
// to a Claude Code sub-agent running in a specified directory. This is
// useful for code generation, refactoring, and PR creation tasks.
//
// Optional `isolate` parameter (default false) runs claude inside a
// fresh git worktree off HEAD and returns the resulting diff
// inline - the user's working tree is never touched. Per OpenAI's
// harness writeup: "we made the app bootable per git worktree, so
// Codex could launch and drive one instance per change." Use
// isolate=true for risky/exploratory tasks; the agent's reply will
// include the diff and the user applies manually.
//
// claudeConfigDir, when non-empty, is set as CLAUDE_CONFIG_DIR on the
// child process so the sub-agent picks up a specific Claude Code
// config/credentials directory instead of whatever the daemon happened
// to inherit. Empty means today's behavior: no override, ambient
// inheritance. The routing decision -- which config dir applies to
// which kind of delegated work -- belongs to the caller (driven by
// aida's own configuration); this function only knows how to apply
// whatever it's given.
func buildDelegateToClaudeCodeTool(claudeConfigDir string) AgentTool {
	return AgentTool{
		Name:        "delegate_to_claude_code",
		Description: "Delegate a task to a Claude Code sub-agent. The sub-agent runs in the specified directory with full access to MCP tools, file editing, and terminal. Use this for code generation, refactoring, debugging, or creating pull requests. Pass isolate=true to run in a discarded git worktree and return the diff inline (preview-only - does not modify the user's working tree).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task": map[string]interface{}{
					"type":        "string",
					"description": "A detailed natural-language description of the task for Claude Code to perform.",
				},
				"directory": map[string]interface{}{
					"type":        "string",
					"description": "The working directory for Claude Code (e.g. a project root). Defaults to the current directory if omitted.",
				},
				"isolate": map[string]interface{}{
					"type":        "boolean",
					"description": "When true, run claude in a fresh git worktree at HEAD and return the diff inline; the worktree is discarded and the original directory is never modified. Requires the directory to be a git repository. Defaults to false (run in-place).",
				},
			},
			"required": []interface{}{"task"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Task      string `json:"task"`
				Directory string `json:"directory"`
				Isolate   bool   `json:"isolate"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if args.Task == "" {
				return "", fmt.Errorf("task description is required")
			}

			runDir := args.Directory
			var worktreeCleanup func()
			isolated := false
			if args.Isolate {
				if runDir == "" {
					return "", fmt.Errorf("isolate=true requires an explicit directory (need a git repo to spawn the worktree)")
				}
				worktree, cleanup, err := spawnDelegateWorktree(runDir)
				if err != nil {
					return "", fmt.Errorf("worktree isolation failed: %w", err)
				}
				worktreeCleanup = cleanup
				runDir = worktree
				isolated = true
			}
			// Defer is safe with nil - no-op. Always cleans up
			// when isolation was used, regardless of success/error.
			defer func() {
				if worktreeCleanup != nil {
					worktreeCleanup()
				}
			}()

			// Resolve the claude binary explicitly, up front, rather
			// than handing exec.Command a bare "claude" and finding
			// out mid-run whether PATH even has it. This also pins
			// down which "claude" we're about to run, instead of
			// leaving it to whatever PATH lookup exec.Command performs
			// internally against the environment the daemon inherited.
			claudePath, lookErr := exec.LookPath("claude")
			if lookErr != nil {
				return "", fmt.Errorf("claude code binary not found on PATH (PATH=%s): %w", os.Getenv("PATH"), lookErr)
			}

			// Build the claude command. Use execx so a backgrounded
			// process the subagent might spawn can't pin our output
			// pipes open past the ambient context deadline, and give
			// it a real ceiling of its own -- without one, a hung
			// claude subprocess would hang this tool call (and
			// whatever job is waiting on it) forever.
			opts := execx.RunOpts{Timeout: delegateToClaudeCodeTimeout}
			if runDir != "" {
				opts.Dir = runDir
			}
			if claudeConfigDir != "" {
				// Env replaces the child's environment wholesale (nil
				// means inherit), so start from the ambient
				// environment and layer the override on top rather
				// than relying on CLAUDE_CONFIG_DIR already being set
				// in it -- that would just be ambient inheritance
				// again, the thing this is meant to replace.
				opts.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+claudeConfigDir)
			}
			res, err := execx.RunCombined(ctx, claudePath, []string{"--print", args.Task}, opts)
			if err != nil {
				return "", fmt.Errorf("claude code failed (binary=%s): %w\nOutput: %s", claudePath, err, string(res.Combined))
			}

			result := strings.TrimSpace(string(res.Combined))
			if isolated {
				diff := captureWorktreeDiff(runDir)
				// Append the diff after claude's narrative output
				// so the agent (and the user reading the
				// transcript) can see exactly what would have
				// happened. The worktree itself is discarded by
				// the deferred cleanup.
				if diff == "" {
					result += "\n\n---\nDIFF: (claude touched nothing in the worktree)"
				} else {
					result += "\n\n---\nDIFF (worktree discarded - apply manually if you want these changes):\n" + diff
				}
			}
			if result == "" {
				return "(claude code produced no output)", nil
			}
			return result, nil
		},
	}
}

// buildAskUserTool creates a meta-tool that asks the user a question
// and reads their response from stdin.
func buildAskUserTool() AgentTool {
	return AgentTool{
		Name:        "ask_user",
		Description: "Ask the user a clarifying question. Use this when you need more information to complete the task, such as choosing between options or confirming an action.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"question": map[string]interface{}{
					"type":        "string",
					"description": "The question to ask the user.",
				},
			},
			"required": []interface{}{"question"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Question string `json:"question"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if args.Question == "" {
				return "", fmt.Errorf("question is required")
			}

			fmt.Printf("\n%s Agent asks: %s\n> ", "?", args.Question)
			scanner := bufio.NewScanner(os.Stdin)
			if scanner.Scan() {
				return strings.TrimSpace(scanner.Text()), nil
			}
			if err := scanner.Err(); err != nil {
				return "", fmt.Errorf("reading user input: %w", err)
			}
			return "", fmt.Errorf("no input received")
		},
	}
}

// buildSubmitResultTool creates the terminal agent tool that lets an
// agent declare a verified outcome instead of ending its run on free
// prose that a downstream reader might mistake for success. The
// orchestrator Runner recognizes this tool by name
// (orchestrator.SubmitResultToolName) and, on a valid call, ends the
// run immediately with Termination "final_response" and the parsed
// payload attached to RunResult.Result. A run that never calls this
// tool carries a nil Result -- callers must treat that as unverified,
// never inferring success from FinalOutput alone.
//
// This tool's own Execute only validates; it never decides to end the
// run itself (the orchestrator does that once, before falling through
// to ordinary tool dispatch -- see driveAgent's handling of
// SubmitResultToolName). Execute exists so a malformed call still
// looks and behaves like any other failed tool call if it ever reaches
// ordinary dispatch (e.g. because validation failed and the Runner let
// it fall through for a retry).
func buildSubmitResultTool() AgentTool {
	return AgentTool{
		Name:        orchestrator.SubmitResultToolName,
		Description: "Declare the final, verified outcome of this task and end the run. Call this exactly once, when you have either achieved the goal or determined it cannot be achieved -- never rely on ordinary text to report success. failure_code is required when outcome is \"not_achieved\".",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"outcome": map[string]interface{}{
					"type":        "string",
					"enum":        []interface{}{string(orchestrator.OutcomeAchieved), string(orchestrator.OutcomeNotAchieved)},
					"description": "Whether the task's goal was achieved.",
				},
				"summary": map[string]interface{}{
					"type":        "string",
					"description": "A short account of what was done (or attempted).",
				},
				"failure_code": map[string]interface{}{
					"type":        "string",
					"description": "A short, stable, machine-readable reason the task was not achieved. Required when outcome is \"not_achieved\".",
				},
			},
			"required": []interface{}{"outcome", "summary"},
		},
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			sr, err := orchestrator.ParseSubmitResult(input)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("result recorded: outcome=%s", sr.Outcome), nil
		},
	}
}
