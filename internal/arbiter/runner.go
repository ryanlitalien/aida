package arbiter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
)

// ErrRunnerExternal is returned by BuildCommand for RunnerAidaAgent --
// that lane runs through the existing `aida --agent` job/loop machinery
// (internal/jobs, aida loop), not through anything this package spawns
// itself. A caller sees this error and hands the RunSpec to that path
// instead.
var ErrRunnerExternal = errors.New("arbiter: runner is handled outside this package")

// RunSpec is everything a Runner needs to execute one lane's command.
type RunSpec struct {
	Lane       Lane
	Model      string
	Prompt     string
	Dir        string
	Timeout    time.Duration
	OutputPath string
	// Env is the base environment to run in (typically os.Environ()).
	// BuildCommand scrubs it of Anthropic credentials before adding the
	// lane's own Env and, for RunnerClaude, CLAUDE_CONFIG_DIR.
	Env []string
}

// RunResult is what came back from running a RunSpec.
type RunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	Duration time.Duration
	Verdict  Verdict
	Matched  string
}

// Runner executes one RunSpec. ExecRunner is the only implementation in
// this package; a test or the loop can substitute a fake.
type Runner interface {
	Run(ctx context.Context, spec RunSpec) (RunResult, error)
}

// BuildCommand is the pure, side-effect-free half of running a lane: it
// decides the executable, arguments, and environment for spec without
// touching the filesystem or spawning anything, so it can be tested
// exhaustively without a real `claude`/`codex`/`agy` binary on PATH.
//
// RunnerClaude builds `claude --print --dangerously-skip-permissions
// --model <model> <prompt>`, matching internal/roster.RunClaudeIn's
// transport plus an explicit --model. RunnerExec substitutes "{model}"
// and "{prompt}" into spec.Lane.Command and splits the first element out
// as the executable name; stdinClosed is always true for it (there is no
// stdin to give an exec-lane command). RunnerAidaAgent returns
// ErrRunnerExternal -- see its doc comment.
//
// Every branch that spawns a real process (RunnerClaude, RunnerExec)
// scrubs spec.Env through config.ScrubAnthropicCreds first: a
// subscription lane must never inherit an ANTHROPIC_BASE_URL/
// ANTHROPIC_AUTH_TOKEN/ANTHROPIC_API_KEY the calling process happened to
// have set (plan section 8 step 2), and an api-key lane's own Env entries
// (added back afterward) are the only place those variables may
// legitimately come from (qwen-ec2's ANTHROPIC_BASE_URL).
func BuildCommand(spec RunSpec) (name string, args []string, env []string, stdinClosed bool, err error) {
	switch spec.Lane.Runner {
	case RunnerClaude:
		env = laneEnv(spec)
		if spec.Lane.ConfigDir != "" {
			env = append(env, "CLAUDE_CONFIG_DIR="+config.ExpandPath(spec.Lane.ConfigDir))
		}
		args = []string{"--print", "--dangerously-skip-permissions", "--model", spec.Model, spec.Prompt}
		return "claude", args, env, false, nil

	case RunnerExec:
		if len(spec.Lane.Command) == 0 {
			return "", nil, nil, false, fmt.Errorf("arbiter: lane %q has runner %q but no command template", spec.Lane.ID, RunnerExec)
		}
		templated := make([]string, len(spec.Lane.Command))
		for i, a := range spec.Lane.Command {
			a = strings.ReplaceAll(a, "{model}", spec.Model)
			a = strings.ReplaceAll(a, "{prompt}", spec.Prompt)
			templated[i] = a
		}
		env = laneEnv(spec)
		return templated[0], templated[1:], env, true, nil

	case RunnerAidaAgent:
		return "", nil, nil, false, ErrRunnerExternal

	default:
		return "", nil, nil, false, fmt.Errorf("arbiter: lane %q: unknown runner %q", spec.Lane.ID, spec.Lane.Runner)
	}
}

