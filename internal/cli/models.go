package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/models"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// modelsProbeTimeout bounds the whole `aida models` probe round. Every
// provider probe runs in its own goroutine (models.Probe fans them out
// in parallel), each already bounded to its own ceiling
// (models.probeTimeoutFor -- 8s for most probe kinds, longer for
// "agy-quota", whose `agy --print /quota` + `agy models` calls run
// sequentially and can take the better part of a minute), so this only
// needs to exceed the SLOWEST individual probe, not their sum.
const modelsProbeTimeout = 110 * time.Second

// modelsAPIResponse is the shared payload shape for `aida models --json`
// and GET /api/models: the roster plus each provider's live Usage,
// side-by-side. Both call sites build it via buildModelsResponse so the
// CLI and the HTTP route can never drift apart.
type modelsAPIResponse struct {
	Updated     string              `json:"updated"`
	GeneratedAt time.Time           `json:"generated_at"`
	Guidance    []string            `json:"guidance,omitempty"`
	Providers   []modelsProviderOut `json:"providers"`
}

// modelsProviderOut is one roster provider plus its probed Usage.
type modelsProviderOut struct {
	models.Provider
	Usage models.Usage `json:"usage"`
}

// buildModelsResponse zips a loaded Roster with its Probe results
// (index-aligned, per models.Probe's documented contract) into the
// shared API payload.
func buildModelsResponse(r *models.Roster, usages []models.Usage) modelsAPIResponse {
	providers := make([]modelsProviderOut, len(r.Providers))
	for i, p := range r.Providers {
		out := modelsProviderOut{Provider: p}
		if i < len(usages) {
			out.Usage = usages[i]
		}
		providers[i] = out
	}
	return modelsAPIResponse{
		Updated:     r.Updated,
		GeneratedAt: time.Now(),
		Guidance:    r.Guidance,
		Providers:   providers,
	}
}

// newModelsCmd builds the `aida models` command tree: a read-only view
// over ~/.aida/models.yaml (config.Config.ModelsPath) plus each
// provider's live usage. There is no `models add`/`models edit` --
// hand-edit the roster YAML, same convention as `aida roster`.
func newModelsCmd() *cobra.Command {
	var jsonOut bool
	var noProbe bool
	var fresh bool

	cmd := &cobra.Command{
		Use:   "models",
		Short: "Show AI model plans, nicknames, and live usage",
		Long: "Reads ~/.aida/models.yaml (the AI provider/plan/nickname roster) and,\n" +
			"unless --no-probe is set, probes each provider's local credential/usage\n" +
			"state (keychain, Codex session logs, the LiteLLM proxy, ...) to show\n" +
			"live rate-limit bars and spend.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			r, err := models.Load(cfg.ModelsPath())
			if err != nil {
				return fmt.Errorf("loading models roster: %w", err)
			}

			usages := make([]models.Usage, len(r.Providers))
			if !noProbe {
				ctx, cancel := context.WithTimeout(cmd.Context(), modelsProbeTimeout)
				defer cancel()
				usages = models.ProbeWithOptions(ctx, r, models.ProbeOptions{Force: fresh})
			}

			resp := buildModelsResponse(r, usages)

			if jsonOut {
				data, err := json.MarshalIndent(resp, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling response: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			printModelsText(resp, cfg.Location())
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the full roster + usage payload as JSON")
	cmd.Flags().BoolVar(&noProbe, "no-probe", false, "skip live probing (roster info only, no network/ssh/keychain)")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "force a re-probe of every provider, ignoring each one's per-provider TTL cache (see internal/models.ProbeOptions.Force)")

	cmd.AddCommand(newModelsResolveCmd())
	return cmd
}

// newModelsResolveCmd builds `aida models resolve <nickname>`.
func newModelsResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <nickname>",
		Short: "Resolve a spoken/typed model nickname to its provider(s) and id",
		Long: "Case-insensitive, punctuation-tolerant lookup over every model's id and\n" +
			"nicknames in ~/.aida/models.yaml. The same nickname (\"sonnet\", \"opus\")\n" +
			"is often reused across several providers/accounts, so more than one\n" +
			"match is normal, not an error.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			r, err := models.Load(cfg.ModelsPath())
			if err != nil {
				return fmt.Errorf("loading models roster: %w", err)
			}

			matches := r.Resolve(args[0])
			if len(matches) == 0 {
				return fmt.Errorf("no match; add it to ~/.aida/models.yaml")
			}
			for _, m := range matches {
				fmt.Println(formatModelsMatch(m))
			}
			return nil
		},
	}
}

