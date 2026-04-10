// Package main: ops/CLI surface.
//
// This file wires every Cortex subcommand documented in docs/spec/cortex-spec.md
// as a cobra.Command under the root. Each command currently returns a
// "not implemented" error so the surface compiles and so that features-dev,
// adapter-dev, and the ops-dev commands below can be filled in without having
// to chase cobra boilerplate.
//
// Subcommand groups (trail, community, subject, ingest) are real parent
// commands so `cortex trail begin` etc. resolve today. Flags that map onto
// acceptance criteria are declared now so the stub shape matches the spec.
//
// Owned by ops-dev (cortex-4kq.22/.26/.27/.30/.33/.34/.31/.43/.45/.46/.50/
// .40/.49/.37/.38/.54/.55) plus stub placeholders for features-dev commands
// (observe/recall/reflect/ingest/analyze) that will be replaced by real
// implementations once the log layer lands.
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// notImplemented returns a RunE that reports the command is not yet wired.
// It satisfies the team-lead instruction to keep every documented subcommand
// addressable immediately, so that downstream beads can swap in real logic
// without having to re-register the cobra node.
func notImplemented(name string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("not implemented: %s", name)
	}
}

// addOpsCommands registers every Phase 1 subcommand on the root command.
// It is called from newRootCmd() in main.go.
func addOpsCommands(root *cobra.Command) {
	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newDoctorCmd())
	root.AddCommand(newTrailCmd())
	root.AddCommand(newHistoryCmd())
	root.AddCommand(newAsOfCmd())
	root.AddCommand(newCommunitiesCmd())
	root.AddCommand(newCommunityCmd())
	root.AddCommand(newPinCmd())
	root.AddCommand(newUnpinCmd())
	root.AddCommand(newEvictCmd())
	root.AddCommand(newUnevictCmd())
	root.AddCommand(newRebuildCmd())
	root.AddCommand(newExportCmd())
	root.AddCommand(newMergeCmd())
	root.AddCommand(newRetractCmd())
	root.AddCommand(newSubjectCmd())
	root.AddCommand(newMigrateCmd())
	root.AddCommand(newBenchCmd())

	// features-dev territory — stubbed here so the root CLI compiles with
	// every documented verb present. features-dev will replace the RunE
	// with real implementations.
	root.AddCommand(newObserveCmd())
	root.AddCommand(newRecallCmd())
	root.AddCommand(newReflectCmd())
	root.AddCommand(newIngestCmd())
	root.AddCommand(newAnalyzeCmd())
}

// ---------------------------------------------------------------------------
// Lifecycle: up / down / status / doctor
// ---------------------------------------------------------------------------

func newUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start managed containers (Weaviate, Neo4j) and probe Ollama",
		Long: "cortex up starts the managed Docker stack (Weaviate, Neo4j+GDS), waits for " +
			"per-service readiness endpoints, probes the host Ollama, and only returns " +
			"success when the 90-second startup budget has been satisfied.",
		RunE: runUp, // implementation in cmd/cortex/up.go (cortex-4kq.22)
	}
	return cmd
}

func newDownCmd() *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop managed containers (Weaviate, Neo4j)",
		Long: "cortex down stops managed containers while preserving named volumes. " +
			"cortex down --purge additionally removes volumes after operator confirmation. " +
			"Neither form ever touches ~/.cortex/log.d/.",
		// implementation in cmd/cortex/down.go (cortex-4kq.26)
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDown(cmd, args, purge)
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove named volumes after confirmation")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report per-component health (shallow, <2s)",
		Long: "cortex status reports each managed dependency as running/stopped/degraded " +
			"with version, log watermark, entry count, and disk usage. Deep checks " +
			"belong to cortex doctor.",
		// implementation in cmd/cortex/status.go (cortex-4kq.27)
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, args, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON output")
	return cmd
}

func newDoctorCmd() *cobra.Command {
	var quick, full, jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run diagnostic checks across Cortex dependencies",
		Long: "cortex doctor --quick runs bounded-time checks (<5s total). " +
			"cortex doctor --full runs adapter probes, segment scan, watermark drift, " +
			"quarantine count, permission audit, disk space, and host prerequisites " +
			"using doctor.parallelism workers.",
		// implementation in cmd/cortex/doctor.go (cortex-4kq.30)
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, args, quick, full, jsonOut)
		},
	}
	cmd.Flags().BoolVar(&quick, "quick", false, "run bounded-time checks only (<5s)")
	cmd.Flags().BoolVar(&full, "full", false, "run every check including slow probes")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit machine-readable JSON output")
	return cmd
}

// ---------------------------------------------------------------------------
// Trail: begin / end / show / list
// ---------------------------------------------------------------------------

// newTrailCmd delegates to the real implementation in trail.go.
// Subcommand RunEs are wired through newTrailCmdReal so begin/end/
// show/list each speak to the internal/trail package.
func newTrailCmd() *cobra.Command {
	return newTrailCmdReal()
}

// ---------------------------------------------------------------------------
// History + as-of
// ---------------------------------------------------------------------------

// newHistoryCmd / newAsOfCmd delegate to the real implementations in
// history.go. Kept as one-line shims so commands.go remains the single
// place where the cobra tree is described.
func newHistoryCmd() *cobra.Command { return newHistoryCmdReal() }

func newAsOfCmd() *cobra.Command { return newAsOfCmdReal() }

