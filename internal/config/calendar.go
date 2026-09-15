package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// CalendarEntry configures one calendar to fetch within a CalendarAccount.
// ID is the calendar identifier passed to google-workspace-mcp's
// listEvents/listCalendars tools (a calendar id, not necessarily an email
// address -- group calendars use an opaque "...@group.calendar.google.com"
// id instead).
type CalendarEntry struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label,omitempty"`
	Enabled bool   `yaml:"enabled"`
	// MatchTitles, left empty (the default), applies no filtering -- every
	// fetched event for this calendar is kept, exactly like today. When
	// non-empty, only an event whose title contains at least one of these
	// strings survives (case-insensitive substring match, not a whole-word
	// match, so "Alice" also matches "Alice-dentist"); every other event on
	// this calendar is dropped before it ever reaches a caller.
	//
	// This exists for a calendar the user only has read access to: someone
	// else's calendar, shared read-only, tracked here purely for the small
	// subset of events naming the user's own children -- the rest is that
	// other person's own business and must never surface as "on my
	// schedule." Because the calendar is read-only, the user has no way to
	// notice or correct a mistitled event on the other end -- an event that
	// doesn't happen to match one of these strings is silently invisible to
	// them, indistinguishable from an event that was never created.
	MatchTitles []string `yaml:"match_titles,omitempty"`
}

// CalendarAccount configures one Google account under the calendar:
// block. Account is the identifier google-workspace-mcp expects as its
// own "account" argument (e.g. "personal", "work"), not an email address.
type CalendarAccount struct {
	Account   string          `yaml:"account"`
	Label     string          `yaml:"label,omitempty"`
	Enabled   bool            `yaml:"enabled"`
	Calendars []CalendarEntry `yaml:"calendars,omitempty"`
}

// CalendarConfig holds settings for the calendar_schedule voice tool and
// its schedule service (internal/calendar). Read from the calendar: block
// of config.yaml.
//
// Unlike MenuConfig/HabitsConfig/TrainingConfig above, the shared Config
// struct in config.go carries no Calendar field of its own --
// (*Config).CalendarConfig parses the calendar: block directly off disk
// instead of off an already-populated struct field, so this file adds the
// capability without changing Config's type definition.
type CalendarConfig struct {
	Timezone       string
	MaxConcurrency int
	CallTimeout    time.Duration
	MaxResults     int
	Accounts       []CalendarAccount
}

const (
	defaultCalendarMaxConcurrency = 4
	defaultCalendarCallTimeout    = 30 * time.Second
	defaultCalendarMaxResults     = 250
)

// calendarConfigYAML is the on-disk shape of the calendar: block. Kept
// separate from CalendarConfig so CallTimeout can be authored as a
// human-friendly Go duration string ("30s") in YAML while the resolved
// struct carries a real time.Duration -- yaml.v3 has no built-in support
// for parsing a duration string into a time.Duration field.
type calendarConfigYAML struct {
	Timezone       string            `yaml:"timezone,omitempty"`
	MaxConcurrency int               `yaml:"max_concurrency,omitempty"`
	CallTimeout    string            `yaml:"call_timeout,omitempty"`
	MaxResults     int               `yaml:"max_results,omitempty"`
	Accounts       []CalendarAccount `yaml:"accounts,omitempty"`
}

type calendarConfigFile struct {
	Calendar calendarConfigYAML `yaml:"calendar"`
}

// ErrNoCalendarAccounts is returned by CalendarConfig when config.yaml is
// missing, has no calendar: block, or that block's accounts: list is
// empty. Every caller must treat this as a configuration error to
// surface, never as "the calendar is empty" -- collapsing the two is
// exactly the false-"you're clear" bug this package exists to prevent.
var ErrNoCalendarAccounts = fmt.Errorf("calendar: config.yaml has no calendar.accounts configured")

// CalendarConfig returns calendar settings for the calendar_schedule
// voice tool, following the pattern of MenuPath/HabitsDBPath: explicit
// config wins, sensible defaults fill in everything else. It reads
// config.yaml directly off disk rather than an already-populated Config
// field -- see the CalendarConfig type's doc comment above for why.
func (c *Config) CalendarConfig() (CalendarConfig, error) {
	path := filepath.Join(Dir(), ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CalendarConfig{}, ErrNoCalendarAccounts
		}
		return CalendarConfig{}, fmt.Errorf("reading config for calendar settings: %w", err)
	}

	var file calendarConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return CalendarConfig{}, fmt.Errorf("parsing calendar config: %w", err)
	}
	return resolveCalendarConfig(file.Calendar)
}

// resolveCalendarConfig applies defaults to a parsed calendar: block.
// timezone falls back to the system local zone, max_concurrency to 4,
// call_timeout to 30s, and max_results to 250 -- an absent or empty
// accounts list is always an error, never defaulted to "no accounts".
func resolveCalendarConfig(raw calendarConfigYAML) (CalendarConfig, error) {
	if len(raw.Accounts) == 0 {
		return CalendarConfig{}, ErrNoCalendarAccounts
	}

	cfg := CalendarConfig{
		Timezone:       time.Local.String(),
		MaxConcurrency: defaultCalendarMaxConcurrency,
		CallTimeout:    defaultCalendarCallTimeout,
		MaxResults:     defaultCalendarMaxResults,
		Accounts:       raw.Accounts,
	}
	if raw.Timezone != "" {
		cfg.Timezone = raw.Timezone
	}
	if raw.MaxConcurrency > 0 {
		cfg.MaxConcurrency = raw.MaxConcurrency
	}
	if raw.MaxResults > 0 {
		cfg.MaxResults = raw.MaxResults
	}
	if raw.CallTimeout != "" {
		d, err := time.ParseDuration(raw.CallTimeout)
		if err != nil {
			return CalendarConfig{}, fmt.Errorf("calendar.call_timeout %q: %w", raw.CallTimeout, err)
		}
		cfg.CallTimeout = d
	}
	return cfg, nil
}
