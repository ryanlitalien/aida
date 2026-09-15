package fleet

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/remotex"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{name: "plain", in: "scarlett-978", wantErr: false},
		{name: "dots and underscores", in: "fleet.start_v2", wantErr: false},
		{name: "single char", in: "a", wantErr: false},
		{name: "digits only", in: "12345", wantErr: false},
		{name: "empty", in: "", wantErr: true},
		{name: "embedded space", in: "scarlett 978", wantErr: true},
		{name: "slash", in: "scarlett/978", wantErr: true},
		{name: "path traversal", in: "../etc/passwd", wantErr: true},
		{name: "leading dash flag-like", in: "-oProxyCommand=x", wantErr: true},
		{name: "shell metacharacter", in: "scarlett;rm", wantErr: true},
		{name: "unicode", in: "café", wantErr: true},
		{name: "too long", in: strings.Repeat("a", maxNameLen+1), wantErr: true},
		{name: "exactly max length", in: strings.Repeat("a", maxNameLen), wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateName(tt.in)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateName(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

func TestTaskFileWriteArgv(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		content string
		want    []string
	}{
		{
			name:    "plain task",
			path:    "/home/ryan/agents-lane4/scarlett-978.task",
			content: "print hello and exit",
			want: []string{
				"sh", "-c",
				`mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`,
				"_", "/home/ryan/agents-lane4/scarlett-978.task", "print hello and exit",
			},
		},
		{
			name:    "empty content",
			path:    "/home/ryan/agents-lane4/empty.task",
			content: "",
			want: []string{
				"sh", "-c",
				`mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`,
				"_", "/home/ryan/agents-lane4/empty.task", "",
			},
		},
		{
			name:    "content with quotes, newlines, and shell metacharacters",
			path:    "/home/ryan/agents-lane4/tricky.task",
			content: "do `whoami`; echo \"$HOME\" && rm -rf / #\nline two",
			want: []string{
				"sh", "-c",
				`mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`,
				"_", "/home/ryan/agents-lane4/tricky.task", "do `whoami`; echo \"$HOME\" && rm -rf / #\nline two",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TaskFileWriteArgv(tt.path, tt.content)
			if len(got) != len(tt.want) {
				t.Fatalf("TaskFileWriteArgv() = %#v, want %#v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("TaskFileWriteArgv()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}

			// Every one of these argv elements -- including the raw,
			// unescaped content -- must survive remotex.BuildRemoteScript
			// without ever being rejected: that function is the actual
			// safety boundary (base64, no shell metacharacters in the
			// script itself), this function just has to hand it a
			// well-formed argv.
			if _, err := remotex.BuildRemoteScript(got); err != nil {
				t.Errorf("BuildRemoteScript(TaskFileWriteArgv(...)) failed: %v", err)
			}
		})
	}
}

func TestLauncherArgv(t *testing.T) {
	tests := []struct {
		name        string
		user        string
		agentName   string
		claudeAgent string
		cwd         string
		effort      string
		model       string
		keepOpen    int
		want        []string
	}{
		{
			name:        "no keep-open",
			user:        "ryan",
			agentName:   "scarlett-978",
			claudeAgent: "swe",
			cwd:         "/home/ryan/dev/butter_stack",
			effort:      "max",
			model:       "claude-opus-4-6",
			keepOpen:    0,
			want: []string{"bash", "-lc",
				"'/home/ryan/agents-lane4/lane4-run.sh' 'scarlett-978' --agent 'swe' --repo '/home/ryan/dev/butter_stack' --effort 'max' --opus 'claude-opus-4-6'"},
		},
		{
			name:        "with keep-open",
			user:        "ryan",
			agentName:   "fleet-start-selftest",
			claudeAgent: "none",
			cwd:         "/home/ryan",
			effort:      "max",
			model:       "claude-opus-4-6",
			keepOpen:    20,
			want: []string{"bash", "-lc",
				"'/home/ryan/agents-lane4/lane4-run.sh' 'fleet-start-selftest' --agent 'none' --repo '/home/ryan' --effort 'max' --opus 'claude-opus-4-6' --keep-open 20"},
		},
		{
			name:        "negative keep-open omitted like zero",
			user:        "ryan",
			agentName:   "n",
			claudeAgent: "swe",
			cwd:         "/home/ryan",
			effort:      "max",
			model:       "claude-opus-4-6",
			keepOpen:    -5,
			want: []string{"bash", "-lc",
				"'/home/ryan/agents-lane4/lane4-run.sh' 'n' --agent 'swe' --repo '/home/ryan' --effort 'max' --opus 'claude-opus-4-6'"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LauncherArgv(tt.user, tt.agentName, tt.claudeAgent, tt.cwd, tt.effort, tt.model, tt.keepOpen)
			if len(got) != len(tt.want) || got[0] != tt.want[0] || got[1] != tt.want[1] || got[2] != tt.want[2] {
				t.Errorf("LauncherArgv() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestHerdrArgvBuilders(t *testing.T) {
	if got, want := AgentListArgv(), []string{HerdrBin, "agent", "list"}; !equalStrings(got, want) {
		t.Errorf("AgentListArgv() = %#v, want %#v", got, want)
	}
	if got, want := WorkspaceCreateArgv("/home/ryan", "scarlett-978"),
		[]string{HerdrBin, "workspace", "create", "--cwd", "/home/ryan", "--label", "scarlett-978", "--no-focus"}; !equalStrings(got, want) {
		t.Errorf("WorkspaceCreateArgv() = %#v, want %#v", got, want)
	}
	launch := []string{"bash", "-lc", "echo hi"}
	if got, want := AgentStartArgv("scarlett-978", "wS", "/home/ryan", launch),
		[]string{HerdrBin, "agent", "start", "scarlett-978", "--workspace", "wS", "--cwd", "/home/ryan", "--no-focus", "--", "bash", "-lc", "echo hi"}; !equalStrings(got, want) {
		t.Errorf("AgentStartArgv() = %#v, want %#v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Real captured envelope shapes, from `ssh minty '/opt/herdr/bin/herdr
// workspace create ...'` / `agent list` / `agent start` against herdr
// 0.7.4 on 2026-09-02 (trimmed to the fields these parsers use -- herdr
// also returns root_pane/tab siblings on workspace create that neither
// parser needs).
const (
	realWorkspaceCreateJSON = `{"id":"cli:workspace:create","result":{"root_pane":{"cwd":"/home/ryan","workspace_id":"wR"},"tab":{"workspace_id":"wR"},"type":"workspace_created","workspace":{"active_tab_id":"wR:t1","label":"fleet-start-probe","workspace_id":"wR"}}}`
	realAgentListEmptyJSON  = `{"id":"cli:agent:list","result":{"agents":[],"type":"agent_list"}}`
	realAgentListOneJSON    = `{"id":"cli:agent:list","result":{"agents":[{"agent_status":"unknown","cwd":"/home/ryan","name":"probe-agent-xyz","pane_id":"wS:p2","workspace_id":"wS"}],"type":"agent_list"}}`
)

func TestParseWorkspaceID(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		want    string
		wantErr bool
	}{
		{
			name:   "real captured workspace create envelope",
			stdout: realWorkspaceCreateJSON,
			want:   "wR",
		},
		{
			name:   "ssh MOTD noise before the JSON",
			stdout: "Welcome to minty\nLast login: Tue Sep  2\n" + realWorkspaceCreateJSON,
			want:   "wR",
		},
		{
			name:    "herdr error envelope",
			stdout:  `{"id":"cli:workspace:create","error":{"code":"bad_cwd","message":"cwd does not exist"}}`,
			wantErr: true,
		},
		{
			name:    "not json at all",
			stdout:  "ssh: connection refused",
			wantErr: true,
		},
		{
			name:    "valid envelope but no workspace_id field",
			stdout:  `{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"label":"x"}}}`,
			wantErr: true,
		},
		{
			name:    "result is not the expected shape",
			stdout:  `{"id":"cli:workspace:create","result":"unexpected string"}`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseWorkspaceID([]byte(tt.stdout))
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseWorkspaceID() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("ParseWorkspaceID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentExists(t *testing.T) {
	tests := []struct {
		name    string
		stdout  string
		target  string
		want    bool
		wantErr bool
	}{
		{
			name:   "empty agent list",
			stdout: realAgentListEmptyJSON,
			target: "scarlett-978",
			want:   false,
		},
		{
			name:   "no match among one agent",
			stdout: realAgentListOneJSON,
			target: "scarlett-978",
			want:   false,
		},
		{
			name:   "matches by name field",
			stdout: realAgentListOneJSON,
			target: "probe-agent-xyz",
			want:   true,
		},
		{
			name:   "matches legacy agent field",
			stdout: `{"id":"cli:agent:list","result":{"agents":[{"agent":"legacy-name","name":""}],"type":"agent_list"}}`,
			target: "legacy-name",
			want:   true,
		},
		{
			name:    "malformed json",
			stdout:  "not json",
			target:  "x",
			wantErr: true,
		},
		{
			name:    "herdr error envelope",
			stdout:  `{"id":"cli:agent:list","error":{"code":"internal","message":"boom"}}`,
			target:  "x",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AgentExists([]byte(tt.stdout), tt.target)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AgentExists() err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got != tt.want {
				t.Errorf("AgentExists(%q) = %v, want %v", tt.target, got, tt.want)
			}
		})
	}
}

func TestDefaultQuestion(t *testing.T) {
	tests := []struct {
		name string
		task string
		want string
	}{
		{name: "short task unchanged", task: "print hello and exit", want: "print hello and exit"},
		{
			name: "multi-line collapsed to one line",
			task: "line one\nline two\n\nline three",
			want: "line one line two line three",
		},
		{
			name: "truncated to 80 runes",
			task: strings.Repeat("a", 120),
			want: strings.Repeat("a", 80),
		},
		{name: "empty", task: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DefaultQuestion(tt.task); got != tt.want {
				t.Errorf("DefaultQuestion(%q) = %q, want %q", tt.task, got, tt.want)
			}
		})
	}
}

// TestStartAgent_HappyPath is the httptest-free assertion that the
// start sequence issues exactly three remote commands, in order, with
// the expected argv: herdr agent list, herdr workspace create, herdr
// agent start. Each FakeRule matches on the exact base64 script
// remotex.BuildRemoteScript would produce for that call's argv -- the
// same bytes that would cross the wire to ssh's stdin -- so this test
// fails if the argv for any of the three calls drifts, not just the
// call count.
func TestStartAgent_HappyPath(t *testing.T) {
	const host, user, cwd, name = "minty", "ryan", "/home/ryan/dev/butter_stack", "scarlett-smoke"
	launchArgv := LauncherArgv(user, name, "swe", cwd, "max", "claude-opus-4-6", 0)

	listScript := mustScript(t, AgentListArgv())
	wsScript := mustScript(t, WorkspaceCreateArgv(cwd, name))
	startScript := mustScript(t, AgentStartArgv(name, "wS", cwd, launchArgv))

	r := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{
			Match:  func(_ string, _ []string, stdin []byte) bool { return bytes.Equal(stdin, listScript) },
			Result: remotex.Result{Stdout: []byte(realAgentListEmptyJSON), ExitCode: 0},
		},
		{
			Match: func(_ string, _ []string, stdin []byte) bool { return bytes.Equal(stdin, wsScript) },
			Result: remotex.Result{
				Stdout:   []byte(`{"id":"cli:workspace:create","result":{"type":"workspace_created","workspace":{"workspace_id":"wS"}}}`),
				ExitCode: 0,
			},
		},
		{
			Match: func(_ string, _ []string, stdin []byte) bool { return bytes.Equal(stdin, startScript) },
			Result: remotex.Result{
				Stdout:   []byte(`{"id":"cli:agent:start","result":{"type":"agent_started","agent":{"name":"scarlett-smoke","workspace_id":"wS"}}}`),
				ExitCode: 0,
			},
		},
	}}

	got, err := StartAgent(context.Background(), r, host, user, cwd, name, launchArgv, time.Second)
	if err != nil {
		t.Fatalf("StartAgent() error = %v", err)
	}
	if got.WorkspaceID != "wS" {
		t.Errorf("StartAgent() WorkspaceID = %q, want %q", got.WorkspaceID, "wS")
	}
	if callCount := r.CallCount(); callCount != 3 {
		t.Fatalf("StartAgent() issued %d remote calls, want 3", callCount)
	}
}

// TestStartAgent_AlreadyExists asserts the refusal path: only the
// pre-flight `herdr agent list` call is made -- no workspace is
// created and no agent is started once a name collision is found.
func TestStartAgent_AlreadyExists(t *testing.T) {
	const host, user, cwd, name = "minty", "ryan", "/home/ryan", "taken-name"
	launchArgv := LauncherArgv(user, name, "swe", cwd, "max", "claude-opus-4-6", 0)

	r := &remotex.FakeRunner{Respond: []remotex.FakeRule{
		{Result: remotex.Result{Stdout: []byte(realAgentListOneJSON), ExitCode: 0}},
	}}

	_, err := StartAgent(context.Background(), r, host, user, cwd, "probe-agent-xyz", launchArgv, time.Second)
	if err == nil {
		t.Fatal("StartAgent() error = nil, want AlreadyExistsError")
	}
	var alreadyErr *AlreadyExistsError
	if !errors.As(err, &alreadyErr) {
		t.Errorf("StartAgent() error = %v, want *AlreadyExistsError", err)
	}
	if callCount := r.CallCount(); callCount != 1 {
		t.Fatalf("StartAgent() issued %d remote calls, want exactly 1 (the existence check)", callCount)
	}
}

// TestStartAgent_InvalidNameNoRunnerCall asserts an invalid name is
// rejected before any remote call is attempted at all.
func TestStartAgent_InvalidNameNoRunnerCall(t *testing.T) {
	r := &remotex.FakeRunner{}
	_, err := StartAgent(context.Background(), r, "minty", "ryan", "/home/ryan", "bad name!", nil, time.Second)
	if err == nil {
		t.Fatal("StartAgent() error = nil, want a validation error")
	}
	if callCount := r.CallCount(); callCount != 0 {
		t.Fatalf("StartAgent() issued %d remote calls for an invalid name, want 0", callCount)
	}
}

func mustScript(t *testing.T, argv []string) []byte {
	t.Helper()
	script, err := remotex.BuildRemoteScript(argv)
	if err != nil {
		t.Fatalf("BuildRemoteScript(%v): %v", argv, err)
	}
	return script
}
