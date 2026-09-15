package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newLibraryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Manage aida intelligence libraries",
		Long: "A library is a folder of layered context, reusable skills, and source-adapter configs.\n" +
			"Libraries are git-backed and live under ~/.aida/. Multiple libraries (defaults,\n" +
			"personal, work) can be aggregated; missing optional libraries are silently skipped.",
	}
	cmd.AddCommand(newLibraryInitCmd())
	cmd.AddCommand(newLibraryImportCmd())
	cmd.AddCommand(newLibraryListCmd())
	cmd.AddCommand(newLibraryDoctorCmd())
	cmd.AddCommand(newLibraryAddCmd())
	cmd.AddCommand(newLibrarySyncCmd())
	cmd.AddCommand(newLibraryScanCmd())
	return cmd
}

func newLibraryScanCmd() *cobra.Command {
	var paths []string
	var write bool
	var includeCode bool
	var includeDocs bool
	var rootPath string
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Walk directories and auto-discover source candidates",
		Long: "Classifies child folders of each path as data/docs/code sources based\n" +
			"on file contents (CSV, SQL, markdown, code). By default only data\n" +
			"sources are written; use --include-docs / --include-code to opt in.\n" +
			"Without --write, prints a dry-run preview.",
		RunE: func(_ *cobra.Command, _ []string) error {
			if len(paths) == 0 {
				paths = defaultScanPaths()
			}
			if rootPath == "" {
				rootPath = filepath.Join(config.Dir(), "library")
			}

			discovered, err := library.ScanForSources(paths)
			if err != nil {
				return err
			}

			// Filter by kind flags.
			var kept []library.DiscoveredSource
			var skipped int
			for _, d := range discovered {
				switch d.Kind {
				case "data":
					kept = append(kept, d)
				case "docs":
					if includeDocs {
						kept = append(kept, d)
					} else {
						skipped++
					}
				case "code":
					if includeCode {
						kept = append(kept, d)
					} else {
						skipped++
					}
				}
			}

			fmt.Printf("Scanned %d path(s), found %d candidate(s) (%d filtered out).\n\n",
				len(paths), len(kept), skipped)
			for _, d := range kept {
				fmt.Printf("  %s  %-12s  %s\n", ui.SuccessIcon, d.Type, d.Path)
				if d.Description != "" {
					fmt.Printf("       %s\n", truncateRunSummary(d.Description, 100))
				}
				if len(d.Capabilities) > 0 {
					fmt.Printf("       capabilities: %s\n", strings.Join(d.Capabilities, ", "))
				}
				if len(d.Entities) > 0 {
					fmt.Printf("       entities:     %s\n", strings.Join(d.Entities, ", "))
				}
				if len(d.Assets) > 0 {
					var assets []string
					for k := range d.Assets {
						assets = append(assets, k)
					}
					fmt.Printf("       assets:       %s\n", strings.Join(assets, ", "))
				}
			}

			if !write {
				if len(kept) > 0 {
					fmt.Printf("\nRun `aida library scan --write` to import them into %s.\n", rootPath)
				}
				return nil
			}

			if dryRunGuard("write discovered sources", fmt.Sprintf("%d source(s) to %s", len(kept), rootPath)) {
				return nil
			}

			// Assign the active profile so generated YAML files are
			// profile-scoped and don't leak across machines.
			if cfg, cfgErr := config.LoadConfig(); cfgErr == nil {
				if _, pName := cfg.ActiveProfileConfig(); pName != "" {
					for i := range kept {
						if len(kept[i].Profiles) == 0 {
							kept[i].Profiles = []string{pName}
						}
					}
				}
			}

			n, err := library.WriteDiscovered(rootPath, kept)
			if err != nil {
				return err
			}
			fmt.Printf("\n%s Wrote %d source(s) into %s\n", ui.SuccessIcon, n, rootPath)
			fmt.Println("  Inspect with: aida library list")
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&paths, "path", nil, "directory to scan (repeatable; default: ~/dev + active profile scan_paths)")
	cmd.Flags().BoolVar(&write, "write", false, "actually write discoveries to the library (default: dry run)")
	cmd.Flags().BoolVar(&includeCode, "include-code", false, "also import pure codebase folders")
	cmd.Flags().BoolVar(&includeDocs, "include-docs", false, "also import documentation folders")
	cmd.Flags().StringVar(&rootPath, "root", "", "library root to write into (default: ~/.aida/library)")
	return cmd
}

