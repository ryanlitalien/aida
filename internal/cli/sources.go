package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
	"github.com/spf13/cobra"
)

func newSourcesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sources",
		Short: "List configured sources",
		Long:  "Lists all configured sources with their type, capabilities, and entity count.",
		Args:  cobra.NoArgs,
		RunE:  runSources,
	}

	cmd.AddCommand(newSourcesEditCmd())

	return cmd
}

func newSourcesEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit",
		Short: "Open sources.yaml in your editor",
		Args:  cobra.NoArgs,
		RunE:  runSourcesEdit,
	}
}

func runSources(_ *cobra.Command, _ []string) error {
	srcs, err := config.LoadSources()
	if err != nil {
		return fmt.Errorf("loading sources: %w", err)
	}

	if len(srcs) == 0 {
		fmt.Printf("%s No sources configured.\n", ui.WarnIcon)
		fmt.Println("  Add sources by editing: aida sources edit")
		return nil
	}

	fmt.Printf("%s %s\n\n",
		ui.HeaderStyle.Render("Sources"),
		ui.DimStyle.Render(fmt.Sprintf("(%d configured)", len(srcs))),
	)

	for name, src := range srcs {
		caps := strings.Join(src.Capabilities, ", ")
		entityCount := len(src.Entities)

		fmt.Printf("  %s  %s\n", ui.SourceStyle.Render(name), ui.DimStyle.Render("["+src.Type+"]"))
		if src.Description != "" {
			fmt.Printf("    %s\n", src.Description)
		}
		if caps != "" {
			fmt.Printf("    Capabilities: %s\n", ui.DimStyle.Render(caps))
		}
		if entityCount > 0 {
			fmt.Printf("    Entities:     %s\n", ui.DimStyle.Render(fmt.Sprintf("%d", entityCount)))
		}
		fmt.Println()
	}

	return nil
}

func runSourcesEdit(_ *cobra.Command, _ []string) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	editor := cfg.GetEditor()
	path := filepath.Join(config.Dir(), config.SourcesFile)

	// Ensure the file exists before opening it.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := config.SaveSources(make(config.Sources)); err != nil {
			return fmt.Errorf("creating sources.yaml: %w", err)
		}
	}

	return openEditor(editor, path)
}

// openEditor launches the user's editor on the given file path.
func openEditor(editor, path string) error {
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("no editor configured")
	}

	args := append(parts[1:], path)
	cmd := exec.Command(parts[0], args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %q exited with error: %w", editor, err)
	}
	return nil
}
