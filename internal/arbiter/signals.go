package arbiter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Verdict is what one wrapper/hook observation says about a lane.
type Verdict string

const (
	// VerdictEmpty means the observed text matched a known usage-limit
	// phrase -- the lane is exhausted.
	VerdictEmpty Verdict = "empty"
	// VerdictNotEmpty means a clean exit with ordinary output -- the
	// lane still has room.
	VerdictNotEmpty Verdict = "not_empty"
	// VerdictAmbiguous means neither of the above could be established
	// confidently (a non-zero exit with no recognized phrase, empty
	// output, or a weak/partial match like the bare word "limit"). Per
	// plan decision 2, Store.Record treats VerdictAmbiguous exactly like
	// VerdictEmpty -- FAIL OPEN -- but tags the mark ambiguous=true so
	// the false-positive rate stays measurable (FalsePositiveSummary).
	VerdictAmbiguous Verdict = "ambiguous"
)

// emptyPhrases are case-insensitive substrings known to appear in a
// usage-limit refusal or rate-limit error body, checked in this order.
// A match here is unconditional: it wins regardless of exit code, since a
// provider can print this text and still exit 0 (e.g. a Claude Code Stop
// hook transcript message that reads as a normal turn end to the process
// exit code, but whose text IS the refusal).
var emptyPhrases = []string{
	"usage limit reached",
	"you've hit your limit",
	"you have hit your limit",
	"rate limit",
	"rate_limit_error",
	"429",
	"quota exceeded",
	"resets at",
	"limit will reset",
	"out of extra usage",
	"insufficient_quota",
	"overloaded_error",
}

// weakLimitWordRe flags a bare, unqualified "limit" -- text that gestures
// at a limit without matching any of emptyPhrases confidently enough to
// call it Empty outright. ClassifySignal treats this as Ambiguous
// regardless of exit code (fail open, but flagged as a weak signal).
var weakLimitWordRe = regexp.MustCompile(`\blimit\b`)

// ClassifySignal turns a wrapper/hook observation (a subprocess exit code
// and its combined output, or a Stop hook's last assistant message) into
// a Verdict. The second return value is the matched phrase for Empty (or
// "" otherwise) -- carried into Signal.Matched for the log.
//
// Precedence, in order:
//
//  1. Any emptyPhrases substring match (case-insensitive) -> Empty,
//     regardless of exit code.
//  2. A bare "limit" word with no full phrase match -> Ambiguous,
//     regardless of exit code (a weak signal, never confidently Empty
//     nor confidently clean).
//  3. Exit code 0 with non-empty text and no phrase match -> NotEmpty.
//  4. Everything else (non-zero exit with no recognized phrase, or empty
//     text) -> Ambiguous.
func ClassifySignal(exitCode int, text string) (Verdict, string) {
	lower := strings.ToLower(text)
	for _, p := range emptyPhrases {
		if strings.Contains(lower, p) {
			return VerdictEmpty, p
		}
	}
	if weakLimitWordRe.MatchString(lower) {
		return VerdictAmbiguous, ""
	}
	if exitCode == 0 && strings.TrimSpace(text) != "" {
		return VerdictNotEmpty, ""
	}
	return VerdictAmbiguous, ""
}

// maxRawLen bounds Signal.Raw -- the signals log is meant for spot-
// checking and FalsePositiveSummary counts, not full transcript replay,
// so a multi-KB refusal body gets truncated rather than bloating
// signals.ndjson.
const maxRawLen = 300

// Signal is one wrapper/hook observation, appended to signals.ndjson.
type Signal struct {
	At         time.Time `json:"at"`
	Lane       string    `json:"lane"`
	Source     string    `json:"source"`
	Verdict    Verdict   `json:"verdict"`
	Ambiguous  bool      `json:"ambiguous"`
	Matched    string    `json:"matched,omitempty"`
	EmptyUntil time.Time `json:"empty_until,omitempty"`
	Raw        string    `json:"raw,omitempty"`
	Host       string    `json:"host,omitempty"`
}

// DefaultEmptyFallback is how long a lane is marked empty when no
// ResetsAt is known -- see ResolveEmptyUntil.
const DefaultEmptyFallback = time.Hour

// ResolveEmptyUntil picks the instant a lane should be treated as empty
// until: resetsAt when it's known and still in the future, else now plus
// fallback (DefaultEmptyFallback when fallback <= 0). A known reset time
// is always the better estimate -- an arbitrary fallback window either
// wastes headroom the lane already has back, or keeps retrying a lane
// that won't refill for hours.
func ResolveEmptyUntil(now, resetsAt time.Time, fallback time.Duration) time.Time {
	if fallback <= 0 {
		fallback = DefaultEmptyFallback
	}
	if !resetsAt.IsZero() && resetsAt.After(now) {
		return resetsAt
	}
	return now.Add(fallback)
}