// ---------------------------------------------------------------------------
// Communities
// ---------------------------------------------------------------------------

// newCommunitiesCmd / newCommunityCmd delegate to the real
// implementations in communities.go. Kept as one-line shims so
// commands.go remains the single place where the cobra tree is
// described.
func newCommunitiesCmd() *cobra.Command { return newCommunitiesCmdReal() }

func newCommunityCmd() *cobra.Command { return newCommunityCmdReal() }

// ---------------------------------------------------------------------------
// Pin / unpin / evict / unevict
// ---------------------------------------------------------------------------

func newPinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pin <entity-id>",
		Short: "Pin an entity so it resists activation decay",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("cortex pin"),
	}
}

func newUnpinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unpin <entity-id>",
		Short: "Remove a pin from an entity",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("cortex unpin"),
	}
}

func newEvictCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "evict <entity-id>",
		Short: "Force activation to zero and block reinforcement",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("cortex evict"),
	}
}

func newUnevictCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unevict <entity-id>",
		Short: "Re-enable reinforcement for a previously evicted entity",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("cortex unevict"),
	}
}

// ---------------------------------------------------------------------------
// Rebuild / export / merge / retract
// ---------------------------------------------------------------------------

func newRebuildCmd() *cobra.Command {
	var acceptDrift bool
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Replay the log into a fresh backend state",
		Long: "cortex rebuild replays every committed datom into a clean Weaviate and " +
			"Neo4j using the embedding_model_digest recorded at write time. " +
			"--accept-drift allows re-embedding under the current model and writes a " +
			"model_rebind audit datom.",
		RunE: notImplemented("cortex rebuild"),
	}
	cmd.Flags().BoolVar(&acceptDrift, "accept-drift", false, "re-embed under the current embedding model")
	return cmd
}

func newExportCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Merge all segments into one tx-sorted stream",
		RunE:  notImplemented("cortex export"),
	}
	cmd.Flags().StringVar(&out, "out", "", "output path for the merged stream (default stdout)")
	return cmd
}

func newMergeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge <segment-path>...",
		Short: "Merge external log segments into ~/.cortex/log.d",
		Args:  cobra.MinimumNArgs(1),
		RunE:  notImplemented("cortex merge"),
	}
	return cmd
}

func newRetractCmd() *cobra.Command {
	var cascade bool
	cmd := &cobra.Command{
		Use:   "retract <entity-id>",
		Short: "Write a retraction datom against an entity",
		Args:  cobra.ExactArgs(1),
		RunE:  notImplemented("cortex retract"),
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "also retract derived-from descendants")
	return cmd
}

// ---------------------------------------------------------------------------
// Subject (PSI) — merge
// ---------------------------------------------------------------------------

func newSubjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subject",
		Short: "Manage PSI subjects",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "merge <canonical-id> <alias-id>...",
		Short: "Accretively merge subjects by adding aliases to a canonical id",
		Args:  cobra.MinimumNArgs(2),
		RunE:  notImplemented("cortex subject merge"),
	})
	return cmd
}

// ---------------------------------------------------------------------------
// Migrate / bench
// ---------------------------------------------------------------------------

func newMigrateCmd() *cobra.Command {
	var fromMempalace string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate content from an external knowledge system",
		RunE:  notImplemented("cortex migrate"),
	}
	cmd.Flags().StringVar(&fromMempalace, "from-mempalace", "", "path to a MemPalace JSONL export")
	return cmd
}

// newBenchCmd delegates to the real implementation in bench.go.
func newBenchCmd() *cobra.Command { return newBenchCmdReal() }

// ---------------------------------------------------------------------------
// features-dev stubs — observe / recall / reflect / ingest / analyze
// ---------------------------------------------------------------------------

// newObserveCmd delegates to the real implementation in observe.go.
// The stub form has been removed; see newObserveCmdReal for the
// full write pipeline wiring.
func newObserveCmd() *cobra.Command {
	return newObserveCmdReal()
}

func newRecallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recall <query>",
		Short: "Retrieve entries matching a query",
		Args:  cobra.MinimumNArgs(1),
		RunE:  notImplemented("cortex recall"),
	}
}

func newReflectCmd() *cobra.Command {
	var dryRun, explain bool
	cmd := &cobra.Command{
		Use:   "reflect",
		Short: "Promote episodic clusters into typed frames",
		RunE:  notImplemented("cortex reflect"),
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate candidates without writing frames")
	cmd.Flags().BoolVar(&explain, "explain", false, "print rejection reasons per candidate")
	return cmd
}

func newIngestCmd() *cobra.Command {
	var project, strategy string
	cmd := &cobra.Command{
		Use:   "ingest <path>",
		Short: "Walk a repository and ingest module summaries",
		Args:  cobra.MinimumNArgs(1),
		RunE:  notImplemented("cortex ingest"),
	}
	cmd.Flags().StringVar(&project, "project", "", "required project name for scoping")
	cmd.Flags().StringVar(&strategy, "strategy", "", "force a built-in language strategy")

	// Nested subcommands documented in the spec.
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Report last-ingested commit and counts for a project",
		RunE:  notImplemented("cortex ingest status"),
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "resume",
		Short: "Process only missing modules for a project",
		RunE:  notImplemented("cortex ingest resume"),
	})
	return cmd
}

func newAnalyzeCmd() *cobra.Command { return newAnalyzeCmdReal() }
