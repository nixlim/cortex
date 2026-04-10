// cmd/cortex/export.go wires `cortex export` onto the
// internal/pipeline/export package. The command opens the multi-
// segment merge-sort reader, hands it to export.Run (or export.ToFile
// when --out is supplied), and reports the datom count to stderr.
//
// Replaces the notImplemented stub in commands.go.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/pipeline/export"
)

// newExportCmdReal returns the wired `cortex export` command.
func newExportCmdReal() *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Merge all segments into one tx-sorted stream",
		Long: "cortex export serializes the merged tx-sorted datom stream from " +
			"~/.cortex/log.d to stdout (or to --out=<path>, created with mode " +
			"0600). The format is canonical JSONL: byte-identical to a fresh " +
			"segment write, suitable as a backup artifact insulated from segment " +
			"file layout.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), false)
			}
			segDir := expandHome(cfg.Log.SegmentDir)
			report, err := log.Load(segDir, log.LoadOptions{})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
					"could not enumerate log segments", err), false)
			}

			if outPath != "" {
				n, err := export.ToFile(report.Healthy, outPath)
				if err != nil {
					return emitAndExit(cmd, errs.Operational("EXPORT_FAILED",
						"failed to write export file", err), false)
				}
				fmt.Fprintf(cmd.ErrOrStderr(),
					"cortex export: wrote %d datoms to %s\n", n, outPath)
				return nil
			}
			if _, err := export.Run(report.Healthy, os.Stdout); err != nil {
				return emitAndExit(cmd, errs.Operational("EXPORT_FAILED",
					"failed to write export stream", err), false)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "output path for the merged stream (default stdout)")
	return cmd
}
