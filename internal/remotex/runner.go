// Package remotex runs commands on remote hosts over SSH, safely.
//
// The central hazard this package exists to neutralize: `ssh host a b c`
// does NOT deliver three argv elements. ssh joins its remote-command argv
// with single spaces into one string and hands that string to the remote
// LOGIN SHELL, which re-parses it. See script.go.
package remotex

import (
	"context"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// Runner abstracts "run this local command, capture the result" so
// callers (and their tests) don't depend on actually shelling out to
// ssh. ExecRunner is the real implementation; FakeRunner is the test
// double.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdin []byte, timeout time.Duration) (Result, error)
}

// Result describes a completed (or timed-out) remote command execution.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	TimedOut bool
	Duration time.Duration
}

// ExecRunner is the production Runner: it shells out via execx, which
// already handles process-group containment and WaitDelay so a wedged
// ssh (or a remote command that backgrounds a grandchild) can't hang
// the caller past ctx/timeout. See internal/execx's package doc.
type ExecRunner struct{}

var _ Runner = ExecRunner{}

func (ExecRunner) Run(ctx context.Context, name string, args []string, stdin []byte, timeout time.Duration) (Result, error) {
	res, err := execx.Run(ctx, name, args, execx.RunOpts{
		Timeout: timeout,
		Stdin:   stdin,
	})
	return Result{
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
		TimedOut: res.TimedOut,
		Duration: res.Duration,
	}, err
}

// FakeCall records one invocation made through a FakeRunner, for tests
// to assert on afterward (e.g. "no fragment of the secret payload ever
// appeared in Args").
type FakeCall struct {
	Name  string
	Args  []string
	Stdin []byte
}

// FakeRule matches a call and supplies the (Result, error) it should
// get back. Rules are tried in order; the first match wins. A rule with
// a nil Match always matches, so it's useful as a catch-all default
// placed last.
type FakeRule struct {
	Match  func(name string, args []string, stdin []byte) bool
	Result Result
	Err    error
	Delay  time.Duration // simulated latency before responding
}

// FakeRunner is a Runner test double for exercising callers (like
// RunRemote and anything built on top of it) without ever invoking a
// real ssh binary. Safe for concurrent use - callers that want to
// assert on parallelism (e.g. a bounded worker pool) can read
// MaxInFlight() after the run.
type FakeRunner struct {
	mu      sync.Mutex
	Calls   []FakeCall
	Respond []FakeRule

	inFlight, maxInFlight int
}

var _ Runner = (*FakeRunner)(nil)

func (f *FakeRunner) Run(ctx context.Context, name string, args []string, stdin []byte, timeout time.Duration) (Result, error) {
	// Copy args/stdin before recording: callers may reuse or mutate
	// their backing arrays after the call returns.
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{
		Name:  name,
		Args:  append([]string(nil), args...),
		Stdin: append([]byte(nil), stdin...),
	})
	f.inFlight++
	if f.inFlight > f.maxInFlight {
		f.maxInFlight = f.inFlight
	}
	var rule *FakeRule
	for i := range f.Respond {
		if f.Respond[i].Match == nil || f.Respond[i].Match(name, args, stdin) {
			rule = &f.Respond[i]
			break
		}
	}
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if rule != nil && rule.Delay > 0 {
		// select on ctx.Done rather than a bare time.Sleep so a
		// caller-side timeout test finishes in the timeout's duration,
		// not the (possibly much longer) simulated Delay.
		timer := time.NewTimer(rule.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Result{TimedOut: true}, ctx.Err()
		case <-timer.C:
		}
	}

	if rule == nil {
		return Result{}, nil
	}
	return rule.Result, rule.Err
}

// CallCount returns the number of Run invocations recorded so far.
func (f *FakeRunner) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// MaxInFlight returns the high-water mark of concurrent in-progress
// Run calls observed so far.
func (f *FakeRunner) MaxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}
