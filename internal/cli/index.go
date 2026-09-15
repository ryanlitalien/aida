package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/index"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

var (
	addPath     string
	generateIdx bool
	indexForce  bool
)

func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Scan and index local codebases",
		Long:  "Walks scan_paths from the active profile, finds directories with CLAUDE.md or README.md, and optionally classifies them via LLM.",
		Args:  cobra.NoArgs,
		RunE:  runIndex,
	}

	cmd.Flags().StringVar(&addPath, "add", "", "add a single directory path to the scan list")
	cmd.Flags().BoolVar(&generateIdx, "generate", false, "use LLM to classify scanned directories and write to library")
	cmd.Flags().BoolVar(&indexForce, "force", false, "overwrite manually-set entities/capabilities/search/description fields (default: merge, preserve manual edits)")

	return cmd
}

func runIndex(_ *cobra.Command, _ []string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	profile, profileName := cfg.ActiveProfileConfig()
	if profile == nil {
		return fmt.Errorf("no active profile found")
	}

	ui.PrintProfile(profileName, 0)

	// If --add flag is provided, just add the path to scan_paths and save.
	if addPath != "" {
		expanded := config.ExpandPath(addPath)
		if dryRunGuard("add scan path", expanded) {
			return nil
		}
		profile.ScanPaths = append(profile.ScanPaths, expanded)
		cfg.Profiles[profileName] = *profile
		if err := config.SaveConfig(cfg); err != nil {
			return fmt.Errorf("saving config: %w", err)
		}
		fmt.Printf("%s Added %s to %s scan_paths\n", ui.SuccessIcon, expanded, profileName)
		return nil
	}

	// Expand scan paths
	paths := make([]string, 0, len(profile.ScanPaths))
	for _, p := range profile.ScanPaths {
		paths = append(paths, config.ExpandPath(p))
	}

	if len(paths) == 0 {
		fmt.Printf("%s No scan_paths configured for profile %q\n", ui.WarnIcon, profileName)
		fmt.Println("  Add paths with: aida index --add ~/dev/myproject")
		return nil
	}

	fmt.Printf("Scanning %d paths...\n", len(paths))

	folders, err := index.ScanPaths(paths)
	if err != nil {
		return fmt.Errorf("scanning paths: %w", err)
	}

	if len(folders) == 0 {
		fmt.Printf("%s No directories with CLAUDE.md or README.md found.\n", ui.WarnIcon)
		return nil
	}

	fmt.Printf("\n%s Found %d indexed directories:\n\n", ui.SuccessIcon, len(folders))
	for _, f := range folders {
		doc := ui.DimStyle.Render("[" + f.DocFile + "]")
		fmt.Printf("  %s %s\n", doc, f.Path)
	}
	fmt.Println()

	if !generateIdx {
		return nil
	}

	// LLM-based classification
	apiKey := cfg.GetAPIKey()
	if apiKey == "" {
		return fmt.Errorf("no API key configured; set ANTHROPIC_API_KEY or run aida init")
	}
	client := llm.NewClient(apiKey, cfg.Model.Primary, offline)
	ctx := context.Background()

	fmt.Printf("Classifying %d directories via LLM...\n\n", len(folders))

	var discoveries []library.DiscoveredSource
	for _, f := range folders {
		gen := index.GenerateSourceEntryLLM(ctx, client, f)
		capsStr := ui.DimStyle.Render("[" + joinCaps(gen.Capabilities) + "]")
		fmt.Printf("  %s %s → %s %s\n", ui.SuccessIcon, filepath.Base(f.Path), gen.Type, capsStr)

		discoveries = append(discoveries, library.DiscoveredSource{
			Name:         gen.Name,
			Path:         gen.Path,
			Type:         gen.Type,
			Kind:         kindFromType(gen.Type),
			Description:  gen.Description,
			Capabilities: gen.Capabilities,
			ContextFile:  gen.Context,
		})
	}

	rootPath := config.ExpandPath("~/.aida/library")
	if dryRunGuard("write discovered sources", fmt.Sprintf("%d source(s) to %s", len(discoveries), rootPath)) {
		return nil
	}

	// Assign the active profile to newly discovered sources.
	if cfg, cfgErr := config.LoadConfig(); cfgErr == nil {
		if _, pName := cfg.ActiveProfileConfig(); pName != "" {
			for i := range discoveries {
				if len(discoveries[i].Profiles) == 0 {
					discoveries[i].Profiles = []string{pName}
				}
			}
		}
	}

	reports, n, err := library.WriteDiscoveredMerge(rootPath, discoveries, indexForce)
	if err != nil {
		return fmt.Errorf("writing to library: %w", err)
	}
	printIndexReport(reports, n, indexForce)
	return nil
}

// printIndexReport renders the end-of-scan change table so Ryan can see
// which source YAMLs were created, updated (with which fields), or left
// alone. Shown after every `aida index --generate` run.
func printIndexReport(reports []library.WriteReport, total int, force bool) {
	fmt.Println()
	if force {
		fmt.Printf("%s aida index (force mode): %d source(s) rewritten in library.\n", ui.SuccessIcon, total)
	} else {
		fmt.Printf("%s aida index (merge mode): %d source(s) processed in library.\n", ui.SuccessIcon, total)
	}
	if len(reports) == 0 {
		return
	}
	var creates, updates, skips int
	for _, r := range reports {
		switch r.Action {
		case library.WriteActionCreate:
			creates++
		case library.WriteActionUpdate:
			updates++
		case library.WriteActionSkip:
			skips++
		}
	}
	fmt.Printf("   %d created  %d updated  %d unchanged\n\n", creates, updates, skips)

	const nameW = 32
	fmt.Printf("  %-*s  %-7s  %s\n", nameW, "source", "action", "fields changed")
	fmt.Printf("  %s  %s  %s\n", repeat('-', nameW), repeat('-', 7), repeat('-', 20))
	for _, r := range reports {
		fields := "-"
		if len(r.FieldsChanged) > 0 {
			fields = strings.Join(r.FieldsChanged, ", ")
		}
		name := r.Name
		if len(name) > nameW {
			name = name[:nameW-1] + "…"
		}
		fmt.Printf("  %-*s  %-7s  %s\n", nameW, name, r.Action, fields)
	}
}

func repeat(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func joinCaps(caps []string) string {
	if len(caps) == 0 {
		return "none"
	}
	result := caps[0]
	for _, c := range caps[1:] {
		result += ", " + c
	}
	return result
}

func kindFromType(t string) string {
	switch t {
	case "data-source":
		return "data"
	case "docs":
		return "docs"
	default:
		return "code"
	}
}
