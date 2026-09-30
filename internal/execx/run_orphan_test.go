//go:build unix

package execx_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// Regression test for the bug that motivated this package:
// a subprocess that backgrounds a grandchild inheriting stdout/stderr
// would pin cmd.Wait() open indefinitely, because Go's exec package
// waits for all pipe writers to close, not just the direct child.
//
// Here: sh backgrounds a `sleep 30` grandchild, disowns it, prints its
// pid, then blocks in `sleep 60`. The Timeout fires after 500ms and
// must (a) return from Run promptly and (b) reap the grandchild.
func TestRun_OrphanGrandchildTimeout(t *testing.T) {
	ctx := context.Background()
	script := `sleep 30 &
GCPID=$!
disown
echo grandchild=$GCPID
sleep 60`

	start := time.Now()
	res, _ := execx.RunShell(ctx, script, execx.RunOpts{
		Timeout:   500 * time.Millisecond,
		WaitDelay: 1 * time.Second,
	})
	dur := time.Since(start)

	if !res.TimedOut {
		t.Errorf("TimedOut=false, want true (timeout never fired). stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
	// Timeout(500ms) + WaitDelay(1s) + generous 1.5s margin.
	maxDur := 3 * time.Second
	if dur > maxDur {
		t.Errorf("took %s to return, want < %s - grandchild pinned the pipe", dur, maxDur)
	}

	pid := parseGrandchildPid(t, string(res.Stdout))
	if pid <= 0 {
		return
	}
	// Poll briefly for the reap to complete. Process-group SIGKILL is
	// async; give the kernel a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return // gone - test passes
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Cleanup so we don't leak if the assertion fails.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild pid %d still alive after Run returned (would have hung for 30s without the fix)", pid)
}

// processGone reports whether pid is dead. A zombie counts as gone: in a
// container with no init reaper (PID 1 is sleep/tail), the killed grandchild
// is reparented to PID 1 and never reaped, so kill(pid, 0) succeeds on it
// forever even though the kill worked. Without /proc (macOS, BSDs) we fall
// back to the kill(0) result alone.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && errors.Is(err, syscall.ESRCH) {
		return true
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// comm (field 2) may contain spaces and parens, so the state is the
	// first field after the LAST ")".
	line := string(data)
	i := strings.LastIndex(line, ")")
	if i < 0 {
		return false
	}
	fields := strings.Fields(line[i+1:])
	return len(fields) > 0 && fields[0] == "Z"
}

func parseGrandchildPid(t *testing.T, out string) int {
	t.Helper()
	idx := strings.Index(out, "grandchild=")
	if idx < 0 {
		t.Fatalf("grandchild marker not in stdout: %q", out)
	}
	rest := out[idx+len("grandchild="):]
	if nl := strings.IndexAny(rest, "\r\n"); nl >= 0 {
		rest = rest[:nl]
	}
	pid, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		t.Fatalf("parse pid %q: %v", rest, err)
	}
	return pid
}

// Quiet the unused-import lint if the package ever gets reorganized.
var _ = fmt.Sprintf
