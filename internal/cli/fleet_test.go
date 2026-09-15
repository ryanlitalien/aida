package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/fleet"
	"github.com/ryanlitalien/aida/internal/remotex"
)

// sshFakeRule matches any ssh invocation (name=="ssh"); RunRemote is the
// only thing pollUntilExit ever calls through the Runner, and it always
// shells out as "ssh".
func sshFakeRule(stdout string, result remotex.Result, err error) remotex.FakeRule {
	if result.Stdout == nil && stdout != "" {
		result.Stdout = []byte(stdout)
	}
	return remotex.FakeRule{
		Match:  func(name string, _ []string, _ []byte) bool { return name == "ssh" },
		Result: result,
		Err:    err,
	}
}

func TestPollUntilExit_NoExitLineYetTimesOut(t *testing.T) {
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshFakeRule("agent is still working, nothing final yet\n", remotex.Result{ExitCode: 0}, nil),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := pollUntilExit(ctx, fake, "minty", "ryan", "/home/ryan/agents-lane4/x.log", 5*time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pollUntilExit error = %v, want context.DeadlineExceeded", err)
	}
	if n := fake.CallCount(); n < 2 {
		t.Errorf("CallCount() = %d, want >= 2 (should have retried while waiting for EXIT)", n)
	}
}

func TestPollUntilExit_Exit0WithPRURL(t *testing.T) {
	log := "Tracer bullet done.\n\n" +
		"PR: https://github.com/ButterStack/butter_stack/pull/1617\n\n" +
		"Tests green.\n" +
		"EXIT=0 2026-09-02T16:06:39-04:00\n"
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshFakeRule(log, remotex.Result{ExitCode: 0}, nil),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := pollUntilExit(ctx, fake, "minty", "ryan", "/home/ryan/agents-lane4/x.log", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("pollUntilExit: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.Timestamp != "2026-09-02T16:06:39-04:00" {
		t.Errorf("Timestamp = %q, want 2026-09-02T16:06:39-04:00", result.Timestamp)
	}
	want := "https://github.com/ButterStack/butter_stack/pull/1617"
	if result.PRURL != want {
		t.Errorf("PRURL = %q, want %q", result.PRURL, want)
	}
	if result.Output == "" || containsExitMarker(result.Output) {
		t.Errorf("Output = %q, want the report with the EXIT line stripped", result.Output)
	}
	// Must return on the very first poll -- no waiting when the log
	// already ends with EXIT.
	if n := fake.CallCount(); n != 1 {
		t.Errorf("CallCount() = %d, want 1 (should resolve on the first attempt)", n)
	}
}

func TestPollUntilExit_Exit1NoPRURL(t *testing.T) {
	log := "Something went wrong.\nEXIT=1 2026-09-02T16:06:39-04:00\n"
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		sshFakeRule(log, remotex.Result{ExitCode: 0}, nil),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := pollUntilExit(ctx, fake, "minty", "ryan", "/home/ryan/agents-lane4/x.log", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("pollUntilExit: %v", err)
	}
	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.PRURL != "" {
		t.Errorf("PRURL = %q, want empty", result.PRURL)
	}
}

// TestPollUntilExit_TransportErrorRetries covers the brief's "transport
// errors are logged and retried" clause: an ssh-level failure on the
// first attempt must not fail the poll -- it should retry and pick up
// the EXIT line on a later tick.
func TestPollUntilExit_TransportErrorRetries(t *testing.T) {
	calls := 0
	fake := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match: func(name string, _ []string, _ []byte) bool {
				calls++
				return name == "ssh" && calls == 1
			},
			Err: errors.New("ssh: connect to host minty port 22: Connection refused"),
		},
		sshFakeRule("EXIT=0 2026-09-02T16:06:39-04:00\n", remotex.Result{ExitCode: 0}, nil),
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, err := pollUntilExit(ctx, fake, "minty", "ryan", "/home/ryan/agents-lane4/x.log", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("pollUntilExit: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if fake.CallCount() < 2 {
		t.Errorf("CallCount() = %d, want >= 2 (should have retried past the transport error)", fake.CallCount())
	}
}

// containsExitMarker reports whether any line of s is itself a lane 4
// EXIT marker -- used to assert Output never retains the line
// pollUntilExit is supposed to have stripped.
func containsExitMarker(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if _, _, ok := fleet.ParseExitLine(line); ok {
			return true
		}
	}
	return false
}
