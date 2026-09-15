package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureHookScriptEmbed guards the embedded asset itself: it must be
// non-empty, a real bash script, and actually hand off to the Go-side
// capture-hook command rather than duplicating its logic.
func TestCaptureHookScriptEmbed(t *testing.T) {
	if strings.TrimSpace(captureHookScript) == "" {
		t.Fatal("captureHookScript is empty")
	}
	if !strings.HasPrefix(captureHookScript, "#!/usr/bin/env bash") {
		t.Errorf("captureHookScript does not start with a bash shebang, got: %q", firstLine(captureHookScript, 40))
	}
	if !strings.Contains(captureHookScript, "aida brain capture-hook") {
		t.Error("captureHookScript does not invoke `aida brain capture-hook`")
	}
}

// TestInstallCaptureHook exercises installCaptureHook against a temp
// ~/.claude-shaped directory: the first call must write the script fresh
// (mode 0755, exact embedded content), and the second call must be a
// no-op that reports installed=false without altering the file.
func TestInstallCaptureHook(t *testing.T) {
	claudeDir := t.TempDir()
	hookPath := filepath.Join(claudeDir, "hooks", "capture-memory.sh")

	installed, err := installCaptureHook(claudeDir)
	if err != nil {
		t.Fatalf("first installCaptureHook: %v", err)
	}
	if !installed {
		t.Error("first installCaptureHook: want installed=true, got false")
	}

	info, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat %s: %v", hookPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0755 {
		t.Errorf("hook file mode = %o, want 0755", perm)
	}

	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read %s: %v", hookPath, err)
	}
	if string(data) != captureHookScript {
		t.Error("hook file content does not match captureHookScript")
	}

	firstModTime := info.ModTime()

	// Second call: content is already identical, so this must be a no-op.
	installed, err = installCaptureHook(claudeDir)
	if err != nil {
		t.Fatalf("second installCaptureHook: %v", err)
	}
	if installed {
		t.Error("second installCaptureHook: want installed=false (already installed), got true")
	}

	info2, err := os.Stat(hookPath)
	if err != nil {
		t.Fatalf("stat %s after second call: %v", hookPath, err)
	}
	if !info2.ModTime().Equal(firstModTime) {
		t.Error("second installCaptureHook rewrote the file; expected a no-op")
	}
}

// TestInstallCaptureHookOverwritesStaleContent covers the third case: an
// existing file whose content has drifted from the embedded script (e.g.
// installed by an older aida build) must be overwritten, not left stale.
func TestInstallCaptureHookOverwritesStaleContent(t *testing.T) {
	claudeDir := t.TempDir()
	hooksDir := filepath.Join(claudeDir, "hooks")
	hookPath := filepath.Join(hooksDir, "capture-memory.sh")

	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/usr/bin/env bash\necho stale\n"), 0644); err != nil {
		t.Fatalf("seed stale hook: %v", err)
	}

	installed, err := installCaptureHook(claudeDir)
	if err != nil {
		t.Fatalf("installCaptureHook: %v", err)
	}
	if !installed {
		t.Error("want installed=true for stale content, got false")
	}

	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read %s: %v", hookPath, err)
	}
	if string(data) != captureHookScript {
		t.Error("stale hook content was not overwritten with captureHookScript")
	}
}
