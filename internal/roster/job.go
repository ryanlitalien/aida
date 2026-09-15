package roster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
)

// defaultJobKind is used when an entry's job: block doesn't set kind.
const defaultJobKind = "agent"

// jobBackend spawns a background `aida --agent` job via the jobs queue and
// returns immediately with a StatusDelegated result -- it never blocks on
// the spawned agent's completion. Mirrors the recipe in
// internal/jarvis/tools/jobs.go's startGenericAgentJob/reapAgentExit.
type jobBackend struct {
	store *jobs.Store
	kind  string
	cwd   string // "" = spawn in the current working directory
	name  string
}

// newJobBackend builds the jobBackend for e, requiring a jobs store in
// deps (the entry's job: block itself is optional -- an empty one just
// means "agent" kind, no cwd override).
func newJobBackend(e *Entry, deps Deps) (Backend, error) {
	if deps.Jobs == nil {
		return nil, fmt.Errorf("entry %q has kind %q but no jobs store in deps", e.Name, KindJob)
	}
	kind := defaultJobKind
	var cwd string
	if e.Job != nil {
		if e.Job.Kind != "" {
			kind = e.Job.Kind
		}
		if e.Job.Cwd != "" {
			cwd = config.ExpandPath(e.Job.Cwd)
		}
	}
	return &jobBackend{store: deps.Jobs, kind: kind, cwd: cwd, name: e.Name}, nil
}

// Kind implements Backend.
func (b *jobBackend) Kind() string { return KindJob }

// buildJobCmd constructs (but does not start) the `aida --agent` process
// for a delegated job. Pure aside from exec.Command's own allocation, so
// tests can assert on Args/Env/Dir without ever starting a process.
func buildJobCmd(aidaBin, runDir, profile, cwd, question string) *exec.Cmd {
	cmd := exec.Command(aidaBin, "--agent", "--run-dir", runDir, question)
	cmd.Env = append(os.Environ(),
		"NO_COLOR=1", "CLICOLOR=0", "TERM=dumb",
		"AIDA_PROFILE="+profile,
	)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// Ask implements Backend by enqueueing a job row, spawning the agent
// process detached, and returning StatusDelegated immediately. It never
// blocks on the job finishing.
func (b *jobBackend) Ask(_ context.Context, req Request) (Result, error) {
	start := time.Now()

	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return Result{Entry: b.name, Status: StatusError, Text: err.Error(), TookMS: time.Since(start).Milliseconds()}, nil
	}

	job, err := b.store.Enqueue(b.kind, "", "", req.Task, "aida:roster")
	if err != nil {
		return Result{Entry: b.name, Status: StatusError, Text: err.Error(), TookMS: time.Since(start).Milliseconds()}, nil
	}
	runDir := jobs.RunDir(b.store.Profile(), job.RunID)

	cmd := buildJobCmd(aidaBin, runDir, b.store.Profile(), b.cwd, req.Task)
	if err := cmd.Start(); err != nil {
		_ = b.store.Fail(job.RunID, "spawn: "+err.Error())
		return Result{Entry: b.name, Status: StatusError, Text: err.Error(), TookMS: time.Since(start).Milliseconds()}, nil
	}

	// Detach: the caller never waits on this job. reapJobExit is a
	// backstop for a process that dies without reaching a terminal state
	// through its own --run-dir MarkRunning/Complete/Fail lifecycle.
	go reapJobExit(b.store, cmd, job.RunID)

	return Result{
		Entry:  b.name,
		Status: StatusDelegated,
		JobID:  job.RunID,
		TookMS: time.Since(start).Milliseconds(),
	}, nil
}

// reapJobExit is the roster-side twin of
// internal/jarvis/tools/jobs.go's reapAgentExit: a backstop that fails the
// job if the spawned process exits without ever reaching a terminal state
// on its own.
func reapJobExit(store *jobs.Store, cmd *exec.Cmd, runID string) {
	if err := cmd.Wait(); err != nil {
		_ = store.Fail(runID, "agent exited: "+err.Error())
		return
	}
	if j, err := store.Get(runID); err == nil && !jobs.IsTerminalState(j.State) {
		_ = store.Fail(runID, "agent exited 0 without finishing")
	}
}
