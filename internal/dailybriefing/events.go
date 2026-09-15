package dailybriefing

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// rawEventTime mirrors Google Calendar API's start/end shape: either `date` (all-day,
// "YYYY-MM-DD") or `dateTime` (RFC 3339).
type rawEventTime struct {
	Date     string `json:"date,omitempty"`
	DateTime string `json:"dateTime,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
}

type rawEvent struct {
	Status       string       `json:"status"`
	Summary      string       `json:"summary"`
	Location     string       `json:"location"`
	Start        rawEventTime `json:"start"`
	End          rawEventTime `json:"end"`
	Transparency string       `json:"transparency,omitempty"` // "" (== opaque) or "transparent"
	EventType    string       `json:"eventType,omitempty"`    // "default", "outOfOffice", "workingLocation", "focusTime"
}

type rawEventsList struct {
	Items []rawEvent `json:"items"`
}

type Event struct {
	Summary  string
	Location string
	Start    time.Time
	End      time.Time
	AllDay   bool
}

// noiseSummaries are user-created self-block events that are opaque (so they pass
// the transparency filter) but aren't actual meetings. Match is case-insensitive
// on the trimmed summary. Move to config if this list grows.
var noiseSummaries = map[string]struct{}{
	"busy":          {},
	"travel":        {},
	"home":          {},
	"ooo":           {},
	"out of office": {},
	"focus time":    {},
	"focus":         {},
	"lunch":         {},
	"break":         {},
	"prep":          {},
	"prep time":     {},
}

// parseEventsListJSON consumes the JSON stdout of `gws calendar events list ...`,
// applies the briefing-relevant filters (transparency, eventType, status, noise
// summaries), and returns events normalized to displayLoc.
//
// Filtered out:
//   - status == "cancelled"
//   - transparency == "transparent" (free-time, Home, OOO from team OOO calendars)
//   - eventType in {"workingLocation"} (always transparent, but explicit)
//   - summary matching noiseSummaries (Busy/Travel/etc - user self-blocks)
func parseEventsListJSON(stdout []byte, displayLoc *time.Location) ([]Event, error) {
	jsonStart := -1
	for i, b := range stdout {
		if b == '{' || b == '[' {
			jsonStart = i
			break
		}
	}
	if jsonStart < 0 {
		return nil, fmt.Errorf("no JSON in gws output: %q", string(stdout))
	}

	var raw rawEventsList
	if err := json.Unmarshal(stdout[jsonStart:], &raw); err != nil {
		return nil, fmt.Errorf("decoding events list: %w", err)
	}

	out := make([]Event, 0, len(raw.Items))
	for _, r := range raw.Items {
		if r.Status == "cancelled" {
			continue
		}
		if r.Transparency == "transparent" {
			continue
		}
		if r.EventType == "workingLocation" {
			continue
		}
		if _, noisy := noiseSummaries[strings.ToLower(strings.TrimSpace(r.Summary))]; noisy {
			continue
		}

		start, allDay, err := parseEventTime(r.Start, displayLoc)
		if err != nil {
			return nil, fmt.Errorf("event %q start: %w", r.Summary, err)
		}
		end, _, err := parseEventTime(r.End, displayLoc)
		if err != nil {
			return nil, fmt.Errorf("event %q end: %w", r.Summary, err)
		}
		out = append(out, Event{
			Summary:  r.Summary,
			Location: r.Location,
			Start:    start,
			End:      end,
			AllDay:   allDay,
		})
	}
	return out, nil
}

func parseEventTime(t rawEventTime, loc *time.Location) (time.Time, bool, error) {
	if t.Date != "" {
		parsed, err := time.ParseInLocation("2006-01-02", t.Date, loc)
		return parsed, true, err
	}
	if t.DateTime != "" {
		parsed, err := time.Parse(time.RFC3339, t.DateTime)
		if err != nil {
			return time.Time{}, false, err
		}
		return parsed.In(loc), false, nil
	}
	return time.Time{}, false, fmt.Errorf("event time has neither date nor dateTime")
}
