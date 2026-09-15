package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/roster"
	"github.com/spf13/cobra"
)

// newRosterCmd builds the `aida roster` command tree: read-only inspection
// of the agent roster (internal/roster). Mutating it is done by hand-
// editing ~/.aida/roster.yaml; there is no `roster add`/`roster edit`.
func newRosterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "roster",
		Short: "Inspect the agent roster",
	}
	cmd.AddCommand(newRosterListCmd())
	cmd.AddCommand(newRosterLintCmd())
	return cmd
}

// newRosterListCmd builds `aida roster list`.
func newRosterListCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List roster entries",
		Long: "Prints the active profile's roster entries (call-sign, kind, profiles,\n" +
			"description). --all shows every profile's entries instead.",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			_, profileName := cfg.ActiveProfileConfig()

			r, err := roster.Load(config.Dir(), profileName)
			if err != nil {
				return fmt.Errorf("loading roster: %w", err)
			}

			entries := r.Entries()
			if all {
				entries = r.AllEntries()
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "CALL-SIGN\tKIND\tPROFILES\tDESCRIPTION")
			for _, e := range entries {
				profiles := "all"
				if len(e.Profiles) > 0 {
					profiles = strings.Join(e.Profiles, ",")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Display(), e.Kind, profiles, truncateDescription(e.Description))
			}
			w.Flush()

			if issues := r.Issues(); len(issues) > 0 {
				fmt.Fprintln(os.Stderr, "\nIssues:")
				for _, issue := range issues {
					fmt.Fprintln(os.Stderr, "  "+issue)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "show every profile's entries")
	return cmd
}

// newRosterLintCmd builds `aida roster lint`.
func newRosterLintCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lint",
		Short: "Validate roster.yaml and report problems",
		Long: "Checks ~/.aida/roster.yaml for problems Load() tolerates or\n" +
			"silently drops into its own issue list: invalid kinds, missing or\n" +
			"doubled-up backend spec blocks, malformed slugs, unknown library\n" +
			"sources, colliding call-signs/aliases, and the reserved \"aida\"\n" +
			"entry. Exits non-zero if any issue has severity \"error\".",
		RunE: func(_ *cobra.Command, _ []string) error {
			// Best-effort: a registry load failure shouldn't stop the lint
			// pass, it just means source-name issues can't be checked.
			var availableSources []string
			if reg, err := library.LoadRegistry(config.Dir()); err == nil {
				availableSources = reg.AvailableSources()
			}

			issues, err := roster.Lint(config.Dir(), availableSources)
			if err != nil {
				return fmt.Errorf("linting roster: %w", err)
			}
			if len(issues) == 0 {
				fmt.Println("roster.yaml: no issues found")
				return nil
			}

			for _, issue := range issues {
				fmt.Printf("%s  %s: %s\n", strings.ToUpper(issue.Severity), issue.Entry, issue.Message)
			}
			if roster.HasError(issues) {
				return fmt.Errorf("roster lint found %d issue(s)", len(issues))
			}
			return nil
		},
	}
	return cmd
}

// descriptionMaxLen caps a roster description at roughly one line so the
// table stays readable.
const descriptionMaxLen = 60

// truncateDescription collapses a (possibly multi-line) description to a
// single line and truncates it to descriptionMaxLen characters.
func truncateDescription(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= descriptionMaxLen {
		return s
	}
	return strings.TrimSpace(s[:descriptionMaxLen-3]) + "..."
}
