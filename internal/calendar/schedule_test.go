package calendar

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// fakeCaller implements MCPCaller by delegating to a plain function --
// enough flexibility for every test below without a mock framework.
type fakeCaller func(ctx context.Context, server, name string, args map[string]any) (string, error)

func (f fakeCaller) CallTool(ctx context.Context, server, name string, args map[string]any) (string, error) {
	return f(ctx, server, name, args)
}

func oneAccountConfig(calendars ...config.CalendarEntry) config.CalendarConfig {
	return config.CalendarConfig{
		Timezone:       "UTC",
		MaxConcurrency: 4,
		CallTimeout:    time.Second,
		MaxResults:     250,
		Accounts: []config.CalendarAccount{
			{Account: "personal", Enabled: true, Calendars: calendars},
		},
	}
}

// TestFetchSchedule_ClearAllowed_RefusedOnPartial is the test for the
// whole point of this package: a false "you're clear" must be structurally
// impossible. One calendar succeeds with zero events, the other fails --
// the empty event list must NOT be trusted.
func TestFetchSchedule_ClearAllowed_RefusedOnPartial(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "ok-cal", Label: "OK", Enabled: true},
		config.CalendarEntry{ID: "bad-cal", Label: "Bad", Enabled: true},
	)
	caller := fakeCaller(func(_ context.Context, _, _ string, args map[string]any) (string, error) {
		if args["calendarId"] == "bad-cal" {
			return "", fmt.Errorf("temporary network error")
		}
		return `{"events":[]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if result.Status != StatusPartial {
		t.Errorf("Status = %q, want %q", result.Status, StatusPartial)
	}
	if result.ClearAllowed {
		t.Error("ClearAllowed = true, want false on a partial result")
	}
	if len(result.Events) != 0 {
		t.Errorf("Events = %v, want none", result.Events)
	}
}

// TestFetchSchedule_ClearAllowed_TrueWhenEverythingChecked is the mirror
// case: every configured calendar succeeded and none had events, so
// ClearAllowed should be true.
func TestFetchSchedule_ClearAllowed_TrueWhenEverythingChecked(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true},
		config.CalendarEntry{ID: "cal-2", Label: "Two", Enabled: true},
	)
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return `{"events":[]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if result.Status != StatusComplete {
		t.Errorf("Status = %q, want %q", result.Status, StatusComplete)
	}
	if !result.ClearAllowed {
		t.Error("ClearAllowed = false, want true when every calendar succeeded with no events")
	}
}

// TestFetchSchedule_NoEnabledCalendars_IsConfigError covers "empty/absent
// accounts is a configuration error, never an empty schedule": here the
// account exists but every calendar under it is disabled, so there is
// nothing to check.
func TestFetchSchedule_NoEnabledCalendars_IsConfigError(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: false},
	)
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		t.Fatal("CallTool should not be invoked when no calendar is enabled")
		return "", nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if result.Status != StatusFailed {
		t.Errorf("Status = %q, want %q", result.Status, StatusFailed)
	}
	if result.ClearAllowed {
		t.Error("ClearAllowed = true, want false when nothing was checked")
	}
}

