package remotex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// base64Alphabet is the set of characters standard base64 can ever
// produce. None of them are shell metacharacters (no space, $, `, ", ',
// ;, |, &, (, ), <, >, \, #, *, ?, [, ], {, }, ~, !). That's the whole
// trick: by construction the payload text that reaches a shell is
// always drawn from this alphabet, regardless of what bytes the caller
// asked to send.
var base64Alphabet = regexp.MustCompile(`^[A-Za-z0-9+/]*={0,2}$`)

// BuildRemoteScript renders a POSIX /bin/sh script whose ONLY dynamic
// content is base64 text drawn from an alphabet with no shell
// metacharacters. The script is delivered on ssh's stdin, so no
// user-controlled byte ever crosses sshd's login shell, sudo -i's login
// shell, or any argv-joining layer.
//
// Each argv element becomes its own base64 blob, decoded back into a
// shell variable, and the variables are exec'd as a properly quoted
// argv - so "argv-in, argv-out" holds exactly even for arguments
// containing spaces, quotes, newlines, or NUL-adjacent binary junk
// (module NUL itself, which can't survive process argv to begin with).
func BuildRemoteScript(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("remotex: BuildRemoteScript requires a non-empty argv")
	}

	var b strings.Builder
	b.WriteString("set -eu\n")

	for i, arg := range argv {
		enc := base64.StdEncoding.EncodeToString([]byte(arg))

		// Belt-and-braces: re-validate the encoder's own output before
		// it goes anywhere near a shell. This is deliberately redundant
		// with the guarantee encoding/base64 already gives us - it's
		// here so that a future refactor (e.g. swapping encoders, or
		// someone "simplifying" this by hand-rolling base64) fails a
		// test immediately instead of quietly reopening the injection
		// this whole package exists to close.
		if !base64Alphabet.MatchString(enc) {
			return nil, fmt.Errorf("remotex: base64 output for argv[%d] contains unexpected characters", i)
		}

		// base64 -d: GNU coreutils and busybox both accept the
		// lowercase long-style -d for decode. macOS/BSD base64 wants
		// -D instead, but that's irrelevant here - the script only
		// ever runs on the remote Linux host via /bin/sh -s.
		//
		// The `; printf x` inside the substitution and `${aN%x}`
		// outside it are load-bearing, not decoration: POSIX command
		// substitution `$(...)` strips ALL trailing newlines from its
		// output, so a bare `a0=$(... | base64 -d)` would silently
		// truncate any argv element ending in "\n". Appending a sentinel
		// byte *inside* the substitution shields any real trailing
		// newlines from that stripping, and `${aN%x}` (POSIX parameter
		// expansion, "remove shortest matching suffix") then removes
		// exactly the one sentinel byte we added. Both are POSIX, so
		// this stays portable to dash/busybox sh.
		fmt.Fprintf(&b, "a%d=$(printf %%s '%s' | base64 -d; printf x); a%d=${a%d%%x}\n", i, enc, i, i)
	}

	b.WriteString("exec")
	for i := range argv {
		fmt.Fprintf(&b, " \"$a%d\"", i)
	}
	b.WriteString("\n")

	return []byte(b.String()), nil
}

// hostRe allows a plain hostname, an ssh_config Host alias, or
// user@host - nothing else. The leading-character class deliberately
// excludes '-' so a string like "-oProxyCommand=..." can never be
// mistaken for an ssh flag even if ValidateHost's caller forgets the
// "--" defense-in-depth in SSHArgs.
var hostRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,253}$`)

// ValidateHost rejects anything that is not a plain hostname, ssh_config
// alias, or user@host. Leading '-' is excluded by the first character
// class so a device name can never be read as an ssh flag.
func ValidateHost(s string) error {
	if !hostRe.MatchString(s) {
		return fmt.Errorf("remotex: invalid host %q", s)
	}
	return nil
}

// targetRe is an allowlist, not an escaping scheme: unknown shapes are
// rejected outright rather than quoted, because a herdr target string
// is about to be threaded through BuildRemoteScript's argv (safe) but
// may ALSO be logged, displayed, or matched against elsewhere - an
// allowlist keeps every one of those call sites safe for free.
var targetRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:%-]{0,127}$`)

// ValidateTarget constrains a herdr agent target (name, pane id,
// terminal id). Allowlist: unknown shapes are rejected, not escaped.
func ValidateTarget(s string) error {
	if !targetRe.MatchString(s) {
		return fmt.Errorf("remotex: invalid target %q", s)
	}
	return nil
}

// SSHArgs returns the fixed local argv for a stdin-script ssh invocation.
// host MUST have passed ValidateHost.
//
// Each "-o key=value" is two separate argv elements (not one string
// with an embedded space) because exec.Command never invokes a shell:
// ssh's own getopt parses argv directly, and a single element like
// "-o BatchMode=yes" would hand it the literal (space-prefixed) string
// " BatchMode=yes" as the option value instead of splitting on the
// space the way a shell would.
func SSHArgs(host string) []string {
	return []string{
		"-T", // no pty - a pty would mangle the script piped over stdin
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=4",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
		"--", // mandatory: without it, a host string starting with '-'
		// becomes a local ssh flag (e.g. -oProxyCommand=... is local
		// code execution). ValidateHost's charset already excludes a
		// leading '-', but this is the second, independent layer.
		host,
	}
}

// RunRemote builds the script, runs `ssh <SSHArgs> <sudoPrefix...> /bin/sh -s`
// with the script on stdin, and returns the result.
// sudoUser may be "" for no sudo.
func RunRemote(ctx context.Context, r Runner, host, sudoUser string, argv []string, timeout time.Duration) (Result, error) {
	if err := ValidateHost(host); err != nil {
		return Result{}, err
	}
	if sudoUser != "" {
		// Same host-ish charset: a POSIX username is a strict subset of
		// what ValidateHost already accepts, and reusing it means one
		// validator to audit instead of two near-duplicate regexes.
		if err := ValidateHost(sudoUser); err != nil {
			return Result{}, fmt.Errorf("remotex: invalid sudo user %q: %w", sudoUser, err)
		}
	}

	script, err := BuildRemoteScript(argv)
	if err != nil {
		return Result{}, err
	}

	args := SSHArgs(host)
	if sudoUser != "" {
		// sudo -i: full login shell for the target user, so the
		// command inherits that user's environment/PATH the way an
		// interactive sudo session would, rather than the invoking
		// user's environment with just the UID flipped.
		args = append(args, "sudo", "-iu", sudoUser, "/bin/sh", "-s")
	} else {
		args = append(args, "/bin/sh", "-s")
	}

	return r.Run(ctx, "ssh", args, script, timeout)
}
