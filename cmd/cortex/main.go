// Command cortex is the single-binary entrypoint for the Cortex knowledge system.
package main

import (
	"fmt"
	"os"

	"github.com/nixlim/cortex/internal/version"
	"github.com/spf13/cobra"
)

// selfHealSkip lists subcommands that must NOT trigger the startup
// self-heal protocol. Lifecycle / diagnostic verbs run before backends
// are guaranteed to be up (cortex up brings them up; cortex doctor is
// allowed to report on a half-broken stack), and cortex version is
// purely local. Every other read- or write-path verb falls through to
// runRootSelfHeal in PersistentPreRunE.
var selfHealSkip = map[string]bool{
	"version": true,
	"up":      true,
	"down":    true,
	"status":  true,
	"doctor":  true,
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cortex",
		Short:         "Cortex — local knowledge substrate for AI agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE wires the self-healing replay protocol
		// (MAJ-002) onto every read- or write-path subcommand. The hook
		// is intentionally tolerant: a missing config file, an empty
		// log directory, an unreachable backend, or a nil applier slot
		// all degrade silently to a no-op so a brand-new install can
		// still run `cortex observe` before `cortex up` populates the
		// backends. Once the neo4j / weaviate adapter packages grow
		// concrete replay.Applier implementations, slot them into
		// runRootSelfHeal and the hook will start performing real heals
		// without any further wiring.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if selfHealSkip[cmd.Name()] {
				return nil
			}
			return runRootSelfHeal(cmd)
		},
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
