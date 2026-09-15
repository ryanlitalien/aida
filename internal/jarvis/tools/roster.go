// roster.go registers Aida's two dispatcher-facing voice tools:
// roster_list (who's on the team) and ask_agent (delegate a task to one of
// them by name). Registered only for the Aida persona (cfg.Dispatcher !=
// nil in jarvis.go); Jarvis's default registry never sees these.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ryanlitalien/aida/internal/dispatch"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/roster"
)

// RegisterRosterTools adds roster_list and ask_agent to r, closing over d so
// both tools see the live roster and dispatcher. Called once, from
// newAssistant, only when the persona carries a non-nil Dispatcher.
func RegisterRosterTools(r *Registry, d *dispatch.Dispatcher) {
	r.Register(rosterListTool(d))
	r.Register(askAgentTool(d))
}

// ─── roster_list ─────────────────────────────────────────────────────────────

func rosterListTool(d *dispatch.Dispatcher) Tool {
	return Tool{
		Name: "roster_list",
		Description: "List the people/agents on the user's roster -- everyone Aida " +
			"can delegate a task to by name via ask_agent. Use when the user asks " +
			"\"who's on my team\", \"who do you have\", \"who can help with X\".",
		Schema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
		Run: func(_ context.Context, _ json.RawMessage) (string, error) {
			return describeRoster(d.Roster.Entries()), nil
		},
	}
}

// describeRoster renders the roster's non-aida entries as one spoken
// sentence, e.g. "Your team: Pamela, the product manager; Devin, devops;
// Coach, personal trainer." The reserved aida entry (the orchestrator
// itself) is never listed as one of its own delegates.
func describeRoster(entries []*roster.Entry) string {
	var parts []string
	for _, e := range entries {
		if e.Kind == roster.KindAida {
			continue
		}
		part := e.Display()
		if clause := firstClause(e.Description); clause != "" {
			part += ", " + clause
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "Your roster is empty, sir."
	}
	return "Your team: " + strings.Join(parts, "; ") + "."
}

// firstClause trims a roster entry's Description down to its first sentence
// or clause -- short enough to read aloud alongside the entry's name. Falls
// back to the whole trimmed description when there's no sentence-ending
// punctuation to cut at.
func firstClause(desc string) string {
	d := strings.TrimSpace(desc)
	if d == "" {
		return ""
	}
	if i := strings.IndexAny(d, ".!?"); i >= 0 {
		d = d[:i]
	}
	return strings.TrimSpace(d)
}

// ─── ask_agent ────────────────────────────────────────────────────────────────

type askAgentInput struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

func askAgentTool(d *dispatch.Dispatcher) Tool {
	return Tool{
		Name:        "ask_agent",
		Description: askAgentDescription(d.Roster.Entries()),
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "the call-sign or name of the roster entry to delegate to, e.g. \"Pamela\" or \"devops\"",
				},
				"task": map[string]interface{}{
					"type":        "string",
					"description": "the task to hand off, in natural language",
				},
			},
			"required": []string{"agent", "task"},
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var in askAgentInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "", err
			}
			agent := strings.TrimSpace(in.Agent)
			task := strings.TrimSpace(in.Task)
			if agent == "" || task == "" {
				return "", fmt.Errorf("ask_agent: agent and task are required")
			}
			return runAskAgent(ctx, d, agent, task)
		},
	}
}

