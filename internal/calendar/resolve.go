// Package calendar resolves semantic schedule requests ("Thursday
// morning", "next week") into concrete time ranges and fetches events for
// them over MCP. Every date/weekday computation happens here, in Go, using
// time.Date/Weekday/AddDate -- never by asking a model to compute a date,
// and never by adding 24*time.Hour to advance a calendar day (which
// silently mis-lands across a DST transition). That is the load-bearing
// fix this package exists for: a caller was once told "you have no events
// Thursday morning" when three real events existed, in part because the
// model had also miscomputed which day Thursday was.
package calendar

import (
	"fmt"
	"strings"
	"time"
)

// WhenKind selects which shape of semantic date reference a ScheduleRequest
// carries.
type WhenKind string

const (
	WhenDay     WhenKind = "day"
	WhenWeekday WhenKind = "weekday"
	WhenWeek    WhenKind = "week"
	WhenDate    WhenKind = "date"
)

// WeekdayRelation qualifies a WhenWeekday request.
type WeekdayRelation string

const (
	// RelationUpcoming is the next occurrence of the named weekday,
	// including today if today is that weekday.
	RelationUpcoming WeekdayRelation = "upcoming"
	// RelationThisWeek is that weekday within the current Mon-Sun week,
	// even if it has already passed.
	RelationThisWeek WeekdayRelation = "this_week"
	// RelationNextWeek is that weekday within next week's Mon-Sun --
	// what "next Thursday" means.
	RelationNextWeek WeekdayRelation = "next_week"
)

// Period narrows a day to a part of it. All times are local to the
// resolved timezone.
type Period string

const (
	PeriodDay       Period = "day"       // [00:00, 24:00)
	PeriodMorning   Period = "morning"   // [00:00, 12:00)
	PeriodAfternoon Period = "afternoon" // [12:00, 18:00)
	PeriodEvening   Period = "evening"   // [18:00, 24:00)
)

// When is a semantic, caller-agnostic reference to a calendar day or week.
// The model supplies this shape (via the calendar_schedule voice tool's
// input schema); it never supplies a computed date or weekday name outside
// of Weekday/Date, and resolveRange does all the arithmetic.
type When struct {
	Kind WhenKind `json:"kind"`
	// Offset applies to Kind day (-1 yesterday, 0 today, 1 tomorrow) and
	// Kind week (0 this week, 1 next week).
	Offset int `json:"offset,omitempty"`
	// Weekday is a weekday name (e.g. "thursday"), for Kind weekday.
	Weekday string `json:"weekday,omitempty"`
	// Relation qualifies Kind weekday: upcoming/this_week/next_week.
	Relation string `json:"relation,omitempty"`
	// Date is an explicit YYYY-MM-DD, for Kind date.
	Date string `json:"date,omitempty"`
}

// ScheduleRequest is the fully-resolved-in-Go input to FetchSchedule.
type ScheduleRequest struct {
	When   When   `json:"when"`
	Period Period `json:"period,omitempty"`
}

// resolvedRange is a concrete [Start, End) instant range plus a spoken
// label. Label is always rendered from the resolved time.Time values,
// never from the caller's own words, so a mis-transcribed or ambiguous
// request can't leak an unverified weekday name into the reply.
type resolvedRange struct {
	Start time.Time
	End   time.Time
	Label string
}

// weekdayIndex maps time.Weekday onto a Monday=0..Sunday=6 scale -- weeks
// in this package start Monday, but time.Weekday numbers from Sunday=0.
func weekdayIndex(w time.Weekday) int {
	return (int(w) + 6) % 7
}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

func parseWeekday(s string) (time.Weekday, error) {
	w, ok := weekdayNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("unrecognized weekday %q", s)
	}
	return w, nil
}

// dayBounds returns the [midnight, next-midnight) instant range for the
// calendar day containing day (day may be any time on that date; only its
// Y/M/D and Location are used). Built with time.Date's own day-overflow
// normalization rather than day.Add(24*time.Hour), so a DST transition
// inside the day doesn't shift the boundary by an hour.
func dayBounds(day time.Time) (time.Time, time.Time) {
	loc := day.Location()
	y, m, d := day.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	end := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	return start, end
}

