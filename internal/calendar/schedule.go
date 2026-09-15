package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// googleWorkspaceServer is the MCP server name exposing Google Calendar
// tools (see internal/mcp's discovery client).
const googleWorkspaceServer = "google-workspace-mcp"

// MCPCaller is the narrow interface FetchSchedule needs to reach an MCP
// tool. *mcp.MCPDiscovery already satisfies this -- its CallTool method has
// this exact signature -- with no wrapper needed. Defined here rather than
// imported from internal/mcp so this package, and its tests, never depend
// on the concrete discovery/session machinery; a test supplies a small fake
// instead.
type MCPCaller interface {
	CallTool(ctx context.Context, server, name string, args map[string]any) (string, error)
}

// Status is FetchSchedule's overall outcome bucket.
type Status string

const (
	StatusComplete Status = "complete"
	StatusPartial  Status = "partial"
	StatusFailed   Status = "failed"
)

// CoverageStatus is the per-(account,calendar) outcome recorded in the
// ledger FetchSchedule returns alongside its events. This ledger is the
// mechanism that makes a false "you're clear" structurally impossible: a
// caller can always see exactly which calendars were, and were not,
// actually checked before trusting an empty event list.
type CoverageStatus string

const (
	CoverageSuccess   CoverageStatus = "success"
	CoverageFailed    CoverageStatus = "failed"
	CoverageSkipped   CoverageStatus = "skipped"
	CoverageTruncated CoverageStatus = "truncated"
)

// CoverageEntry records the outcome of fetching one configured calendar.
type CoverageEntry struct {
	Account    string         `json:"account"`
	CalendarID string         `json:"calendar_id"`
	Label      string         `json:"label,omitempty"`
	Status     CoverageStatus `json:"status"`
	Error      string         `json:"error,omitempty"`
}

// Event is a rendered calendar event. Only display fields are exposed --
// no description, no attendee list -- and Summary/Location are always
// treated as opaque, untrusted display strings, never as instructions,
// since they originate from other people's calendar entries.
type Event struct {
	Summary  string `json:"summary"`
	Start    string `json:"start"` // RFC3339 (timed) or YYYY-MM-DD (all-day)
	End      string `json:"end"`
	AllDay   bool   `json:"all_day"`
	Location string `json:"location,omitempty"`
	Account  string `json:"account"`
	Calendar string `json:"calendar,omitempty"`
}

// ScheduleResult is FetchSchedule's full answer: not just events, but
// enough coverage information for a caller (or a model reading this JSON)
// to tell "checked everything, you're clear" apart from "couldn't check
// everything, don't know."
type ScheduleResult struct {
	Status       Status          `json:"status"`
	ClearAllowed bool            `json:"clear_allowed"`
	Timezone     string          `json:"timezone"`
	RangeLabel   string          `json:"range_label"`
	RangeStart   string          `json:"range_start"`
	RangeEnd     string          `json:"range_end"`
	Coverage     []CoverageEntry `json:"coverage"`
	Events       []Event         `json:"events"`
	Speech       string          `json:"speech"`
}

// ErrCalendarIncomplete signals a partial result: at least one enabled
// calendar was fetched successfully, but at least one other failed, was
// skipped, or was truncated. Callers should still surface the payload's
// events -- this error means "don't trust this as exhaustive," not "there
// is nothing to show."
var ErrCalendarIncomplete = errors.New("calendar: partial coverage, some calendars could not be checked")

// ErrCalendarUnavailable signals no calendar could be checked at all --
// either every fetch failed, or there were zero enabled calendars to
// begin with. Never treat this the same as "you're clear."
var ErrCalendarUnavailable = errors.New("calendar: no calendar could be checked")

// FetchSchedule resolves req to a concrete time range (in Go -- see
// resolveRange in resolve.go -- never in a model) and fetches every
// enabled (account, calendar) pair from cfg, concurrently across accounts
// bounded by cfg.MaxConcurrency. It always returns a ScheduleResult;
// callers read Status/ClearAllowed rather than an error to decide what
// happened. See the ClearAllowed predicate below for why an empty Events
// slice is only ever trustworthy when every configured calendar was
// actually, successfully checked.
func FetchSchedule(ctx context.Context, now time.Time, req ScheduleRequest, cfg config.CalendarConfig, caller MCPCaller) ScheduleResult {
	loc := resolveLocation(cfg.Timezone)

	rr, err := resolveRange(now, loc, req)
	if err != nil {
		return ScheduleResult{
			Status:   StatusFailed,
			Timezone: loc.String(),
			Speech:   fmt.Sprintf("I couldn't work out which day you meant: %v.", err),
		}
	}

	result := ScheduleResult{
		Timezone:   loc.String(),
		RangeLabel: rr.Label,
		RangeStart: rr.Start.Format(time.RFC3339),
		RangeEnd:   rr.End.Format(time.RFC3339),
	}

	pairs := enabledPairs(cfg)
	expected := len(pairs)
	if expected == 0 {
		result.Status = StatusFailed
		result.Speech = "I don't have any calendars configured to check, so I can't tell you about " + rr.Label + "."
		return result
	}

	entries, events := dispatchFetches(ctx, pairs, rr, cfg, caller, loc)
	result.Coverage = entries
	result.Events = events

	var succeeded, failed, skipped, truncated int
	for _, e := range entries {
		switch e.Status {
		case CoverageSuccess:
			succeeded++
		case CoverageFailed:
			failed++
		case CoverageSkipped:
			skipped++
		case CoverageTruncated:
			truncated++
		}
	}

	result.ClearAllowed = expected > 0 && succeeded == expected &&
		failed == 0 && skipped == 0 && truncated == 0 && len(events) == 0

	switch {
	case succeeded == expected:
		result.Status = StatusComplete
	case succeeded > 0:
		result.Status = StatusPartial
	default:
		result.Status = StatusFailed
	}

	result.Speech = buildSpeech(result, succeeded, expected)
	return result
}

