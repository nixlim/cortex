// cmd/cortex/selfheal.go implements runRootSelfHeal, the body of the
// root cobra cmd's PersistentPreRunE. It is the single bridge between
// the CLI and internal/replay.SelfHeal: it loads the segment list,
// opens the watermark store, and invokes SelfHeal.
//
// Graceful degradation is the design centre. A missing config file,
// an empty log directory, or an unreachable Bolt endpoint must NOT
// prevent the subcommand body from running — the subcommand has its
// own backend wiring and will surface its own NEO4J_UNAVAILABLE /
// CONFIG_LOAD_FAILED error if it actually needs the backend. The
// PreRunE only blocks startup when SelfHeal returns an error AND we
// successfully reached the replay step (i.e., the log was non-empty
// AND the watermark store was reachable AND a backend applier
// reported a hard failure mid-stream). At that point the operator
// needs to know which backend choked on which tx, so we surface the
// error verbatim.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Self-healing startup"
//	bead cortex-4kq.31, code-review fix MAJ-002
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/replay"
	"github.com/nixlim/cortex/internal/watermark"
	"github.com/nixlim/cortex/internal/weaviate"
)

// runRootSelfHeal is the body of the root cmd's PersistentPreRunE.
// See the file header for the degradation policy.
func runRootSelfHeal(cmd *cobra.Command) error {
	ctx := cmd.Context()

	// Step 1: load config. A missing or unreadable config is treated
	// as "fresh install" — there is nothing to heal because there is
	// no log directory yet. The subcommand body will produce its own
	// CONFIG_LOAD_FAILED error if it actually needs the file.
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil
	}

	// Step 2: enumerate healthy segments. log.Load returns an empty
	// report (and no error) when the directory does not yet exist,
	// which is the fast path for first-run.
	segDir := expandHome(cfg.Log.SegmentDir)
	report, err := log.Load(segDir, log.LoadOptions{})
	if err != nil {
		// A tx collision (ErrTxCollision) or a filesystem fault is a
		// real startup blocker — refuse to run the subcommand. Wrap
		// it as Operational so emitAndExit picks the right exit code
		// when the subcommand surfaces it.
		return errs.Operational("LOG_LOAD_FAILED",
			"could not load segment directory during self-heal", err)
	}
	if len(report.Healthy) == 0 {
		// Empty log: nothing to heal, no watermark store needed.
		return nil
	}

	// Step 3: open Bolt + Weaviate clients for the watermark store.
	// Either may legitimately be nil in a degraded install — the
	// store treats nil clients as "never written" and SelfHeal skips
	// the corresponding replay branch.
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, boltErr := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  2,
	})
	if boltErr != nil {
		// Backend not reachable: degrade to no-op rather than blocking
		// the subcommand. The subcommand will produce its own
		// NEO4J_UNAVAILABLE if it actually needs the connection.
		return nil
	}
	defer func() { _ = bolt.Close(context.Background()) }()

	weaviateClient := newWeaviateClient(cfg)

	store := watermark.NewStore(bolt, weaviateClient)

	// Step 4: run the heal with real backend appliers. The Neo4j and
	// Weaviate adapter packages now expose concrete BackendApplier
	// types (cortex-4kq adapter beads, MAJ-001 / MAJ-007), so SelfHeal
	// actually advances the per-backend watermarks and replays any tx
	// the previous run committed to the log but failed to apply to
	// the store.
	neoApplier := neo4j.NewBackendApplier(bolt)
	weaviateApplier := weaviate.NewBackendApplier(weaviateClient)
	_, err = replay.SelfHeal(ctx, report.Healthy, store, neoApplier, weaviateApplier)
	if err != nil {
		return errs.Operational("SELFHEAL_FAILED",
			fmt.Sprintf("self-heal replay failed for command %q", cmd.Name()), err)
	}
	return nil
}
