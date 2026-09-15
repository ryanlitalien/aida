package calendar

import (
	"testing"
	"time"
)

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skipf("tzdata for %s not available: %v", name, err)
	}
	return loc
}

// TestResolveDay_UpcomingWeekday_CrossesMonthBoundary is the load-bearing
// regression test: from Friday, January 30, 2026, "the upcoming Monday"
// must resolve to Monday, February 2, 2026 -- a month rollover that a
// naive string/number computation (or a model doing the math itself) gets
// wrong far more often than a plain day-offset case.
func TestResolveDay_UpcomingWeekday_CrossesMonthBoundary(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 30, 9, 0, 0, 0, loc) // Friday
	when := When{Kind: WhenWeekday, Weekday: "monday", Relation: string(RelationUpcoming)}

	got, err := resolveDay(now, loc, when)
	if err != nil {
		t.Fatalf("resolveDay: %v", err)
	}
	want := time.Date(2026, 2, 2, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("resolveDay = %v, want %v", got, want)
	}
	if got.Weekday() != time.Monday {
		t.Errorf("resolved day is %v, want Monday", got.Weekday())
	}
}

// TestResolveDay_WeekdayRelations exercises upcoming/this_week/next_week
// against a fixed "today" (Wednesday) so the three relations are clearly
// distinguishable.
func TestResolveDay_WeekdayRelations(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 9, 10, 0, 0, 0, loc) // Wednesday, Sept 9, 2026

	cases := []struct {
		name     string
		weekday  string
		relation WeekdayRelation
		want     time.Time
	}{
		{"upcoming same day is today", "wednesday", RelationUpcoming, time.Date(2026, 9, 9, 0, 0, 0, 0, loc)},
		{"upcoming forward within week", "friday", RelationUpcoming, time.Date(2026, 9, 11, 0, 0, 0, 0, loc)},
		{"upcoming wraps to next week", "monday", RelationUpcoming, time.Date(2026, 9, 14, 0, 0, 0, 0, loc)},
		{"this_week allows a day already past", "monday", RelationThisWeek, time.Date(2026, 9, 7, 0, 0, 0, 0, loc)},
		{"this_week same day as today", "wednesday", RelationThisWeek, time.Date(2026, 9, 9, 0, 0, 0, 0, loc)},
		{"next_week thursday", "thursday", RelationNextWeek, time.Date(2026, 9, 17, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			when := When{Kind: WhenWeekday, Weekday: c.weekday, Relation: string(c.relation)}
			got, err := resolveDay(now, loc, when)
			if err != nil {
				t.Fatalf("resolveDay: %v", err)
			}
			if !got.Equal(c.want) {
				t.Errorf("resolveDay(%s, %s) = %v, want %v", c.weekday, c.relation, got, c.want)
			}
		})
	}
}

// TestDayBounds_DSTSpringForward asserts dayBounds computes the correct
// calendar-day boundary across America/New_York's 2026 spring-forward
// transition (night of March 7->8, clocks jump 2am to 3am), and that the
// resulting day is 23 wall-clock hours long -- proof the boundary was
// built with time.Date's normalization, not a naive 24*time.Hour add,
// which would silently land an hour off.
func TestDayBounds_DSTSpringForward(t *testing.T) {
	loc := mustLoadLocation(t, "America/New_York")
	day := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)

	start, end := dayBounds(day)
	wantStart := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	wantEnd := time.Date(2026, 3, 9, 0, 0, 0, 0, loc)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
	if got := end.Sub(start); got != 23*time.Hour {
		t.Errorf("day duration = %v, want 23h (the DST-short day)", got)
	}
}

// TestResolveDay_WeekdayAcrossDSTTransition asserts weekday resolution
// lands on the correct calendar date when the target date sits on the
// other side of a DST transition from "now".
func TestResolveDay_WeekdayAcrossDSTTransition(t *testing.T) {
	loc := mustLoadLocation(t, "America/New_York")
	now := time.Date(2026, 3, 7, 10, 0, 0, 0, loc) // Saturday, before the transition
	when := When{Kind: WhenWeekday, Weekday: "monday", Relation: string(RelationUpcoming)}

	got, err := resolveDay(now, loc, when)
	if err != nil {
		t.Fatalf("resolveDay: %v", err)
	}
	want := time.Date(2026, 3, 9, 0, 0, 0, 0, loc) // Monday, after the transition
	if !got.Equal(want) {
		t.Errorf("resolveDay = %v, want %v", got, want)
	}
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Errorf("resolved day is not at local midnight: %v", got)
	}
}

func TestResolveRange_Periods(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, loc) // Thursday

	cases := []struct {
		period    Period
		wantStart time.Time
		wantEnd   time.Time
	}{
		{PeriodDay, time.Date(2026, 9, 10, 0, 0, 0, 0, loc), time.Date(2026, 9, 11, 0, 0, 0, 0, loc)},
		{PeriodMorning, time.Date(2026, 9, 10, 0, 0, 0, 0, loc), time.Date(2026, 9, 10, 12, 0, 0, 0, loc)},
		{PeriodAfternoon, time.Date(2026, 9, 10, 12, 0, 0, 0, loc), time.Date(2026, 9, 10, 18, 0, 0, 0, loc)},
		{PeriodEvening, time.Date(2026, 9, 10, 18, 0, 0, 0, loc), time.Date(2026, 9, 11, 0, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		t.Run(string(c.period), func(t *testing.T) {
			req := ScheduleRequest{When: When{Kind: WhenDay, Offset: 0}, Period: c.period}
			rr, err := resolveRange(now, loc, req)
			if err != nil {
				t.Fatalf("resolveRange: %v", err)
			}
			if !rr.Start.Equal(c.wantStart) || !rr.End.Equal(c.wantEnd) {
				t.Errorf("range = [%v, %v), want [%v, %v)", rr.Start, rr.End, c.wantStart, c.wantEnd)
			}
		})
	}
}

func TestResolveRange_Week(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, loc) // Thursday, Sept 10, 2026

	req := ScheduleRequest{When: When{Kind: WhenWeek, Offset: 1}}
	rr, err := resolveRange(now, loc, req)
	if err != nil {
		t.Fatalf("resolveRange: %v", err)
	}
	wantStart := time.Date(2026, 9, 14, 0, 0, 0, 0, loc) // next Monday
	wantEnd := time.Date(2026, 9, 21, 0, 0, 0, 0, loc)
	if !rr.Start.Equal(wantStart) || !rr.End.Equal(wantEnd) {
		t.Errorf("range = [%v, %v), want [%v, %v)", rr.Start, rr.End, wantStart, wantEnd)
	}
}

func TestResolveRange_ExplicitDate(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	req := ScheduleRequest{When: When{Kind: WhenDate, Date: "2026-12-25"}}
	rr, err := resolveRange(now, loc, req)
	if err != nil {
		t.Fatalf("resolveRange: %v", err)
	}
	want := time.Date(2026, 12, 25, 0, 0, 0, 0, loc)
	if !rr.Start.Equal(want) {
		t.Errorf("Start = %v, want %v", rr.Start, want)
	}
}

func TestResolveRange_UnrecognizedWeekday(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
	req := ScheduleRequest{When: When{Kind: WhenWeekday, Weekday: "funday"}}
	if _, err := resolveRange(now, loc, req); err == nil {
		t.Fatal("expected an error for an unrecognized weekday, got nil")
	}
}