// periodBounds narrows a day's [dayStart, dayEnd) to the requested period.
func periodBounds(dayStart, dayEnd time.Time, period Period) (time.Time, time.Time, error) {
	y, m, d := dayStart.Date()
	loc := dayStart.Location()
	switch period {
	case "", PeriodDay:
		return dayStart, dayEnd, nil
	case PeriodMorning:
		return dayStart, time.Date(y, m, d, 12, 0, 0, 0, loc), nil
	case PeriodAfternoon:
		return time.Date(y, m, d, 12, 0, 0, 0, loc), time.Date(y, m, d, 18, 0, 0, 0, loc), nil
	case PeriodEvening:
		return time.Date(y, m, d, 18, 0, 0, 0, loc), dayEnd, nil
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unrecognized period %q", period)
	}
}

func periodLabelSuffix(period Period) string {
	switch period {
	case PeriodMorning:
		return " morning"
	case PeriodAfternoon:
		return " afternoon"
	case PeriodEvening:
		return " evening"
	default:
		return ""
	}
}

// resolveDay resolves When to a single calendar day (midnight, in loc) for
// Kind day/weekday/date. Kind week spans seven days and is handled
// separately by resolveRange.
func resolveDay(now time.Time, loc *time.Location, when When) (time.Time, error) {
	today := now.In(loc)
	ty, tm, td := today.Date()
	todayMidnight := time.Date(ty, tm, td, 0, 0, 0, 0, loc)

	switch when.Kind {
	case WhenDay:
		return todayMidnight.AddDate(0, 0, when.Offset), nil

	case WhenDate:
		d, err := time.ParseInLocation("2006-01-02", when.Date, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid date %q: %w", when.Date, err)
		}
		return d, nil

	case WhenWeekday:
		target, err := parseWeekday(when.Weekday)
		if err != nil {
			return time.Time{}, err
		}
		mondayThisWeek := todayMidnight.AddDate(0, 0, -weekdayIndex(todayMidnight.Weekday()))
		switch WeekdayRelation(when.Relation) {
		case "", RelationUpcoming:
			diff := (weekdayIndex(target) - weekdayIndex(todayMidnight.Weekday()) + 7) % 7
			return todayMidnight.AddDate(0, 0, diff), nil
		case RelationThisWeek:
			return mondayThisWeek.AddDate(0, 0, weekdayIndex(target)), nil
		case RelationNextWeek:
			return mondayThisWeek.AddDate(0, 0, 7+weekdayIndex(target)), nil
		default:
			return time.Time{}, fmt.Errorf("unrecognized weekday relation %q", when.Relation)
		}

	default:
		return time.Time{}, fmt.Errorf("resolveDay: unsupported kind %q for a single-day request", when.Kind)
	}
}

// resolveRange resolves req into a concrete instant range plus a spoken
// label. now is the caller's current instant (normally time.Now()); loc is
// the configured IANA zone.
func resolveRange(now time.Time, loc *time.Location, req ScheduleRequest) (resolvedRange, error) {
	if req.When.Kind == WhenWeek {
		today := now.In(loc)
		ty, tm, td := today.Date()
		todayMidnight := time.Date(ty, tm, td, 0, 0, 0, 0, loc)
		mondayThisWeek := todayMidnight.AddDate(0, 0, -weekdayIndex(todayMidnight.Weekday()))
		weekStart := mondayThisWeek.AddDate(0, 0, 7*req.When.Offset)
		weekEnd := weekStart.AddDate(0, 0, 7)
		label := fmt.Sprintf("the week of %s to %s",
			weekStart.Format("January 2"),
			weekEnd.AddDate(0, 0, -1).Format("January 2, 2006"))
		return resolvedRange{Start: weekStart, End: weekEnd, Label: label}, nil
	}

	day, err := resolveDay(now, loc, req.When)
	if err != nil {
		return resolvedRange{}, err
	}
	dayStart, dayEnd := dayBounds(day)
	start, end, err := periodBounds(dayStart, dayEnd, req.Period)
	if err != nil {
		return resolvedRange{}, err
	}
	label := day.Format("Monday") + periodLabelSuffix(req.Period) + ", " + day.Format("January 2, 2006")
	return resolvedRange{Start: start, End: end, Label: label}, nil
}
