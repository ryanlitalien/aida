package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/mcp"
)

func TestNormalizeTaskRef(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"162", "#162"},
		{"#162", "#162"},
		{"task 162", "#162"},
		{"Task 162", "#162"},
		{"task-slug-name", "task-slug-name"},
		{"water the plants", "water-the-plants"},
		{"  Water The Plants!  ", "water-the-plants"},
		{"task water the plants", "water-the-plants"},
	}
	for _, c := range cases {
		got := normalizeTaskRef(c.raw)
		if got != c.want {
			t.Errorf("normalizeTaskRef(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// normalizeTaskRef's conversational-phrase path calls brain.SlugifyTaskTitle
// directly (no local duplicate), so it lands inside the same hyphenated
// shape stored task slugs use. Exercised indirectly through AddTask's slug
// in TestResolveTaskRef_ConversationalPhrase below, and directly here for
// the edge cases (punctuation, repeated separators, long titles).
func TestNormalizeTaskRef_SlugifyEdgeCases(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"water the plants", "water-the-plants"},
		{"Ryan's oil change!!", "ryans-oil-change"},
		{"a   b---c", "a-b-c"},
		{"---leading and trailing---", "leading-and-trailing"},
	}
	for _, c := range cases {
		got := normalizeTaskRef(c.in)
		if got != c.want {
			t.Errorf("normalizeTaskRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTaskSlug_CallSitesAgree ties together the two production call sites
// that both depend on SlugifyTaskTitle: AddTask's on-disk slug (built by
// brain's slugifyTask, which prepends a date to SlugifyTaskTitle's output)
// and a spoken task reference's normalization (normalizeTaskRef, which
// calls SlugifyTaskTitle directly). These were once two independent,
// hand-duplicated implementations (PR #105's slugifyTask/dedupeTaskSlug and
// PR #106's now-removed local slugifyRef); if they were to drift apart
// again a task created through AddTask would silently fail to resolve by
// voice, since ResolveTaskRef's `slug LIKE '%partial%'` query would just
// come back empty with no error. Representative inputs: spaces,
// punctuation, mixed case, unicode, and a very long title.
func TestTaskSlug_CallSitesAgree(t *testing.T) {
	titles := []string{
		"Water the plants",
		"Investigate checkout timeout spike!",
		"  Leading and trailing whitespace  ",
		"MIXED Case Title",
		"Café déjà vu: naïve résumé",
		"This is a very long task title that definitely exceeds the fifty character truncation limit by quite a lot of characters",
	}
	const datePrefixLen = len("2006-01-02-")
	for _, title := range titles {
		t.Run(title, func(t *testing.T) {
			b := openTestBrain(t)
			created, err := b.AddTask(title, nil, "")
			if err != nil {
				t.Fatalf("AddTask(%q): %v", title, err)
			}
			if len(created.Slug) < datePrefixLen {
				t.Fatalf("slug %q shorter than the expected date prefix", created.Slug)
			}

			want := brain.SlugifyTaskTitle(title)
			if gotFromFile := created.Slug[datePrefixLen:]; gotFromFile != want {
				t.Errorf("AddTask slug suffix = %q, want %q (from SlugifyTaskTitle)", gotFromFile, want)
			}
			if gotFromRef := normalizeTaskRef(title); gotFromRef != want {
				t.Errorf("normalizeTaskRef(%q) = %q, want %q (from SlugifyTaskTitle)", title, gotFromRef, want)
			}
		})
	}
}

// A conversational reference ("water the plants") must resolve to a task
// whose stored slug is the date-prefixed, hyphenated form of that same
// title -- bug #172's failing case.
func TestResolveTaskRef_ConversationalPhrase(t *testing.T) {
	b := openTestBrain(t)

	created, err := b.AddTask("Water the plants", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if !strings.Contains(created.Slug, "water-the-plants") {
		t.Fatalf("test setup: slug %q does not contain expected fragment", created.Slug)
	}

	rec, err := resolveTaskRef(b, "water the plants")
	if err != nil {
		t.Fatalf("resolveTaskRef(%q): %v", "water the plants", err)
	}
	if rec.Slug != created.Slug {
		t.Errorf("resolved slug %q, want %q", rec.Slug, created.Slug)
	}
}

// Numeric and "#N" references must keep working unchanged.
func TestResolveTaskRef_NumericAndHashRef(t *testing.T) {
	b := openTestBrain(t)

	created, err := b.AddTask("Investigate checkout timeout", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	for _, ref := range []string{
		"#" + strconv.Itoa(created.TaskID),
		strconv.Itoa(created.TaskID),
	} {
		rec, err := resolveTaskRef(b, ref)
		if err != nil {
			t.Fatalf("resolveTaskRef(%q): %v", ref, err)
		}
		if rec.Slug != created.Slug {
			t.Errorf("resolveTaskRef(%q) slug = %q, want %q", ref, rec.Slug, created.Slug)
		}
	}
}

// Exact and partial slugs must keep working unchanged.
func TestResolveTaskRef_ExactAndPartialSlug(t *testing.T) {
	b := openTestBrain(t)

	created, err := b.AddTask("Migrate the cron job", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	rec, err := resolveTaskRef(b, created.Slug)
	if err != nil {
		t.Fatalf("resolveTaskRef(exact slug %q): %v", created.Slug, err)
	}
	if rec.Slug != created.Slug {
		t.Errorf("exact slug resolved to %q, want %q", rec.Slug, created.Slug)
	}

	rec, err = resolveTaskRef(b, "cron-job")
	if err != nil {
		t.Fatalf("resolveTaskRef(partial slug): %v", err)
	}
	if rec.Slug != created.Slug {
		t.Errorf("partial slug resolved to %q, want %q", rec.Slug, created.Slug)
	}
}

// When a task's title is edited after creation, UpdateTask keeps the
// original slug/file, so a conversational reference to the CURRENT title
// no longer slug-matches. The title-substring fallback must still find it.
func TestResolveTaskRef_TitleFallbackAfterRetitle(t *testing.T) {
	b := openTestBrain(t)

	created, err := b.AddTask("fix bug", nil, "")
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	newTitle := "Fix the checkout timeout regression"
	if _, err := b.UpdateTask(created.Slug, brain.TaskPatch{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	// The slug-based path alone should miss: the stored slug still says
	// "fix-bug", not "fix-the-checkout-timeout-regression".
	if _, err := b.ResolveTaskRef(normalizeTaskRef("fix the checkout timeout regression")); err == nil {
		t.Fatalf("test setup: expected slug-based ResolveTaskRef to miss after retitle")
	}

	rec, err := resolveTaskRef(b, "checkout timeout regression")
	if err != nil {
		t.Fatalf("resolveTaskRef title fallback: %v", err)
	}
	if rec.Slug != created.Slug {
		t.Errorf("resolveTaskRef title fallback resolved to %q, want %q", rec.Slug, created.Slug)
	}
}

// A ref that matches nothing, by slug or title, must still return a clear
// not-found error rather than panicking or matching arbitrarily.
func TestResolveTaskRef_NoMatch(t *testing.T) {
	b := openTestBrain(t)
	if _, err := b.AddTask("Unrelated task", nil, ""); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	if _, err := resolveTaskRef(b, "completely different thing"); err == nil {
		t.Error("expected an error for a non-matching ref, got nil")
	}
}

// aidaQueryTimeout was raised again (task #193) alongside the routing fix,
// but a bare limit increase alone already failed once (90s -> 180s still
// wasn't enough). The bump must be modest, not another blind doubling.
func TestAidaQueryTimeout_ModestIncrease(t *testing.T) {
	const previous = 180 * time.Second
	if aidaQueryTimeout <= previous {
		t.Fatalf("aidaQueryTimeout = %s, want > previous %s", aidaQueryTimeout, previous)
	}
	if aidaQueryTimeout > previous+2*time.Minute {
		t.Errorf("aidaQueryTimeout = %s, want a modest bump over %s, not a large one -- "+
			"the routing fix, not a bigger number, is the real fix for chained work",
			aidaQueryTimeout, previous)
	}
}

// The timeout error must steer a retry toward job_start rather than just
// inviting the model to call aida_query again against the same hard limit.
func TestAidaQueryTimeoutError_RoutesToJobStart(t *testing.T) {
	err := aidaQueryTimeoutError()
	if err == nil {
		t.Fatal("aidaQueryTimeoutError() = nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "job_start") {
		t.Errorf("timeout error %q does not mention job_start", msg)
	}
	if !strings.Contains(msg, aidaQueryTimeout.String()) {
		t.Errorf("timeout error %q does not name the actual timeout duration", msg)
	}
}

// aida_query's own description must point composed, multi-step requests at
// job_start instead of implying repeated aida_query calls are fine.
func TestAidaQueryToolDescription_MentionsJobStartForChainedWork(t *testing.T) {
	tool := aidaQueryTool(nil, nil)
	if !strings.Contains(tool.Description, "job_start") {
		t.Errorf("aida_query description does not mention job_start: %q", tool.Description)
	}
}

// job_start's description must give a concrete example matching the actual
// failure shape (chained email search -> GitHub correlation -> draft),
// not just a vague "long-running work" gesture.
func TestJobStartToolDescription_MentionsChainedExample(t *testing.T) {
	tool := jobStartTool(nil)
	if !strings.Contains(tool.Description, "GitHub issue") {
		t.Errorf("job_start description missing the chained-request example: %q", tool.Description)
	}
}

// writeCalendarConfigForTest writes a minimal ~/.aida/config.yaml with one
// enabled account/calendar under HOME, mirroring
// internal/config/calendar_test.go's writeCalendarConfig for this package's
// own tests.
func writeCalendarConfigForTest(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, config.ConfigDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "calendar:\n  accounts:\n    - account: personal\n      enabled: true\n" +
		"      calendars:\n        - {id: \"example@example.com\", label: Personal, enabled: true}\n"
	if err := os.WriteFile(filepath.Join(dir, config.ConfigFile), []byte(body), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
}

// TestDaySummaryCalendarLine_NilDiscovery covers the "no MCP client at all"
// degrade path: day_summary must say so plainly rather than silently
// omitting the calendar line or implying the day is clear.
func TestDaySummaryCalendarLine_NilDiscovery(t *testing.T) {
	line := daySummaryCalendarLine(context.Background(), time.Now(), nil)
	if line == "" {
		t.Fatal("daySummaryCalendarLine returned empty string with nil discovery")
	}
	if strings.Contains(strings.ToLower(line), "clear") {
		t.Errorf("line = %q, must never claim the day is clear when there is no calendar connection", line)
	}
	if !strings.Contains(strings.ToLower(line), "calendar") {
		t.Errorf("line = %q, should say something about the calendar being unavailable", line)
	}
}

// TestDaySummaryCalendarLine_ConfigError covers the "calendar: config
// missing/invalid" degrade path, exercised with a real (but disconnected)
// *mcp.MCPDiscovery so the nil-discovery branch above isn't what's actually
// being tested here.
func TestDaySummaryCalendarLine_ConfigError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // no ~/.aida/config.yaml at all

	discovery := mcp.NewMCPDiscovery(false)
	line := daySummaryCalendarLine(context.Background(), time.Now(), discovery)
	if line == "" {
		t.Fatal("daySummaryCalendarLine returned empty string on a config error")
	}
	if strings.Contains(strings.ToLower(line), "clear") {
		t.Errorf("line = %q, must never claim the day is clear when the config couldn't be read", line)
	}
}

// TestDaySummaryCalendarLine_UsesFetchScheduleSpeech proves the calendar
// line is actually wired to calendar.FetchSchedule's own deterministic
// Speech field rather than some hand-rolled re-description: with a real
// config naming one calendar and a real (but unconnected, since no MCP
// server was actually dialed) *mcp.MCPDiscovery, listEvents fails with
// "server not connected", which is not an auth-shaped error, so
// FetchSchedule reports StatusFailed and a Speech sentence saying the
// schedule couldn't be checked. That exact wording must come through
// verbatim.
func TestDaySummaryCalendarLine_UsesFetchScheduleSpeech(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCalendarConfigForTest(t, home)

	discovery := mcp.NewMCPDiscovery(false)
	line := daySummaryCalendarLine(context.Background(), time.Now(), discovery)
	if !strings.Contains(line, "couldn't check your calendar") {
		t.Errorf("line = %q, want FetchSchedule's own failed-coverage speech", line)
	}
}

// TestDaySummaryTool_CalendarLineAlwaysPresent covers the composite tool
// end to end: the Calendar: line must always appear in day_summary's
// output, regardless of discovery being nil, and must never be silently
// dropped alongside the time/weather/tasks sections.
func TestDaySummaryTool_CalendarLineAlwaysPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	b := openTestBrain(t)

	tool := daySummaryTool(b, "", nil)
	out, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("day_summary Run: %v", err)
	}
	if !strings.Contains(out, "Calendar:") {
		t.Fatalf("day_summary output missing a Calendar: line entirely: %q", out)
	}
	if !strings.Contains(out, "Time:") {
		t.Errorf("day_summary output missing a Time: line: %q", out)
	}
}
