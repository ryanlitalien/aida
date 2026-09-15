// Package execx runs subprocesses that can't wedge aida.
//
// The standard os/exec trap: when cmd.Stdout / cmd.Stderr are io.Writers
// (including bytes.Buffer), cmd.Wait() blocks until every writer on the
// underlying pipe closes -- not just the direct child. A subprocess that
// spawns a backgrounded grandchild inheriting those fds will pin Wait()
// open forever, even after a context timeout kills the direct child.
//
// execx defends against this in two layers:
//
//  1. Process-group containment. Every child runs in its own PGID via
//     Setpgid=true. On ctx cancel, cmd.Cancel SIGTERMs the whole group
//     (negative pgid), then SIGKILL as a follow-up. Grandchildren die
//     with the child.
//  2. WaitDelay backstop. If something still slips through (a process
//     that setsid'd itself out of our group), cmd.WaitDelay forces Go
//     to close our pipe fds and return from Wait() anyway.
//
// WaitDelay does NOT cap the command's runtime. It only governs how long
// Wait blocks *after* ctx cancels or the direct child exits. Long-running
// commands (snow, claude) are unaffected until their own Timeout fires.
package execx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// Defaults used when RunOpts fields are zero.
const (
	DefaultWaitDelay   = 3 * time.Second
	DefaultMaxOutBytes = 4 * 1024 * 1024 // 4 MiB
)

// RunOpts configures a Run invocation. All fields are optional.
type RunOpts struct {
	Timeout     time.Duration // total command ceiling; 0 = inherit ctx deadline only
	WaitDelay   time.Duration // 0 => DefaultWaitDelay
	Dir         string
	Env         []string // nil => inherit current environment
	MaxOutBytes int64    // 0 => DefaultMaxOutBytes; per stream for Run, combined for RunCombined
	Stdin       []byte   // nil => no stdin (child sees an immediately-closed pipe, as today)
}

// Result describes a completed (or timed-out) subprocess execution.
type Result struct {
	Stdout        []byte
	Stderr        []byte
	Combined      []byte // only populated by RunCombined
	ExitCode      int
	TimedOut      bool // ctx deadline fired
	OrphansKilled int  // >0 if a final group SIGKILL reaped leftover processes
	Truncated     bool // output exceeded MaxOutBytes and was cut off
	Duration      time.Duration
}

// Run executes name+args, capturing stdout and stderr separately.
//
// On ctx cancellation or Timeout expiry, the entire process group is
// killed and Run returns promptly with TimedOut=true.
func Run(ctx context.Context, name string, args []string, opts RunOpts) (Result, error) {
	return runCmd(ctx, name, args, opts, false)
}

// RunShell runs the given shell command string via `sh -c`.
func RunShell(ctx context.Context, shellCmd string, opts RunOpts) (Result, error) {
	return runCmd(ctx, "sh", []string{"-c", shellCmd}, opts, false)
}

// RunCombined is like Run but merges stdout and stderr into Result.Combined,
// matching exec.Cmd.CombinedOutput semantics.
func RunCombined(ctx context.Context, name string, args []string, opts RunOpts) (Result, error) {
	return runCmd(ctx, name, args, opts, true)
}

func runCmd(ctx context.Context, name string, args []string, opts RunOpts, combined bool) (Result, error) {
	if opts.WaitDelay <= 0 {
		opts.WaitDelay = DefaultWaitDelay
	}
	if opts.MaxOutBytes <= 0 {
		opts.MaxOutBytes = DefaultMaxOutBytes
	}

	execCtx := ctx
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(execCtx, name, args...)
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	if opts.Stdin != nil {
		cmd.Stdin = bytes.NewReader(opts.Stdin)
	}
	cmd.WaitDelay = opts.WaitDelay
	cmd.Cancel = func() error { return killGroup(cmd) }

	applyProcessGroup(cmd)

	var outBuf, errBuf, combBuf bytes.Buffer
	var outLim, errLim, combLim *limitWriter
	if combined {
		combLim = &limitWriter{w: &combBuf, max: opts.MaxOutBytes}
		cmd.Stdout = combLim
		cmd.Stderr = combLim
	} else {
		outLim = &limitWriter{w: &outBuf, max: opts.MaxOutBytes}
		errLim = &limitWriter{w: &errBuf, max: opts.MaxOutBytes}
		cmd.Stdout = outLim
		cmd.Stderr = errLim
	}

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)

	// Belt-and-suspenders: if we cancelled, try a group SIGKILL to reap
	// anything that SIGTERM missed (e.g. handlers that trapped SIGTERM).
	orphansKilled := 0
	if timedOut || errors.Is(execCtx.Err(), context.Canceled) {
		orphansKilled = killGroupFinal(cmd)
	}

	res := Result{
		TimedOut:      timedOut,
		OrphansKilled: orphansKilled,
		Duration:      duration,
	}
	if combined {
		res.Combined = combBuf.Bytes()
		res.Truncated = combLim.truncated
	} else {
		res.Stdout = outBuf.Bytes()
		res.Stderr = errBuf.Bytes()
		res.Truncated = outLim.truncated || errLim.truncated
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	if err != nil && !timedOut {
		return res, err
	}
	return res, nil
}

// limitWriter writes at most max bytes to w, flagging truncation and
// silently swallowing the overflow so the child's Write calls never
// block or error on buffer overflow.
type limitWriter struct {
	w         io.Writer
	max       int64
	n         int64
	truncated bool
}

func (lw *limitWriter) Write(p []byte) (int, error) {
	if lw.n >= lw.max {
		lw.truncated = true
		return len(p), nil
	}
	remaining := lw.max - lw.n
	if int64(len(p)) > remaining {
		n, err := lw.w.Write(p[:remaining])
		lw.n += int64(n)
		lw.truncated = true
		if err != nil {
			return n, err
		}
		return len(p), nil
	}
	n, err := lw.w.Write(p)
	lw.n += int64(n)
	return n, err
}
