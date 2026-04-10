// cmd/cortex/history.go wires `cortex history` and `cortex as-of`
// onto the internal/history package. Both subcommands are read-only:
// they enumerate segments, hand them to the helpers, and render in
// human or JSON form.
//
// Replaces the notImplemented stubs in commands.go for newHistoryCmd
// and newAsOfCmd.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/history"
	"github.com/nixlim/cortex/internal/log"
)

// newHistoryCmdReal returns the wired `cortex history` command. The
// caller (commands.go) installs it in place of the stub.
func newHistoryCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "history <entity-id>",
		Short: "Show the full retract-aware history of an entity",
		Long: "cortex history walks the segmented datom log and prints every datom " +
			"whose entity field equals <entity-id>, in tx-ULID-ascending order. " +
			"Activation reinforcements, retractions, and any other attributes are " +
			"all preserved verbatim — the command never collapses by attribute, " +
			"so the operator sees the full lineage that the spec guarantees.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entityID := args[0]
			segments, err := loadHealthySegments(jsonFlag, cmd)
			if err != nil {
				return err
			}
			lineage, err := history.History(segments, entityID)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(lineage)
			}
			renderHistory(cmd, lineage)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// renderHistory prints one row per datom in a column-aligned form
// suitable for terminal review. The format is intentionally terse:
// tx, ts, op, attribute, then the raw value bytes.
func renderHistory(cmd *cobra.Command, lineage []datom.Datom) {
	w := cmd.OutOrStdout()
	if len(lineage) == 0 {
		fmt.Fprintln(w, "(no datoms for this entity)")
		return
	}
	for _, d := range lineage {
		fmt.Fprintf(w, "%s  %s  %-8s  %-20s  %s\n",
			d.Tx, d.Ts, d.Op, d.A, string(d.V))
	}
}

// newAsOfCmdReal returns the wired `cortex as-of` command. AsOf takes
// a single transaction id argument and prints every entry-prefixed
// entity that existed at or before that tx, satisfying both AC2 (tx
// cutoff isolation) and AC3 (NOT_FOUND on unknown tx → exit 1).
func newAsOfCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "as-of <tx-id>",
		Short: "Query Cortex as it was at a given transaction",
		Long: "cortex as-of restricts visibility to datoms whose tx is at or before " +
			"the given transaction id and prints the entries visible at that " +
			"point. The id must exist in the log; an unknown tx exits 1 with " +
			"NOT_FOUND.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			txID := args[0]
			segments, err := loadHealthySegments(jsonFlag, cmd)
			if err != nil {
				return err
			}
			views, err := history.AsOf(segments, txID)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(views)
			}
			w := cmd.OutOrStdout()
			for _, v := range views {
				fmt.Fprintf(w, "%s  introduced_at=%s\n", v.Entity, v.Tx)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// loadHealthySegments centralizes the config-load + log.Load dance
// shared by history and as-of. It returns the healthy-segment slice
// or, on failure, an *exitCodeErr already routed through emitAndExit
// so the caller can simply propagate it.
func loadHealthySegments(jsonMode bool, cmd *cobra.Command) ([]string, error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err), jsonMode)
	}
	segDir := expandHome(cfg.Log.SegmentDir)
	report, err := log.Load(segDir, log.LoadOptions{})
	if err != nil {
		return nil, emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
			"could not enumerate log segments", err), jsonMode)
	}
	return report.Healthy, nil
}
