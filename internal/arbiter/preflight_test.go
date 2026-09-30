package arbiter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyAuth(t *testing.T) {
	cases := []struct {
		name     string
		exitCode int
		text     string
		wantOK   bool
	}{
		{"clean exit with output", 0, "ok", true},
		{"not logged in", 1, "Error: you are not logged in", false},
		{"please run /login", 1, "please run /login to continue", false},
		{"oauth error", 1, "oauth token expired", false},
		{"authentication_error", 1, `{"type":"authentication_error"}`, false},
		{"invalid api key", 1, "Invalid API key provided", false},
		{"unauthorized", 1, "401 Unauthorized", false},
		{"bare 401", 1, "request failed: 401", false},
		{"nonzero exit with unrelated output", 1, "syntax error in prompt", true},
		{"nonzero exit with no output", 1, "", false},
		{"case insensitive match", 1, "PLEASE LOG IN", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := ClassifyAuth(tc.exitCode, tc.text)
			if ok != tc.wantOK {
				t.Errorf("ClassifyAuth(%d, %q) = (%v, %q), want ok=%v", tc.exitCode, tc.text, ok, reason, tc.wantOK)
			}
			if !ok && reason == "" {
				t.Error("expected a non-empty reason when ok=false")
			}
		})
	}
}

// fakePreflightRunner is a canned Runner for PreflightLane's tests -- no
// real claude/codex/agy process, ever.
type fakePreflightRunner struct {
	result   RunResult
	err      error
	sawSpec  RunSpec
	blockCtx bool // when true, Run blocks until ctx is done (simulates a hang)
}

func (f *fakePreflightRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	f.sawSpec = spec
	if f.blockCtx {
		<-ctx.Done()
		return RunResult{TimedOut: true}, nil
	}
	return f.result, f.err
}

func testLane(runner string) Lane {
	return Lane{
		ID:          "test-lane",
		Runner:      runner,
		DataClasses: []DataClass{DataClassPersonal},
		Models:      map[string]string{RoleExecutor: "exec-model", RoleTrivial: "trivial-model"},
	}
}

func TestPreflightLane(t *testing.T) {
	t.Run("aida-agent lane skips the run entirely", func(t *testing.T) {
		fr := &fakePreflightRunner{}
		res := PreflightLane(context.Background(), testLane(RunnerAidaAgent), fr, time.Second)
		if !res.OK {
			t.Errorf("OK = false, want true (no preflight for aida-agent)")
		}
		if fr.sawSpec.Model != "" {
			t.Error("aida-agent lane should never call Runner.Run")
		}
	})

	t.Run("clean run is OK and uses the trivial model", func(t *testing.T) {
		fr := &fakePreflightRunner{result: RunResult{ExitCode: 0, Stdout: "ok"}}
		res := PreflightLane(context.Background(), testLane(RunnerClaude), fr, time.Second)
		if !res.OK {
			t.Errorf("OK = false, reason=%q, want true", res.Reason)
		}
		if fr.sawSpec.Model != "trivial-model" {
			t.Errorf("model = %q, want trivial-model", fr.sawSpec.Model)
		}
		if fr.sawSpec.Prompt != preflightPrompt {
			t.Errorf("prompt = %q, want %q", fr.sawSpec.Prompt, preflightPrompt)
		}
	})

	t.Run("falls back to executor model when trivial is unset", func(t *testing.T) {
		lane := testLane(RunnerClaude)
		lane.Models = map[string]string{RoleExecutor: "exec-model"}
		fr := &fakePreflightRunner{result: RunResult{ExitCode: 0, Stdout: "ok"}}
		PreflightLane(context.Background(), lane, fr, time.Second)
		if fr.sawSpec.Model != "exec-model" {
			t.Errorf("model = %q, want fallback exec-model", fr.sawSpec.Model)
		}
	})

	t.Run("auth failure is not OK", func(t *testing.T) {
		fr := &fakePreflightRunner{result: RunResult{ExitCode: 1, Stderr: "please run /login"}}
		res := PreflightLane(context.Background(), testLane(RunnerClaude), fr, time.Second)
		if res.OK {
			t.Error("OK = true, want false for an auth failure")
		}
		if !strings.Contains(res.Reason, "login") {
			t.Errorf("reason = %q, want it to mention the login phrase", res.Reason)
		}
	})

	t.Run("a launch error is a failure", func(t *testing.T) {
		fr := &fakePreflightRunner{err: errors.New("exec: \"claude\": executable file not found in $PATH")}
		res := PreflightLane(context.Background(), testLane(RunnerClaude), fr, time.Second)
		if res.OK {
			t.Error("OK = true, want false when the runner itself errors")
		}
	})

	t.Run("a timeout is not treated as an auth failure", func(t *testing.T) {
		fr := &fakePreflightRunner{blockCtx: true}
		res := PreflightLane(context.Background(), testLane(RunnerClaude), fr, 20*time.Millisecond)
		if !res.OK {
			t.Errorf("OK = false, reason=%q, want true (timeout is not an auth failure)", res.Reason)
		}
		if !strings.Contains(res.Reason, "timed out") {
			t.Errorf("reason = %q, want it to mention the timeout", res.Reason)
		}
	})
}