// laneStateEntry is lane-state.json's per-lane value.
type laneStateEntry struct {
	EmptyUntil time.Time `json:"empty_until"`
	Since      time.Time `json:"since"`
	Source     string    `json:"source"`
	Ambiguous  bool      `json:"ambiguous"`
}

// Store is the on-disk signal log and per-lane empty-until state under
// ~/.aida/arbiter/ (plan section 5): signals.ndjson is append-only and
// never rewritten; lane-state.json is the current per-lane mark, replaced
// wholesale on every write.
type Store struct {
	dir string
}

// OpenStore returns a Store rooted at dir, creating it if necessary.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("arbiter: creating signal store dir %q: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) signalsPath() string { return filepath.Join(s.dir, "signals.ndjson") }
func (s *Store) statePath() string   { return filepath.Join(s.dir, "lane-state.json") }

// Record appends sig to signals.ndjson and, when its Verdict is Empty or
// Ambiguous, marks the lane empty in lane-state.json (plan decision 2:
// FAIL OPEN -- an ambiguous signal is treated exactly like an empty one,
// just flagged so the false-positive rate stays measurable).
func (s *Store) Record(sig Signal) error {
	if sig.At.IsZero() {
		sig.At = time.Now()
	}
	if len(sig.Raw) > maxRawLen {
		sig.Raw = sig.Raw[:maxRawLen]
	}
	sig.Ambiguous = sig.Verdict == VerdictAmbiguous

	line, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("arbiter: marshaling signal: %w", err)
	}
	f, err := os.OpenFile(s.signalsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("arbiter: opening signals log %q: %w", s.signalsPath(), err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("arbiter: writing signal: %w", err)
	}

	if sig.Verdict != VerdictEmpty && sig.Verdict != VerdictAmbiguous {
		return nil
	}
	until := sig.EmptyUntil
	if until.IsZero() {
		until = ResolveEmptyUntil(sig.At, time.Time{}, DefaultEmptyFallback)
	}
	return s.markEmpty(sig.Lane, until, sig.Source, sig.Ambiguous)
}

func (s *Store) loadState() (map[string]laneStateEntry, error) {
	data, err := os.ReadFile(s.statePath())
	if os.IsNotExist(err) {
		return map[string]laneStateEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]laneStateEntry{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (s *Store) saveState(m map[string]laneStateEntry) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

func (s *Store) markEmpty(lane string, until time.Time, source string, ambiguous bool) error {
	m, err := s.loadState()
	if err != nil {
		return fmt.Errorf("arbiter: reading lane state: %w", err)
	}
	m[lane] = laneStateEntry{EmptyUntil: until, Since: time.Now(), Source: source, Ambiguous: ambiguous}
	if err := s.saveState(m); err != nil {
		return fmt.Errorf("arbiter: writing lane state: %w", err)
	}
	return nil
}

// Clear removes lane's empty mark, if any -- used once a lane's window is
// known to have reset.
func (s *Store) Clear(lane string) error {
	m, err := s.loadState()
	if err != nil {
		return fmt.Errorf("arbiter: reading lane state: %w", err)
	}
	if _, ok := m[lane]; !ok {
		return nil
	}
	delete(m, lane)
	return s.saveState(m)
}

// EmptyUntil reports lane's active empty_until mark, if it's still in the
// future relative to now. Store implements the SignalReader interface
// Pick consumes via this method.
func (s *Store) EmptyUntil(lane string, now time.Time) (time.Time, bool) {
	m, err := s.loadState()
	if err != nil {
		return time.Time{}, false
	}
	e, ok := m[lane]
	if !ok {
		return time.Time{}, false
	}
	if now.Before(e.EmptyUntil) {
		return e.EmptyUntil, true
	}
	return time.Time{}, false
}

// ReadAll returns every signal recorded so far, in file order, tolerating
// (skipping) any corrupt line rather than failing the whole read -- the
// log is diagnostic history, not a transactional record, so one bad line
// must never hide every other one from `aida arbiter signals`.
func (s *Store) ReadAll() ([]Signal, error) {
	data, err := os.ReadFile(s.signalsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("arbiter: reading signals log %q: %w", s.signalsPath(), err)
	}
	var out []Signal
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var sig Signal
		if err := json.Unmarshal([]byte(line), &sig); err != nil {
			continue
		}
		out = append(out, sig)
	}
	return out, nil
}

// FalsePositiveSummary counts how many of sigs were logged Ambiguous --
// the measurable proxy plan decision 2 calls for ("so the false-positive
// rate is measurable later").
func FalsePositiveSummary(sigs []Signal) (total, ambiguous int) {
	for _, sig := range sigs {
		total++
		if sig.Ambiguous {
			ambiguous++
		}
	}
	return total, ambiguous
}