// laneEnv scrubs spec.Env of Anthropic routing variables and appends
// spec.Lane.Env on top, keys sorted for a deterministic result.
func laneEnv(spec RunSpec) []string {
	env := config.ScrubAnthropicCreds(spec.Env)
	if len(spec.Lane.Env) == 0 {
		return env
	}
	keys := make([]string, 0, len(spec.Lane.Env))
	for k := range spec.Lane.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+spec.Lane.Env[k])
	}
	return env
}

// ExecRunner runs a RunSpec via internal/execx, the process-group-safe
// subprocess runner every other long-running command in this codebase
// uses. It never touches a real network account in its own tests --
// runner_test.go exercises it against `sh -c` fixtures only.
type ExecRunner struct{}

// Run implements Runner. execx.Run's RunOpts.Stdin defaults to nil, which
// the package's own doc comment defines as "the child sees an
// immediately-closed pipe" -- exactly the "stdin closed" BuildCommand
// promises for RunnerExec, so no extra plumbing is needed here to
// achieve it.
//
// A timeout is deliberately NOT scored as VerdictEmpty: TimedOut means
// the command simply didn't finish in time, which says nothing about
// whether the lane's quota is exhausted -- scoring it Empty would mark a
// merely-slow lane as out of capacity. See RunResult.TimedOut.
//
// A nonzero exit is an expected, ordinary outcome here -- the whole point
// of running a lane's command is to observe whether it failed with a
// usage-limit refusal -- so it is never returned as a Go error; only a
// genuine launch failure (the binary wasn't found, couldn't fork, ...) is.
// This mirrors internal/roster.RunClaudeIn's own convention: a status
// worth inspecting comes back on the result, not as err.
func (ExecRunner) Run(ctx context.Context, spec RunSpec) (RunResult, error) {
	name, args, env, _, err := BuildCommand(spec)
	if err != nil {
		return RunResult{}, err
	}

	res, runErr := execx.Run(ctx, name, args, execx.RunOpts{
		Timeout: spec.Timeout,
		Dir:     spec.Dir,
		Env:     env,
	})

	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return RunResult{}, fmt.Errorf("arbiter: running %s: %w", name, runErr)
	}

	out := RunResult{
		ExitCode: res.ExitCode,
		Stdout:   string(res.Stdout),
		Stderr:   string(res.Stderr),
		TimedOut: res.TimedOut,
		Duration: res.Duration,
	}

	var writeErr error
	if spec.OutputPath != "" {
		if werr := os.WriteFile(spec.OutputPath, res.Stdout, 0o644); werr != nil {
			writeErr = fmt.Errorf("arbiter: writing output to %q: %w", spec.OutputPath, werr)
		}
	}

	if out.TimedOut {
		out.Verdict = VerdictNotEmpty
		return out, writeErr
	}

	out.Verdict, out.Matched = ClassifySignal(out.ExitCode, SignalText(out.ExitCode, out.Stderr, out.Stdout))
	return out, writeErr
}

// SignalTailBytes is how much of a successful run's output SignalText
// keeps for classification. A usage-limit refusal from `claude --print`
// or `codex exec` is the LAST thing the process prints, so the tail is
// where the evidence lives.
const SignalTailBytes = 1200

// SignalText picks the text ClassifySignal should see for a finished run.
// On a non-zero exit the whole combined output is fair game: the process
// failed and any limit phrase anywhere is worth acting on (fail open,
// decision 2). On exit 0 only the tail is classified, because a coding
// agent that succeeded may legitimately have printed "rate limit" or
// "429" in the middle of its transcript (it edited rate-limit code, it
// quoted an HTTP status) and scoring that as lane-empty would park a
// healthy lane for an hour on every such task. The refusal case still
// ends the transcript, so the tail catches it.
func SignalText(exitCode int, stderr, stdout string) string {
	combined := stderr + "\n" + stdout
	if exitCode != 0 {
		return combined
	}
	if len(combined) > SignalTailBytes {
		return combined[len(combined)-SignalTailBytes:]
	}
	return combined
}
