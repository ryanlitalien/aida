package cli

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/runs"
	"github.com/ryanlitalien/aida/internal/ui"
)

// timeShortcutPattern is an exact-match (anchored ^...$) whitelist of
// time/date questions the shortcut handles with zero LLM calls. Anything
// with extra tokens ("what time is it in tokyo", "what time is the game",
// "what date works for the meeting") deliberately falls through to the
// full pipeline -- this shortcut only fires on the plain "what time/date
// is it" shape, never anything with additional scope.
var timeShortcutPattern = regexp.MustCompile(`^(` +
	`what time is it( now| right now)?` + `|` +
	`whats the time` + `|` +
	`what is the time` + `|` +
	`whats todays date` + `|` +
	`what is todays date` + `|` +
	`what day is it( today)?` + `|` +
	`whats the date( today)?` + `|` +
	`what is the date( today)?` + `|` +
	`current time` + `|` +
	`time now` + `|` +
	`what timezone am i in` + `|` +
	`what time zone am i in` + `|` +
	`whats the day today` +
	`)$`)

var trailingPunctPattern = regexp.MustCompile(`[?.!]+$`)

// apostropheStripper drops both the plain ASCII apostrophe and the two
// common Unicode "smart quote" variants, so "what's", "whatʼs", and
// "what’s" all normalize to "whats" and match the same whitelist entries.
var apostropheStripper = strings.NewReplacer("'", "", "’", "", "‘", "")

// HandleTimeShortcut answers a small, exact-match whitelist of time/date
// questions ("what time is it", "what's today's date", ...) straight from
// the system clock -- zero LLM calls, zero source execution. Mirrors
// HandleTaskShortcut's pre-parse-intercept shape (internal/cli/tasks.go):
// it saves a minimal run record so a follow-up `aida thumbs-down`/
// `thumbs-up` still has a run to attach feedback to, and emits the same
// [aida-run-id:...] stderr sentinel the full pipeline emits so the Jarvis
// aida_query tool's run-id parsing keeps working.
//
// Returns true if the question was handled.
func HandleTimeShortcut(question string, cfg *config.Config) bool {
	if !matchesTimeShortcut(question) {
		return false
	}

	startedAt := time.Now()
	loc := cfg.Location()
	now := time.Now().In(loc)

	zone := loc.String()
	abbr := now.Format("MST")
	answer := fmt.Sprintf("It's %s %s on %s.",
		now.Format("3:04 PM"),
		abbr,
		now.Format("Monday, January 2, 2006"),
	)
	// Only append zone suffix if it's a real IANA zone name (not "Local", not empty, not equal to abbreviation)
	if zone != "Local" && zone != "" && zone != abbr {
		answer = fmt.Sprintf("It's %s %s on %s (%s).",
			now.Format("3:04 PM"),
			abbr,
			now.Format("Monday, January 2, 2006"),
			zone,
		)
	}

	ui.PrintResult(answer)
	saveTimeShortcutRun(question, cfg, startedAt, answer)
	return true
}

// matchesTimeShortcut reports whether question is an exact match (after
// normalization) for the time/date whitelist. Factored out from
// HandleTimeShortcut so tests can exercise the matcher directly without a
// config or touching disk via run-saving.
func matchesTimeShortcut(question string) bool {
	return timeShortcutPattern.MatchString(normalizeTimeQuestion(question))
}

// normalizeTimeQuestion lowercases, strips apostrophes (so "what's" and
// "whats" match the same whitelist entries), trims trailing punctuation
// (?.!), and collapses whitespace.
func normalizeTimeQuestion(q string) string {
	q = strings.ToLower(strings.TrimSpace(q))
	q = apostropheStripper.Replace(q)
	q = trailingPunctPattern.ReplaceAllString(q, "")
	return strings.Join(strings.Fields(q), " ")
}

// saveTimeShortcutRun records a minimal run log entry for the shortcut
// path, where no Intent has been parsed and no source was actually
// executed. Mirrors saveTaskShortcutRun (internal/cli/tasks.go) and the
// [aida-run-id:...] sentinel emitted at the end of the full pipeline
// (query.go) so feedback commands and the Jarvis run-id parsing behave
// identically regardless of which path answered the question.
func saveTimeShortcutRun(question string, cfg *config.Config, startedAt time.Time, answer string) {
	cwd, _ := os.Getwd()
	_, profileName := cfg.ActiveProfileConfig()
	durationMs := time.Since(startedAt).Milliseconds()

	run := &runs.Run{
		StartedAt: startedAt,
		TotalMs:   durationMs,
		Question:  question,
		Cwd:       cwd,
		Profile:   profileName,
		Action:    "query",
		Strategy:  "lookup",
		Phases: []runs.PhaseRun{
			{
				Name: "execute",
				Sources: []runs.SourceRun{
					{
						Name:          "current-time",
						Status:        "success",
						Summary:       answer,
						ArtifactCount: 1,
						DurationMs:    durationMs,
					},
				},
			},
		},
		Answer: answer,
	}

	if _, err := runs.Save(run); err != nil {
		ui.PrintVerbose("Run log", "time shortcut save error: "+err.Error())
		return
	}

	// Machine-parseable run-id sentinel -- see query.go's buildRunRecord
	// call site for the canonical version and rationale.
	fmt.Fprintf(os.Stderr, "[aida-run-id:%s]\n", run.ID)
}
