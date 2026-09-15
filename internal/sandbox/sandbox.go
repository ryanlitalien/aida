// Package sandbox confines a command (the autonomous loop's agent work) inside
// a Docker Sandboxes microVM via the standalone `sbx` CLI, so an unattended
// (voice-launchable) agent can't touch the host filesystem or network.
//
// Tiers:
//   - docker: a `docker sandbox` microVM per worktree via the `sbx` CLI
//     (brew install docker/tap/sbx) - its own Arm-Linux kernel/filesystem/
//     network, independent of any host docker daemon, so it coexists with
//     OrbStack and needs no Docker Desktop.
//   - none:   no confinement (the historical behavior).
//
// (An earlier revision had a macOS Seatbelt tier; it was dropped in favor of
// the stronger, kernel-isolated sbx microVM.)
//
// IMPORTANT: an sbx sandbox is a LINUX microVM. A macOS-native binary like
// `aida` cannot run inside it directly. Wrap closes that gap via Policy.
// ProvisionBin: set it to a linux/arm64 build of the command and Wrap copies
// the binary into the VM (sbx cp) after create, then runs it from its
// in-sandbox path (provisionPath) instead of the host `name`. The loop builds
// that linux `aida` on demand (CGO_ENABLED=0, pure-Go sqlite cross-compiles
// cleanly) and passes it through. Without ProvisionBin the docker tier still
// runs any VM-native command (create -> exec -> cleanup).
package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Tier is the confinement strength.
type Tier string

const (
	TierNone   Tier = "none"
	TierDocker Tier = "docker" // Docker Sandboxes, via the `sbx` CLI
)

// sbxBin is the Docker Sandboxes CLI. Homebrew links it at /opt/homebrew/bin.
const sbxBin = "sbx"

// ParseTier validates a tier string (empty => none). "sbx" is accepted as an
// alias for "docker" (the Docker Sandboxes product is driven by the sbx CLI).
func ParseTier(s string) (Tier, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", string(TierNone):
		return TierNone, nil
	case string(TierDocker), "sbx":
		return TierDocker, nil
	default:
		return TierNone, fmt.Errorf("unknown sandbox tier %q (want none|docker)", s)
	}
}

// Policy describes the confinement for one command.
type Policy struct {
	WorkDir     string            // the worktree mounted into the sandbox and used as the workdir
	ExtraMounts []string          // additional host paths to mount (append ":ro" for read-only)
	MemoryLimit string            // sbx -m, e.g. "8g"; empty => sbx default (50% host)
	Env         map[string]string // env to set inside the sandbox (via sbx exec -e)

	// ProvisionBin, when set (docker tier only), is the host path to a
	// linux/arm64 build of the command. Wrap copies it into the microVM
	// (sbx cp) after create and runs the command from its in-sandbox path
	// instead of the host `name`. This is how a macOS-native binary like
	// `aida` actually runs inside the Linux microVM - without it, the docker
	// tier can only run VM-native commands. Ignored by TierNone.
	ProvisionBin string
}

// Available reports whether the tooling for a tier is present. For docker this
// is the real `sbx` binary - NOT a `docker sandbox` subcommand probe, which
// false-positives on OrbStack (it prints generic help and exits 0).
func Available(t Tier) bool {
	switch t {
	case TierNone:
		return true
	case TierDocker:
		_, err := exec.LookPath(sbxBin)
		return err == nil
	}
	return false
}

// Resolve returns the requested tier if its tooling is present, else falls back
// to none with an explanatory note (there is no intermediate tier).
func Resolve(want Tier) (Tier, string) {
	if want == TierDocker && !Available(TierDocker) {
		return TierNone, "sbx (Docker Sandboxes CLI) not found - running UNCONFINED; install: brew install docker/tap/sbx"
	}
	return want, ""
}

