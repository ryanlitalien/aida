package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/models"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// newBurndownCmd builds the `aida burndown` command tree. Read-only for
// now: `capacity` is the only subcommand. The picker, scheduler, runners
// and ledger the design doc describes are not built -- see CLAUDE.md,
// "Burn-down capacity (aida burndown)".
func newBurndownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "burndown",
		Short: "Quota-aware burn-down: capacity view over the AI model roster",
		Long: "Read-only for now: `aida burndown capacity` shows how much of each\n" +
			"rate-limit window an unattended burn-down loop could safely spend\n" +
			"without eating into Ryan's own interactive reserve. Nothing under\n" +
			"this command dispatches, spawns an agent, or mutates a task -- the\n" +
			"picker and scheduler this feeds are not built yet.",
	}
	cmd.AddCommand(newBurndownCapacityCmd())
	return cmd
}

// burndownCapacityResponse is `aida burndown capacity --json`'s payload
// shape: every probed provider, plus its window-by-window Capacity rows.
// Mirrors modelsAPIResponse/modelsProviderOut's own provider-grouped
// shape (internal/cli/models.go) rather than a flat array, for the same
// reason that shape earns its keep there -- plan/account context next to
// each provider's rows.
type burndownCapacityResponse struct {
	GeneratedAt time.Time              `json:"generated_at"`
	At          time.Time              `json:"at"`
	Providers   []burndownProviderJSON `json:"providers"`
}

// burndownProviderJSON is one provider's plan/account plus its Capacity
// rows.
type burndownProviderJSON struct {
	Label      string              `json:"label"`
	Plan       string              `json:"plan,omitempty"`
	Account    string              `json:"account,omitempty"`
	Capacities []burndown.Capacity `json:"capacities"`
}

// burndownGroup is the in-process (pre-JSON) form of burndownProviderJSON
// -- built once by buildBurndownGroups and used by both the text and
// JSON renderers so they can never drift apart.
type burndownGroup struct {
	Label      string
	Plan       string
	Account    string
	Capacities []burndown.Capacity
}

// buildBurndownGroups pairs the roster's providers with their probed
// usage (index-aligned, the same contract models.ProbeWithOptions
// documents) and computes each provider's Capacity rows via
// burndown.Report.
func buildBurndownGroups(providers []models.Provider, usages []models.Usage, bcfg *burndown.Config, now time.Time) []burndownGroup {
	groups := make([]burndownGroup, 0, len(providers))
	for i, p := range providers {
		if i >= len(usages) {
			break
		}
		u := usages[i]

		plan := u.Plan
		if plan == "" {
			plan = p.Plan
		}
		account := u.Account
		if account == "" {
			account = p.Account
		}

		groups = append(groups, burndownGroup{
			Label:      p.Label,
			Plan:       plan,
			Account:    account,
			Capacities: burndown.Report([]models.Provider{p}, []models.Usage{u}, bcfg, now),
		})
	}
	return groups
}

// newBurndownCapacityCmd builds `aida burndown capacity`.
func newBurndownCapacityCmd() *cobra.Command {
	var jsonOut bool
	var fresh bool
	var at string

	cmd := &cobra.Command{
		Use:   "capacity",
		Short: "Show burn-down headroom per rate-limit window",
		Long: "Probes every AI provider's live usage (like `aida models`) and, for each\n" +
			"rate-limit window, applies the floors config at ~/.aida/burndown.yaml\n" +
			"(config.Config.BurndownPath) to report the effective floor and the\n" +
			"headroom above it a burn-down loop could spend right now. Read-only --\n" +
			"this never dispatches anything.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			r, err := models.Load(cfg.ModelsPath())
			if err != nil {
				return fmt.Errorf("loading models roster: %w", err)
			}

			bcfg, err := burndown.Load(cfg.BurndownPath())
			if err != nil {
				return fmt.Errorf("loading burndown config: %w", err)
			}

			now := time.Now()
			if at != "" {
				parsed, err := time.Parse(time.RFC3339, at)
				if err != nil {
					return fmt.Errorf("parsing --at %q (want RFC3339, e.g. 2026-09-15T02:00:00-04:00): %w", at, err)
				}
				now = parsed
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), modelsProbeTimeout)
			defer cancel()
			usages := models.ProbeWithOptions(ctx, r, models.ProbeOptions{Force: fresh})

			groups := buildBurndownGroups(r.Providers, usages, bcfg, now)

			if jsonOut {
				resp := burndownCapacityResponse{GeneratedAt: time.Now(), At: now}
				for _, g := range groups {
					resp.Providers = append(resp.Providers, burndownProviderJSON{
						Label: g.Label, Plan: g.Plan, Account: g.Account, Capacities: g.Capacities,
					})
				}
				data, err := json.MarshalIndent(resp, "", "  ")
				if err != nil {
					return fmt.Errorf("marshaling report: %w", err)
				}
				fmt.Println(string(data))
				return nil
			}

			printBurndownCapacityText(groups, now, bcfg, cfg.Location())
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the full per-provider Capacity payload as JSON")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "force a re-probe of every provider, ignoring the TTL cache (see aida models --fresh)")
	cmd.Flags().StringVar(&at, "at", "", "compute capacity as of this RFC3339 time instead of now (e.g. 2026-09-15T02:00:00-04:00)")
	return cmd
}

