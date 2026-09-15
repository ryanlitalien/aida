package cli

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
	"github.com/ryanlitalien/aida/internal/ui"
)

// The claude -p connector fallback. When aida's own pipeline can't answer a
// connector-shaped question (email/calendar/slack/drive/airtable/notion/attio),
// it shells out to `claude -p`, which reaches the user's OAuth MCP connectors
// that aida's discovery can't (their tokens live in Claude Code's keychain). The
// answer flows back through the normal answer path, so any caller - terminal,
// Jarvis voice (via aida_query), cron - benefits without changes.
//
// Two call sites in query.go invoke maybeClaudeFallback: the pre-synthesis
// "no sources matched" short-circuit, and the post-synthesis useless-answer
// guard. Both own their own printing + run/lesson recording.

// connectorKeywords maps a connector name to the distinctive phrases that
// signal a question targets it. Kept tight on purpose: a false positive only
// costs a wasted claude call (and only when aida already failed), but the sets
// avoid generic words ("message", "base", "pipeline", "channel") that collide
// with non-connector queries - especially in this user's dev/payments domain.
var connectorKeywords = map[string][]string{
	"gmail":    {"email", "emails", "e-mail", "inbox", "gmail", "unread", "message from", "reply to"},
	"calendar": {"calendar", "schedule", "meeting", "meetings", "appointment", "next call", "agenda", "availability", "free time", "calendar event"},
	"slack":    {"slack", " dm ", "direct message", "slack channel", "slack thread", "message in slack"},
	"drive":    {"google drive", "google doc", "google docs", "google sheet", "google sheets", "spreadsheet", "gdoc", "gsheet"},
	"airtable": {"airtable"},
	"notion":   {"notion"},
	"attio":    {"attio", "crm", "contact record", "company record"},
}

// weakCatchAllSources are sources that "match" almost any topic as a last
// resort (web-search is aida's general fallback). When a query's plan
// contains ONLY these, aida has no source that actually specializes in the topic
// - the signal the upfront connector route uses to skip straight to claude -p.
var weakCatchAllSources = map[string]bool{"web-search": true}

// onlyWeakSources reports whether names is non-empty and every entry is a weak
// catch-all. Empty (no sources at all) is handled by the separate no-source
// give-up path, so it returns false here.
func onlyWeakSources(names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !weakCatchAllSources[n] {
			return false
		}
	}
	return true
}

// connectorIntent reports whether the question looks like it targets one of the
// user's claude.ai OAuth connectors, and which one (for verbose logging). Pure;
// unit-tested. Keys are iterated in sorted order so the returned name is stable
// for a given input.
func connectorIntent(question string) (bool, string) {
	s := " " + strings.ToLower(strings.TrimSpace(question)) + " "
	groups := make([]string, 0, len(connectorKeywords))
	for g := range connectorKeywords {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		for _, kw := range connectorKeywords[g] {
			if strings.Contains(s, kw) {
				return true, g
			}
		}
	}
	return false, ""
}

// claudeFallbackEnv builds the env for the claude -p subprocess: Anthropic
// credentials scrubbed (MANDATORY - with an API key set, claude runs in
// API-key mode and has NO connectors), plus the recursion guard that stops a
// claude-loaded `aida serve` MCP child from re-triggering this fallback.
func claudeFallbackEnv() []string {
	env := scrubAnthropicCreds(os.Environ())
	return append(env, "AIDA_NO_CLAUDE_FALLBACK=1")
}

// claudeFallbackDir is the cwd for claude -p: the configured dir, else $HOME.
// Account connectors load regardless of cwd; running from home avoids loading
// the project .mcp.json (the aida MCP server, plus any broken/stdio servers).
func claudeFallbackDir(fb config.ClaudeFallbackConfig) string {
	if fb.Cwd != "" {
		return fb.Cwd
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// runClaudeFallback shells out to `claude --print <question>` and returns the
// answer. It NEVER returns an error: any failure (claude not on PATH, timeout,
// empty or itself-useless output) yields ok=false so the caller falls through
// to aida's normal give-up path. --print yields plain text on stdout (no
// stream-json envelope to parse); --dangerously-skip-permissions avoids
// stalling on MCP permission prompts.
func runClaudeFallback(ctx context.Context, question string, fb config.ClaudeFallbackConfig) (string, bool) {
	if _, err := exec.LookPath("claude"); err != nil {
		ui.PrintVerbose("claude-fallback", "claude not on PATH; skipping")
		return "", false
	}
	args := []string{"--print", "--dangerously-skip-permissions", question}
	if fb.Model != "" {
		args = append([]string{"--model", fb.Model}, args...)
	}
	res, err := execx.Run(ctx, "claude", args, execx.RunOpts{
		Timeout: time.Duration(fb.TimeoutSeconds) * time.Second,
		Dir:     claudeFallbackDir(fb),
		Env:     claudeFallbackEnv(),
	})
	if res.TimedOut {
		ui.PrintVerbose("claude-fallback", "claude -p timed out")
		return "", false
	}
	if err != nil {
		ui.PrintVerbose("claude-fallback", "claude -p error: "+err.Error())
		return "", false
	}
	answer := strings.TrimSpace(string(res.Stdout))
	if answer == "" || answerIsUseless(answer) {
		ui.PrintVerbose("claude-fallback", "claude -p produced no usable answer")
		return "", false
	}
	return answer, true
}

// maybeClaudeFallback is the single entry the two query.go call sites use. It
// gates cheapest-first - recursion guard, then config-enabled, then the
// connector-intent check - before spending a subprocess. Returns the answer and
// ok=true only when claude produced a usable one; the caller does the printing
// and run/lesson recording.
func maybeClaudeFallback(ctx context.Context, question string, cfg *config.Config) (string, bool) {
	if os.Getenv("AIDA_NO_CLAUDE_FALLBACK") != "" {
		return "", false // already inside a claude -p invocation; don't recurse
	}
	fb := cfg.ActiveClaudeFallbackConfig()
	if fb.Enabled == nil || !*fb.Enabled {
		return "", false
	}
	ok, connector := connectorIntent(question)
	if !ok {
		return "", false
	}
	ui.PrintVerbose("claude-fallback", "connector-shaped ("+connector+"); proxying to claude -p")
	return runClaudeFallback(ctx, question, fb)
}
