// Package dispatch is Aida's orchestrator: given a natural-language task, it
// resolves or selects one or more roster entries, runs their backends (in
// parallel on a fan-out), and aggregates the replies into one answer. It holds
// to the engine's "LLM at the edges, deterministic middle" shape: zero LLM
// calls when a call-sign is named, one select call otherwise, and one
// synthesize call only when more than one agent answered.
package dispatch

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/roster"
)

// Completer is the subset of *llm.Client the dispatcher's two LLM edges use
// (select + aggregate). *llm.Client satisfies it; tests supply a fake. Kept as
// an interface so a nil means "no LLM" without the typed-nil trap.
type Completer interface {
	CompleteJSONWithStage(ctx context.Context, stage, systemPrompt, userPrompt string, schema map[string]interface{}) (string, error)
	CompleteWithStage(ctx context.Context, stage, systemPrompt, userPrompt string) (string, error)
}

// fallbackTimeout bounds the general-query fallback (a full `aida <task>` run)
// when the select step routes to nobody. Matches aidaQueryTimeout.
const fallbackTimeout = 240 * time.Second

// maxConcurrent caps parallel backend calls during a fan-out, mirroring the
// engine executor's semaphore limit.
const maxConcurrent = 4

// Dispatcher routes a task across the roster and aggregates the result.
type Dispatcher struct {
	Roster *roster.Roster
	LLM    Completer // select and aggregate edges only; nil = named-only routing + fallback
	Deps   roster.Deps

	// charterDir overrides where fallback looks for AGENTS.md; empty means
	// the real default (~/.aida). Unexported and test-only -- production
	// callers never set it.
	charterDir string
}

// Report is the outcome of a Dispatch: the per-backend results, the aggregated
// user-facing answer, and whether the task fanned out to more than one agent.
type Report struct {
	Routes []roster.Result
	Answer string
	Fanout bool
}

// routeSel is one chosen route: an entry plus the slice of the task meant for it.
type routeSel struct {
	entry   *roster.Entry
	subtask string
}

// routed pairs a chosen route with the result its backend returned.
type routed struct {
	entry   *roster.Entry
	subtask string
	result  roster.Result
}

// Dispatch runs the full flow: resolve-or-select, execute, aggregate.
func (d *Dispatcher) Dispatch(ctx context.Context, task, origin string) (*Report, error) {
	routes := d.chooseRoutes(ctx, task)
	if len(routes) == 0 {
		answer, err := d.fallback(ctx, task)
		if err != nil {
			return nil, err
		}
		return &Report{Answer: answer}, nil
	}

	executed := d.execute(ctx, routes, origin)

	results := make([]roster.Result, len(executed))
	for i, e := range executed {
		results[i] = e.result
	}

	answer := d.aggregate(ctx, task, executed)
	return &Report{
		Routes: results,
		Answer: answer,
		Fanout: len(executed) > 1,
	}, nil
}

// execute runs each route's backend concurrently (bounded by maxConcurrent)
// and returns the results in route order. A backend construction or call
// failure lands as a StatusError result rather than sinking the whole report.
func (d *Dispatcher) execute(ctx context.Context, routes []routeSel, origin string) []routed {
	out := make([]routed, len(routes))
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup

	for i, rt := range routes {
		i, rt := i, rt
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			out[i] = routed{entry: rt.entry, subtask: rt.subtask}

			backend, err := d.Roster.BackendFor(rt.entry, d.Deps)
			if err != nil {
				out[i].result = roster.Result{Entry: rt.entry.Name, Status: roster.StatusError, Text: err.Error()}
				return
			}
			res, err := backend.Ask(ctx, roster.Request{
				Task:    rt.subtask,
				Profile: d.Deps.Profile,
				Origin:  origin,
			})
			if err != nil {
				out[i].result = roster.Result{Entry: rt.entry.Name, Status: roster.StatusError, Text: err.Error()}
				return
			}
			out[i].result = res
		}()
	}
	wg.Wait()
	return out
}

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// charterAGENTSFile is the file whose presence in a directory marks it as
// Aida's own charter -- her persona definition, not just a scratch config
// dir.
const charterAGENTSFile = "AGENTS.md"

// charterPromptPrefix is prepended to the raw task before handing it to
// the charter subagent, so the persona (not a bare question) is what
// `claude --print` sees.
const charterPromptPrefix = "You are Aida (your definition is AGENTS.md in this directory). " +
	"Answer this directly and concisely; for library or codebase questions you may run " +
	"`aida --source <name> \"<question>\"` yourself. Task: "

// fallback answers a task that matched no roster entry. When Aida has a
// charter (AGENTS.md in ~/.aida, or charterDir in tests), she answers in
// persona through the same subagent transport a roster entry uses --
// roster.RunClaudeIn -- rather than the general engine below. The engine
// path hedges (it's built to say "I don't know" over guessing) and costs
// three LLM calls (parse, plan, synthesize) to get an answer; the charter
// is one `claude --print` call in her own voice. Only when no charter
// directory is configured does fallback keep the older behavior: a plain
// `aida <task>` subprocess.
func (d *Dispatcher) fallback(ctx context.Context, task string) (string, error) {
	dir := d.charterDir
	if dir == "" {
		dir = config.ExpandPath("~/.aida")
	}
	if info, err := os.Stat(filepath.Join(dir, charterAGENTSFile)); err == nil && !info.IsDir() {
		return d.fallbackCharter(ctx, dir, task)
	}
	return d.fallbackEngine(ctx, task)
}

// fallbackCharter answers task in Aida's own persona by running the
// charter directory's subagent transport.
func (d *Dispatcher) fallbackCharter(ctx context.Context, dir, task string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fallbackTimeout)
	defer cancel()

	prompt := charterPromptPrefix + task
	env := roster.BumpDispatchDepthEnv(config.ScrubAnthropicCreds(os.Environ()))
	res := roster.RunClaudeIn(ctx, dir, prompt, "", fallbackTimeout, env)

	if res.Status == roster.StatusTimeout {
		return res.Text, fmt.Errorf("fallback query timed out after %s", fallbackTimeout)
	}
	if res.Status != roster.StatusSuccess {
		if res.Text != "" {
			return res.Text, nil // usable output despite a non-success status
		}
		return "", fmt.Errorf("charter fallback failed: %s", res.Status)
	}
	return res.Text, nil
}

// fallbackEngine answers a task that matched no roster entry (and has no
// charter) by running a normal full-pipeline `aida <task>` query as a
// subprocess, the same isolation rationale as the aida_query voice tool.
func (d *Dispatcher) fallbackEngine(ctx context.Context, task string) (string, error) {
	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return "", fmt.Errorf("locating aida binary for fallback: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fallbackTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, aidaBin, task).CombinedOutput()
	clean := strings.TrimSpace(ansiPattern.ReplaceAllString(string(out), ""))
	if ctx.Err() == context.DeadlineExceeded {
		return clean, fmt.Errorf("fallback query timed out after %s", fallbackTimeout)
	}
	if err != nil {
		if clean != "" {
			return clean, nil // the subprocess printed something usable despite a non-zero exit
		}
		return "", fmt.Errorf("fallback query failed: %w", err)
	}
	return clean, nil
}

// handleFor is a thin wrapper so aggregate can name a delegated job.
func handleFor(runID string) string { return jobs.DeriveHandle(runID) }
