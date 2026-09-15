package dailybriefing

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

// ComposeBody builds the plain-text email body. Schema mirrors the prior
// claude-prompt template so downstream eyes don't have to relearn it.
//
// triage may be nil - in that case the EMAILS section reports "(triage unavailable)".
func ComposeBody(events []Event, openTasks, newTasks []brain.TaskRecord, triage *TriageResult, today time.Time, newWindow time.Duration) string {
	var b strings.Builder

	fmt.Fprintf(&b, "TODAY'S CALENDAR (ET) -- %s\n", today.Format("Mon Jan 2"))
	if len(events) == 0 {
		b.WriteString("- No events scheduled\n")
	} else {
		for _, line := range formatCalendar(events, today) {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	if len(newTasks) > 0 {
		fmt.Fprintf(&b, "\nNEW TASKS (added in the last %s)\n", humanizeWindow(newWindow))
		for _, line := range formatTaskList(newTasks) {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	b.WriteString("\nOPEN WORK TASKS (left to do)\n")
	if len(openTasks) == 0 {
		b.WriteString("- No open tasks\n")
	} else {
		for _, line := range formatTaskList(openTasks) {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}

	b.WriteString("\nEMAILS NEEDING ATTENTION\n")
	writeTriage(&b, triage)

	b.WriteString("\n---\n")
	b.WriteString("Sent by aida daily (gws pipeline).\n")
	return b.String()
}

// writeTriage renders the EMAILS NEEDING ATTENTION section: flagged buckets in
// the order they appear in the result map (sorted for stability), a stale-unread
// block, and a partner-email block. Empty buckets are skipped silently.
func writeTriage(b *strings.Builder, t *TriageResult) {
	if t == nil {
		b.WriteString("- (triage unavailable - see daily-briefing.log)\n")
		return
	}
	hasAny := false

	bucketNames := make([]string, 0, len(t.Flagged))
	for name := range t.Flagged {
		bucketNames = append(bucketNames, name)
	}
	sort.Strings(bucketNames)
	for _, name := range bucketNames {
		rows := t.Flagged[name]
		if len(rows) == 0 {
			continue
		}
		hasAny = true
		fmt.Fprintf(b, "\n[%s]\n", name)
		for _, r := range rows {
			fmt.Fprintf(b, "- %s\n", formatFlaggedRow(r))
		}
	}

	if len(t.Stale) > 0 {
		hasAny = true
		b.WriteString("\n[Stale Unread - Primary tab, 1d+]\n")
		for _, r := range t.Stale {
			fmt.Fprintf(b, "- %s\n", formatStaleRow(r))
		}
	}

	if len(t.Partners) > 0 {
		hasAny = true
		b.WriteString("\n[Partner Emails]\n")
		for _, r := range t.Partners {
			fmt.Fprintf(b, "- %s\n", formatPartnerRow(r))
		}
	}

	if !hasAny {
		b.WriteString("- No emails needing attention\n")
	}
}

// truncateSnippet caps snippet length so a single row stays scannable.
func truncateSnippet(s string) string {
	const max = 140
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func formatFlaggedRow(r EmailRow) string {
	parts := []string{r.Subject}
	if r.From != "" {
		parts = append(parts, "from: "+r.From)
	}
	if !r.Date.IsZero() {
		parts = append(parts, "latest "+r.Date.Format("2006-01-02"))
	}
	head := parts[0] + " (" + strings.Join(parts[1:], ", ") + ")"
	if r.Snippet != "" {
		return head + `: "` + truncateSnippet(r.Snippet) + `"`
	}
	return head
}

func formatStaleRow(r EmailRow) string {
	age := ""
	if !r.Date.IsZero() {
		age = fmt.Sprintf("%dd old", int(time.Since(r.Date).Hours()/24))
	}
	parts := []string{r.Subject}
	if r.From != "" {
		parts = append(parts, "from: "+r.From)
	}
	if age != "" {
		parts = append(parts, age)
	}
	head := parts[0] + " (" + strings.Join(parts[1:], ", ") + ")"
	if r.Snippet != "" {
		return head + `: "` + truncateSnippet(r.Snippet) + `"`
	}
	return head
}

func formatPartnerRow(r EmailRow) string {
	tag := r.PartnerTag
	if tag == "" {
		tag = "Partner"
	}
	parts := []string{r.Subject}
	if r.From != "" {
		parts = append(parts, "from: "+r.From)
	}
	if !r.Date.IsZero() {
		parts = append(parts, r.Date.Format("2006-01-02"))
	}
	head := fmt.Sprintf("[%s] %s (%s)", tag, parts[0], strings.Join(parts[1:], ", "))
	if r.Snippet != "" {
		return head + `: "` + truncateSnippet(r.Snippet) + `"`
	}
	return head
}

func humanizeWindow(d time.Duration) string {
	if d == 24*time.Hour {
		return "24h"
	}
	return d.String()
}

func formatCalendar(events []Event, today time.Time) []string {
	loc := today.Location()
	startOfDay := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, loc)
	endOfDay := startOfDay.Add(24 * time.Hour)

	type row struct {
		key  time.Time
		text string
	}
	var rows []row

	for _, e := range events {
		if e.AllDay {
			if !overlapsAllDay(e, startOfDay, endOfDay) {
				continue
			}
			rows = append(rows, row{
				key:  startOfDay,
				text: fmt.Sprintf("(all day) %s", e.Summary),
			})
			continue
		}
		if !overlapsTimed(e, startOfDay, endOfDay) {
			continue
		}
		rows = append(rows, row{
			key:  e.Start,
			text: fmt.Sprintf("%s  %s", formatStartTime(e.Start), e.Summary),
		})
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].key.Before(rows[j].key) })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.text)
	}
	return out
}

func overlapsAllDay(e Event, dayStart, dayEnd time.Time) bool {
	if e.End.IsZero() {
		return !e.Start.After(dayEnd) && !e.Start.Before(dayStart)
	}
	return e.Start.Before(dayEnd) && e.End.After(dayStart)
}

func overlapsTimed(e Event, dayStart, dayEnd time.Time) bool {
	end := e.End
	if end.IsZero() {
		end = e.Start
	}
	return e.Start.Before(dayEnd) && end.After(dayStart)
}

func formatStartTime(t time.Time) string {
	return strings.ToLower(t.Format("3:04pm"))
}

// formatTaskList renders tasks as "- [pN] #M Title (created YYYY-MM-DD)".
// Drops the noisy tag list - the priority is in the bracket, the slack-followup
// links are already in the title, and other tags are not actionable for the
// reader at briefing time.
func formatTaskList(tasks []brain.TaskRecord) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		idLabel := "#?"
		if t.TaskID > 0 {
			idLabel = fmt.Sprintf("#%d", t.TaskID)
		}
		created := t.Created
		if len(created) >= 10 {
			created = created[:10]
		}
		out = append(out, fmt.Sprintf("[%s] %s %s (created %s)", t.PriorityTag(), idLabel, t.Title, created))
	}
	return out
}
