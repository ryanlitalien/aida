package bifrost_test

import (
	"context"
	"encoding/base64"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ryanlitalien/aida/internal/bifrost"
	"github.com/ryanlitalien/aida/internal/remotex"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// blobRe matches the single-quoted base64 blobs BuildRemoteScript
// embeds in the stdin script (see remotex/script.go and
// remotex/script_test.go, which this helper mirrors).
var blobRe = regexp.MustCompile(`'([A-Za-z0-9+/]*={0,2})'`)

// decodeScriptArgv extracts the original herdr argv out of a
// RunRemote stdin script by independently base64-decoding each blob,
// rather than trusting BuildRemoteScript to check its own work.
func decodeScriptArgv(t *testing.T, stdin []byte) []string {
	t.Helper()
	matches := blobRe.FindAllStringSubmatch(string(stdin), -1)
	argv := make([]string, len(matches))
	for i, m := range matches {
		decoded, err := base64.StdEncoding.DecodeString(m[1])
		if err != nil {
			t.Fatalf("blob %d is not valid base64: %v", i, err)
		}
		argv[i] = string(decoded)
	}
	return argv
}

func newTestClient(t *testing.T, fr *remotex.FakeRunner) *bifrost.Client {
	t.Helper()
	c, err := bifrost.New(fr, "minty", "heimdall", []bifrost.Preset{
		{Name: "claude-work", Cwd: "/home/heimdall/proj", Argv: []string{"claude", "--dangerously-skip-permissions"}},
	})
	if err != nil {
		t.Fatalf("bifrost.New: %v", err)
	}
	return c
}

func TestAgentsParsesFixture(t *testing.T) {
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: loadFixture(t, "agent_list.json"), ExitCode: 0}},
	}}
	c := newTestClient(t, fr)

	agents, err := c.Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("len(agents) = %d, want 1", len(agents))
	}

	a := agents[0]
	if a.Name != "phone" {
		t.Errorf("Name = %q, want phone", a.Name)
	}
	if a.AgentStatus != "idle" {
		t.Errorf("AgentStatus = %q, want idle", a.AgentStatus)
	}
	if a.Cwd != "/home/heimdall/work" {
		t.Errorf("Cwd = %q, want /home/heimdall/work", a.Cwd)
	}
	if a.PaneID != "w4:p2" {
		t.Errorf("PaneID = %q, want w4:p2", a.PaneID)
	}
}

func TestStatusParsesFixture(t *testing.T) {
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: loadFixture(t, "status.txt"), ExitCode: 0}},
	}}
	c := newTestClient(t, fr)

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Running {
		t.Errorf("Running = false, want true")
	}
	if st.Version != "0.7.4" {
		t.Errorf("Version = %q, want 0.7.4", st.Version)
	}
	if st.Socket != "/home/heimdall/.config/herdr/herdr.sock" {
		t.Errorf("Socket = %q, want /home/heimdall/.config/herdr/herdr.sock", st.Socket)
	}
	if st.Raw == "" {
		t.Errorf("Raw = empty, want the full report")
	}
}

func TestAgentsHerdrError(t *testing.T) {
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: []byte(`{"id":"cli:agent:list","error":{"code":"E_NO_SERVER","message":"herdr server not running"}}`), ExitCode: 0}},
	}}
	c := newTestClient(t, fr)

	agents, err := c.Agents(context.Background())
	if err == nil {
		t.Fatal("Agents: got nil error, want error")
	}
	if agents != nil {
		t.Errorf("Agents = %v, want nil on herdr error (not a silent empty list)", agents)
	}
	if !strings.Contains(err.Error(), "E_NO_SERVER") {
		t.Errorf("error = %v, want it to mention the herdr error code", err)
	}
}

func TestAgentsTolerateMOTD(t *testing.T) {
	fixture := loadFixture(t, "agent_list.json")
	withMOTD := append([]byte("Welcome to Ubuntu 22.04\n\n"), fixture...)

	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: withMOTD, ExitCode: 0}},
	}}
	c := newTestClient(t, fr)

	agents, err := c.Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "phone" {
		t.Errorf("agents = %+v, want the single phone agent despite MOTD prefix", agents)
	}
}

