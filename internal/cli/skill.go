package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/engine"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage and run library skills",
		Long:  "Skills are reusable procedures (SKILL.md + helper scripts) in the library.",
	}

	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillRunCmd())
	cmd.AddCommand(newSkillShowCmd())

	return cmd
}

func newSkillListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := library.LoadRegistry(config.Dir())
			if err != nil {
				return err
			}

			if len(reg.Skills) == 0 {
				fmt.Println("No skills found in library.")
				fmt.Println("Skills are directories in ~/.aida/library/<root>/skills/ containing a SKILL.md file.")
				return nil
			}

			fmt.Printf("Available skills (%d):\n\n", len(reg.Skills))
			for name, skill := range reg.Skills {
				status := "available"
				if !skill.Available {
					status = fmt.Sprintf("unavailable (missing: %s)", strings.Join(skill.MissingTools, ", "))
				}
				fmt.Printf("  %-24s %s  [%s]\n", name, skill.AbsDir, status)
			}
			return nil
		},
	}
}

func newSkillRunCmd() *cobra.Command {
	var inputFlags []string
	cmd := &cobra.Command{
		Use:   "run [skill-name]",
		Short: "Execute a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			skillName := args[0]

			reg, err := library.LoadRegistry(config.Dir())
			if err != nil {
				return err
			}

			skill, ok := reg.Skills[skillName]
			if !ok {
				return fmt.Errorf("skill %q not found. Run 'aida skill list' to see available skills", skillName)
			}
			if !skill.Available {
				return fmt.Errorf("skill %q is unavailable (missing tools: %s)", skillName, strings.Join(skill.MissingTools, ", "))
			}

			// Parse inputs from --input key=value flags
			inputs := make(map[string]string)
			for _, kv := range inputFlags {
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) == 2 {
					inputs[parts[0]] = parts[1]
				}
			}

			cfg, _ := config.LoadConfig()
			apiKey := cfg.GetAPIKey()
			model := cfg.Model.Primary
			isOffline := cfg.Model.OfflineMode
			if isOffline {
				model = cfg.Model.Fallback
			}
			client := llm.NewClient(apiKey, model, isOffline)

			spinner := ui.NewSpinner()
			spinner.Start(fmt.Sprintf("Running skill %s...", skillName))
			result, err := engine.ExecuteSkill(context.Background(), client, skill.AbsDir, inputs)
			if result != nil {
				spinner.Stop(fmt.Sprintf("%s Skill %s (%s, %s)", ui.SuccessIcon, skillName, result.Status, result.Duration.Round(time.Millisecond)))
			} else {
				spinner.Stop(fmt.Sprintf("%s Skill failed", ui.WarnIcon))
			}
			if err != nil {
				return err
			}

			fmt.Println()
			fmt.Println(result.Output)
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&inputFlags, "input", nil, "skill input as key=value (repeatable)")
	return cmd
}

func newSkillShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [skill-name]",
		Short: "Show a skill's procedure (SKILL.md)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := library.LoadRegistry(config.Dir())
			if err != nil {
				return err
			}

			skill, ok := reg.Skills[args[0]]
			if !ok {
				return fmt.Errorf("skill %q not found", args[0])
			}

			data, err := os.ReadFile(filepath.Join(skill.AbsDir, "SKILL.md"))
			if err != nil {
				return fmt.Errorf("reading SKILL.md: %w", err)
			}
			fmt.Println(string(data))
			return nil
		},
	}
}
