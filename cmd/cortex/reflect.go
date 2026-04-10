// cmd/cortex/reflect.go wires `cortex reflect` onto the reflect.Pipeline.
//
// The command parses flags, validates inputs, and constructs a
// production pipeline. In Phase 1 the ClusterSource (Neo4j Leiden
// enumeration with cosine/MDL scoring) and FrameProposer (Ollama
// frame-proposal prompt with JSON parsing) adapters are still owned
// by adapter-dev; until they land, buildReflectPipeline returns an
// operational error that names the missing adapter so operators get
// a precise diagnostic instead of the generic "not implemented".
//
// This file still owns the full CLI surface (flags, renderers, exit
// codes), so the only change required to light up reflect end-to-end
// once the adapters exist is a drop-in replacement for
// buildReflectPipeline.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-3, FR-018/019/020/052
//	bead cortex-4kq.44
package main

import (
	"encoding/json"
	"fmt"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/reflect"
)

// newReflectCmdReal returns the production-wired `cortex reflect`
// command. It replaces the notImplemented stub in commands.go.
func newReflectCmdReal() *cobra.Command {
	var (
		dryRun   bool
		explain  bool
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "reflect",
		Short: "Promote episodic clusters into typed frames",
		Long: "cortex reflect reads the per-frame reflection watermark, asks the " +
			"cluster source for candidates committed after that watermark, applies " +
			"the four threshold rules (size, distinct timestamps, cosine floor, " +
			"MDL ratio), asks the LLM to propose a frame, and on acceptance appends " +
			"the frame datoms and advances the watermark. --dry-run skips the log " +
			"writes; --explain records a rejection reason per candidate.",
		RunE: func(cmd *cobra.Command, args []string) error {
			pipeline, cleanup, err := buildReflectPipeline()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			res, err := pipeline.Reflect(cmd.Context(), reflect.RunOptions{
				DryRun:  dryRun,
				Explain: explain,
			})
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			return renderReflectResult(cmd, res, reflect.RunOptions{DryRun: dryRun, Explain: explain}, jsonFlag)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate candidates without writing frames")
	cmd.Flags().BoolVar(&explain, "explain", false, "print rejection reasons per candidate")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// buildReflectPipeline constructs a reflect.Pipeline from live
// adapters. Phase 1 returns an operational error with a precise code
// until the Neo4j cluster source, Neo4j reflection-watermark store,
// and Ollama frame proposer adapters are wired (see cortex-4kq.44
// adapter-dev follow-up beads). The pipeline itself is fully exercised
// by internal/reflect/pipeline_test.go.
func buildReflectPipeline() (*reflect.Pipeline, func(), error) {
	// TODO(adapter-dev): build Neo4j ClusterSource (Leiden/Louvain
	// stream + exemplar cosine/MDL scoring), Neo4j WatermarkStore
	// (Reflection node label), Ollama FrameProposer (frame_proposal
	// prompt + JSON parsing), and a log.Writer LogAppender. Signature
	// mirrors buildObservePipeline: load config, open log, build
	// adapters, return pipeline + cleanup.
	_ = ulid.Make // keep imports stable once wired
	return nil, func() {}, errs.Operational("REFLECT_NOT_WIRED",
		"cortex reflect requires a Neo4j ClusterSource, Neo4j reflection "+
			"watermark store, and Ollama FrameProposer that are not yet "+
			"wired in the CLI; the pipeline is fully exercised by "+
			"internal/reflect unit tests", nil)
}

// renderReflectResult prints a human-readable summary or a JSON
// envelope of the reflect run.
func renderReflectResult(cmd *cobra.Command, res *reflect.Result, opts reflect.RunOptions, jsonFlag bool) error {
	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	w := cmd.OutOrStdout()
	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(w, "%s%d candidates evaluated, %d accepted, watermark=%s\n",
		prefix, len(res.Outcomes), len(res.Accepted), res.Watermark)
	if opts.Explain {
		for _, o := range res.Outcomes {
			status := "accepted"
			if !o.Accepted {
				status = string(o.Reason)
			}
			fmt.Fprintf(w, "  cluster=%s  %s\n", o.Cluster.ID, status)
		}
	}
	return nil
}
