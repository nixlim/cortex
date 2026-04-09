// Command cortex is the single-binary entrypoint for the Cortex knowledge system.
package main

import (
	"fmt"
	"os"

	"github.com/nixlim/cortex/internal/version"
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cortex",
		Short:         "Cortex — local knowledge substrate for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the Cortex version",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
			return nil
		},
	})

	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "cortex:", err)
		os.Exit(1)
	}
}
