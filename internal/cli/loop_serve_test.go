package cli

import (
	"testing"
	"time"
)

// TestDefaultLoopOptsAreSafe guards the serve --loop integration: serve reuses
// defaultLoopOpts() for the loop knobs it doesn't expose as flags, so those
// defaults must be non-zero. A bare loopOpts{} would give perTask=0 (every
// spawned agent's context cancels immediately) and maxFix=0 (no fix attempts).
func TestDefaultLoopOptsAreSafe(t *testing.T) {
	o := defaultLoopOpts()
	if o.perTask <= 0 {
		t.Errorf("perTask must be > 0 (a zero timeout cancels every agent immediately), got %v", o.perTask)
	}
	if o.maxFix <= 0 {
		t.Errorf("maxFix must be > 0, got %d", o.maxFix)
	}
	if o.poll <= 0 {
		t.Errorf("poll must be > 0 (daemon idle sleep), got %v", o.poll)
	}
	if o.concurrency < 1 {
		t.Errorf("concurrency must be >= 1, got %d", o.concurrency)
	}
	if o.base == "" {
		t.Error("base branch must default to a non-empty value")
	}
	// Sanity: match the documented per-task default so serve and `aida loop`
	// behave identically when neither overrides it.
	if o.perTask != 450*time.Second {
		t.Errorf("perTask default = %v, want 450s (keep in sync with newLoopCmd)", o.perTask)
	}
}

// TestServeRegistersLoopFlags confirms the --loop* flags are wired onto the
// serve command (so `aida serve --loop --loop-tag auto ...` parses).
func TestServeRegistersLoopFlags(t *testing.T) {
	cmd := newServeCmd()
	for _, name := range []string{"loop", "loop-tag", "loop-worktree", "loop-pr", "loop-sandbox", "loop-check", "loop-concurrency", "loop-reviewer", "loop-sandbox-memory", "loop-provision-aida"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("serve is missing the --%s flag", name)
		}
	}
}

// TestLoopOptsValidate covers the shared flag-combo validation that both
// `aida loop` and `aida serve --loop` run (the latter for fail-fast UX).
func TestLoopOptsValidate(t *testing.T) {
	cases := []struct {
		name    string
		o       loopOpts
		wantErr bool
	}{
		{"defaults ok", defaultLoopOpts(), false},
		{"pr and commit conflict", loopOpts{pr: true, commit: true}, true},
		{"pr implies worktree (sandbox ok)", loopOpts{pr: true, sandboxTier: "docker"}, false},
		{"sandbox docker without worktree", loopOpts{sandboxTier: "docker"}, true},
		{"sandbox docker with worktree", loopOpts{sandboxTier: "docker", worktree: true}, false},
		{"concurrency without worktree", loopOpts{concurrency: 3, sandboxTier: "none"}, true},
		{"concurrency with worktree", loopOpts{concurrency: 3, worktree: true, sandboxTier: "none"}, false},
		{"bad tier string", loopOpts{sandboxTier: "seatbelt"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
