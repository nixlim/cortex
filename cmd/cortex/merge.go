// cmd/cortex/merge.go wires `cortex merge` onto the
// internal/pipeline/mergeseg package. The command validates one or
// more external segment files and renames each into ~/.cortex/log.d/
// so the next read sees the deduplicated union.
//
// Replaces the notImplemented stub in commands.go.
package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/pipeline/mergeseg"
)

// newMergeCmdReal returns the wired `cortex merge` command.
func newMergeCmdReal() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge <segment-path>...",
		Short: "Merge external log segments into ~/.cortex/log.d",
		Long: "cortex merge validates every datom's checksum in each external " +
			"segment file and renames it into the local log directory. The " +
			"merge-sort reader handles deduplication of overlapping tx values, " +
			"so merging log B into log A produces a reader whose tx set is the " +
			"deduplicated union of both inputs. A checksum failure leaves the " +
			"external file untouched and exits 1.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), false)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			for _, srcPath := range args {
				res, err := mergeseg.Merge(srcPath, segDir)
				if err != nil {
					if errors.Is(err, mergeseg.ErrChecksumMismatch) {
						return emitAndExit(cmd, errs.Operational("CHECKSUM_MISMATCH",
							fmt.Sprintf("external segment %s failed checksum verification; not imported", srcPath),
							err), false)
					}
					return emitAndExit(cmd, errs.Operational("MERGE_FAILED",
						fmt.Sprintf("could not merge %s into log dir", srcPath), err), false)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"cortex merge: imported %s -> %s (%d datoms, %d tx)\n",
					res.SourcePath, res.DestPath, res.DatomCount, res.TxCount)
			}
			return nil
		},
	}
	return cmd
}
