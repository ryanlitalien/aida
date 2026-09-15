//go:build unix

package execx_test

import (
	"context"
	"errors"
	"fmt"
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
		err := syscall.Kill(pid, 0)
		if err != nil && errors.Is(err, syscall.ESRCH) {
			return // gone - test passes
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Cleanup so we don't leak if the assertion fails.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild pid %d still alive after Run returned (would have hung for 30s without the fix)", pid)
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
