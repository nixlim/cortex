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
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
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
// Neo4j / Ollama / log adapters. Bridge types live in
// reflect_adapters.go. Returns a cleanup that closes the Bolt client
// and the log writer.
func buildReflectPipeline() (*reflect.Pipeline, func(), error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, func() {}, errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err)
	}

	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})
	if err != nil {
		return nil, func() {}, errs.Operational("NEO4J_UNAVAILABLE",
			"could not open Neo4j Bolt client", err)
	}

	segDir := expandHome(cfg.Log.SegmentDir)
	writer, err := log.NewWriter(segDir)
	if err != nil {
		_ = bolt.Close(context.Background())
		return nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}
	cleanup := func() {
		_ = writer.Close()
		_ = bolt.Close(context.Background())
	}

	generator, err := newGenerator(cfg, time.Duration(cfg.Timeouts.ReflectionSeconds)*time.Second)
	if err != nil {
		_ = writer.Close()
		_ = bolt.Close(context.Background())
		return nil, func() {}, errs.Operational("LLM_CONFIG_INVALID",
			"could not construct LLM generator", err)
	}
	pipeline := &reflect.Pipeline{
		Source:       &neo4jReflectClusterSource{client: bolt},
		Proposer:     &reflectFrameProposerBridge{client: generator},
		Watermark:    &neo4jReflectionWatermarkStore{client: bolt},
		Log:          writer,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
	}
	return pipeline, cleanup, nil
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
