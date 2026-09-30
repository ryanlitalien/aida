package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
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

// TestDefaultLoopOptsArbiterOnByDefault pins the 2026-09-25 decision:
// lane enforcement is on everywhere by default, including every caller of
// defaultLoopOpts() (aida loop, aida serve --loop, aida swarm) -- not just
// aida loop's own flag default.
func TestDefaultLoopOptsArbiterOnByDefault(t *testing.T) {
	if !defaultLoopOpts().arbiter {
		t.Error("defaultLoopOpts().arbiter = false, want true (enforcement on everywhere)")
	}
}

// TestLoopArbiterFlagDefaultsTrue confirms `aida loop`'s --arbiter flag
// itself defaults to true (not just defaultLoopOpts()'s struct value,
// which newLoopCmd's flag registration could silently override).
func TestLoopArbiterFlagDefaultsTrue(t *testing.T) {
	cmd := newLoopCmd()
	f := cmd.Flags().Lookup("arbiter")
	if f == nil {
		t.Fatal("aida loop is missing the --arbiter flag")
	}
	if f.DefValue != "true" {
		t.Errorf("--arbiter default = %q, want %q", f.DefValue, "true")
	}
}

// TestServeLoopArbiterFlagDefaultsTrue is --loop-arbiter's twin for `aida
// serve`.
func TestServeLoopArbiterFlagDefaultsTrue(t *testing.T) {
	cmd := newServeCmd()
	f := cmd.Flags().Lookup("loop-arbiter")
	if f == nil {
		t.Fatal("aida serve is missing the --loop-arbiter flag")
	}
	if f.DefValue != "true" {
		t.Errorf("--loop-arbiter default = %q, want %q", f.DefValue, "true")
	}
}

// TestCheckLanesFileMissingErrors covers the helper `aida serve --loop`
// calls when enforcement is on: a missing lane roster is an error naming
// the path, matching every other lane-roster consumer.
func TestCheckLanesFileMissingErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lanes.yaml")
	cfg := &config.Config{Lanes: config.LanesConfig{Path: path}}
	err := checkLanesFile(cfg)
	if err == nil {
		t.Fatal("checkLanesFile(missing roster) = nil error, want one naming the path")
	}
	if !strings.Contains(err.Error(), "no lane roster at "+path) {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
}

// TestServeLoopArbiterFailsFastWithoutLanesFile is the end-to-end version
// of TestCheckLanesFileMissingErrors: `aida serve --loop` with enforcement
// on (the default) and no lane roster must fail before ever starting the
// HTTP daemon, so the port is never bound. HOME is overridden to a fresh
// tmp dir so this never depends on -- or interferes with -- the real
// ~/.aida/lanes.yaml this dev machine may have.
func TestServeLoopArbiterFailsFastWithoutLanesFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := newServeCmd()
	cmd.SetArgs([]string{"--http", "--loop", "--no-jarvis", "--no-listen", "--no-ptt"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("aida serve --loop with no lane roster = nil error, want one before the daemon starts")
	}
	if !strings.Contains(err.Error(), "no lane roster at") {
		t.Errorf("error = %q, want it to mention the missing lane roster", err)
	}
}

// TestServeRegistersLoopFlags confirms the --loop* flags are wired onto the
// serve command (so `aida serve --loop --loop-tag auto ...` parses).
func TestServeRegistersLoopFlags(t *testing.T) {
	cmd := newServeCmd()
	for _, name := range []string{"loop", "loop-tag", "loop-worktree", "loop-pr", "loop-sandbox", "loop-check", "loop-concurrency", "loop-reviewer", "loop-sandbox-memory", "loop-provision-aida", "loop-arbiter"} {
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
