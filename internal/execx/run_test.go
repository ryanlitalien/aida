package execx_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

func TestRun_Happy(t *testing.T) {
	ctx := context.Background()
	res, err := execx.RunShell(ctx, "echo hi", execx.RunOpts{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hi" {
		t.Errorf("stdout = %q, want %q", got, "hi")
	}
	if res.TimedOut {
		t.Error("TimedOut=true, want false")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRun_NonZeroExit(t *testing.T) {
	ctx := context.Background()
	res, err := execx.RunShell(ctx, "exit 7", execx.RunOpts{Timeout: 2 * time.Second})
	if err == nil {
		t.Error("want error from non-zero exit, got nil")
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.TimedOut {
		t.Error("TimedOut=true, want false")
	}
}

func TestRun_DirectChildTimeout(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	res, _ := execx.RunShell(ctx, "sleep 60", execx.RunOpts{
		Timeout:   500 * time.Millisecond,
		WaitDelay: 1 * time.Second,
	})
	dur := time.Since(start)
	if !res.TimedOut {
		t.Error("TimedOut=false, want true")
	}
	if dur > 2500*time.Millisecond {
		t.Errorf("took %s to time out, want < 2.5s", dur)
	}
}

func TestRun_CombinedOutput(t *testing.T) {
	ctx := context.Background()
	// stdout then stderr, in order, to a combined stream.
	res, err := execx.RunCombined(ctx, "sh", []string{"-c", "echo out; echo err 1>&2"}, execx.RunOpts{
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(res.Combined)
	if !strings.Contains(got, "out") || !strings.Contains(got, "err") {
		t.Errorf("combined output missing entries: %q", got)
	}
}

func TestRun_Truncation(t *testing.T) {
	ctx := context.Background()
	// Generate 100 KiB of output with a 4 KiB cap.
	res, err := execx.RunShell(ctx, "yes x | head -c 102400", execx.RunOpts{
		Timeout:     3 * time.Second,
		MaxOutBytes: 4096,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int64(len(res.Stdout)) > 4096 {
		t.Errorf("stdout len = %d, want <= 4096", len(res.Stdout))
	}
	if !res.Truncated {
		t.Error("Truncated=false, want true for overflow output")
	}
}

func TestRun_Stdin(t *testing.T) {
	ctx := context.Background()
	res, err := execx.Run(ctx, "cat", nil, execx.RunOpts{
		Timeout: 2 * time.Second,
		Stdin:   []byte("hello from stdin"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(res.Stdout); got != "hello from stdin" {
		t.Errorf("stdout = %q, want %q", got, "hello from stdin")
	}
}

func TestRun_NilStdinClosesImmediately(t *testing.T) {
	ctx := context.Background()
	// cat with no stdin data should see EOF right away and exit 0 with
	// empty output - confirms nil Stdin is backward compatible with the
	// pre-Stdin behavior (an immediately-closed pipe), not a hang.
	res, err := execx.Run(ctx, "cat", nil, execx.RunOpts{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout = %q, want empty", res.Stdout)
	}
}

// Ensure a long-running command that produces output stays within its
// Timeout rather than being killed earlier by WaitDelay. (Regression
// guard against a misunderstanding of WaitDelay semantics - see the
// package doc.)
func TestRun_WaitDelayDoesNotCapRuntime(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	res, err := execx.RunShell(ctx, "sleep 1; echo done", execx.RunOpts{
		Timeout:   5 * time.Second,
		WaitDelay: 100 * time.Millisecond,
	})
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(res.Stdout)) != "done" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "done")
	}
	if dur < 900*time.Millisecond {
		t.Errorf("returned in %s, expected ~1s (command used its full runtime)", dur)
	}
	if res.TimedOut {
		t.Error("TimedOut=true, want false")
	}
}
