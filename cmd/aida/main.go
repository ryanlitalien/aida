package main

import (
	"fmt"
	"os"

	"github.com/ryanlitalien/aida/internal/cli"
	"github.com/ryanlitalien/aida/internal/ui"
)

func main() {
	cmd := cli.NewRootCmd()
	if err := cmd.Execute(); err != nil {
		ui.PrintError(err)
		fmt.Fprintln(os.Stderr) // blank line after error
		os.Exit(1)
	}
}