// defaultScanPaths returns the set of directories auto-scanned when the
// user runs `aida library scan` without --path. We start from the active
// profile's scan_paths (if any) and always add ~/dev because it's the
// overwhelmingly common location for personal projects and data.
func defaultScanPaths() []string {
	paths := []string{"~/dev"}
	cfg, err := config.LoadConfig()
	if err == nil {
		if p, _ := cfg.ActiveProfileConfig(); p != nil {
			for _, sp := range p.ScanPaths {
				if sp != "" && !sliceContains(paths, sp) {
					paths = append(paths, sp)
				}
			}
		}
	}
	return paths
}

func sliceContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func newLibraryAddCmd() *cobra.Command {
	var name string
	var owner string
	var optional bool
	var gitRemote string
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register an existing folder as a library root",
		Long: "Adds a new entry to ~/.aida/library.yaml pointing at <path>. The path\n" +
			"must already contain a library.yaml manifest (run `aida library init --path <path>`\n" +
			"first if it doesn't). The folder is not modified.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rootPath := args[0]
			absPath, err := filepath.Abs(rootPath)
			if err != nil {
				return err
			}
			if name == "" {
				name = filepath.Base(absPath)
			}
			ownerVal := library.OwnerSelf
			if owner == string(library.OwnerWork) {
				ownerVal = library.OwnerWork
			}

			aidaDir := config.Dir()
			cfg, err := library.LoadConfig(aidaDir)
			if err != nil {
				return err
			}
			for _, r := range cfg.Roots {
				if r.Name == name {
					return fmt.Errorf("a root named %q is already registered (path=%s)", name, r.Path)
				}
				if r.Path == absPath {
					return fmt.Errorf("path %s is already registered as %q", absPath, r.Name)
				}
			}
			if dryRunGuard("register library root", name+" at "+absPath) {
				return nil
			}
			cfg.Roots = append(cfg.Roots, library.RootRef{
				Name:     name,
				Path:     absPath,
				Owner:    ownerVal,
				Git:      gitRemote,
				Optional: optional,
			})
			if err := library.SaveConfig(aidaDir, cfg); err != nil {
				return err
			}
			fmt.Printf("%s Registered root %q at %s\n", ui.SuccessIcon, name, absPath)
			if _, statErr := os.Stat(absPath); statErr != nil {
				if optional {
					fmt.Printf("  %s path does not exist yet -- optional, will be skipped until present\n", ui.WarnIcon)
				} else {
					fmt.Printf("  %s path does not exist yet -- run `aida library sync` after cloning\n", ui.WarnIcon)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name for this root (default: basename of path)")
	cmd.Flags().StringVar(&owner, "owner", "self", "owner: \"self\" or \"work\"")
	cmd.Flags().BoolVar(&optional, "optional", false, "skip silently if missing on disk (use for work-only roots)")
	cmd.Flags().StringVar(&gitRemote, "git", "", "git remote URL (used by aida library sync)")
	return cmd
}

func newLibrarySyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Pull latest changes for every registered library root",
		Long: "For each registered root that exists on disk and is a git working\n" +
			"tree, runs `git pull --ff-only` and reports per-root status. For roots\n" +
			"that have a `git:` remote configured but the path is missing, runs\n" +
			"`git clone` instead.",
		RunE: func(_ *cobra.Command, _ []string) error {
			aidaDir := config.Dir()
			cfg, err := library.LoadConfig(aidaDir)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			for _, ref := range cfg.Roots {
				absPath := library.ExpandPath(ref.Path)
				_, statErr := os.Stat(absPath)
				if statErr != nil {
					// Path missing -- clone if we have a remote, otherwise warn.
					if ref.Git == "" {
						mark := ui.WarnIcon
						if ref.Optional {
							mark = ui.DimStyle.Render("·")
						}
						fmt.Printf("%s %-20s missing, no git remote configured\n", mark, ref.Name)
						continue
					}
					fmt.Printf("%s %-20s cloning %s ...\n", ui.SuccessIcon, ref.Name, ref.Git)
					if err := runGit(ctx, "", "clone", ref.Git, absPath); err != nil {
						fmt.Printf("  %s clone failed: %v\n", ui.ErrorIcon, err)
					}
					continue
				}
				// Path exists -- pull if it's a git repo.
				if _, err := os.Stat(filepath.Join(absPath, ".git")); err != nil {
					fmt.Printf("%s %-20s present but not a git repo (skipping)\n", ui.DimStyle.Render("·"), ref.Name)
					continue
				}
				if err := runGit(ctx, absPath, "pull", "--ff-only"); err != nil {
					fmt.Printf("%s %-20s pull failed: %v\n", ui.ErrorIcon, ref.Name, err)
					continue
				}
				fmt.Printf("%s %-20s up to date\n", ui.SuccessIcon, ref.Name)
			}
			return nil
		},
	}
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func newLibraryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show all registered library roots and their counts",
		RunE: func(_ *cobra.Command, _ []string) error {
			aidaDir := config.Dir()
			cfg, err := library.LoadConfig(aidaDir)
			if err != nil {
				return err
			}
			reg, err := library.BuildRegistry(cfg)
			if err != nil {
				return err
			}
			fmt.Printf("Registry: %s\n\n", library.ConfigPath(aidaDir))
			fmt.Printf("Roots (%d present):\n", len(reg.Roots))
			for _, root := range reg.Roots {
				fmt.Printf("  %s %-20s  %s\n", ui.SuccessIcon, root.Ref.Name, root.AbsPath)
				layerCount, skillCount, sourceCount, outputStyleCount := 0, 0, 0, 0
				if root.Manifest != nil {
					layerCount = len(root.Manifest.Layers)
					skillCount = len(root.Manifest.Skills)
					sourceCount = len(root.Manifest.Sources)
					outputStyleCount = len(root.Manifest.OutputStyles)
				}
				fmt.Printf("       layers:%d  skills:%d  sources:%d  output-styles:%d  owner:%s\n",
					layerCount, skillCount, sourceCount, outputStyleCount, root.Ref.Owner)
			}
			for _, ref := range reg.MissingRoots {
				fmt.Printf("  %s %-20s  %s (missing)\n", ui.WarnIcon, ref.Name, ref.Path)
			}
			layerCount := len(reg.AvailableLayers())
			sourceCount := len(reg.AvailableSources())
			skillCount := len(reg.AvailableSkills())
			outputStyleCount := len(reg.AvailableOutputStyles())
			fmt.Printf("\nAvailable on this machine: layers:%d  skills:%d  sources:%d  output-styles:%d\n",
				layerCount, skillCount, sourceCount, outputStyleCount)

			if sourceCount == 0 {
				fmt.Println()
				fmt.Println(ui.HeaderStyle.Render("No sources configured yet."))
				fmt.Println("  - If you have an old ~/.aida/sources.yaml, run: aida library import")
				fmt.Println("  - Otherwise create one by hand in ~/.aida/library/sources/<name>.yaml")
				fmt.Println("    and register it in ~/.aida/library/library.yaml under sources:")
				fmt.Println("  - Add a route in ~/.aida/library/routes.yaml so it gets picked up.")
			}
			return nil
		},
	}
}

func newLibraryDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Show what's missing or filtered out on this machine",
		RunE: func(_ *cobra.Command, _ []string) error {
			aidaDir := config.Dir()
			cfg, err := library.LoadConfig(aidaDir)
			if err != nil {
				return err
			}
			reg, err := library.BuildRegistry(cfg)
			if err != nil {
				return err
			}

			fmt.Println(ui.HeaderStyle.Render("Library doctor:"))
			fmt.Println()

			if len(reg.MissingRoots) > 0 {
				fmt.Println(ui.HeaderStyle.Render("Missing required roots:"))
				for _, ref := range reg.MissingRoots {
					fmt.Printf("  %s %s -> %s\n", ui.WarnIcon, ref.Name, ref.Path)
					if ref.Git != "" {
						fmt.Printf("       try: git clone %s %s\n", ref.Git, ref.Path)
					}
				}
				fmt.Println()
			}

			unavailable := unavailableEntries(reg)
			if len(unavailable) == 0 {
				fmt.Printf("%s All registered entries are available on this machine.\n", ui.SuccessIcon)
				return nil
			}
			fmt.Println(ui.HeaderStyle.Render("Filtered out (missing tools):"))
			for _, line := range unavailable {
				fmt.Printf("  %s %s\n", ui.WarnIcon, line)
			}
			return nil
		},
	}
}

func unavailableEntries(reg *library.Registry) []string {
	var out []string
	for name, l := range reg.Layers {
		if !l.Available {
			out = append(out, fmt.Sprintf("layer  %-30s requires %v (root=%s)", name, l.MissingTools, l.Root.Ref.Name))
		}
	}
	for name, s := range reg.Skills {
		if !s.Available {
			out = append(out, fmt.Sprintf("skill  %-30s requires %v (root=%s)", name, s.MissingTools, s.Root.Ref.Name))
		}
	}
	for name, s := range reg.Sources {
		if !s.Available {
			out = append(out, fmt.Sprintf("source %-30s requires %v (root=%s)", name, s.MissingTools, s.Root.Ref.Name))
		}
	}
	for name, o := range reg.OutputStyles {
		if !o.Available {
			out = append(out, fmt.Sprintf("output-style %-30s requires %v (root=%s)", name, o.MissingTools, o.Root.Ref.Name))
		}
	}
	return out
}