// TestFetchSchedule_AllDayEventIncludedInMorningQuery covers the all-day
// inclusion rule: an all-day event has no hours of its own, so it must
// still surface in a morning-only query for that date.
func TestFetchSchedule_AllDayEventIncludedInMorningQuery(t *testing.T) {
	cfg := oneAccountConfig(config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true})
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return `{"events":[{"id":"e1","summary":"Company Holiday","start":"2026-09-10","end":"2026-09-11","status":"confirmed"}]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}, Period: PeriodMorning}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 1 {
		t.Fatalf("Events = %v, want exactly one all-day event", result.Events)
	}
	if !result.Events[0].AllDay {
		t.Error("expected AllDay = true")
	}
	if result.Events[0].Summary != "Company Holiday" {
		t.Errorf("Summary = %q, want %q", result.Events[0].Summary, "Company Holiday")
	}
}

// TestFetchSchedule_BusyAndUntitledRendering covers the exact rendering
// rules for two known-tricky event shapes: a blank title, and Google
// Calendar's own "Busy" placeholder with no description. Neither may be
// dropped or given an invented name.
func TestFetchSchedule_BusyAndUntitledRendering(t *testing.T) {
	cfg := oneAccountConfig(config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true})
	body := `{"events":[
		{"id":"e1","summary":"","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"confirmed"},
		{"id":"e2","summary":"Busy","start":"2026-09-10T11:00:00-04:00","end":"2026-09-10T12:00:00-04:00","status":"confirmed"},
		{"id":"e3","summary":"Standup","start":"2026-09-10T13:00:00-04:00","end":"2026-09-10T13:15:00-04:00","status":"confirmed"}
	]}`
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return body, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 3 {
		t.Fatalf("Events = %+v, want 3", result.Events)
	}
	got := map[string]bool{}
	for _, e := range result.Events {
		got[e.Summary] = true
	}
	for _, want := range []string{"Untitled event", "Busy, details unavailable", "Standup"} {
		if !got[want] {
			t.Errorf("missing rendered summary %q in %+v", want, result.Events)
		}
	}
}

// TestFetchSchedule_CancelledEventsExcluded ensures a cancelled event never
// reaches the output, since it is not actually on the calendar anymore.
func TestFetchSchedule_CancelledEventsExcluded(t *testing.T) {
	cfg := oneAccountConfig(config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true})
	body := `{"events":[{"id":"e1","summary":"Cancelled Meeting","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"cancelled"}]}`
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return body, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 0 {
		t.Errorf("Events = %+v, want none (cancelled)", result.Events)
	}
	if !result.ClearAllowed {
		t.Error("ClearAllowed = false, want true -- the only event was cancelled")
	}
}

// TestFetchSchedule_AuthFailureSkipsRestOfAccount covers the coverage
// ledger's account-level rule: once one calendar on an account fails with
// what looks like an auth error, the remaining calendars on that SAME
// account are marked skipped rather than each being attempted and failing
// the same way.
func TestFetchSchedule_AuthFailureSkipsRestOfAccount(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true},
		config.CalendarEntry{ID: "cal-2", Label: "Two", Enabled: true},
		config.CalendarEntry{ID: "cal-3", Label: "Three", Enabled: true},
	)
	var attempted []string
	caller := fakeCaller(func(_ context.Context, _, _ string, args map[string]any) (string, error) {
		id := args["calendarId"].(string)
		attempted = append(attempted, id)
		if id == "cal-1" {
			return "", fmt.Errorf("401 unauthorized: token expired")
		}
		return `{"events":[]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(attempted) != 1 || attempted[0] != "cal-1" {
		t.Errorf("attempted = %v, want only cal-1 to have been called", attempted)
	}
	statuses := map[string]CoverageStatus{}
	for _, e := range result.Coverage {
		statuses[e.CalendarID] = e.Status
	}
	if statuses["cal-1"] != CoverageFailed {
		t.Errorf("cal-1 status = %q, want failed", statuses["cal-1"])
	}
	if statuses["cal-2"] != CoverageSkipped || statuses["cal-3"] != CoverageSkipped {
		t.Errorf("cal-2/cal-3 status = %q/%q, want skipped/skipped", statuses["cal-2"], statuses["cal-3"])
	}
	if result.Status != StatusFailed {
		t.Errorf("Status = %q, want %q (none succeeded)", result.Status, StatusFailed)
	}
}

// TestFetchSchedule_TimeoutOnlyFailsThatCalendar covers the other half of
// the coverage rule: a per-calendar timeout must not take down the rest of
// the account's calendars the way an auth failure does.
func TestFetchSchedule_TimeoutOnlyFailsThatCalendar(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "slow-cal", Label: "Slow", Enabled: true},
		config.CalendarEntry{ID: "fast-cal", Label: "Fast", Enabled: true},
	)
	cfg.CallTimeout = 20 * time.Millisecond
	caller := fakeCaller(func(ctx context.Context, _, _ string, args map[string]any) (string, error) {
		if args["calendarId"] == "slow-cal" {
			select {
			case <-time.After(500 * time.Millisecond):
				return `{"events":[]}`, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return `{"events":[]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	statuses := map[string]CoverageStatus{}
	for _, e := range result.Coverage {
		statuses[e.CalendarID] = e.Status
	}
	if statuses["slow-cal"] != CoverageFailed {
		t.Errorf("slow-cal status = %q, want failed", statuses["slow-cal"])
	}
	if statuses["fast-cal"] != CoverageSuccess {
		t.Errorf("fast-cal status = %q, want success", statuses["fast-cal"])
	}
}

// TestFetchSchedule_MatchTitles_KeepsOnlyMatchingTitles covers the basic
// per-calendar filter: only an event whose title contains one of
// match_titles' strings (case-insensitively, substring not whole-word)
// survives on that calendar.
func TestFetchSchedule_MatchTitles_KeepsOnlyMatchingTitles(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "shared-cal", Label: "Shared", Enabled: true, MatchTitles: []string{"Alice", "Bob"}},
	)
	body := `{"events":[
		{"id":"e1","summary":"alice-dentist","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"confirmed"},
		{"id":"e2","summary":"Bob soccer","start":"2026-09-10T11:00:00-04:00","end":"2026-09-10T12:00:00-04:00","status":"confirmed"},
		{"id":"e3","summary":"Dentist appointment","start":"2026-09-10T13:00:00-04:00","end":"2026-09-10T13:30:00-04:00","status":"confirmed"}
	]}`
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return body, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 2 {
		t.Fatalf("Events = %+v, want 2 (only the Alice/Bob titles)", result.Events)
	}
	got := map[string]bool{}
	for _, e := range result.Events {
		got[e.Summary] = true
	}
	if !got["alice-dentist"] || !got["Bob soccer"] {
		t.Errorf("Events = %+v, want alice-dentist and Bob soccer", result.Events)
	}
	if got["Dentist appointment"] {
		t.Errorf("Events = %+v, Dentist appointment should have been filtered out", result.Events)
	}
}

// TestFetchSchedule_MatchTitles_FullyFilteredCalendarStillSucceeds is the
// load-bearing test for the coverage/filter interaction: a calendar that
// was fetched successfully and then had every one of its events filtered
// away by match_titles must still show CoverageSuccess in the ledger, and
// must not flip the overall result to partial or block ClearAllowed. Only
// this one calendar is configured, so a wrong "filtering counts as a
// coverage failure" implementation would show up here as Status=partial (or
// worse, failed) and ClearAllowed=false, when the correct answer is
// complete/true -- everything was checked, nothing relevant was found.
func TestFetchSchedule_MatchTitles_FullyFilteredCalendarStillSucceeds(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "shared-cal", Label: "Shared", Enabled: true, MatchTitles: []string{"Alice", "Bob"}},
	)
	body := `{"events":[
		{"id":"e1","summary":"Quarterly review","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"confirmed"},
		{"id":"e2","summary":"Dentist appointment","start":"2026-09-10T13:00:00-04:00","end":"2026-09-10T13:30:00-04:00","status":"confirmed"}
	]}`
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return body, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 0 {
		t.Fatalf("Events = %+v, want none -- both titles were filtered out", result.Events)
	}
	if len(result.Coverage) != 1 || result.Coverage[0].Status != CoverageSuccess {
		t.Fatalf("Coverage = %+v, want a single success entry (filtering is not a fetch failure)", result.Coverage)
	}
	if result.Status != StatusComplete {
		t.Errorf("Status = %q, want %q -- a fully-filtered calendar is still fully checked", result.Status, StatusComplete)
	}
	if !result.ClearAllowed {
		t.Error("ClearAllowed = false, want true -- the calendar was checked, filtering just found nothing relevant")
	}
}

