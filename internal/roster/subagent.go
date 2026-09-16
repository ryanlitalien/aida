package roster

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
	"github.com/ryanlitalien/aida/internal/refusal"
)

// DefaultSubagentTimeout bounds a `claude --print` call when the entry
// doesn't set subagent.timeout_seconds. Mirrors claudeProjectTimeout in
// internal/sources/claude_project.go.
const DefaultSubagentTimeout = 120 * time.Second

// dispatchDepthEnvKey is read by `aida ask` (internal/cli/ask.go) as a
// one-hop depth guard: a subagent spawned via RunClaudeIn carries this
// var one higher than whatever it inherited, so an agent that itself
// calls `aida ask` is at depth 1 (allowed) and a second hop lands at
// depth 2, which ask.go refuses rather than letting agents call agents
// indefinitely.
const dispatchDepthEnvKey = "AIDA_DISPATCH_DEPTH"

// subagentBackend delegates a Request to a Claude Code subagent via
// `claude --print` run in a target directory -- the same recipe as
// internal/sources.ClaudeProjectAdapter.Execute, plus an optional
// prompt-directed persona targeting a named .claude/agents/ slug.
type subagentBackend struct {
	name    string
	dir     string
	agent   string
	model   string
	timeout time.Duration
}

// newSubagentBackend builds the subagentBackend for e, resolving the
// timeout override and expanding e.Subagent.Dir (which may carry a `~/`).
func newSubagentBackend(e *Entry, _ Deps) (Backend, error) {
	if e.Subagent == nil || e.Subagent.Dir == "" {
		return nil, fmt.Errorf("entry %q has kind %q but no subagent.dir", e.Name, KindSubagent)
	}
	timeout := DefaultSubagentTimeout
	if e.Subagent.Timeout > 0 {
		timeout = time.Duration(e.Subagent.Timeout) * time.Second
	}
	return &subagentBackend{
		name:    e.Name,
		dir:     config.ExpandPath(e.Subagent.Dir),
		agent:   e.Subagent.Agent,
		model:   e.Subagent.Model,
		timeout: timeout,
	}, nil
}

// Kind implements Backend.
func (b *subagentBackend) Kind() string { return KindSubagent }

// buildSubagentPrompt wraps task with a directive to delegate to the named
// subagent and reply with only its final report. An empty agent means
// plain claude-project delegation, so the task passes through verbatim --
// this is the whole targeting mechanism, since `claude --print` has no
// first-class flag to select a named subagent yet.
func buildSubagentPrompt(agent, task string) string {
	if agent == "" {
		return task
	}
	return fmt.Sprintf("Use the %s subagent for this request, and reply with only its final report.\n\nRequest: %s", agent, task)
}

// Ask implements Backend by delegating to RunClaudeIn with the process
// environment scrubbed of Anthropic credentials (so the sub-agent falls
// back to the OAuth/subscription path, same rationale as
// ClaudeProjectAdapter.Execute) and the dispatch-depth guard bumped.
//
// Backend problems -- empty output, a refusal, a timeout, a non-zero exit
// -- come back as a Result status, never a Go error: only a process that
// never ran at all would be a Go error, and there isn't one on this path.
func (b *subagentBackend) Ask(ctx context.Context, req Request) (Result, error) {
	prompt := buildSubagentPrompt(b.agent, req.Task)
	env := BumpDispatchDepthEnv(config.ScrubAnthropicCreds(os.Environ()))
	res := RunClaudeIn(ctx, b.dir, prompt, b.model, b.timeout, env)
	res.Entry = b.name
	return res, nil
}

// claudeArgs builds the argv passed to the `claude` binary: `--print
// --dangerously-skip-permissions`, an optional `--model <model>` when
// model is non-empty, and prompt last. Factored out of RunClaudeIn so the
// argv shape is unit-testable without shelling out.
func claudeArgs(prompt, model string) []string {
	args := []string{"--print", "--dangerously-skip-permissions"}
	if model != "" {
		args = append(args, "--model", model)
	}
	return append(args, prompt)
}

// RunClaudeIn runs `claude --print --dangerously-skip-permissions
// [--model <model>] <prompt>` in dir, bounded by timeout, with env as the
// subprocess environment. When model is empty, no `--model` flag is
// passed and `claude` inherits Claude Code's configured default. It is
// the shared transport behind every roster subagent backend and the
// "aida ask aida" charter fallback (internal/dispatch), so both paths
// behave identically -- same timeout handling, same empty/refusal
// detection. The returned Result has no Entry set; callers that need
// attribution (a named roster entry) fill it in themselves.
func RunClaudeIn(ctx context.Context, dir, prompt, model string, timeout time.Duration, env []string) Result {
	res, err := execx.Run(ctx, "claude",
		claudeArgs(prompt, model),
		execx.RunOpts{
			Timeout: timeout,
			Dir:     dir,
			Env:     env,
		})

	if res.TimedOut {
		return Result{
			Status: StatusTimeout,
			Text:   "timed out after " + timeout.String(),
			TookMS: res.Duration.Milliseconds(),
		}
	}
	if err != nil {
		stderrText := strings.TrimSpace(string(res.Stderr))
		if stderrText == "" {
			stderrText = err.Error()
		}
		return Result{
			Status: StatusError,
			Text:   stderrText,
			TookMS: res.Duration.Milliseconds(),
		}
	}

	text := strings.TrimSpace(string(res.Stdout))
	if text == "" {
		return Result{
			Status: StatusEmpty,
			Text:   "sub-agent returned no output",
			TookMS: res.Duration.Milliseconds(),
		}
	}
	if refusal.LooksLikeRefusal(text) {
		return Result{
			Status: StatusEmpty,
			Text:   "declined: " + refusal.FirstLine(text),
			TookMS: res.Duration.Milliseconds(),
		}
	}

	return Result{
		Status: StatusSuccess,
		Text:   text,
		TookMS: res.Duration.Milliseconds(),
	}
}

// BumpDispatchDepthEnv returns env with AIDA_DISPATCH_DEPTH set to one
// more than whatever value it currently carries (0 if unset or
// unparseable), replacing any prior entry. This is the one-hop depth
// guard: `aida ask` (internal/cli/ask.go) refuses to run once the
// spawned process sees this at 2 or more, so a subagent calling `aida
// ask` once (depth 1) is fine but a second hop is not.
func BumpDispatchDepthEnv(env []string) []string {
	depth := 0
	prefix := dispatchDepthEnvKey + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			if v, err := strconv.Atoi(strings.TrimPrefix(kv, prefix)); err == nil {
				depth = v
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out, fmt.Sprintf("%s%d", prefix, depth+1))
}