// formatModelsMatch renders one Resolve match as
// "<provider>  <id>  (<tier>) <note>", omitting the trailing parenthetical
// entirely when the model carries neither a tier nor a note.
func formatModelsMatch(m models.Match) string {
	line := fmt.Sprintf("%s  %s", m.Provider, m.ModelID)
	tierPart := ""
	if m.Tier != "" {
		tierPart = "(" + m.Tier + ")"
	}
	extra := strings.TrimSpace(tierPart + " " + m.Note)
	if extra != "" {
		line += "  " + extra
	}
	return line
}

// ---- text rendering for `aida models` (no --json) ----

// printModelsText prints the compact per-provider table: a header line,
// any live bars, any live spend rows, a compact models list, an
// unlisted-models warning when present, and a usage error line when the
// probe didn't produce data. loc renders every timestamp in local time
// (cfg.Location(), which itself falls back to time.Local when no
// timezone is configured).
func printModelsText(resp modelsAPIResponse, loc *time.Location) {
	if strip := paceExceptionStrip(resp.Providers); strip != "" {
		fmt.Println(strip)
		fmt.Println()
	}

	for _, p := range resp.Providers {
		// A live probe's own resolved plan (e.g. Codex's JWT-derived
		// chatgpt_plan_type, which already overwrites Usage.Plan in
		// probeCodexSessions) wins over the roster's static description;
		// fall back to the roster's when the probe never ran or found
		// nothing to override it with.
		plan := p.Usage.Plan
		if plan == "" {
			plan = p.Plan
		}

		header := p.Label
		if plan != "" {
			header += "  plan: " + models.RoundPricesForDisplay(plan)
		}
		if p.Account != "" {
			header += "  account: " + p.Account
		}
		fmt.Println(header)

		for _, b := range p.Usage.Bars {
			fmt.Printf("  %-14s %s\n", b.Label, formatBarLine(b, loc))
		}
		for _, s := range p.Usage.Spend {
			fmt.Printf("  %-14s %s\n", s.Key, formatSpendLine(s, loc))
		}
		if len(p.Models) > 0 {
			fmt.Printf("  models: %s\n", formatModelsList(p.Models))
		}
		if unlisted, ok := p.Usage.Detail["unlisted_models"]; ok && unlisted != "" {
			fmt.Printf("  unlisted locally, not in roster: %s\n", unlisted)
		}
		if rcStr, ok := p.Usage.Detail["reset_credits"]; ok {
			if rc, err := strconv.Atoi(rcStr); err == nil && rc > 0 {
				fmt.Printf("  reset credits: %d free full resets available\n", rc)
			}
		}
		// A Warn line (last-good fallback, see internal/models/probe_cache.go)
		// replaces the Err line entirely -- Err is always cleared whenever
		// Warn is set, so this is never both.
		if p.Usage.Warn != "" {
			fmt.Printf("  usage: %s\n", p.Usage.Warn)
		} else if p.Usage.Err != "" {
			fmt.Printf("  usage: %s\n", p.Usage.Err)
		}
		fmt.Println()
	}

	if len(resp.Guidance) > 0 {
		fmt.Println("guidance:")
		for _, g := range resp.Guidance {
			fmt.Printf("  - %s\n", g)
		}
	}
}

// pacedPace returns b's Pace when it carries a verdict worth printing --
// nil (unknown/short window, no reset time) and "early" (too little of
// the window has elapsed yet) both fall back to the plain used/left
// rendering every unpaced row already had, mirroring dashboard_web.html's
// pacedPace.
func pacedPace(p *models.Pace) *models.Pace {
	if p == nil || p.Verdict == models.PaceEarly {
		return nil
	}
	return p
}

// styledPaceChip colors chip (from models.FormatPaceChip) by verdict
// using this package's existing lipgloss conventions (internal/ui):
// ui.ErrorStyle for hot, ui.SourceStyle -- already the "cool/blue
// accent" used elsewhere in this CLI -- for idle, and ui.DimStyle
// (faint) for on-pace, since being on pace isn't something to flag.
func styledPaceChip(verdict, chip string) string {
	switch verdict {
	case models.PaceHot:
		return ui.ErrorStyle.Render(chip)
	case models.PaceIdle:
		return ui.SourceStyle.Render(chip)
	case models.PaceOnPace:
		return ui.DimStyle.Render(chip)
	default:
		return chip
	}
}

// formatBarLine renders one rate-limit bar's line: the original plain
// "X% used  (Y% left)  resets ..." for an unpaced bar, unchanged, or --
// once it carries a real pace verdict -- "X% used  ·  <chip>  ·  <detail>
// ·  resets ...", dropping the redundant "% left".
func formatBarLine(b models.Bar, loc *time.Location) string {
	reset := formatResetsAt(b.ResetsAt, loc)
	pace := pacedPace(b.Pace)
	if pace == nil {
		return fmt.Sprintf("%3.0f%% used  (%3.0f%% left)  %s", b.Percent, b.Left, reset)
	}
	chip := styledPaceChip(pace.Verdict, models.FormatPaceChip(pace))
	line := fmt.Sprintf("%3.0f%% used  ·  %s", b.Percent, chip)
	if pace.Detail != "" {
		line += "  ·  " + pace.Detail
	}
	return line + "  ·  " + reset
}

