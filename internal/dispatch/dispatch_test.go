package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeShim writes an executable shell script named name into dir and
// returns dir prepended onto a fresh PATH so it resolves first, mirroring
// the shim pattern in internal/remotex/script_test.go.
func writeShim(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim %s: %v", name, err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestFallback_UsesCharterWhenAGENTSMdExists verifies the new behavior:
// when the charter directory has an AGENTS.md, fallback runs the subagent
// transport (roster.RunClaudeIn) in that directory instead of shelling
// out to a plain `aida <task>` subprocess.
func TestFallback_UsesCharterWhenAGENTSMdExists(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell shim")
	}

	charterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(charterDir, "AGENTS.md"), []byte("# Aida\n"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	shimDir := t.TempDir()
	// $1=--print $2=--dangerously-skip-permissions $3=prompt (execx.Run
	// passes args directly, no shell word-splitting, so $3 is the whole
	// prompt string including spaces).
	newPath := writeShim(t, shimDir, "claude", "#!/bin/sh\necho \"PWD=$(pwd)\"\necho \"PROMPT=$3\"\n")
	t.Setenv("PATH", newPath)

	d := &Dispatcher{charterDir: charterDir}
	answer, err := d.fallback(context.Background(), "what's on my plate today")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}

	wantDir, err := filepath.EvalSymlinks(charterDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.Contains(answer, "PWD="+wantDir) {
		t.Errorf("fallback answer = %q, want it to report running in %q", answer, wantDir)
	}
	if !strings.Contains(answer, "You are Aida") {
		t.Errorf("fallback answer = %q, want the charter persona preamble", answer)
	}
	if !strings.Contains(answer, "Task: what's on my plate today") {
		t.Errorf("fallback answer = %q, want the raw task appended verbatim", answer)
	}
}

// TestFallback_KeepsEngineWhenNoAGENTSMd verifies the pre-existing
// behavior is untouched when there's no charter: fallback shells out to
// `aida <task>` rather than trying the subagent transport.
func TestFallback_KeepsEngineWhenNoAGENTSMd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell shim")
	}

	charterDir := t.TempDir() // no AGENTS.md written

	shimDir := t.TempDir()
	newPath := writeShim(t, shimDir, "aida", "#!/bin/sh\necho \"engine reply: $1\"\n")
	t.Setenv("PATH", newPath)

	d := &Dispatcher{charterDir: charterDir}
	answer, err := d.fallback(context.Background(), "what's the weather")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if answer != "engine reply: what's the weather" {
		t.Errorf("fallback answer = %q, want the plain engine subprocess path used", answer)
	}
}

// TestFallback_DepthGuardEnvSetOnCharterCall confirms the charter path
// bumps AIDA_DISPATCH_DEPTH the same way a roster subagent backend does,
// so an `aida ask aida` fallback is subject to the same one-hop guard.
func TestFallback_DepthGuardEnvSetOnCharterCall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX shell shim")
	}

	charterDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(charterDir, "AGENTS.md"), []byte("# Aida\n"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	shimDir := t.TempDir()
	newPath := writeShim(t, shimDir, "claude", "#!/bin/sh\necho \"DEPTH=$AIDA_DISPATCH_DEPTH\"\n")
	t.Setenv("PATH", newPath)
	t.Setenv("AIDA_DISPATCH_DEPTH", "") // ensure a clean baseline for this process's own env

	d := &Dispatcher{charterDir: charterDir}
	answer, err := d.fallback(context.Background(), "anything")
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if !strings.Contains(answer, "DEPTH=1") {
		t.Errorf("fallback answer = %q, want AIDA_DISPATCH_DEPTH bumped to 1", answer)
	}
}
