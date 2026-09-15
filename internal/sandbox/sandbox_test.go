package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestParseTier(t *testing.T) {
	for in, want := range map[string]Tier{
		"": TierNone, "none": TierNone, "docker": TierDocker, "DOCKER": TierDocker, "sbx": TierDocker,
	} {
		got, err := ParseTier(in)
		if err != nil || got != want {
			t.Errorf("ParseTier(%q) = %q,%v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseTier("seatbelt"); err == nil {
		t.Error("ParseTier(seatbelt) should error now that the tier is dropped")
	}
}

func TestSandboxName(t *testing.T) {
	if got := SandboxName("/x/y/loop-12-fix-thing"); got != "aida-loop-12-fix-thing" {
		t.Errorf("name = %q", got)
	}
	if got := SandboxName("/x/weird name.v2"); got != "aida-weird-name-v2" {
		t.Errorf("name = %q (should sanitize)", got)
	}
}

func TestSbxCreateArgs(t *testing.T) {
	got := sbxCreateArgs("aida-x", Policy{WorkDir: "/w", MemoryLimit: "8g", ExtraMounts: []string{"/run:ro"}})
	want := []string{"create", "--name", "aida-x", "-m", "8g", "shell", "/w", "/run:ro"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("create args = %v, want %v", got, want)
	}
	// No memory limit => no -m flag.
	got = sbxCreateArgs("aida-x", Policy{WorkDir: "/w"})
	if strings.Contains(strings.Join(got, " "), "-m") {
		t.Errorf("create args should omit -m when MemoryLimit empty: %v", got)
	}
}

func TestSbxExecArgs(t *testing.T) {
	p := Policy{WorkDir: "/w", Env: map[string]string{"AIDA_PROFILE": "work", "NO_COLOR": "1"}}
	got := sbxExecArgs("aida-x", p, "/bin/sh", []string{"-c", "echo hi"})
	joined := strings.Join(got, " ")
	// workdir, both env vars (sorted), then sandbox name, command, args - all discrete argv.
	want := "exec -w /w -e AIDA_PROFILE=work -e NO_COLOR=1 aida-x /bin/sh -c echo hi"
	if joined != want {
		t.Errorf("exec args =\n  %q\nwant\n  %q", joined, want)
	}
}

func TestProvisionPath(t *testing.T) {
	cases := map[string]string{
		"/Users/fakehome/go/bin/aida": "/usr/local/bin/aida",
		"aida":                        "/usr/local/bin/aida",
		"/tmp/aida-linux-arm64":       "/usr/local/bin/aida-linux-arm64",
	}
	for in, want := range cases {
		if got := provisionPath(in); got != want {
			t.Errorf("provisionPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TierNone must ignore ProvisionBin entirely - it runs the host binary in
// place (no sandbox to copy into), so the returned cmd is the bare `name`
// with the host working dir, regardless of ProvisionBin.
func TestWrapNoneIgnoresProvisionBin(t *testing.T) {
	p := Policy{WorkDir: "/w", ProvisionBin: "/tmp/aida-linux-arm64"}
	cmd, cleanup, err := Wrap(context.Background(), TierNone, p, "/bin/echo", "hi")
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer cleanup()
	if cmd.Path != "/bin/echo" || cmd.Dir != "/w" {
		t.Errorf("TierNone should run the host binary in place: path=%q dir=%q", cmd.Path, cmd.Dir)
	}
	if strings.Contains(strings.Join(cmd.Args, " "), "aida-linux-arm64") {
		t.Errorf("TierNone must not reference ProvisionBin: %v", cmd.Args)
	}
}

func TestResolveFallsBackToNoneWithoutSbx(t *testing.T) {
	// We can't assert sbx presence either way on CI, but the contract holds:
	// requesting none always resolves to none with no note.
	got, note := Resolve(TierNone)
	if got != TierNone || note != "" {
		t.Errorf("Resolve(none) = %q,%q; want none,\"\"", got, note)
	}
	// docker resolves to docker iff sbx is installed, else none with a note.
	got, note = Resolve(TierDocker)
	if Available(TierDocker) {
		if got != TierDocker {
			t.Errorf("sbx present but Resolve(docker)=%q", got)
		}
	} else {
		if got != TierNone || note == "" {
			t.Errorf("sbx absent: Resolve(docker)=%q note=%q; want none + note", got, note)
		}
	}
}