// formatSpendLine mirrors formatBarLine for a LiteLLM budget row.
func formatSpendLine(s models.Spend, loc *time.Location) string {
	amounts := models.RoundPricesForDisplay(fmt.Sprintf("$%.2f / $%.2f", s.Spend, s.Budget))
	reset := formatResetsDate(s.ResetsAt, loc)
	pace := pacedPace(s.Pace)
	if pace == nil {
		return fmt.Sprintf("%s  %s", amounts, reset)
	}
	chip := styledPaceChip(pace.Verdict, models.FormatPaceChip(pace))
	line := fmt.Sprintf("%s  ·  %s", amounts, chip)
	if pace.Detail != "" {
		line += "  ·  " + pace.Detail
	}
	if reset != "" {
		line += "  ·  " + reset
	}
	return line
}

// collectPaceExceptions walks every provider's bars and spend rows and
// buckets the off-pace ones (hot/idle) by verdict, each entry prefixed
// with its provider's label so the strip is unambiguous across
// providers that share a bar label (e.g. two accounts both having a
// "7-day" bar). Mirrors dashboard_web.html's collectPaceExceptions.
func collectPaceExceptions(providers []modelsProviderOut) (hot, idle []string) {
	for _, p := range providers {
		for _, b := range p.Usage.Bars {
			if b.Pace == nil {
				continue
			}
			switch b.Pace.Verdict {
			case models.PaceHot:
				hot = append(hot, p.Label+" "+b.Label)
			case models.PaceIdle:
				idle = append(idle, p.Label+" "+b.Label)
			}
		}
		for _, s := range p.Usage.Spend {
			if s.Pace == nil {
				continue
			}
			switch s.Pace.Verdict {
			case models.PaceHot:
				hot = append(hot, p.Label+" "+s.Key)
			case models.PaceIdle:
				idle = append(idle, p.Label+" "+s.Key)
			}
		}
	}
	return hot, idle
}

// paceExceptionStrip renders the one-line "HOT: ... · IDLE: ..." summary
// printed under `aida models`' header, or "" when every paced row is on
// pace (nothing to flag).
func paceExceptionStrip(providers []modelsProviderOut) string {
	hot, idle := collectPaceExceptions(providers)
	if len(hot) == 0 && len(idle) == 0 {
		return ""
	}
	var parts []string
	if len(hot) > 0 {
		parts = append(parts, ui.ErrorStyle.Render("HOT:")+" "+strings.Join(hot, ", "))
	}
	if len(idle) > 0 {
		parts = append(parts, ui.SourceStyle.Render("IDLE:")+" "+strings.Join(idle, ", "))
	}
	return strings.Join(parts, "  ·  ")
}

// formatResetsAt renders a rate-limit bar's reset time as a local clock
// time plus a relative hint from formatRelative, e.g.
// "resets 1:10pm (in 2h13m)" or, past 23h, "resets 4:00pm (in 6d 22h)".
// A zero time (no reset info from the probe) renders as "resets ?".
func formatResetsAt(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "resets ?"
	}
	local := t.In(loc)
	return fmt.Sprintf("resets %s (%s)", local.Format("3:04pm"), formatRelative(time.Until(t)))
}

// formatResetsDate renders a budget row's reset time as a short local
// date, e.g. "resets Oct 1". A zero time renders as "" (the CLI table
// just omits the trailing field rather than printing "resets ?" on
// every spend row -- LiteLLM budgets don't always carry a reset date).
func formatResetsDate(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return ""
	}
	return "resets " + t.In(loc).Format("Jan 2")
}

// formatRelative renders a duration as a compact "in <duration>" hint via
// models.FormatDurationHuman ("in 42m", "in 3h15m", "in 6d 22h" past 23h);
// a negative duration (a reset time already in the past, e.g. a stale
// probe) renders as "past due" rather than a confusing "in -5m".
func formatRelative(d time.Duration) string {
	if d < 0 {
		return "past due"
	}
	return "in " + models.FormatDurationHuman(d)
}

// formatModelsList renders a provider's model list as
// "<nickname> (<id>), <nickname2> (<id2>), ..." using each model's first
// nickname (falling back to its id when it has none).
func formatModelsList(ms []models.Model) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		label := m.ID
		if len(m.Nicknames) > 0 {
			label = m.Nicknames[0]
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", label, m.ID))
	}
	return strings.Join(parts, ", ")
}
