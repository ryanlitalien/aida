package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	examplesfs "github.com/ryanlitalien/aida/examples"
	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

// knownTools returns the list of tools to probe during init and profile scan.
// Derived from the built-in tool registry so there's a single source of truth.
var knownTools = config.KnownToolNames()

func newInitCmd() *cobra.Command {
	var brainRemote, configRemote string
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize Aida configuration",
		Long:  "Creates ~/.aida/ and writes default config, library, brain, and example sources.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runInit(brainRemote, configRemote, force)
		},
	}
	cmd.Flags().StringVar(&brainRemote, "brain-remote", "", "git remote URL for the brain's private backup repo")
	cmd.Flags().StringVar(&configRemote, "config-remote", "", "git remote URL for ~/.aida/ itself (private backup); unset means no config repo is created")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config.yaml with fresh defaults (everything else stays additive/idempotent regardless)")
	return cmd
}

func runInit(brainRemote, configRemote string, force bool) error {
	if dryRunGuard("initialize aida", config.Dir()) {
		return nil
	}
	dir := config.Dir()
	reader := bufio.NewReader(os.Stdin)

	// 1. Ensure directory exists
	if err := config.EnsureDir(); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}
	fmt.Printf("%s Created %s\n", ui.SuccessIcon, dir)

	// 2. Write config.yaml, or leave an existing one alone. This is the
	// one genuinely destructive step (SaveConfig always overwrites), so
	// it's the only step --force actually gates -- everything below this
	// (library scaffold, brain repo, example sources, roster/env files,
	// tool detection) is already additive/idempotent on its own and reruns
	// safely with or without --force.
	configPath := filepath.Join(dir, config.ConfigFile)
	var cfg *config.Config
	if _, err := os.Stat(configPath); err == nil && !force {
		fmt.Printf("%s %s already exists, leaving it alone (pass --force to overwrite with fresh defaults)\n", ui.DimStyle.Render("·"), configPath)
		cfg, err = config.LoadConfig()
		if err != nil {
			return fmt.Errorf("loading existing config.yaml: %w", err)
		}
	} else {
		cfg = config.DefaultConfig()
		if err := config.SaveConfig(cfg); err != nil {
			return fmt.Errorf("writing config.yaml: %w", err)
		}
		fmt.Printf("%s Wrote %s\n", ui.SuccessIcon, configPath)
	}

	// 3. Scaffold the default library root + register it. The library is
	// the source of truth for sources and layered context. Legacy
	// ~/.aida/sources.yaml is no longer written here -- it remains a
	// back-compat fallback if you have one on disk. The old partner
	// registry has no such fallback: it was removed 2026-09-12 and its
	// on-disk file, if any, is simply never read.
	defaultRootPath := filepath.Join(dir, "library")
	if err := library.ScaffoldRoot(defaultRootPath, "defaults"); err != nil {
		return fmt.Errorf("scaffolding library root: %w", err)
	}
	if _, err := library.EnsureDefaultRegistered(dir); err != nil {
		return fmt.Errorf("registering library: %w", err)
	}
	fmt.Printf("%s Scaffolded library at %s\n", ui.SuccessIcon, defaultRootPath)
	fmt.Printf("%s Registry at %s\n", ui.SuccessIcon, library.ConfigPath(dir))

	// 3a. Copy the fictional example sources (examples/sources/*.yaml, see
	// examples/README.md) into the library so the very first query has
	// something to route to. Skips any file that already exists, so this
	// is safe to run again and safe after you've edited or deleted one.
	if n, err := library.WriteExampleSources(defaultRootPath, examplesfs.FS, "sources"); err == nil && n > 0 {
		fmt.Printf("%s Copied %d example source(s) into the library (see examples/README.md)\n", ui.SuccessIcon, n)
	} else if err != nil {
		fmt.Printf("%s Copying example sources failed: %v\n", ui.WarnIcon, err)
	}

	// 3b. Auto-discover data sources in common locations. This is the
	//     equivalent of `aida library scan --write` against the active
	//     profile's scan paths + ~/dev, but scoped to "data" kinds only
	//     so a fresh install doesn't grab every Go repo in sight.
	scanPaths := defaultScanPaths()
	discovered, derr := library.ScanForSources(scanPaths)
	if derr == nil {
		var dataOnly []library.DiscoveredSource
		for _, d := range discovered {
			if d.Kind == "data" {
				dataOnly = append(dataOnly, d)
			}
		}
		if len(dataOnly) > 0 {
			// Assign the active profile to newly discovered sources.
			if cfg, cfgErr := config.LoadConfig(); cfgErr == nil {
				if _, pName := cfg.ActiveProfileConfig(); pName != "" {
					for i := range dataOnly {
						if len(dataOnly[i].Profiles) == 0 {
							dataOnly[i].Profiles = []string{pName}
						}
					}
				}
			}
			n, werr := library.WriteDiscovered(defaultRootPath, dataOnly)
			if werr == nil {
				fmt.Printf("%s Auto-discovered %d data source(s) under %s:\n", ui.SuccessIcon, n, strings.Join(scanPaths, ", "))
				for _, d := range dataOnly {
					fmt.Printf("    %s %s  (%s)\n", ui.DimStyle.Render("·"), d.Name, d.Path)
				}
			} else {
				fmt.Printf("%s Scan found sources but writing failed: %v\n", ui.WarnIcon, werr)
			}
		} else {
			fmt.Printf("%s No data folders auto-discovered under %s.\n", ui.DimStyle.Render("·"), strings.Join(scanPaths, ", "))
			fmt.Println("    Run `aida library scan --include-code --include-docs` to widen the search.")
		}
	}

	// 4. Initialize the brain as a local git repo (idempotent: InitRepo is
	// a no-op beyond adding a missing remote when one is already there).
	// --brain-remote sets a private backup remote at bootstrap instead of
	// requiring a manual `git -C ~/.aida/brain remote add origin <url>`
	// later; CommitAndPush already skips the push step when no remote is
	// configured, so leaving it empty is a fully supported local-only mode.
	brainPath := cfg.BrainPath()
	if err := brain.InitRepo(brainPath, brainRemote); err != nil {
		fmt.Printf("%s Initializing brain repo failed: %v\n", ui.WarnIcon, err)
	} else {
		fmt.Printf("%s Brain initialized at %s\n", ui.SuccessIcon, brainPath)
	}

	// 4a. --config-remote: an explicit opt-in to also back up ~/.aida/
	// itself (config.yaml, library/, roster.yaml, ...) to a private
	// remote, dotfiles-style. Unlike the brain, this repo is NOT created
	// unless a remote is given -- there's no equivalent auto-commit path
	// for ~/.aida/ today, so an empty local-only repo here would just be
	// clutter with nothing to push it.
	if configRemote != "" {
		if err := ensureConfigRepo(dir, configRemote); err != nil {
			fmt.Printf("%s Setting up the config backup repo failed: %v\n", ui.WarnIcon, err)
		} else {
			fmt.Printf("%s Config backup repo at %s (remote: %s)\n", ui.SuccessIcon, dir, configRemote)
		}
	}

	// 4b. Copy the example roster + env files, never overwriting anything
	// already there.
	copyEmbeddedIfMissing(examplesfs.FS, "roster.yaml", filepath.Join(dir, "roster.yaml"))
	copyEmbeddedIfMissing(examplesfs.FS, "env.example", filepath.Join(dir, "env.example"))
	copyEmbeddedIfMissing(examplesfs.FS, "op.env.example", filepath.Join(dir, "op.env.example"))

	// 4c. Copy the fictional golden-eval seed set (examples/golden/) so
	// `scripts/run-goldens.sh` / `go run ./cmd/golden-log` -- which now
	// read from ~/.aida/ instead of this repo's scripts/ dir -- have
	// something to run against out of the box. Real seeds belong in
	// aida-config, never in this repo; see examples/README.md.
	copyEmbeddedIfMissing(examplesfs.FS, "golden/questions.txt", filepath.Join(dir, "golden-questions.txt"))
	copyEmbeddedIfMissing(examplesfs.FS, "golden/expectations.yaml", filepath.Join(dir, "golden-expectations.yaml"))

	// 5. Prompt for Anthropic API key
	fmt.Println()
	envPath := filepath.Join(dir, ".env")
	existingKey := os.Getenv("ANTHROPIC_API_KEY")
	if existingKey != "" {
		fmt.Printf("%s Anthropic API key already set in environment\n", ui.SuccessIcon)
	} else {
		// Check if .env already has a key
		if data, err := os.ReadFile(envPath); err == nil && strings.Contains(string(data), "ANTHROPIC_API_KEY") {
			fmt.Printf("%s Anthropic API key already saved in %s\n", ui.SuccessIcon, envPath)
		} else {
			fmt.Print("Enter your Anthropic API key (or press Enter to skip): ")
			apiKey, _ := reader.ReadString('\n')
			apiKey = strings.TrimSpace(apiKey)
			if apiKey != "" {
				if err := os.WriteFile(envPath, []byte("ANTHROPIC_API_KEY="+apiKey+"\n"), 0600); err != nil {
					return fmt.Errorf("writing .env: %w", err)
				}
				fmt.Printf("%s Saved API key to %s\n", ui.SuccessIcon, envPath)
			} else {
				fmt.Printf("%s Skipped. Set ANTHROPIC_API_KEY in env or add to %s later.\n", ui.WarnIcon, envPath)
			}
		}
	}

	// 6. Detect tools on PATH and write to active profile
	fmt.Println()
	detected := detectToolsOnPath()
	fmt.Println(ui.HeaderStyle.Render("Detected tools:"))
	found := 0
	for _, tool := range knownTools {
		if path, ok := detected[tool]; ok {
			fmt.Printf("  %s %s  %s\n", ui.SuccessIcon, tool, ui.DimStyle.Render(path))
			found++
		} else {
			fmt.Printf("  %s %s  %s\n", ui.WarnIcon, tool, ui.DimStyle.Render("not found"))
		}
	}
	if found > 0 {
		// Reload config (we just wrote it above) and update the active profile's tools.
		cfg2, err2 := config.LoadConfig()
		if err2 == nil {
			cfg = cfg2
			if added := writeDetectedTools(cfg, detected); added > 0 {
				fmt.Printf("%s Added %d tool(s) to active profile\n", ui.SuccessIcon, added)
			}
		}

		// Write library source configs for each detected builtin tool so
		// `aida library list` shows real sources out of the box.
		if n, err := library.WriteBuiltinToolSources(defaultRootPath, detected); err == nil && n > 0 {
			fmt.Printf("%s Wrote %d tool source(s) to library\n", ui.SuccessIcon, n)
		}
	}

	// 7. Print next steps
	fmt.Println()
	fmt.Println(ui.HeaderStyle.Render("Next steps:"))
	fmt.Println("  1. Check sources:  aida library list    (verify detected tools and data sources)")
	fmt.Println("  2. Scan for more:  aida library scan --include-code --include-docs")
	fmt.Println("  3. Ask a question: aida \"what happened with checkout errors yesterday?\"")
	fmt.Println("  4. Tune a bad answer: aida tune")
	fmt.Println("  5. Claude Code:      aida setup          (register aida tools globally)")

	if found == 0 {
		fmt.Printf("%s No tools detected. Aida will have limited capabilities.\n", ui.WarnIcon)
	}

	// 8. Key diagnostic: what's set, what it unlocks, and the one-line fix
	// for anything missing -- so a stranger's first run tells them what to
	// export instead of failing later somewhere deep in the pipeline.
	fmt.Println()
	printKeyDiagnostic(cfg)

	return nil
}

