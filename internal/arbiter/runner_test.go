package arbiter

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildCommandClaudeWithConfigDirAndScrubbedCreds(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:        "claude-pro-bs",
			Runner:    RunnerClaude,
			Auth:      AuthSubscription,
			ConfigDir: "~/.claude-butterstack",
		},
		Model:  "claude-sonnet-5",
		Prompt: "hello",
		Env:    []string{"ANTHROPIC_API_KEY=not-a-real-key-placeholder", "OTHER_VAR=bar"},
	}

	name, args, env, stdinClosed, err := BuildCommand(spec)
	if err != nil {
		t.Fatalf("BuildCommand error: %v", err)
	}
	if name != "claude" {
		t.Errorf("name = %q, want claude", name)
	}
	wantArgs := []string{"--print", "--dangerously-skip-permissions", "--model", "claude-sonnet-5", "hello"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
	if stdinClosed {
		t.Error("stdinClosed = true, want false for RunnerClaude")
	}
	if containsEnvKey(env, "ANTHROPIC_API_KEY") {
		t.Errorf("env still contains ANTHROPIC_API_KEY: %v", env)
	}
	if !containsEnv(env, "OTHER_VAR=bar") {
		t.Errorf("env missing OTHER_VAR=bar: %v", env)
	}
	home, err2 := os.UserHomeDir()
	if err2 != nil {
		t.Fatalf("UserHomeDir: %v", err2)
	}
	want := "CLAUDE_CONFIG_DIR=" + home + "/.claude-butterstack"
	if !containsEnv(env, want) {
		t.Errorf("env missing %q: %v", want, env)
	}
}

func TestBuildCommandExecSubstitution(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:      "gemini-agy",
			Runner:  RunnerExec,
			Auth:    AuthSubscription,
			Command: []string{"agy", "-i", "--model", "{model}", "{prompt}"},
		},
		Model:  "gemini-3.1-pro",
		Prompt: "summarize this",
	}

	name, args, _, stdinClosed, err := BuildCommand(spec)
	if err != nil {
		t.Fatalf("BuildCommand error: %v", err)
	}
	if name != "agy" {
		t.Errorf("name = %q, want agy", name)
	}
	wantArgs := []string{"-i", "--model", "gemini-3.1-pro", "summarize this"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %v, want %v", args, wantArgs)
	}
	if !stdinClosed {
		t.Error("stdinClosed = false, want true for RunnerExec")
	}
}

func TestBuildCommandExecMissingCommand(t *testing.T) {
	spec := RunSpec{Lane: Lane{ID: "x", Runner: RunnerExec}}
	_, _, _, _, err := BuildCommand(spec)
	if err == nil {
		t.Fatal("expected an error for an exec lane with no command template")
	}
}

func TestBuildCommandAidaAgentReturnsExternalError(t *testing.T) {
	spec := RunSpec{Lane: Lane{ID: "litellm", Runner: RunnerAidaAgent}}
	_, _, _, _, err := BuildCommand(spec)
	if !errors.Is(err, ErrRunnerExternal) {
		t.Fatalf("expected ErrRunnerExternal, got %v", err)
	}
}

func TestBuildCommandUnknownRunner(t *testing.T) {
	spec := RunSpec{Lane: Lane{ID: "x", Runner: "carrier-pigeon"}}
	_, _, _, _, err := BuildCommand(spec)
	if err == nil {
		t.Fatal("expected an error for an unknown runner")
	}
}

func TestBuildCommandSubscriptionLaneNeverGetsAnthropicBaseURL(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:     "claude-max",
			Runner: RunnerClaude,
			Auth:   AuthSubscription,
			// No lane.Env at all -- nothing should reintroduce this even
			// though the inherited process env had it set.
		},
		Model:  "claude-sonnet-5",
		Prompt: "hi",
		Env:    []string{"ANTHROPIC_BASE_URL=http://evil.example"},
	}
	_, _, env, _, err := BuildCommand(spec)
	if err != nil {
		t.Fatalf("BuildCommand error: %v", err)
	}
	if containsEnvKey(env, "ANTHROPIC_BASE_URL") {
		t.Errorf("subscription lane env still carries ANTHROPIC_BASE_URL: %v", env)
	}
}

func TestBuildCommandAPIKeyLaneAddsOwnEnv(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:     "qwen-ec2",
			Runner: RunnerClaude,
			Auth:   AuthAPIKey,
			Env:    map[string]string{"ANTHROPIC_BASE_URL": "http://minty:4000"},
		},
		Model:  "qwen",
		Prompt: "hi",
		Env:    []string{"UNRELATED=1"},
	}
	_, _, env, _, err := BuildCommand(spec)
	if err != nil {
		t.Fatalf("BuildCommand error: %v", err)
	}
	if !containsEnv(env, "ANTHROPIC_BASE_URL=http://minty:4000") {
		t.Errorf("expected the lane's own ANTHROPIC_BASE_URL to be present: %v", env)
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func containsEnvKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// --- ExecRunner tests (sh -c fixtures only, no real CLI dependency) ---

func TestExecRunnerEchoesPrompt(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:      "echo-lane",
			Runner:  RunnerExec,
			Command: []string{"sh", "-c", "echo {prompt}"},
		},
		Prompt:  "hello world",
		Timeout: 5 * time.Second,
	}
	res, err := (ExecRunner{}).Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := trimNL(res.Stdout); got != "hello world" {
		t.Errorf("Stdout = %q, want %q", got, "hello world")
	}
	if res.Verdict != VerdictNotEmpty {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictNotEmpty)
	}
}

func TestExecRunnerDetectsUsageLimit(t *testing.T) {
	spec := RunSpec{
		Lane: Lane{
			ID:      "limited-lane",
			Runner:  RunnerExec,
			Command: []string{"sh", "-c", `echo "usage limit reached"; exit 1`},
		},
		Timeout: 5 * time.Second,
	}
	res, err := (ExecRunner{}).Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	if res.Verdict != VerdictEmpty {
		t.Errorf("Verdict = %q, want %q", res.Verdict, VerdictEmpty)
	}
	if res.Matched != "usage limit reached" {
		t.Errorf("Matched = %q, want %q", res.Matched, "usage limit reached")
	}
}

func TestExecRunnerBuildCommandErrorPropagates(t *testing.T) {
	spec := RunSpec{Lane: Lane{ID: "no-command", Runner: RunnerExec}}
	_, err := (ExecRunner{}).Run(context.Background(), spec)
	if err == nil {
		t.Fatal("expected an error when the lane has no command template")
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func TestSignalTextTailOnlyOnSuccess(t *testing.T) {
	filler := strings.Repeat("ok line\n", 400) // well past SignalTailBytes
	cases := []struct {
		name     string
		exitCode int
		stderr   string
		stdout   string
		want     Verdict
	}{
		{"success mentioning rate limit mid-transcript is not empty", 0, "", "edited the rate limit handler for 429s\n" + filler + "done", VerdictNotEmpty},
		{"success ending in a refusal is empty", 0, "", filler + "You've hit your limit. Resets at 3pm.", VerdictEmpty},
		{"failure mentioning a limit anywhere is empty", 1, "rate_limit_error: too many requests", filler, VerdictEmpty},
		{"failure with no phrase is ambiguous", 1, "boom", filler, VerdictAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ClassifySignal(tc.exitCode, SignalText(tc.exitCode, tc.stderr, tc.stdout))
			if got != tc.want {
				t.Errorf("verdict = %q, want %q", got, tc.want)
			}
		})
	}
}
