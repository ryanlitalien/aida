package models

import (
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// FormatDurationHuman renders a non-negative duration in a compact,
// human-scaled form used by both `aida models` (internal/cli/models.go)
// and the Models panel on /dashboard (dashboard_web.html's own JS
// equivalent, formatDurationHuman -- keep the two in sync):
//
//   - 24h and over: days + hours, minutes dropped ("6d 22h", "7d 0h")
//   - 1h up to 23h59m: the existing compact hour/minute form ("3h15m")
//   - under 1h: minutes only ("42m")
//
// A negative duration (a reset time already in the past) is clamped to
// zero here -- callers that need a "past due" message (both current
// call sites do) check for that before calling this, since "past due" is
// a different kind of message, not a duration to format.
func FormatDurationHuman(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalMinutes := int64(d / time.Minute)
	if totalMinutes >= 24*60 {
		days := totalMinutes / (24 * 60)
		hours := (totalMinutes % (24 * 60)) / 60
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if totalMinutes >= 60 {
		return fmt.Sprintf("%dh%dm", totalMinutes/60, totalMinutes%60)
	}
	return fmt.Sprintf("%dm", totalMinutes)
}

// FormatPaceChip renders a Bar or Spend row's Pace (see pace.go) as the
// short token `aida models` and the dashboard's Models panel print next
// to a paced row's used% -- "2.9x HOT", "on pace", "0.1x IDLE" -- or ""
// for a nil Pace (unpaced: unknown/short window, no reset time, or still
// "early" in the window; see PaceFor). Plain text only -- coloring the
// token by verdict is each caller's own job (internal/cli/models.go's
// lipgloss styling, dashboard_web.html's .pace-hot/.pace-idle/.pace-on
// classes).
func FormatPaceChip(p *Pace) string {
	if p == nil {
		return ""
	}
	switch p.Verdict {
	case PaceHot:
		return fmt.Sprintf("%.1fx HOT", p.Ratio)
	case PaceIdle:
		return fmt.Sprintf("%.1fx IDLE", p.Ratio)
	case PaceOnPace:
		return "on pace"
	default:
		return ""
	}
}

// priceDotDigits matches a dollar amount with exactly two decimal digits
// (e.g. "$19.99", "$75.00") anywhere inside a larger string -- a roster
// provider's free-text plan/billing field ("Claude Pro ($20/mo)",
// "$74.99/mo") or a pre-formatted "$32.81 / $75.00" spend/budget line.
var priceDotDigits = regexp.MustCompile(`\$(\d+)\.(\d{2})`)

// RoundPricesForDisplay applies two cosmetic, render-time-only rules to
// every dollar amount found in s. It never touches the underlying roster
// YAML or probed Usage data -- only how `aida models` and the Models
// dashboard panel print it (dashboard_web.html's JS equivalent,
// roundPricesForDisplay, mirrors this exactly):
//
//   - a price ending in .99 rounds up to the next whole dollar
//     ("$19.99" -> "$20", "$74.99/mo" -> "$75/mo")
//   - a price that is already a whole-dollar amount drops its trailing
//     ".00" ("$75.00" -> "$75")
//
// Any other cents value is left exactly as-is ("$32.81" -> "$32.81").
func RoundPricesForDisplay(s string) string {
	return priceDotDigits.ReplaceAllStringFunc(s, func(match string) string {
		parts := priceDotDigits.FindStringSubmatch(match)
		whole, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return match
		}
		cents, err := strconv.Atoi(parts[2])
		if err != nil {
			return match
		}
		if cents == 99 {
			whole++
			cents = 0
		}
		if cents == 0 {
			return fmt.Sprintf("$%d", whole)
		}
		return match
	})
}