// keyCheck is one row of the closing key diagnostic: an env var, the
// feature it unlocks, and what to do if it's missing.
type keyCheck struct {
	env     string
	feature string
	fix     string
}

// printKeyDiagnostic reports which env vars are set, which features that
// unlocks, and the one-line fix for each gap.
func printKeyDiagnostic(cfg *config.Config) {
	voyageEnv := cfg.VoyageKeyEnv()
	checks := []keyCheck{
		{
			env:     "ANTHROPIC_API_KEY",
			feature: "the engine (parse/classify/plan/execute/synthesize LLM calls)",
			fix:     "export ANTHROPIC_API_KEY=sk-ant-... (or set model.offline_mode + Ollama in config.yaml)",
		},
		{
			env:     voyageEnv,
			feature: "brain semantic recall (embeddings)",
			fix:     fmt.Sprintf("export %s=... (keyword/FTS search still works without it)", voyageEnv),
		},
		{
			env:     "ELEVENLABS_API_KEY",
			feature: "ElevenLabs voice (optional -- the bundled Piper voice needs no key)",
			fix:     "export ELEVENLABS_API_KEY=... (or skip: `aida jarvis greet` already works via Piper)",
		},
	}

	fmt.Println(ui.HeaderStyle.Render("Key diagnostic:"))
	for _, c := range checks {
		if os.Getenv(c.env) != "" {
			fmt.Printf("  %s %-20s set    -- unlocks %s\n", ui.SuccessIcon, c.env, c.feature)
			continue
		}
		fmt.Printf("  %s %-20s unset  -- unlocks %s\n", ui.WarnIcon, c.env, c.feature)
		fmt.Printf("      fix: %s\n", c.fix)
	}
}