func newLibraryImportCmd() *cobra.Command {
	var rootPath string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Migrate sources.yaml + per-source CLAUDE.md into a library root",
		Long: "Reads ~/.aida/sources.yaml and copies each source's context file\n" +
			"(CLAUDE.md/README.md) into <root>/layers/sources/<name>.md.\n" +
			"The legacy YAML file is NOT modified -- this is purely additive.",
		RunE: func(_ *cobra.Command, _ []string) error {
			aidaDir := config.Dir()
			if rootPath == "" {
				rootPath = filepath.Join(aidaDir, "library")
			}
			// Ensure target root exists with the standard layout.
			if err := library.ScaffoldRoot(rootPath, "imported"); err != nil {
				return fmt.Errorf("scaffolding root: %w", err)
			}
			result, err := library.ImportLegacy(rootPath)
			if err != nil {
				return err
			}
			fmt.Printf("%s Imported into %s\n", ui.SuccessIcon, result.RootPath)
			fmt.Printf("  source configs: %d\n", len(result.SourceConfigs))
			fmt.Printf("  source layers:  %d\n", len(result.SourceLayers))
			if len(result.SkippedSources) > 0 {
				fmt.Printf("  %s skipped (no context file): %v\n", ui.WarnIcon, result.SkippedSources)
			}
			fmt.Println()
			fmt.Println("The legacy sources.yaml file was not modified.")
			fmt.Println("Run `aida \"<your question>\"` and watch verbose output to see library layers in use.")
			return nil
		},
	}
	cmd.Flags().StringVar(&rootPath, "path", "", "library root to import into (default: ~/.aida/library)")
	return cmd
}

func newLibraryInitCmd() *cobra.Command {
	var rootName string
	var rootPath string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a library root and register it",
		Long: "Creates a library root directory layout (layers/, skills/, sources/, routes.yaml,\n" +
			"library.yaml) and registers it in ~/.aida/library.yaml. Idempotent: existing\n" +
			"files are not overwritten.",
		RunE: func(_ *cobra.Command, _ []string) error {
			aidaDir := config.Dir()
			if err := config.EnsureDir(); err != nil {
				return err
			}

			// Default: scaffold the "defaults" root at ~/.aida/library.
			if rootName == "" {
				rootName = "defaults"
			}
			if rootPath == "" {
				rootPath = filepath.Join(aidaDir, "library")
			}

			// 1. Scaffold the root layout.
			if err := library.ScaffoldRoot(rootPath, rootName); err != nil {
				return fmt.Errorf("scaffolding root: %w", err)
			}
			fmt.Printf("%s Scaffolded library root at %s\n", ui.SuccessIcon, rootPath)

			// 2. Ensure the registry exists.
			cfg, err := library.EnsureDefaultRegistered(aidaDir)
			if err != nil {
				return fmt.Errorf("registering root: %w", err)
			}

			// 3. If --root was given and isn't already in the registry, append it.
			if rootName != "defaults" {
				found := false
				for _, r := range cfg.Roots {
					if r.Name == rootName {
						found = true
						break
					}
				}
				if !found {
					if dryRunGuard("register library root", rootName+" at "+rootPath) {
						return nil
					}
					cfg.Roots = append(cfg.Roots, library.RootRef{
						Name:  rootName,
						Path:  rootPath,
						Owner: library.OwnerSelf,
					})
					if err := library.SaveConfig(aidaDir, cfg); err != nil {
						return fmt.Errorf("saving registry: %w", err)
					}
					fmt.Printf("%s Registered root %q in %s\n", ui.SuccessIcon, rootName, library.ConfigPath(aidaDir))
				}
			} else {
				fmt.Printf("%s Registry at %s\n", ui.SuccessIcon, library.ConfigPath(aidaDir))
			}

			fmt.Println()
			fmt.Println(ui.HeaderStyle.Render("Next steps:"))
			fmt.Printf("  1. Edit  %s/layers/global.md\n", rootPath)
			fmt.Printf("  2. Add routes in %s/routes.yaml\n", rootPath)
			fmt.Println("  3. (Coming soon) aida library list / doctor / sync")

			return nil
		},
	}
	cmd.Flags().StringVar(&rootName, "name", "", "name for the library root (default: \"defaults\")")
	cmd.Flags().StringVar(&rootPath, "path", "", "path for the library root (default: ~/.aida/library)")
	return cmd
}
