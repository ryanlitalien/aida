package jobs

import (
	"fmt"
	"os"
	"strings"
)

// UnverifiedResultNotice is the exact sentence Change 2 requires
// whenever a job's terminal record can't be backed up by a usable
// final result. The whole point is that job_status, job_list, and
// the spoken completion notice all emit the IDENTICAL wording instead
// of each improvising its own hedge. Callers append the actual stop
// reason after it via DescribeOutcome.
const UnverifiedResultNotice = "This run ended without a usable final result. " +
	"Completion of the requested task is unverified. Do not report that the " +
	"task succeeded. Some actions may have occurred; their outcome is unknown."

// isTruncatingTermination reports whether reason means the run was cut
// off before it could produce a deliberate final answer: the agent
// ran out of tokens, wall-clock time, or budget mid-flight. Even if
// some text happens to sit in the output file for one of these, it is
// a fragment, not a final result, and must not be read as one.
func isTruncatingTermination(reason string) bool {
	switch reason {
	case TerminationModelTokenLimit, TerminationWallTimeLimit, TerminationCostLimit:
		return true
	}
	return false
}

// outputUsable reports whether j's output file backs up a completion
// claim: it exists, it's readable, it has non-whitespace content, and
// the run wasn't truncated out from under it.
func outputUsable(j *Job, reason string) bool {
	if isTruncatingTermination(reason) {
		return false
	}
	data, err := os.ReadFile(OutputPath(j.Profile, j.RunID))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

// EffectiveState computes the state and termination reason a caller
// should ACT on, correcting pre-result-contract ("legacy") records so
// an old "done" is never read as a verified success just because
// nothing else was ever recorded about it. Also returns the job's
// ORIGINALLY stored state for auditing: a legacy downgrade changes
// what callers report, never what's on disk.
//
// Rules, gated on j.SchemaVersion < resultContractVersion (a record
// written before TerminationReason/Outcome existed on disk at all,
// not a record that simply happens to have empty ones, e.g. a
// Complete/Fail job with its own verification contract):
//
//	legacy "done"   -> "incomplete", reason LegacyUnverifiedReason
//	legacy "failed" -> stays "failed", reason unchanged (never invented)
//	anything else   -> unchanged
//
// Used by DescribeOutcome so job_status, job_list, and the spoken
// completion notice all read one compatibility mapping, not three.
func EffectiveState(j *Job) (state, reason, originalState string) {
	if j == nil {
		return "", "", ""
	}
	originalState = j.State
	if j.SchemaVersion >= resultContractVersion {
		return j.State, j.TerminationReason, originalState
	}
	if j.State == StateDone {
		return StateIncomplete, LegacyUnverifiedReason, originalState
	}
	return j.State, j.TerminationReason, originalState
}

// DescribeOutcome renders the evidence-based description Change 2
// requires: the state, the termination reason, the verified outcome
// (or its explicit absence), and a final summary when there is a
// usable one. Shared verbatim by job_status, job_list (jarvis voice
// tools), and the spoken completion notice (serve.go), see their
// call sites, so no surface can improvise a success claim the
// underlying record doesn't back up.
//
// A non-terminal job (queued/running/awaiting_*) has nothing to
// verify yet, so this just names the state.
//
// A job that reads as "done" (post-EffectiveState) but whose outcome
// isn't actually achieved+backed-by-usable-output, and any
// "incomplete" job (including a legacy-downgraded "done"), gets
// UnverifiedResultNotice verbatim plus the real stop reason when one
// is known. This is the branch that forecloses the exact failure
// mode reported against this package: a run recorded done with
// nothing behind it. A "failed" job is never routed through that
// notice: the state itself already says the task didn't succeed, and
// whatever real evidence exists (Error, a not_achieved outcome, a
// summary) is reported directly instead of being buried under a
// generic disclaimer.
func DescribeOutcome(j *Job) string {
	if j == nil {
		return "unknown"
	}
	state, reason, orig := EffectiveState(j)
	if !IsTerminalState(state) {
		return "state: " + state
	}

	origNote := ""
	if orig != state {
		origNote = fmt.Sprintf(" (originally recorded %s)", orig)
	}

	claimsSuccess := state == StateDone && j.Outcome == OutcomeAchieved && outputUsable(j, reason)
	if state == StateIncomplete || (state == StateDone && !claimsSuccess) {
		msg := fmt.Sprintf("state: %s%s. %s", state, origNote, UnverifiedResultNotice)
		if reason != "" {
			msg += fmt.Sprintf(" Stop reason: %s.", reason)
		}
		return msg
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "state: %s%s", state, origNote)
	if reason != "" {
		fmt.Fprintf(&sb, ", stopped because: %s", reason)
	}
	switch state {
	case StateDone:
		sb.WriteString(". Verified outcome: achieved")
	case StateFailed:
		if j.Outcome == OutcomeNotAchieved {
			sb.WriteString(". Verified outcome: not achieved")
			if j.FailureCode != "" {
				fmt.Fprintf(&sb, " (%s)", j.FailureCode)
			}
		} else {
			sb.WriteString(". No verified outcome recorded")
		}
	}
	if j.Summary != "" {
		fmt.Fprintf(&sb, ". Summary: %s", j.Summary)
	} else if j.Error != "" {
		fmt.Fprintf(&sb, ". Error: %s", j.Error)
	}
	return sb.String()
}