// copyEmbeddedIfMissing copies one file out of the embedded examples.FS to
// destPath, doing nothing if destPath already exists -- the same
// never-overwrite convention every other init step follows.
func copyEmbeddedIfMissing(src fsReadFile, name, destPath string) {
	if _, err := os.Stat(destPath); err == nil {
		return
	}
	data, err := src.ReadFile(name)
	if err != nil {
		fmt.Printf("%s Reading embedded %s failed: %v\n", ui.WarnIcon, name, err)
		return
	}
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		fmt.Printf("%s Writing %s failed: %v\n", ui.WarnIcon, destPath, err)
		return
	}
	fmt.Printf("%s Wrote %s\n", ui.SuccessIcon, destPath)
}

// fsReadFile is the minimal embed.FS surface copyEmbeddedIfMissing needs;
// keeping it narrow avoids an io/fs import purely for a type name.
type fsReadFile interface {
	ReadFile(name string) ([]byte, error)
}

// ensureConfigRepo makes ~/.aida/ itself a git repo for private backup,
// mirroring brain.InitRepo's idempotent shape: safe to call on every
// `aida init --config-remote` run, only adds a remote when one isn't
// already configured. brain/ is excluded via .gitignore since it already
// has its own repo + remote (a nested .git dir would otherwise make git
// treat it as an embedded repo / gitlink).
func ensureConfigRepo(dir, remote string) error {
	gitDir := filepath.Join(dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		if remote == "" {
			return nil
		}
		if err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Run(); err != nil {
			return exec.Command("git", "-C", dir, "remote", "add", "origin", remote).Run()
		}
		return nil
	}

	if err := exec.Command("git", "-C", dir, "init").Run(); err != nil {
		return fmt.Errorf("git init: %w", err)
	}

	gitignorePath := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		// .env/op.env hold plaintext secrets and must never be committed
		// (see CLAUDE.md's op.env convention); brain/ has its own repo.
		gitignore := "brain/\n.env\nop.env\n*.log\nworktrees/\njobs/\n"
		if err := os.WriteFile(gitignorePath, []byte(gitignore), 0644); err != nil {
			return err
		}
	}

	if remote != "" {
		if err := exec.Command("git", "-C", dir, "remote", "add", "origin", remote).Run(); err != nil {
			return fmt.Errorf("add remote: %w", err)
		}
	}

	exec.Command("git", "-C", dir, "add", "-A").Run()
	exec.Command("git", "-C", dir, "commit", "-m", "Initialize aida config").Run()

	return nil
}
