package cli

import (
	"fmt"

	"github.com/ryanlitalien/aida/internal/jarvis/lmd"
	"github.com/spf13/cobra"
)

// newLMDCmd groups the L.M.D. (Life Model Decoy) Android-bridge
// subcommands. The listener itself lives behind `aida serve --lmd`; this
// tree is for out-of-band token management (see docs/lmd-protocol.md).
func newLMDCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lmd",
		Short: "L.M.D. (Life Model Decoy) Android bridge - bearer token management",
	}
	cmd.AddCommand(newLMDTokenCmd())
	return cmd
}

// newLMDTokenCmd prints the current LMD bearer token, generating and
// persisting one on first use exactly as `aida serve --lmd` would.
// --rotate replaces it, invalidating whatever the phone currently holds
// until the new value is pasted into its settings.
func newLMDTokenCmd() *cobra.Command {
	var rotate bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print the LMD bearer token (--rotate to generate a new one)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if rotate {
				tok, err := lmd.RotateToken()
				if err != nil {
					return fmt.Errorf("rotate LMD token: %w", err)
				}
				fmt.Println(tok)
				return nil
			}
			tok, err := lmd.LoadOrGenerateToken()
			if err != nil {
				return fmt.Errorf("load LMD token: %w", err)
			}
			fmt.Println(tok)
			return nil
		},
	}
	cmd.Flags().BoolVar(&rotate, "rotate", false, "generate and persist a new token, invalidating the old one")
	return cmd
}
