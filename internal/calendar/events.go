package calendar

import (
	"sort"
	"strings"
	"time"
)

// mcpEvent is the raw shape of one event in google-workspace-mcp's
// listEvents response. Start/End are RFC3339 with an offset for timed
// events, or a bare YYYY-MM-DD for all-day events.
type mcpEvent struct {
	ID          string `json:"id"`
	Summary     string `json:"summary"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

type listEventsResponse struct {
	Events []mcpEvent `json:"events"`
}

// renderEvents converts one calendar's raw MCP events into the output
// Event shape: drops cancelled events and anything outside rr, applies the
// title-rendering rules (never blank, never invented), and never copies
// Description into the output. Event text (Summary, Location, Description)
// is untrusted data from someone else's calendar entry -- it is displayed
// verbatim, never interpreted as an instruction or used to build a command.
func renderEvents(raw []mcpEvent, p accountCalendarPair, rr resolvedRange, loc *time.Location) []Event {
	var out []Event
	for _, e := range raw {
		if strings.EqualFold(e.Status, "cancelled") {
			continue
		}
		start, end, allDay, ok := parseEventRange(e.Start, e.End, loc)
		if !ok {
			continue
		}
		// Half-open interval overlap: event.start < range.end && event.end
		// > range.start. This same formula covers all-day events too, since
		// their start/end are parsed as midnight-to-midnight(exclusive)
		// boundaries -- an all-day event spanning the requested day
		// overlaps any sub-day period query (morning/afternoon/evening) on
		// that day without needing separate all-day handling.
		if !(start.Before(rr.End) && end.After(rr.Start)) {
			continue
		}
		out = append(out, Event{
			Summary:  renderTitle(e.Summary, e.Description),
			Start:    e.Start,
			End:      e.End,
			AllDay:   allDay,
			Location: e.Location,
			Account:  p.Account,
			Calendar: p.Label,
		})
	}
	return out
}

// renderTitle applies the "never blank, never invented" naming rule: an
// empty summary becomes "Untitled event"; a bare "Busy" placeholder (the
// summary Google Calendar shows for events another calendar's owner
// hasn't shared details of) with no description becomes "Busy, details
// unavailable" rather than being silently dropped or given a made-up name.
func renderTitle(summary, description string) string {
	if strings.TrimSpace(summary) == "" {
		return "Untitled event"
	}
	if summary == "Busy" && strings.TrimSpace(description) == "" {
		return "Busy, details unavailable"
	}
	return summary
}

// parseEventRange parses an event's start/end strings into concrete
// instants plus whether the event is all-day. A malformed pair returns
// ok=false so the event is skipped rather than corrupting the overlap
// check with a zero time.Time.
func parseEventRange(start, end string, loc *time.Location) (time.Time, time.Time, bool, bool) {
	s, sAllDay, ok := parseEventTime(start, loc)
	if !ok {
		return time.Time{}, time.Time{}, false, false
	}
	e, _, ok := parseEventTime(end, loc)
	if !ok {
		return time.Time{}, time.Time{}, false, false
	}
	return s, e, sAllDay, true
}

// parseEventTime parses one event timestamp: RFC3339 with an offset for a
// timed event, or a bare YYYY-MM-DD (parsed as midnight in loc) for an
// all-day event's date.
func parseEventTime(s string, loc *time.Location) (time.Time, bool, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, false, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, true
	}
	return time.Time{}, false, false
}

// filterByTitles applies a calendar's optional match_titles config: keep an
// event only if its Summary contains at least one of match, case
// insensitively (substring, not whole-word, so "Alice" also matches
// "Alice-dentist"). An empty match list is a no-op -- returns events
// unchanged -- which is what every calendar without match_titles configured
// gets, preserving current behavior exactly.
//
// Callers must run this after renderEvents, so it only ever sees events that
// already passed the cancelled-event and range-overlap filtering; it must
// never influence the coverage ledger, which records whether the fetch
// itself succeeded, not how many events survived filtering.
func filterByTitles(events []Event, match []string) []Event {
	if len(match) == 0 {
		return events
	}
	var out []Event
	for _, e := range events {
		title := strings.ToLower(e.Summary)
		for _, m := range match {
			if strings.Contains(title, strings.ToLower(m)) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// sortEvents orders all-day events first, then timed events chronologically.
func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].AllDay != events[j].AllDay {
			return events[i].AllDay
		}
		ti, oki := eventSortKey(events[i])
		tj, okj := eventSortKey(events[j])
		if oki && okj {
			return ti.Before(tj)
		}
		return false
	})
}

func eventSortKey(e Event) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, e.Start); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", e.Start); err == nil {
		return t, true
	}
	return time.Time{}, false
}
