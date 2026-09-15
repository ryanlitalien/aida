package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

// withTempHome points config.Dir() (and thus every ~/.aida path helper) at
// a fresh temp dir for the duration of one test, matching the
// t.Setenv("HOME", ...) convention used elsewhere in this package.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// runInit only reads from stdin to prompt for an Anthropic key when
	// none is already set; pre-setting it here keeps these tests from
	// blocking on os.Stdin under `go test`.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-EXAMPLE-not-a-real-key")
	return tmp
}

func TestRunInit_BootstrapsFromNothing(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".aida")

	if err := runInit("", "", false); err != nil {
		t.Fatalf("runInit: %v", err)
	}

	for _, rel := range []string{
		"config.yaml",
		"library.yaml",
		"library/sources",
		"library/routes.yaml",
		"brain/.git",
		"roster.yaml",
		"env.example",
		"op.env.example",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("expected %s to exist after runInit: %v", rel, err)
		}
	}

	// The example sources should have been copied into the library.
	for _, name := range []string{"codebase-example.yaml", "docs-example.yaml", "exec-example.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, "library", "sources", name)); err != nil {
			t.Errorf("expected example source %s to be copied: %v", name, err)
		}
	}
}

func TestRunInit_IdempotentWithoutForce(t *testing.T) {
	home := withTempHome(t)
	configPath := filepath.Join(home, ".aida", "config.yaml")

	if err := runInit("", "", false); err != nil {
		t.Fatalf("first runInit: %v", err)
	}

	// Simulate a hand edit to config.yaml -- a real re-run must not stomp it.
	marker := []byte("# hand-edited marker\n")
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config.yaml: %v", err)
	}
	if err := os.WriteFile(configPath, append(original, marker...), 0644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}

	if err := runInit("", "", false); err != nil {
		t.Fatalf("second runInit: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config.yaml after rerun: %v", err)
	}
	if string(after) != string(append(original, marker...)) {
		t.Errorf("runInit without --force modified config.yaml; want the hand-edited marker preserved")
	}
}

func TestRunInit_ForceOverwritesConfig(t *testing.T) {
	home := withTempHome(t)
	configPath := filepath.Join(home, ".aida", "config.yaml")

	if err := runInit("", "", false); err != nil {
		t.Fatalf("first runInit: %v", err)
	}
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config.yaml: %v", err)
	}
	if err := os.WriteFile(configPath, append(original, []byte("# marker\n")...), 0644); err != nil {
		t.Fatalf("writing marker: %v", err)
	}

	if err := runInit("", "", true); err != nil {
		t.Fatalf("forced runInit: %v", err)
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config.yaml after forced rerun: %v", err)
	}
	if string(after) == string(append(original, []byte("# marker\n")...)) {
		t.Errorf("runInit --force should have overwritten the hand-edited config.yaml")
	}
	// The forced rewrite should still be a valid, loadable config.
	if _, err := config.LoadConfig(); err != nil {
		t.Errorf("config.yaml after --force failed to load: %v", err)
	}
}

func TestRunInit_ConfigRemoteOnlyWhenRequested(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".aida")

	if err := runInit("", "", false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Errorf("expected no config git repo without --config-remote")
	}
}

func TestRunInit_ConfigRemoteInitializesRepo(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".aida")

	if err := runInit("", "https://example.invalid/backup.git", false); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("expected a config git repo with --config-remote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); err != nil {
		t.Errorf("expected a .gitignore excluding brain/ and secrets: %v", err)
	}
}
