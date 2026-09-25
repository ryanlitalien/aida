package models

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// agyQuotaTimeout bounds `agy --print /quota --output-format text`
// itself -- it took about 10s the day this probe was built, so 60s
// leaves real headroom without letting a wedged call run forever.
const agyQuotaTimeout = 60 * time.Second

// agyModelsTimeout bounds the separate `agy models` call used only for
// the unlisted-model diff (Detail["unlisted_models"]). Best-effort: a
// failure or timeout here never fails the probe as a whole, it just
// means that one Detail key is missing.
const agyModelsTimeout = 30 * time.Second

// agyProbeTimeout is this probe kind's own per-provider ceiling in Probe
// (see probeTimeoutFor). Both agy calls above run sequentially inside
// it, so it needs to cover agyQuotaTimeout + agyModelsTimeout plus
// slack -- the shared 8s every other probe gets would kill this one
// mid-flight on a normal day, not just a wedged one.
const agyProbeTimeout = 100 * time.Second

// agyQuotaRow is one parsed line of `agy --print /quota --output-format
// text`: a group ("Gemini Models", "Claude and GPT models"), a raw
// window label as agy prints it (e.g. "Five Hour Limit Remaining"), a
// percent REMAINING (agy's own convention -- converted to this
// package's used-percent convention in mapAgyQuotaBars), and a reset
// time.
type agyQuotaRow struct {
	Group     string
	Window    string
	Remaining float64
	ResetsAt  time.Time
}

// parseAgyQuotaLine parses one tab-separated line of `agy --print
// /quota --output-format text`:
//
//	<group>\t<window label>\t<percent>%\t<RFC3339 reset time>
//
// A line that doesn't split into exactly 4 fields, or whose percent
// doesn't parse, is skipped (ok=false) rather than erroring the whole
// scan -- `--output-format text` is a human-readable table, not a
// versioned machine contract, so a stray or reformatted line degrades
// gracefully instead of blanking out every bar.
func parseAgyQuotaLine(line string) (agyQuotaRow, bool) {
	fields := strings.Split(line, "\t")
	if len(fields) != 4 {
		return agyQuotaRow{}, false
	}
	group := strings.TrimSpace(fields[0])
	window := strings.TrimSpace(fields[1])
	pctStr := strings.TrimSuffix(strings.TrimSpace(fields[2]), "%")
	pct, err := strconv.ParseFloat(pctStr, 64)
	if err != nil {
		return agyQuotaRow{}, false
	}
	resetsAt := parseTimeLoose(strings.TrimSpace(fields[3]))
	return agyQuotaRow{Group: group, Window: window, Remaining: pct, ResetsAt: resetsAt}, true
}

