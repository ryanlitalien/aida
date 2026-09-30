package arbiter

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// preflight.go is docs/arbiter-plan.md section 8 step 4's "preflight of
// lane 1 before a wave (speak an alert if the OAuth session expired)": a
// cheap, single trivial-role turn run against a lane before an unattended
// wave commits to it, so a stale or revoked login is caught with a
// speakable alert instead of burning the wave's first task on a lane
// that was never going to work.

// authFailurePhrases are case-insensitive substrings that mean "this
// lane's credential isn't working," as opposed to any other kind of
// failure (a bad prompt, a transient network blip, a real rate limit --
// that's ClassifySignal's job, not this one). Distinct from
// signals.go's emptyPhrases: those detect quota exhaustion, these
// detect an auth problem, and the two must never be confused -- an
// exhausted quota is not something reauthenticating fixes, and an
// expired session is not something waiting out a reset window fixes.
var authFailurePhrases = []string{
	"not logged in",
	"please log in",
	"please run /login",
	"login required",
	"oauth",
	"authentication_error",
	"invalid api key",
	"unauthorized",
	"401",
}

// ClassifyAuth reports whether a preflight run's exit code and output
// indicate a working credential. false (with a reason) when the text
// matches a known auth-failure phrase, or when the process exited
// non-zero with no output at all (a launch-shaped failure with nothing
// to go on -- conservatively treated as "can't confirm this lane
// works," not "assume it's fine"). Everything else -- a clean exit, or a
// non-zero exit that at least printed something not matching an auth
// phrase (e.g. the model itself complained about the prompt) -- is
// reported ok, since the failure evidence doesn't point at the
// credential.
func ClassifyAuth(exitCode int, text string) (ok bool, reason string) {
	lower := strings.ToLower(text)
	for _, p := range authFailurePhrases {
		if strings.Contains(lower, p) {
			return false, fmt.Sprintf("matched auth-failure phrase %q", p)
		}
	}
	if exitCode != 0 && strings.TrimSpace(text) == "" {
		return false, fmt.Sprintf("exit code %d with no output", exitCode)
	}
	return true, ""
}

// PreflightResult is one lane's preflight outcome.
type PreflightResult struct {
	Lane     string
	OK       bool
	Reason   string
	Duration time.Duration
}

// preflightPrompt is deliberately trivial -- the point is only to prove
// the credential still works, not to exercise the model.
const preflightPrompt = "Reply with the single word ok."

// PreflightLane runs a minimal turn on lane's trivial-role model (falling
// back to its executor model via Lane.ModelFor, same as Pick's own role
// fallback) and classifies the result with ClassifyAuth.
//
// RunnerAidaAgent lanes have no CLI login session of their own to check
// -- that runner goes through the existing aida --agent/jobs machinery,
// which already surfaces its own auth failures -- so PreflightLane
// reports OK without running anything.
//
// A timeout is NOT a failure: it says the command didn't finish in time,
// which says nothing about whether the login works (mirrors
// ExecRunner.Run's own TimedOut convention for VerdictEmpty). A launch
// error (runner.Run itself returning an error, e.g. the binary wasn't
// found) IS treated as a failure -- there's no output to classify, and a
// lane whose CLI can't even launch is not safely eligible for an
// unattended wave.
func PreflightLane(ctx context.Context, lane Lane, runner Runner, timeout time.Duration) PreflightResult {
	if lane.Runner == RunnerAidaAgent {
		return PreflightResult{Lane: lane.ID, OK: true, Reason: "no preflight for aida-agent"}
	}

	model := lane.ModelFor(RoleTrivial)
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res, err := runner.Run(pctx, RunSpec{
		Lane:    lane,
		Model:   model,
		Prompt:  preflightPrompt,
		Timeout: timeout,
	})
	dur := time.Since(start)

	if err != nil {
		return PreflightResult{Lane: lane.ID, OK: false, Reason: "preflight run failed to launch: " + err.Error(), Duration: dur}
	}
	if res.TimedOut {
		return PreflightResult{Lane: lane.ID, OK: true, Reason: "timed out; not treated as an auth failure", Duration: dur}
	}

	text := SignalText(res.ExitCode, res.Stderr, res.Stdout)
	ok, reason := ClassifyAuth(res.ExitCode, text)
	return PreflightResult{Lane: lane.ID, OK: ok, Reason: reason, Duration: dur}
}