// Wrap builds an *exec.Cmd that runs (name, args...) under the given tier and
// policy, plus a cleanup func to call after the command finishes.
//
//   - TierNone:   the bare command, cmd.Dir = WorkDir, cmd.Env = host env + Policy.Env.
//   - TierDocker: provisions an `sbx` microVM over WorkDir (the built-in `shell`
//     base) and returns an `sbx exec` command that runs (name, args...) inside
//     it with -w WorkDir and -e env; cleanup removes the sandbox. Args are
//     passed as discrete argv (no shell), so an agent prompt can't inject.
func Wrap(ctx context.Context, tier Tier, p Policy, name string, args ...string) (*exec.Cmd, func() error, error) {
	noop := func() error { return nil }
	switch tier {
	case TierNone:
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = p.WorkDir
		cmd.Env = append(os.Environ(), envSlice(p.Env)...)
		return cmd, noop, nil

	case TierDocker:
		if !Available(TierDocker) {
			return nil, noop, fmt.Errorf("sbx not found (Docker Sandboxes CLI); install: brew install docker/tap/sbx")
		}
		sbxName := SandboxName(p.WorkDir)
		// Clear any stale sandbox of the same name from a prior crashed run,
		// then create a fresh one over the worktree.
		_ = exec.Command(sbxBin, "rm", "-f", sbxName).Run()
		if out, err := exec.CommandContext(ctx, sbxBin, sbxCreateArgs(sbxName, p)...).CombinedOutput(); err != nil {
			return nil, noop, fmt.Errorf("sbx create: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		cleanup := func() error { return exec.Command(sbxBin, "rm", "-f", sbxName).Run() }

		// Run the host `name` by default. If a linux build was provisioned,
		// copy it into the VM and run THAT path instead - a macOS `aida` can't
		// execute in a Linux microVM, so we substitute its linux/arm64 build.
		execName := name
		if p.ProvisionBin != "" {
			inVM := provisionPath(name)
			if out, err := exec.CommandContext(ctx, sbxBin, "cp", p.ProvisionBin, sbxName+":"+inVM).CombinedOutput(); err != nil {
				_ = cleanup()
				return nil, noop, fmt.Errorf("sbx cp %s -> %s: %w (%s)", p.ProvisionBin, inVM, err, strings.TrimSpace(string(out)))
			}
			execName = inVM
		}
		cmd := exec.CommandContext(ctx, sbxBin, sbxExecArgs(sbxName, p, execName, args)...)
		return cmd, cleanup, nil
	}
	return nil, noop, fmt.Errorf("unknown sandbox tier %q", tier)
}

// sbxCreateArgs builds the argv for `sbx create` over the policy's worktree
// using the built-in `shell` base image. Extra mounts and a memory limit are
// appended when set.
func sbxCreateArgs(name string, p Policy) []string {
	args := []string{"create", "--name", name}
	if p.MemoryLimit != "" {
		args = append(args, "-m", p.MemoryLimit)
	}
	args = append(args, "shell", p.WorkDir)
	args = append(args, p.ExtraMounts...)
	return args
}

// sbxExecArgs builds the argv for `sbx exec` that runs (name, cmdArgs...) inside
// the sandbox, with the workdir set to the (same-path) mounted worktree and env
// forwarded via -e. The command + args are discrete argv elements - no shell.
func sbxExecArgs(name string, p Policy, cmdName string, cmdArgs []string) []string {
	args := []string{"exec"}
	if p.WorkDir != "" {
		args = append(args, "-w", p.WorkDir)
	}
	for _, kv := range envSlice(p.Env) {
		args = append(args, "-e", kv)
	}
	args = append(args, name, cmdName)
	args = append(args, cmdArgs...)
	return args
}

// provisionPath is the in-sandbox absolute path a provisioned binary is copied
// to and executed from. /usr/local/bin is on PATH and writable in the sbx
// `shell` base; the command's basename keeps the in-VM name familiar.
func provisionPath(name string) string {
	return "/usr/local/bin/" + filepath.Base(name)
}

// SandboxName derives a stable, sbx-safe sandbox name from the worktree path
// (basename, sanitized) so re-runs reuse/replace cleanly.
func SandboxName(workDir string) string {
	base := workDir
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	var b strings.Builder
	b.WriteString("aida-")
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// envSlice flattens an env map into sorted "K=V" strings (sorted for
// deterministic command construction + tests).
func envSlice(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