func TestAgentsGarbageStdout(t *testing.T) {
	fr := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: []byte("not json at all"), ExitCode: 0}},
	}}
	c := newTestClient(t, fr)

	agents, err := c.Agents(context.Background())
	if err == nil {
		t.Fatal("Agents: got nil error, want error")
	}
	if agents != nil {
		t.Errorf("Agents = %v, want nil", agents)
	}
	if !strings.Contains(err.Error(), "not json at all") {
		t.Errorf("error = %v, want it to name what was received", err)
	}
}

func TestSendBuildsExpectedInvocation(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	target := "phone"
	payload := "x'; touch /tmp/pwned; id #"

	if err := c.Send(context.Background(), target, payload, false); err != nil {
		t.Fatalf("Send: %v", err)
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
		}
	}

	// The whole point of shipping the herdr argv as a stdin script:
	// no fragment of it - not the payload, not even benign parts like
	// "herdr" or "send" - should ever show up in the LOCAL argv ssh
	// itself sees (and that `ps` would show).
	fragments := []string{"herdr", "agent", "send", target, payload}
	for _, a := range call.Args {
		for _, frag := range fragments {
			if frag != "" && strings.Contains(a, frag) {
				t.Errorf("Args element %q contains argv fragment %q - payload leaked into local argv", a, frag)
			}
		}
	}

	gotArgv := decodeScriptArgv(t, call.Stdin)
	wantArgv := []string{"herdr", "agent", "send", target, payload}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("decoded argv = %v, want %v", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Errorf("decoded argv[%d] = %q, want %q", i, gotArgv[i], wantArgv[i])
		}
	}
}

func TestSendRejectsOversizeAndNUL(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	oversized := strings.Repeat("a", 8*1024+1)
	if err := c.Send(context.Background(), "phone", oversized, false); err == nil {
		t.Error("Send(oversized): got nil error, want error")
	}

	withNUL := "hello\x00world"
	if err := c.Send(context.Background(), "phone", withNUL, false); err == nil {
		t.Error("Send(NUL): got nil error, want error")
	}

	if fr.CallCount() != 0 {
		t.Errorf("CallCount = %d, want 0 (validation should reject before any remote call)", fr.CallCount())
	}
}

func TestSendEnterAppendsCR(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	text := "echo hi"
	if err := c.Send(context.Background(), "phone", text, true); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if fr.CallCount() != 1 {
		t.Fatalf("CallCount = %d, want 1", fr.CallCount())
	}
	argv := decodeScriptArgv(t, fr.Calls[0].Stdin)
	if len(argv) != 5 {
		t.Fatalf("decoded argv = %v, want 5 elements", argv)
	}
	want := text + "\r"
	if argv[4] != want {
		t.Errorf("argv[4] = %q, want %q", argv[4], want)
	}
}

func TestReadValidatesSourceAndClampsLines(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	if _, err := c.Read(context.Background(), "phone", "bogus-source", 10); err == nil {
		t.Error("Read(bad source): got nil error, want error")
	}
	if fr.CallCount() != 0 {
		t.Errorf("CallCount after bad source = %d, want 0", fr.CallCount())
	}

	// 0 lines -> default 200.
	fr2 := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: []byte(`{"id":"x","result":{"read":{"text":"hi"}}}`), ExitCode: 0}},
	}}
	c2 := newTestClient(t, fr2)
	if _, err := c2.Read(context.Background(), "phone", "", 0); err != nil {
		t.Fatalf("Read(0 lines): %v", err)
	}
	argv := decodeScriptArgv(t, fr2.Calls[0].Stdin)
	if !containsPair(argv, "--lines", "200") {
		t.Errorf("argv = %v, want --lines 200", argv)
	}

	// 99999 lines -> clamped to 2000.
	fr3 := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: []byte(`{"id":"x","result":{"read":{"text":"hi"}}}`), ExitCode: 0}},
	}}
	c3 := newTestClient(t, fr3)
	if _, err := c3.Read(context.Background(), "phone", "visible", 99999); err != nil {
		t.Fatalf("Read(99999 lines): %v", err)
	}
	argv3 := decodeScriptArgv(t, fr3.Calls[0].Stdin)
	if !containsPair(argv3, "--lines", "2000") {
		t.Errorf("argv = %v, want --lines 2000", argv3)
	}
}

func containsPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