// TestFetchSchedule_MatchTitles_MixedWithUnfilteredCalendar covers two
// calendars together: one with match_titles that drops all of its events,
// and one plain calendar with a real event. Both must still show as
// CoverageSuccess, and the surviving event must be exactly the unfiltered
// calendar's.
func TestFetchSchedule_MatchTitles_MixedWithUnfilteredCalendar(t *testing.T) {
	cfg := oneAccountConfig(
		config.CalendarEntry{ID: "shared-cal", Label: "Shared", Enabled: true, MatchTitles: []string{"Alice", "Bob"}},
		config.CalendarEntry{ID: "own-cal", Label: "Own", Enabled: true},
	)
	caller := fakeCaller(func(_ context.Context, _, _ string, args map[string]any) (string, error) {
		if args["calendarId"] == "shared-cal" {
			return `{"events":[{"id":"e1","summary":"Quarterly review","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"confirmed"}]}`, nil
		}
		return `{"events":[{"id":"e2","summary":"Standup","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T09:15:00-04:00","status":"confirmed"}]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Events) != 1 || result.Events[0].Summary != "Standup" {
		t.Fatalf("Events = %+v, want only Standup", result.Events)
	}
	for _, e := range result.Coverage {
		if e.Status != CoverageSuccess {
			t.Errorf("Coverage[%s] = %q, want success", e.CalendarID, e.Status)
		}
	}
	if result.Status != StatusComplete {
		t.Errorf("Status = %q, want %q", result.Status, StatusComplete)
	}
}

// TestFetchSchedule_TruncationDetected covers the "received exactly
// max_results events" truncation rule.
func TestFetchSchedule_TruncationDetected(t *testing.T) {
	cfg := oneAccountConfig(config.CalendarEntry{ID: "cal-1", Label: "One", Enabled: true})
	cfg.MaxResults = 1
	caller := fakeCaller(func(_ context.Context, _, _ string, _ map[string]any) (string, error) {
		return `{"events":[{"id":"e1","summary":"Only One","start":"2026-09-10T09:00:00-04:00","end":"2026-09-10T10:00:00-04:00","status":"confirmed"}]}`, nil
	})

	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}}
	result := FetchSchedule(context.Background(), now, req, cfg, caller)

	if len(result.Coverage) != 1 || result.Coverage[0].Status != CoverageTruncated {
		t.Errorf("Coverage = %+v, want a single truncated entry", result.Coverage)
	}
	if result.ClearAllowed {
		t.Error("ClearAllowed = true, want false on truncated coverage")
	}
}
