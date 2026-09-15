package brain

// Task status taxonomy. `open` and `in-progress` are "active" - visible by
// default. `hold` and `deferred` are non-terminal but hidden from the default
// list. `done` and `closed` are terminal: `done` means I completed it
// (counts as my work), `closed` means it's no longer relevant - coworker did
// it, outdated, won't do.
const (
	StatusOpen       = "open"
	StatusInProgress = "in-progress"
	StatusHold       = "hold"
	StatusDeferred   = "deferred"
	StatusDone       = "done"
	StatusClosed     = "closed"
)

// AllStatuses returns every canonical status in display order.
func AllStatuses() []string {
	return []string{StatusOpen, StatusInProgress, StatusHold, StatusDeferred, StatusDone, StatusClosed}
}

// ActiveStatuses are shown by default in `aida tasks`.
func ActiveStatuses() []string {
	return []string{StatusOpen, StatusInProgress}
}

// NonTerminalStatuses are every status that is not done/closed - broader
// than ActiveStatuses, which is only the two shown by default. Used by
// callers that need "is this still an open concern" (e.g. dedup checks)
// rather than "is this in the default list view".
func NonTerminalStatuses() []string {
	return []string{StatusOpen, StatusInProgress, StatusHold, StatusDeferred}
}

// IsValidStatus reports whether s is one of the canonical statuses.
func IsValidStatus(s string) bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusHold, StatusDeferred, StatusDone, StatusClosed:
		return true
	}
	return false
}

// IsTerminalStatus reports whether s represents a terminal state. Terminal
// tasks have `completed = 1` in the DB.
func IsTerminalStatus(s string) bool {
	return s == StatusDone || s == StatusClosed
}
