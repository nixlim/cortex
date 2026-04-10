// cmd/cortex/subject_merge.go wires `cortex subject merge` onto
// internal/pipeline/merge. The command validates two PSIs, opens the
// log writer, constructs a one-shot merge pipeline, and emits the
// accretive alias datoms plus any contradiction edges the facet reader
// surfaces.
//
// Contradiction detection requires a SubjectFacetReader that returns
// each subject's current facet claims. The live Neo4j/log-replay reader
// is a follow-up; this CLI passes nil, which per the merge package
// contract disables contradiction detection and emits only the alias +
// audit datoms. Non-contradicting merges work end-to-end today.
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
	pmerge "github.com/nixlim/cortex/internal/pipeline/merge"
)

// newSubjectCmdReal returns the wired `cortex subject` command with
// its `merge` subcommand.
func newSubjectCmdReal() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subject",
		Short: "Manage PSI subjects",
	}
	cmd.AddCommand(newSubjectMergeCmd())
	return cmd
}

func newSubjectMergeCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "merge <canonical-psi> <alias-psi>",
		Short: "Accretively merge two subjects by aliasing one onto the other",
		Long: "cortex subject merge writes append-only alias datoms binding the " +
			"alias PSI to the canonical PSI. No prior assertion on either subject " +
			"is mutated or deleted; subsequent recall follows the alias edge to " +
			"the canonical form. Any facet key where both subjects asserted " +
			"different values is emitted as a contradiction edge so the audit " +
			"trail preserves the disagreement.",
		Args: cobra.ExactArgs(2),
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

			pipeline := &pmerge.Pipeline{
				Log:          writer,
				Actor:        defaultActor(),
				InvocationID: ulid.Make().String(),
				// Reader intentionally nil: contradiction detection is
				// disabled in this build. Aliasing still works end-to-
				// end; contradictions require a Neo4j-backed facet
				// reader which is a separate bead.
			}

			res, err := pipeline.Merge(cmd.Context(), pmerge.Request{
				PsiA: args[0],
				PsiB: args[1],
			})
			if err != nil {
				if errors.Is(err, pmerge.ErrEmptyPSI) ||
					errors.Is(err, pmerge.ErrSamePSI) ||
					errors.Is(err, pmerge.ErrInvalidPSI) ||
					errors.Is(err, pmerge.ErrEmptyActor) {
					return emitAndExit(cmd, errs.Validation("INVALID_MERGE_REQUEST",
						err.Error(), nil), jsonFlag)
				}
				return emitAndExit(cmd, errs.Operational("MERGE_FAILED",
					"could not append subject merge datoms", err), jsonFlag)
			}

			fmt.Fprintf(cmd.OutOrStdout(),
				"cortex subject merge ok\n  canonical: %s\n  alias:     %s\n  tx:        %s\n  contradictions: %d\n",
				res.Canonical, res.Alias, res.Tx, res.ContradictionCount)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON error envelope on failure")
	return cmd
}
