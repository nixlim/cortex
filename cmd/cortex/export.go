// cmd/cortex/export.go wires `cortex export` onto internal/exportlog.
// The command opens the multi-segment merge-sort reader, hands it to
// exportlog.Stream, and writes the JSONL stream to stdout or to the
// path supplied via --out. The output file is created with mode
// 0600 per the spec acceptance criterion.
//
// Replaces the notImplemented stub in commands.go.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/exportlog"
	"github.com/nixlim/cortex/internal/log"
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
			reader, err := log.NewReader(report.Healthy)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_READER_FAILED",
					"could not open multi-segment log reader", err), false)
			}
			source := &exportReaderSource{r: reader}

			// Output destination: stdout or a 0600-mode file. The
			// file is closed after Stream returns; we don't fsync
			// because export is a backup operation, not a
			// commit-point write — the operator can re-run on
			// failure.
			var dst *os.File
			closeDst := func() {}
			if outPath == "" {
				dst = os.Stdout
			} else {
				f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
				if err != nil {
					return emitAndExit(cmd, errs.Operational("EXPORT_OPEN_FAILED",
						fmt.Sprintf("could not open %s for writing", outPath), err), false)
				}
				dst = f
				closeDst = func() { _ = f.Close() }
			}
			defer closeDst()

			res, err := exportlog.Stream(cmd.Context(), source, dst)
			if err != nil {
				return emitAndExit(cmd, err, false)
			}

			// Human-readable confirmation goes to stderr so it does
			// not contaminate the JSONL stream when --out is absent.
			if outPath != "" {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"cortex export: wrote %d datoms (%d bytes) to %s\n",
					res.DatomCount, res.BytesOut, outPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "output path for the merged stream (default stdout)")
	return cmd
}

// exportReaderSource adapts *log.Reader to exportlog.DatomSource.
// The log reader's Next() takes no context, so we honour cmd.Context()
// by checking ctx.Err() before each pull.
type exportReaderSource struct {
	r *log.Reader
}

func (s *exportReaderSource) Next(ctx context.Context) (datom.Datom, bool, error) {
	if err := ctx.Err(); err != nil {
		return datom.Datom{}, false, err
	}
	return s.r.Next()
}

func (s *exportReaderSource) Close() error { return s.r.Close() }