func resolveLocation(tz string) *time.Location {
	if tz == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Local
	}
	return loc
}

// accountCalendarPair is one enabled (account, calendar) combination to
// fetch -- the unit the coverage ledger tracks.
type accountCalendarPair struct {
	Account     string
	AccountIdx  int // index into cfg.Accounts, for grouping by account
	CalendarID  string
	Label       string
	MatchTitles []string // this calendar's config.CalendarEntry.MatchTitles, if any
}

// enabledPairs flattens cfg into the calendars actually eligible for a
// fetch: the account itself must be enabled, and so must the individual
// calendar entry. Config file order is preserved, which matters for the
// "unattempted calendars are skipped in order" rule an account-level auth
// failure triggers (see fetchAccount).
func enabledPairs(cfg config.CalendarConfig) []accountCalendarPair {
	var pairs []accountCalendarPair
	for i, acc := range cfg.Accounts {
		if !acc.Enabled {
			continue
		}
		for _, cal := range acc.Calendars {
			if !cal.Enabled {
				continue
			}
			pairs = append(pairs, accountCalendarPair{
				Account:     acc.Account,
				AccountIdx:  i,
				CalendarID:  cal.ID,
				Label:       cal.Label,
				MatchTitles: cal.MatchTitles,
			})
		}
	}
	return pairs
}

// dispatchFetches runs one fetch per enabled (account, calendar) pair,
// concurrently across accounts (bounded by cfg.MaxConcurrency) but
// sequentially within an account. Sequential-per-account is what makes
// "an account-level auth failure skips the rest of that account's
// calendars" a well-defined, order-preserving outcome instead of a race.
func dispatchFetches(ctx context.Context, pairs []accountCalendarPair, rr resolvedRange, cfg config.CalendarConfig, caller MCPCaller, loc *time.Location) ([]CoverageEntry, []Event) {
	byAccount := make(map[int][]accountCalendarPair)
	var accountOrder []int
	for _, p := range pairs {
		if _, ok := byAccount[p.AccountIdx]; !ok {
			accountOrder = append(accountOrder, p.AccountIdx)
		}
		byAccount[p.AccountIdx] = append(byAccount[p.AccountIdx], p)
	}

	concurrency := cfg.MaxConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)

	entriesByAccount := make([][]CoverageEntry, len(accountOrder))
	eventsByAccount := make([][]Event, len(accountOrder))
	var wg sync.WaitGroup

	for pos, accIdx := range accountOrder {
		pos, accIdx := pos, accIdx
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			entries, events := fetchAccount(ctx, byAccount[accIdx], rr, cfg, caller, loc)
			entriesByAccount[pos] = entries
			eventsByAccount[pos] = events
		}()
	}
	wg.Wait()

	var allEntries []CoverageEntry
	var allEvents []Event
	for i := range accountOrder {
		allEntries = append(allEntries, entriesByAccount[i]...)
		allEvents = append(allEvents, eventsByAccount[i]...)
	}

	sortEvents(allEvents)
	return allEntries, allEvents
}

// fetchAccount fetches every calendar for one account, in order, stopping
// early -- marking the rest skipped -- the moment an account-level auth
// failure is seen. A per-calendar timeout or other error only fails that
// one calendar; the loop continues to the next.
func fetchAccount(ctx context.Context, pairs []accountCalendarPair, rr resolvedRange, cfg config.CalendarConfig, caller MCPCaller, loc *time.Location) ([]CoverageEntry, []Event) {
	var entries []CoverageEntry
	var events []Event
	authFailed := false

	for _, p := range pairs {
		if authFailed {
			entries = append(entries, CoverageEntry{
				Account:    p.Account,
				CalendarID: p.CalendarID,
				Label:      p.Label,
				Status:     CoverageSkipped,
				Error:      "skipped after an account-level auth failure",
			})
			continue
		}

		entry, evs, isAuth := fetchOne(ctx, p, rr, cfg, caller, loc)
		entries = append(entries, entry)
		events = append(events, evs...)
		if isAuth {
			authFailed = true
		}
	}
	return entries, events
}

