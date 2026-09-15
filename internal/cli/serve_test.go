package cli

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/jobs"
)

func TestResolveServeFlag(t *testing.T) {
	trueP, falseP := true, false

	cases := []struct {
		name     string
		profile  *config.Profile
		field    string
		cliValue bool
		cliSet   bool
		want     bool
	}{
		{
			name:     "explicit CLI flag wins over profile config",
			profile:  &config.Profile{Serve: &config.ServeConfig{Jarvis: &trueP}},
			field:    "jarvis",
			cliValue: false,
			cliSet:   true,
			want:     false,
		},
		{
			name:     "profile config used when CLI flag not set",
			profile:  &config.Profile{Serve: &config.ServeConfig{Jarvis: &falseP}},
			field:    "jarvis",
			cliValue: true,
			cliSet:   false,
			want:     false,
		},
		{
			name:     "CLI default used when profile field unset",
			profile:  &config.Profile{Serve: &config.ServeConfig{}},
			field:    "jarvis",
			cliValue: true,
			cliSet:   false,
			want:     true,
		},
		{
			name:     "CLI default used when profile.Serve is nil",
			profile:  &config.Profile{},
			field:    "jarvis",
			cliValue: true,
			cliSet:   false,
			want:     true,
		},
		{
			name:     "CLI default used when profile itself is nil",
			profile:  nil,
			field:    "listen",
			cliValue: true,
			cliSet:   false,
			want:     true,
		},
		{
			name:     "listen field reads its own pointer",
			profile:  &config.Profile{Serve: &config.ServeConfig{Jarvis: &trueP, Listen: &falseP}},
			field:    "listen",
			cliValue: true,
			cliSet:   false,
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveServeFlag(tc.profile, tc.field, tc.cliValue, tc.cliSet)
			if got != tc.want {
				t.Fatalf("resolveServeFlag(%q) = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}

func TestBindWithTakeoverFreePort(t *testing.T) {
	// :0 picks an ephemeral free port - the common case must bind directly
	// without any kill/lsof path.
	ln, err := bindWithTakeover("127.0.0.1:0")
	if err != nil {
		t.Fatalf("bindWithTakeover on free port: %v", err)
	}
	defer ln.Close()
	if ln == nil {
		t.Fatal("expected a listener, got nil")
	}
}

func TestBindWithTakeoverRefusesSelf(t *testing.T) {
	// Hold a port from within this (non-"aida") test process. bindWithTakeover
	// must NOT try to kill us: the holder is either our own pid (guarded) or
	// fails the isAidaDaemon check. Either way it returns an error and we
	// must still be alive afterward.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("seed listener: %v", err)
	}
	defer held.Close()

	if _, err := bindWithTakeover(held.Addr().String()); err == nil {
		t.Fatal("expected bindWithTakeover to refuse a port held by a non-aida process")
	}
}

// Every spoken notification must include the job's derived handle - the
// system prompt tells the model to pass "the handle from the most recent
// notification" to job_send_input, and resolveJobRef matches handles but
// not kinds, so a notice without one names a job that resolves nothing.
func TestNoticeFormattersIncludeHandle(t *testing.T) {
	j := &jobs.Job{
		RunID:          "20260622-022838-create-a-file",
		Kind:           "pr_work",
		AwaitingPrompt: "which branch?",
		Error:          "boom",
	}
	handle := jobs.DeriveHandle(j.RunID)
	notices := map[string]string{
		"awaiting": formatAwaitingNotice(j),
		"approval": formatApprovalNotice(j),
		"done":     formatDoneNotice(j),
		"failed":   formatFailedNotice(j),
	}
	for name, msg := range notices {
		if !strings.Contains(msg, handle) {
			t.Errorf("%s notice %q missing handle %q", name, msg, handle)
		}
		if !strings.Contains(msg, "pr_work") {
			t.Errorf("%s notice %q missing the descriptive label", name, msg)
		}
	}
	if msg := formatAwaitingNotice(j); !strings.Contains(msg, "which branch?") {
		t.Errorf("awaiting notice %q missing the prompt", msg)
	}
}

// TestProcessAlive exercises both outcomes of processAlive: true for a pid we
// know is running (ourselves), false for a pid we've reaped after killing it.
// Wait() after Kill() is required - without it the child is a zombie and
// still has a pid entry, so processAlive would keep reporting it alive.
func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("processAlive(os.Getpid()) = false, want true")
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid

	if !processAlive(pid) {
		t.Fatalf("processAlive(%d) = false while sleep is running, want true", pid)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill sleep: %v", err)
	}
	_ = cmd.Wait() // reap the zombie so processAlive reflects reality

	if processAlive(pid) {
		t.Fatalf("processAlive(%d) = true after kill+wait, want false", pid)
	}
}

// TestReapDaemonKillsLingeringProcess is the regression test for the actual
// incident: a process that released the port but never exited must still get
// force-killed by reapDaemon, and within a bounded time. sleep 30 never
// receives (and wouldn't act on) a SIGTERM here, so this only passes if
// reapDaemon's own timeout-then-SIGKILL path fires.
func TestReapDaemonKillsLingeringProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid

	start := time.Now()
	reapDaemon(pid)
	elapsed := time.Since(start)

	_ = cmd.Wait()

	if processAlive(pid) {
		t.Fatalf("pid %d still alive after reapDaemon", pid)
	}
	// Regression guard: a bug that dropped the timeout (or widened it
	// unboundedly) would hang this test instead of failing it fast.
	if elapsed > 6*time.Second {
		t.Fatalf("reapDaemon took %v, want well under 6s", elapsed)
	}
}

// TestReapDaemonReturnsPromptlyWhenAlreadyExited proves the happy path (pid
// already gone by the time we check) doesn't pay the 3s timeout - the poll
// loop's first processAlive check should already see it gone.
func TestReapDaemonReturnsPromptlyWhenAlreadyExited(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	pid := cmd.Process.Pid

	start := time.Now()
	reapDaemon(pid)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("reapDaemon on an already-exited pid took %v, want well under the 3s timeout", elapsed)
	}
}

// TestStdioFlagForcesMCPMode table-tests serveHTTPMode, the pure helper
// extracted from RunE's mode-selection logic. --stdio must win any time it's
// set, regardless of --http or stdin's TTY-ness - it's the safety net for an
// MCP client that allocated a PTY for the child.
func TestStdioFlagForcesMCPMode(t *testing.T) {
	cases := []struct {
		name       string
		stdioOnly  bool
		httpOnly   bool
		stdinIsTTY bool
		want       bool
	}{
		{"stdio wins over a TTY stdin", true, false, true, false},
		{"TTY stdin alone selects HTTP", false, false, true, true},
		{"http flag alone selects HTTP", false, true, false, true},
		{"stdio wins even when http is also set", true, true, false, false},
		{"neither flag, non-TTY stdin selects stdio mode", false, false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serveHTTPMode(tc.stdioOnly, tc.httpOnly, tc.stdinIsTTY)
			if got != tc.want {
				t.Fatalf("serveHTTPMode(%v, %v, %v) = %v, want %v",
					tc.stdioOnly, tc.httpOnly, tc.stdinIsTTY, got, tc.want)
			}
		})
	}
}
