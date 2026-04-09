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

	// ops-dev: register every Phase 1 subcommand (see commands.go).
	addOpsCommands(root)

	return root
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Subcommands that have already rendered their own error
		// envelope return an *exitCodeErr so main knows exactly
		// which exit code to use (validation=2, operational=1) and
		// must not print anything further. Anything else is an
		// unexpected cobra-level failure (unknown flag, usage) and
		// maps to exit 1 with a generic prefix.
		if _, ok := err.(*exitCodeErr); ok {
			os.Exit(exitCodeFromError(err))
		}
		fmt.Fprintln(os.Stderr, "cortex:", err)
		os.Exit(1)
	}
}