// fetchOne calls listEvents for a single (account, calendar) pair, bounded
// by cfg.CallTimeout, and classifies the outcome into a CoverageEntry. The
// third return value is true only when the failure looks like an
// account-level auth problem (see isAuthError), signaling the caller to
// skip the rest of this account's calendars.
//
// The CoverageEntry's status is decided from the raw listEvents response
// (did the call succeed, was it truncated) BEFORE p.MatchTitles is applied
// -- a calendar that fetched fine and then had every event filtered away by
// match_titles is still a successful fetch, never a failure, partial, or
// skip. Filtering removes events from the answer; it never removes a
// calendar from the ledger.
func fetchOne(ctx context.Context, p accountCalendarPair, rr resolvedRange, cfg config.CalendarConfig, caller MCPCaller, loc *time.Location) (CoverageEntry, []Event, bool) {
	entry := CoverageEntry{Account: p.Account, CalendarID: p.CalendarID, Label: p.Label}

	timeout := cfg.CallTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	maxResults := cfg.MaxResults
	if maxResults <= 0 {
		maxResults = 250
	}
	args := map[string]any{
		"account":    p.Account,
		"calendarId": p.CalendarID,
		"timeMin":    rr.Start.Format(time.RFC3339),
		"timeMax":    rr.End.Format(time.RFC3339),
		"maxResults": maxResults,
	}

	raw, err := caller.CallTool(callCtx, googleWorkspaceServer, "listEvents", args)
	if err != nil {
		entry.Status = CoverageFailed
		entry.Error = err.Error()
		return entry, nil, isAuthError(err)
	}

	var resp listEventsResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		entry.Status = CoverageFailed
		entry.Error = fmt.Sprintf("malformed listEvents response: %v", err)
		return entry, nil, false
	}

	events := renderEvents(resp.Events, p, rr, loc)

	if len(resp.Events) == maxResults {
		entry.Status = CoverageTruncated
	} else {
		entry.Status = CoverageSuccess
	}

	// match_titles is applied last, after cancelled-event and overlap
	// filtering, and after the coverage status above is already decided --
	// see the doc comment on fetchOne.
	events = filterByTitles(events, p.MatchTitles)

	return entry, events, false
}

// isAuthError heuristically detects an account-level auth/permission
// failure from an MCP error message, so the rest of that account's
// calendars can be skipped instead of each hitting the same wall
// individually and burning cfg.CallTimeout for no reason. There is no
// structured error code for this in the MCP protocol, so this is a
// substring check -- deliberately generous, since missing a real auth
// failure just costs a few wasted calls, while treating an ordinary
// failure as an auth failure would incorrectly skip calendars that might
// have succeeded on their own.
func isAuthError(err error) bool {
	s := strings.ToLower(err.Error())
	for _, needle := range []string{"unauthor", "forbidden", "permission denied", "invalid_grant", "401", "403"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// buildSpeech renders ScheduleResult into one deterministic sentence (or
// short paragraph) for the voice layer. Deterministic means the same
// result always produces the same speech -- no randomness, no model call
// -- so this string is exactly as trustworthy as the structured fields it
// summarizes.
func buildSpeech(r ScheduleResult, succeeded, expected int) string {
	switch r.Status {
	case StatusFailed:
		if expected == 0 {
			return "I don't have any calendars configured, so I can't check " + r.RangeLabel + "."
		}
		msg := "I couldn't check your calendar for " + r.RangeLabel + ", so I can't say whether you're clear."
		if len(r.Events) > 0 {
			msg += " Here's what I did find: " + summarizeEvents(r.Events)
		}
		return msg
	case StatusPartial:
		msg := fmt.Sprintf("I could only check %d of %d calendars for %s, so I can't say you're fully clear.", succeeded, expected, r.RangeLabel)
		if len(r.Events) > 0 {
			msg += " Here's what I did find: " + summarizeEvents(r.Events)
		}
		return msg
	default: // StatusComplete
		if r.ClearAllowed {
			return "You're clear for " + r.RangeLabel + "."
		}
		return fmt.Sprintf("For %s: %s", r.RangeLabel, summarizeEvents(r.Events))
	}
}

func summarizeEvents(events []Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		if e.AllDay {
			parts = append(parts, e.Summary+" (all day)")
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Start)
		if err != nil {
			parts = append(parts, e.Summary)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s at %s", e.Summary, t.Format("3:04 PM")))
	}
	return strings.Join(parts, "; ")
}
