// cmd/cortex/retract.go wires `cortex retract` onto
// internal/pipeline/retract. The command opens the log for append,
// constructs a one-shot retract pipeline, executes the request, and
// renders a terse summary (tx id per retracted entity).
//
// Cascade support (--cascade) requires a ChildResolver that walks
// DERIVED_FROM edges through the graph adapter. The live Neo4j
// resolver is a follow-up; this CLI passes nil, which makes the
// pipeline return retract.ErrNoResolver with a precise message the
// operator can act on. Non-cascade retractions work end-to-end today.
//
// Replaces the notImplemented stub in commands.go (CRIT-003 fix).
package main

import (
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/pipeline/retract"
)

// newRetractCmdReal returns the wired `cortex retract` command.
func newRetractCmdReal() *cobra.Command {
	var (
		cascade  bool
		reason   string
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "retract <entity-id>",
		Short: "Write a retraction datom against an entity",
		Long: "cortex retract emits an append-only tombstone (OpRetract against the " +
			"entity's exists attribute) plus audit datoms recording the operator " +
			"identity and --reason. --cascade walks DERIVED_FROM children and " +
			"retracts each with a shared cascade_source audit. Nothing is ever " +
			"deleted; the original assertions remain visible to cortex history " +
			"and cortex as-of, while default recall hides retracted entities.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			writer, err := log.NewWriter(segDir)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_OPEN_FAILED",
					"could not open log segment directory", err), jsonFlag)
			}
			defer writer.Close()

			pipeline := &retract.Pipeline{
				Log:          writer,
				Actor:        defaultActor(),
				InvocationID: ulid.Make().String(),
				// Resolver intentionally nil: cascade requires a Neo4j-
				// backed DERIVED_FROM walker which is a separate bead.
				// Passing --cascade today returns ErrNoResolver with a
				// precise message the operator can act on.
			}

			res, err := pipeline.Retract(cmd.Context(), retract.Request{
				EntityID: args[0],
				Reason:   reason,
				Cascade:  cascade,
			})
			if err != nil {
				if errors.Is(err, retract.ErrNoResolver) {
					return emitAndExit(cmd, errs.Operational("CASCADE_NOT_WIRED",
						"cortex retract --cascade requires a Neo4j child resolver which is not yet wired in this build",
						err), jsonFlag)
				}
				if errors.Is(err, retract.ErrEmptyEntityID) ||
					errors.Is(err, retract.ErrEmptyActor) {
					return emitAndExit(cmd, errs.Validation("INVALID_RETRACT_REQUEST",
						err.Error(), nil), jsonFlag)
				}
				return emitAndExit(cmd, errs.Operational("RETRACT_FAILED",
					"could not append retraction datoms", err), jsonFlag)
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "cortex retract ok (%d entities)\n", len(res.EntityIDs))
			for i, id := range res.EntityIDs {
				fmt.Fprintf(w, "  %s tx=%s\n", id, res.TxIDs[i])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "also retract derived-from descendants (requires graph resolver)")
	cmd.Flags().StringVar(&reason, "reason", "", "operator-supplied retraction reason (recorded in audit datom)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON error envelope on failure")
	return cmd
}
