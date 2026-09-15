package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveSbxHelloWorld exercises the REAL sbx microVM path end to end through
// Wrap (create -> exec -> cleanup), proving valid data comes back from inside a
// Linux microVM with the worktree mounted and env forwarded.
//
// Validated 2026-06-17 (sbx 0.32, balanced policy): output was
// `uname=Linux aarch64`, `HELLO=world`, and `pwd` equal to the host worktree
// path - confirming sbx mounts the workspace at the SAME absolute path, so the
// `-w WorkDir` flag in sbxExecArgs is correct.
//
// Skipped by default - it needs `sbx` installed + authenticated (`sbx login`),
// a default network policy (`sbx policy set-default balanced`), and a few GB of
// free disk for the microVM image. Run it with:
//
//	SBX_LIVE=1 go test ./internal/sandbox/ -run TestLiveSbxHelloWorld -v -timeout 10m
func TestLiveSbxHelloWorld(t *testing.T) {
	if os.Getenv("SBX_LIVE") == "" {
		t.Skip("set SBX_LIVE=1 to run the live sbx microVM test (needs sbx login + disk)")
	}
	if !Available(TierDocker) {
		t.Skip("sbx not installed")
	}

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "marker.txt"), []byte("from-host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Policy{WorkDir: work, Env: map[string]string{"HELLO": "world"}}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	probe := `echo "uname=$(uname -s -m)"; echo "HELLO=$HELLO"; echo "pwd=$(pwd)"; ls`
	cmd, cleanup, err := Wrap(ctx, TierDocker, p, "/bin/sh", "-c", probe)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer cleanup()

	out, err := cmd.CombinedOutput()
	t.Logf("sbx exec output:\n%s", out)
	if err != nil {
		t.Fatalf("sbx exec: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "uname=Linux") {
		t.Errorf("expected a Linux microVM, got:\n%s", s)
	}
	if !strings.Contains(s, "HELLO=world") {
		t.Errorf("env not forwarded into the sandbox:\n%s", s)
	}
	if !strings.Contains(s, "marker.txt") {
		t.Errorf("worktree not mounted at the exec workdir (no marker.txt); the -w mount path may differ:\n%s", s)
	}
}

// TestLiveSbxRunsAida proves the follow-on goal: a REAL linux/arm64 `aida`
// runs inside the sbx microVM. It cross-compiles aida (CGO_ENABLED=0, GOOS=linux
// GOARCH=arm64) on the host, then Wrap provisions it (sbx cp) into a fresh
// microVM and runs `aida --version` in place of the macOS binary - exercising
// the Policy.ProvisionBin path end to end.
//
// Same gating as TestLiveSbxHelloWorld: needs `sbx` installed + authenticated
// + a network policy + disk, plus the Go toolchain to cross-compile. Run with:
//
//	SBX_LIVE=1 go test ./internal/sandbox/ -run TestLiveSbxRunsAida -v -timeout 10m
func TestLiveSbxRunsAida(t *testing.T) {
	if os.Getenv("SBX_LIVE") == "" {
		t.Skip("set SBX_LIVE=1 to run the live sbx microVM test (needs sbx login + disk)")
	}
	if !Available(TierDocker) {
		t.Skip("sbx not installed")
	}

	// Cross-compile a linux/arm64 aida into a temp path. Pure-Go sqlite +
	// CGO_ENABLED=0 means no cross C toolchain is needed.
	work := t.TempDir()
	linuxAida := filepath.Join(work, "aida-linux-arm64")
	build := exec.Command("go", "build", "-o", linuxAida, "github.com/ryanlitalien/aida/cmd/aida")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=arm64")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile linux aida: %v\n%s", err, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// name "aida" -> in-VM /usr/local/bin/aida; ProvisionBin is the file copied in.
	p := Policy{WorkDir: work, ProvisionBin: linuxAida}
	cmd, cleanup, err := Wrap(ctx, TierDocker, p, "aida", "--version")
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer cleanup()

	out, err := cmd.CombinedOutput()
	t.Logf("in-VM `aida --version` output:\n%s", out)
	if err != nil {
		t.Fatalf("run provisioned aida in sandbox: %v", err)
	}
	// `aida --version` prints "aida <buildVersion>" (e.g. "aida v0.117.1-... <sha>").
	// A non-empty line beginning with the binary name proves the linux build
	// executed in the VM (a missing/non-exec binary would error above instead).
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "aida ") {
		t.Errorf("provisioned aida did not report its version:\n%s", out)
	}
}