// parseAgyQuotaText parses the whole `agy --print /quota
// --output-format text` output into rows, skipping any line
// parseAgyQuotaLine rejects (blank lines included). The pure, no-I/O
// unit this package's tests exercise directly against the canned
// four-line example.
func parseAgyQuotaText(text string) []agyQuotaRow {
	var rows []agyQuotaRow
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if row, ok := parseAgyQuotaLine(line); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

// agyGroupLabel maps agy's own group names to the short form used in a
// Bar's Label ("Gemini", "Claude/GPT"). An unrecognized group renders
// verbatim rather than being dropped, so a group agy adds later still
// shows up as *something* instead of silently vanishing.
func agyGroupLabel(group string) string {
	switch group {
	case "Gemini Models":
		return "Gemini"
	case "Claude and GPT models":
		return "Claude/GPT"
	default:
		return group
	}
}

// agyWindowLabel maps agy's own window names to this package's
// "5-hour"/"7-day" convention, matching the Codex and Claude bar
// labels. An unrecognized window renders verbatim.
func agyWindowLabel(window string) string {
	switch {
	case strings.Contains(window, "Five Hour"):
		return "5-hour"
	case strings.Contains(window, "Weekly"):
		return "7-day"
	default:
		return window
	}
}

// agyWindowMins is agyWindowLabel's parallel: agy's own window names
// mapped to minutes, matching the Codex/Claude 300 (5-hour) / 10080
// (7-day) convention. An unrecognized window returns 0 ("unknown"), the
// same convention every other prober in this package uses on Bar.WindowMins.
func agyWindowMins(window string) float64 {
	switch {
	case strings.Contains(window, "Five Hour"):
		return 300
	case strings.Contains(window, "Weekly"):
		return 10080
	default:
		return 0
	}
}

// agyWindowSortKey orders bars 5-hour before 7-day within a group,
// matching the order Codex's primary/secondary windows render in;
// anything else sorts after both.
func agyWindowSortKey(label string) int {
	switch label {
	case "5-hour":
		return 0
	case "7-day":
		return 1
	default:
		return 2
	}
}

// mapAgyQuotaBars converts parsed rows into Bars, converting agy's
// REMAINING percent into this package's used-percent convention
// (Percent = 100 - Remaining; see the invariant documented on Bar).
// Bars are grouped in the order groups first appear in rows and, within
// a group, ordered 5-hour before 7-day. The pure mapping step, unit
// tested directly against parseAgyQuotaText's output.
func mapAgyQuotaBars(rows []agyQuotaRow) []Bar {
	if len(rows) == 0 {
		return nil
	}

	var groupOrder []string
	seen := map[string]bool{}
	byGroup := map[string][]agyQuotaRow{}
	for _, r := range rows {
		g := agyGroupLabel(r.Group)
		if !seen[g] {
			seen[g] = true
			groupOrder = append(groupOrder, g)
		}
		byGroup[g] = append(byGroup[g], r)
	}

	var bars []Bar
	for _, g := range groupOrder {
		rs := byGroup[g]
		sort.SliceStable(rs, func(i, j int) bool {
			return agyWindowSortKey(agyWindowLabel(rs[i].Window)) < agyWindowSortKey(agyWindowLabel(rs[j].Window))
		})
		for _, r := range rs {
			used := 100 - r.Remaining
			bars = append(bars, Bar{
				Label:      agyWindowLabel(r.Window) + " " + g,
				Percent:    used,
				ResetsAt:   r.ResetsAt,
				Severity:   severityForPercent(used),
				WindowMins: agyWindowMins(r.Window),
			})
		}
	}
	return bars
}

// agyModelEntry is one line of `agy models`: a model id and its display
// name.
type agyModelEntry struct {
	ID          string
	DisplayName string
}

// parseAgyModelsOutput parses `agy models`' output: a "Fetching
// available models..." status line (skipped, like any other line with
// no tab) followed by tab-separated `id\tdisplay name` lines.
func parseAgyModelsOutput(text string) []agyModelEntry {
	var out []agyModelEntry
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(strings.TrimSpace(raw), "\r")
		if line == "" || !strings.Contains(line, "\t") {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		id := strings.TrimSpace(fields[0])
		if id == "" {
			continue
		}
		name := ""
		if len(fields) == 2 {
			name = strings.TrimSpace(fields[1])
		}
		out = append(out, agyModelEntry{ID: id, DisplayName: name})
	}
	return out
}

// agyEffortSuffixes are the effort-tier suffixes agy appends to a base
// model id ("gemini-3.8-flash-high", "-medium", "-low"). The roster
// convention (see ~/.aida/models.yaml's google provider) is to store
// only the base id and list the effort variants in the Model's Note, so
// agyModelsUnlisted strips one of these off before checking membership.
var agyEffortSuffixes = []string{"-high", "-medium", "-low"}

// agyModelBaseID strips a trailing effort suffix, if present; an id with
// none (e.g. "claude-sonnet-4-6", already stored verbatim in the
// roster) returns unchanged.
func agyModelBaseID(id string) string {
	lower := strings.ToLower(id)
	for _, suf := range agyEffortSuffixes {
		if strings.HasSuffix(lower, suf) {
			return id[:len(id)-len(suf)]
		}
	}
	return id
}

// agyModelsUnlisted returns a comma-joined, sorted list of model ids
// `agy models` offers that the roster's `known` set (lowercased model
// IDs) doesn't recognize -- checked both verbatim and after stripping an
// effort suffix (agyModelBaseID), so a roster entry can store either a
// base id ("gemini-3.1-pro") or a literal one carrying its own suffix
// ("gpt-oss-120b-medium") without false-flagging as unlisted. Mirrors
// unlistedCodexModels's contract for the Codex probe.
func agyModelsUnlisted(text string, known map[string]bool) string {
	var unlisted []string
	for _, m := range parseAgyModelsOutput(text) {
		if m.ID == "" {
			continue
		}
		lower := strings.ToLower(m.ID)
		if known[lower] {
			continue
		}
		if known[strings.ToLower(agyModelBaseID(m.ID))] {
			continue
		}
		unlisted = append(unlisted, m.ID)
	}
	sort.Strings(unlisted)
	return strings.Join(unlisted, ", ")
}

// probeAgyQuota is the agy-quota prober: Antigravity's `agy` CLI reports
// live rate-limit percentages for the google provider -- something
// gemini-local never had (Code Assist's retrieveUserQuota needs the
// CLI's own OAuth refresh flow, not available out-of-process) -- via
// `agy --print /quota --output-format text`, plus a model-id diff via
// `agy models`. gemini-local's own local auth/version detail
// (geminiLocalDetail) rides along in this probe's Detail too, so
// pointing the google provider's `probe:` at "agy-quota" in the roster
// loses none of gemini-local's information.
func probeAgyQuota(ctx context.Context, p Provider) Usage {
	u := Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}
	detail := geminiLocalDetail(ctx)

	res, err := execx.Run(ctx, "agy", []string{"--print", "/quota", "--output-format", "text"}, execx.RunOpts{Timeout: agyQuotaTimeout})
	if err != nil {
		u.Detail = detail
		if errors.Is(err, exec.ErrNotFound) {
			u.Err = "no key (agy CLI not installed)"
		} else {
			u.Err = "error: running agy --print /quota: " + err.Error()
		}
		return u
	}
	if res.TimedOut {
		u.Detail = detail
		u.Err = "error: agy --print /quota timed out"
		return u
	}

	rows := parseAgyQuotaText(string(res.Stdout))
	bars := mapAgyQuotaBars(rows)
	if len(bars) == 0 {
		u.Detail = detail
		u.Err = "no data"
		return u
	}
	u.Bars = bars

	known := p.KnownModelIDs()
	if modelsRes, merr := execx.Run(ctx, "agy", []string{"models"}, execx.RunOpts{Timeout: agyModelsTimeout}); merr == nil && !modelsRes.TimedOut {
		if unlisted := agyModelsUnlisted(string(modelsRes.Stdout), known); unlisted != "" {
			detail["unlisted_models"] = unlisted
		}
	}

	u.Detail = detail
	return u
}
