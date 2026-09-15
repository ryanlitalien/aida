// calendar_schedule is Jarvis's real-calendar lookup tool. It exists
// because a model once answered "you have no events Thursday morning,
// your calendar is clear until noon" when three real events existed --
// nothing had queried a calendar, and the model had also miscomputed the
// weekday. See internal/calendar for the fix: every date/weekday
// computation happens in Go, and an empty result is only ever reported as
// "clear" when every configured calendar was actually, successfully
// checked.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ryanlitalien/aida/internal/calendar"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/mcp"
)

func calendarScheduleTool(discovery *mcp.MCPDiscovery) Tool {
	return Tool{
		Name: "calendar_schedule",
		Description: "Look up the user's real calendar for a day, part of a day, " +
			"or a week. Call this for ANY schedule/calendar question (\"what's on " +
			"my schedule\", \"am I free Thursday morning\", \"what do I have next " +
			"week\", \"is my Friday clear\"). NEVER answer from memory, and NEVER " +
			"compute a weekday or date yourself -- describe WHEN semantically " +
			"(a day offset, a named weekday with a relation, a week offset, or an " +
			"explicit date the user stated) and this tool resolves the actual " +
			"date. Use the returned range_label and events verbatim rather than " +
			"restating the user's own words for the day/date. If the result's " +
			"status is not \"complete\" or clear_allowed is false, do NOT tell " +
			"the user their calendar is clear -- say what could and couldn't be " +
			"checked, using the speech field as a starting point.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"when": map[string]interface{}{
					"type":        "object",
					"description": "semantic reference to a day or week -- never a computed date",
					"properties": map[string]interface{}{
						"kind": map[string]interface{}{
							"type": "string",
							"enum": []string{"day", "weekday", "week", "date"},
							"description": "day: relative to today by offset. weekday: a named " +
								"weekday. week: relative week by offset. date: an explicit " +
								"YYYY-MM-DD, only when the user gave one.",
						},
						"offset": map[string]interface{}{
							"type": "integer",
							"description": "for kind=day: -1 yesterday, 0 today, 1 tomorrow. " +
								"For kind=week: 0 this week, 1 next week.",
						},
						"weekday": map[string]interface{}{
							"type":        "string",
							"description": "for kind=weekday: the weekday name, e.g. 'thursday'",
						},
						"relation": map[string]interface{}{
							"type": "string",
							"enum": []string{"upcoming", "this_week", "next_week"},
							"description": "for kind=weekday: upcoming (the next occurrence, " +
								"including today), this_week (that weekday in the current " +
								"Mon-Sun week, even if already past), next_week (that weekday " +
								"next week -- this is what \"next Thursday\" means)",
						},
						"date": map[string]interface{}{
							"type":        "string",
							"description": "for kind=date: explicit YYYY-MM-DD, only when the user stated one",
						},
					},
					"required": []string{"kind"},
				},
				"period": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"day", "morning", "afternoon", "evening"},
					"description": "part of the day to check; defaults to the whole day",
				},
			},
			"required": []string{"when"},
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
			var req calendar.ScheduleRequest
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &req); err != nil {
					return "", fmt.Errorf("calendar_schedule: %w", err)
				}
			}
			if req.When.Kind == "" {
				return "", fmt.Errorf("calendar_schedule: missing when.kind")
			}

			cfg, err := config.LoadConfig()
			if err != nil {
				return "", fmt.Errorf("calendar_schedule: loading config: %w", err)
			}
			calCfg, err := cfg.CalendarConfig()
			if err != nil {
				return calendarUnavailablePayload(err), fmt.Errorf("calendar_schedule: %w", calendar.ErrCalendarUnavailable)
			}

			result := calendar.FetchSchedule(ctx, time.Now(), req, calCfg, discovery)
			payload, merr := json.Marshal(result)
			if merr != nil {
				return "", fmt.Errorf("calendar_schedule: marshaling result: %w", merr)
			}

			switch result.Status {
			case calendar.StatusComplete:
				return string(payload), nil
			case calendar.StatusPartial:
				return string(payload), calendar.ErrCalendarIncomplete
			default:
				return string(payload), calendar.ErrCalendarUnavailable
			}
		},
	}
}

// calendarUnavailablePayload builds a minimal ScheduleResult JSON blob for
// the case where calendar: config itself couldn't be loaded -- there is no
// range to resolve or coverage ledger to report, but Run must still return
// a payload string (never bare "", err) so the model sees a structured
// reason rather than an opaque tool error.
func calendarUnavailablePayload(cfgErr error) string {
	result := calendar.ScheduleResult{
		Status: calendar.StatusFailed,
		Speech: "I don't have any calendars configured, so I can't check your schedule.",
	}
	payload, err := json.Marshal(result)
	if err != nil {
		// Marshaling a literal struct with only string fields cannot fail;
		// this is unreachable in practice, but the fallback still avoids
		// dropping cfgErr's detail entirely.
		return fmt.Sprintf(`{"status":"failed","speech":%q}`, cfgErr.Error())
	}
	return string(payload)
}