// askAgentDescription builds the ask_agent tool description with the
// current roster embedded, the same way aidaQueryTool injects its available
// source domains -- so the model knows who exists without a separate
// roster_list round trip first.
func askAgentDescription(entries []*roster.Entry) string {
	desc := "Delegate a task to a specific agent on the roster."
	var lines []string
	for _, e := range entries {
		if e.Kind == roster.KindAida {
			continue
		}
		line := e.Display()
		if clause := firstClause(e.Description); clause != "" {
			line += " - " + clause
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return desc + " No agents are currently configured on the roster."
	}
	return desc + " Available agents:\n" + strings.Join(lines, "\n")
}

// runAskAgent resolves agent against the roster and routes the task to
// whatever backs it: the orchestrator itself (a fan-out Dispatch), a
// background job, or a synchronous backend call.
func runAskAgent(ctx context.Context, d *dispatch.Dispatcher, agent, task string) (string, error) {
	entry, err := d.Roster.Resolve(agent)
	var note string
	if err != nil {
		var other *roster.ErrOtherProfile
		switch {
		case errors.As(err, &other):
			// Cross-profile is allowed when the user named the entry
			// explicitly -- proceed with the match, just flag it.
			entry = other.Entry
			note = fmt.Sprintf("(%s is on your %s roster, but I'll ask anyway.) ", entry.Display(), other.Profile)
		case errors.Is(err, roster.ErrNotFound):
			return fmt.Sprintf("I don't have anyone called %s on your roster, sir.", agent), nil
		case errors.Is(err, roster.ErrAmbiguous):
			return "That could be more than one of my people; which one?", nil
		default:
			return "", err
		}
	}

	if entry.Kind == roster.KindAida {
		// "Ask aida ..." fans out through the full dispatcher rather than
		// pinning a single backend.
		report, derr := d.Dispatch(ctx, task, "voice")
		if derr != nil {
			return "", derr
		}
		return note + report.Answer, nil
	}

	decision := dispatch.Decide([]*roster.Entry{entry}, "voice")
	if decision.Background && d.Deps.Jobs != nil {
		handle, serr := spawnAskJob(d.Deps.Jobs, entry.Name, task)
		if serr != nil {
			return "", serr
		}
		return fmt.Sprintf("%sWorking on it, sir. I'll have %s report back -- I'll call this one %s.",
			note, entry.Display(), handle), nil
	}

	backend, berr := d.Roster.BackendFor(entry, d.Deps)
	if berr != nil {
		return "", berr
	}
	res, aerr := backend.Ask(ctx, roster.Request{
		Task:    task,
		Profile: d.Deps.Profile,
		Origin:  "voice",
	})
	if aerr != nil {
		return "", aerr
	}

	switch res.Status {
	case roster.StatusSuccess:
		return fmt.Sprintf("%s%s: %s", note, entry.Display(), res.Text), nil
	case roster.StatusDelegated:
		return fmt.Sprintf("%s%s handed this off -- I'll call it %s.",
			note, entry.Display(), jobs.DeriveHandle(res.JobID)), nil
	default:
		return fmt.Sprintf("%s%s couldn't help, sir (%s).", note, entry.Display(), res.Status), nil
	}
}

// spawnAskJob mirrors startGenericAgentJob (jobs.go) but drives
// `aida ask --raw <agent> <task>` instead of `aida --agent --run-dir`,
// since `aida ask` has no run-dir to update its own manifest through --
// this function owns the job's state transitions (MarkRunning / Complete /
// Fail) itself rather than leaning on the child process to report them.
func spawnAskJob(store *jobs.Store, agentName, task string) (string, error) {
	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return "", fmt.Errorf("aida not on PATH: %w", err)
	}

	job, err := store.Enqueue("ask", "", "", task, "aida:ask:"+agentName)
	if err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}

	runDir := jobs.RunDir(store.Profile(), job.RunID)
	// Best-effort output capture -- a failure to open the file just means
	// the job runs without a transcript, not a failed dispatch.
	var f *os.File
	if out, oerr := os.Create(filepath.Join(runDir, "output.md")); oerr == nil {
		f = out
	}

	cmd := exec.Command(aidaBin, "ask", "--raw", agentName, task)
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
		"AIDA_PROFILE="+store.Profile(),
	)
	if f != nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = store.Fail(job.RunID, "spawn: "+err.Error())
		return "", fmt.Errorf("spawn ask agent: %w", err)
	}
	host, _ := os.Hostname()
	_ = store.MarkRunning(job.RunID, host, cmd.Process.Pid)

	go func() {
		werr := cmd.Wait()
		if f != nil {
			_ = f.Close()
		}
		if werr != nil {
			_ = store.Fail(job.RunID, "ask agent failed: "+werr.Error())
			return
		}
		_ = store.Complete(job.RunID)
	}()

	return jobs.DeriveHandle(job.RunID), nil
}