func TestTargetValidationRejectsTraversal(t *testing.T) {
	bad := []string{"../etc", "a b", "a;b", ""}
	for _, target := range bad {
		t.Run(target, func(t *testing.T) {
			fr := &remotex.FakeRunner{}
			c := newTestClient(t, fr)

			if _, err := c.Read(context.Background(), target, "recent", 100); err == nil {
				t.Errorf("Read(%q): got nil error, want error", target)
			}
			if err := c.Send(context.Background(), target, "hi", false); err == nil {
				t.Errorf("Send(%q): got nil error, want error", target)
			}
			if fr.CallCount() != 0 {
				t.Errorf("CallCount for target %q = %d, want 0", target, fr.CallCount())
			}
		})
	}
}

func TestStartPresetUnknownName(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	if err := c.StartPreset(context.Background(), "does-not-exist"); err == nil {
		t.Error("StartPreset(unknown): got nil error, want error")
	}
	if fr.CallCount() != 0 {
		t.Errorf("CallCount = %d, want 0", fr.CallCount())
	}
}

func TestStartPresetBuildsArgv(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c := newTestClient(t, fr)

	if err := c.StartPreset(context.Background(), "claude-work"); err != nil {
		t.Fatalf("StartPreset: %v", err)
	}
	if fr.CallCount() != 1 {
		t.Fatalf("CallCount = %d, want 1", fr.CallCount())
	}

	gotArgv := decodeScriptArgv(t, fr.Calls[0].Stdin)
	wantArgv := []string{"herdr", "agent", "start", "claude-work", "--cwd", "/home/heimdall/proj", "--", "claude", "--dangerously-skip-permissions"}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("decoded argv = %v, want %v", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Errorf("decoded argv[%d] = %q, want %q", i, gotArgv[i], wantArgv[i])
		}
	}
}

// ---- New: preset validation ----

func TestNewRejectsMismatchedPresetUser(t *testing.T) {
	fr := &remotex.FakeRunner{}
	_, err := bifrost.New(fr, "minty", "ryan", []bifrost.Preset{
		{Name: "work-session", User: "heimdall", Cwd: "/home/heimdall/work", Argv: []string{"claude"}},
	})
	if err == nil {
		t.Fatal("New: got nil error, want error (preset user heimdall does not match client user ryan)")
	}
}

func TestNewAcceptsMatchingPresetUser(t *testing.T) {
	fr := &remotex.FakeRunner{}
	c, err := bifrost.New(fr, "minty", "ryan", []bifrost.Preset{
		{Name: "work-session", User: "ryan", Cwd: "/home/ryan/dev/acme_widgets", Argv: []string{"claude"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(c.Presets) != 1 {
		t.Fatalf("len(Presets) = %d, want 1", len(c.Presets))
	}
}

func TestNewAcceptsEmptyPresetUser(t *testing.T) {
	// An empty User is "belongs to whichever lane the caller assigned it
	// to" -- New has no lane list to check that against, so it must not
	// reject an empty User regardless of the client's own user.
	fr := &remotex.FakeRunner{}
	if _, err := bifrost.New(fr, "minty", "ryan", []bifrost.Preset{
		{Name: "work-session", Cwd: "/home/ryan/dev/acme_widgets", Argv: []string{"claude"}},
	}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestNewRejectsEmptyPresetName(t *testing.T) {
	fr := &remotex.FakeRunner{}
	if _, err := bifrost.New(fr, "minty", "heimdall", []bifrost.Preset{
		{Argv: []string{"claude"}},
	}); err == nil {
		t.Error("New: got nil error, want error (empty preset name)")
	}
}

func TestNewRejectsEmptyPresetArgv(t *testing.T) {
	fr := &remotex.FakeRunner{}
	if _, err := bifrost.New(fr, "minty", "heimdall", []bifrost.Preset{
		{Name: "work-session"},
	}); err == nil {
		t.Error("New: got nil error, want error (empty preset argv)")
	}
}

func TestNewRejectsDuplicatePresetName(t *testing.T) {
	fr := &remotex.FakeRunner{}
	if _, err := bifrost.New(fr, "minty", "heimdall", []bifrost.Preset{
		{Name: "work-session", Argv: []string{"claude"}},
		{Name: "work-session", Argv: []string{"claude"}},
	}); err == nil {
		t.Error("New: got nil error, want error (duplicate preset name)")
	}
}
