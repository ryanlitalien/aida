package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile [name]",
		Short: "Show or switch the active profile",
		Long:  "Without arguments, shows the active profile and its configuration. With a name argument, switches the active profile.",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runProfile,
	}
	cmd.AddCommand(newProfileScanCmd())
	return cmd
}

func newProfileScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Detect tools on PATH and update the active profile",
		Long:  "Scans PATH for known tools and adds any new ones to the active profile's tools map. Existing entries are never overwritten.",
		Args:  cobra.NoArgs,
		RunE:  runProfileScan,
	}
}

func runProfileScan(_ *cobra.Command, _ []string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	profile, profileName := cfg.ActiveProfileConfig()
	if profile == nil {
		return fmt.Errorf("no active profile found")
	}

	detected := detectToolsOnPath()

	// Show what was found
	fmt.Printf("%s Profile: %s\n", ui.ProfileIcon, ui.HeaderStyle.Render(profileName))
	fmt.Println()
	for _, tool := range config.KnownToolNames() {
		if path, ok := detected[tool]; ok {
			existing := ""
			if profile.Tools != nil {
				if _, has := profile.Tools[tool]; has {
					existing = ui.DimStyle.Render(" (already registered)")
				}
			}
			fmt.Printf("  %s %s  %s%s\n", ui.SuccessIcon, tool, ui.DimStyle.Render(path), existing)
		} else {
			fmt.Printf("  %s %s  %s\n", ui.DimStyle.Render("·"), tool, ui.DimStyle.Render("not found"))
		}
	}

	added := writeDetectedTools(cfg, detected)
	fmt.Println()
	if added > 0 {
		fmt.Printf("%s Added %d new tool(s) to profile %q\n", ui.SuccessIcon, added, profileName)
	} else {
		fmt.Printf("%s All detected tools already registered\n", ui.SuccessIcon)
	}
	return nil
}

func runProfile(_ *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Switch profile if a name was provided.
	if len(args) == 1 {
		name := args[0]
		if _, ok := cfg.Profiles[name]; !ok && name != "auto" {
			available := make([]string, 0, len(cfg.Profiles))
			for k := range cfg.Profiles {
				available = append(available, k)
			}
			return fmt.Errorf("unknown profile %q (available: %s)", name, strings.Join(available, ", "))
		}
		if dryRunGuard("switch profile", "to "+name) {
			return nil
		}
		cfg.ActiveProfile = name
		if err := config.SaveConfig(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("%s Switched active profile to %q\n", ui.SuccessIcon, name)
		return nil
	}

	// Show current profile information.
	profile, profileName := cfg.ActiveProfileConfig()
	fmt.Printf("%s %s %s",
		ui.ProfileIcon,
		ui.LabelStyle.Render("Active profile:"),
		ui.HeaderStyle.Render(profileName),
	)
	if env := os.Getenv("AIDA_PROFILE"); env != "" && env == profileName {
		fmt.Printf(" %s", ui.DimStyle.Render("(via AIDA_PROFILE)"))
	} else if cfg.ActiveProfile == "auto" {
		fmt.Printf(" %s", ui.DimStyle.Render("(auto-detected)"))
	}
	fmt.Println()

	if profile == nil {
		fmt.Printf("  %s No profile resolved.\n", ui.WarnIcon)
		return nil
	}

	// Detection rules
	if profile.Detect.HasTool != "" {
		fmt.Printf("  Detect:     has_tool=%s\n", profile.Detect.HasTool)
	}
	if profile.Detect.MissingTool != "" {
		fmt.Printf("  Detect:     missing_tool=%s\n", profile.Detect.MissingTool)
	}

	// Scan paths
	if len(profile.ScanPaths) > 0 {
		fmt.Printf("  Scan paths: %s\n", strings.Join(profile.ScanPaths, ", "))
	}

	// Tools
	if len(profile.Tools) > 0 {
		fmt.Println("  Tools:")
		for k, v := range profile.Tools {
			fmt.Printf("    %s = %s\n", k, v)
		}
	}

	// List all available profiles
	fmt.Println()
	fmt.Println(ui.LabelStyle.Render("All profiles:"))
	for name := range cfg.Profiles {
		marker := "  "
		if name == profileName {
			marker = "> "
		}
		fmt.Printf("  %s%s\n", marker, name)
	}

	return nil
}