// ---- text rendering for `aida burndown capacity` (no --json) ----

// formatBurndownHeader renders the header line, e.g. "Burn-down capacity
// (as of 9:14pm EDT, daytime floors)".
func formatBurndownHeader(now time.Time, loc *time.Location, overnight bool) string {
	phase := "daytime floors"
	if overnight {
		phase = "overnight floors"
	}
	return fmt.Sprintf("Burn-down capacity  (as of %s, %s)", now.In(loc).Format("3:04pm MST"), phase)
}

// formatCapacityLine renders one Capacity row: the plain
// "X% left · floor Y% (source) · headroom Z%" form, or "no floor
// configured" in place of the floor/headroom fields when FloorSource is
// empty (see CapacityFor). Off-pace windows (hot/idle) get an annotation
// appended; a drain-to-zero window gets a DRAIN marker.
func formatCapacityLine(c burndown.Capacity) string {
	left := fmt.Sprintf("%3.0f%% left", c.LeftPct)

	var floorPart string
	if c.FloorSource == "" {
		floorPart = "no floor configured"
	} else {
		floorPart = fmt.Sprintf("floor %3.0f%% (%s)", c.Floor, c.FloorSource)
	}

	line := fmt.Sprintf("%-14s  %s  ·  %-24s", c.Label, left, floorPart)
	if c.FloorSource != "" {
		line += fmt.Sprintf("  ·  headroom %3.0f%%", c.Headroom)
	}

	var tags []string
	if c.DrainToZero {
		tags = append(tags, ui.WarnStyle.Render("DRAIN"))
	}
	if c.Pace != nil {
		switch c.Pace.Verdict {
		case models.PaceHot:
			tags = append(tags, ui.ErrorStyle.Render(fmt.Sprintf("HOT, running %.1fx", c.Pace.Ratio)))
		case models.PaceIdle:
			tags = append(tags, ui.SourceStyle.Render(fmt.Sprintf("IDLE, running %.1fx", c.Pace.Ratio)))
		}
	}
	if len(tags) > 0 {
		line += "   " + strings.Join(tags, "  ")
	}
	if c.Note != "" && c.Note != noFloorConfiguredNote {
		line += "  (" + c.Note + ")"
	}
	return line
}

// noFloorConfiguredNote mirrors internal/burndown's own unexported
// constant of the same value -- kept as a package-local copy here since
// that constant isn't exported, and this is the one place the CLI needs
// to distinguish "no floor" from a real note worth appending.
const noFloorConfiguredNote = "no floor configured"

// pluralSuffix returns "" for n==1, else "s".
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatBurndownSummary renders the trailing "Total headroom: ..." line:
// how many rows have any usable (strictly positive) headroom right now,
// and which rate-limit window has the most. A spend row's Headroom is a
// percent of its dollar budget, not a percent of a rate-limit quota, so
// it isn't comparable to a Bar row's -- an $8-of-$10 LiteLLM reserve
// would otherwise outrank a real multi-day quota window at a much
// smaller percentage. Spend rows still count toward the window tally;
// they're just excluded from the "largest" pick.
func formatBurndownSummary(groups []burndownGroup) string {
	var usable []burndown.Capacity
	var usableWindows []burndown.Capacity
	for _, g := range groups {
		for _, c := range g.Capacities {
			if c.Headroom <= 0 {
				continue
			}
			usable = append(usable, c)
			if !c.IsSpend {
				usableWindows = append(usableWindows, c)
			}
		}
	}
	if len(usable) == 0 {
		return "Total headroom: no windows with usable capacity right now."
	}
	if len(usableWindows) == 0 {
		return fmt.Sprintf("Total headroom: %d window%s with usable capacity, all spend rows (not ranked against quota windows)",
			len(usable), pluralSuffix(len(usable)))
	}
	sort.Slice(usableWindows, func(i, j int) bool { return usableWindows[i].Headroom > usableWindows[j].Headroom })
	top := usableWindows[0]
	return fmt.Sprintf("Total headroom: %d window%s with usable capacity, largest %s %s (%.0f%%)",
		len(usable), pluralSuffix(len(usable)), top.Provider, top.Label, top.Headroom)
}

// printBurndownCapacityText prints the full table: a header line, each
// provider's rows, and the trailing summary line.
func printBurndownCapacityText(groups []burndownGroup, now time.Time, bcfg *burndown.Config, loc *time.Location) {
	overnight := false
	if bcfg != nil {
		overnight = burndown.IsOvernight(bcfg.Overnight, now)
	}
	fmt.Println(formatBurndownHeader(now, loc, overnight))
	fmt.Println()

	for _, g := range groups {
		if len(g.Capacities) == 0 {
			continue
		}
		header := g.Label
		if g.Plan != "" {
			header += "  " + models.RoundPricesForDisplay(g.Plan)
		}
		fmt.Println(header)
		for _, c := range g.Capacities {
			fmt.Println("  " + formatCapacityLine(c))
		}
		fmt.Println()
	}

	fmt.Println(formatBurndownSummary(groups))
}
