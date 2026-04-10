// cmd/cortex/analyze.go wires `cortex analyze --find-patterns` onto
// the analyze.Pipeline. The command file parses flags, builds the
// minimum dependency set, runs the pipeline, and renders results in
// human or JSON form.
//
// Phase 1 note: the ClusterSource and FrameProposer adapters (Neo4j
// cluster enumeration and Ollama frame proposal) are wired in
// adapter-dev's beads. Until those adapters land, running `cortex
// analyze --find-patterns` returns an operational error identifying
// the missing adapter. The pipeline itself is fully exercised by
// the unit tests in internal/analyze/pipeline_test.go.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-4 (cross-project analysis BDDs)
//	bead cortex-4kq.52
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/analyze"
	"github.com/nixlim/cortex/internal/community"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
)

// newAnalyzeCmdReal returns the production-wired `cortex analyze`
// command. It replaces the notImplemented stub in commands.go.
func newAnalyzeCmdReal() *cobra.Command {
	var (
		findPatterns    bool
		includeMigrated bool
		dryRun          bool
		explain         bool
		jsonFlag        bool
	)
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Run cross-project pattern analysis",
		Long: "cortex analyze --find-patterns accepts clusters spanning at least 2 " +
			"distinct projects with no more than 70% of exemplars from any single " +
			"project, applies the relaxed MDL ratio of 1.15, marks accepted frames " +
			"cross_project=true, boosts importance by +0.20, and performs a full " +
			"community refresh.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !findPatterns {
				return emitAndExit(cmd, errs.Validation("MISSING_FIND_PATTERNS",
					"cortex analyze requires --find-patterns in Phase 1", nil), jsonFlag)
			}

			pipeline, cleanup, err := buildAnalyzePipeline()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			opts := analyze.RunOptions{
				DryRun:          dryRun,
				Explain:         explain,
				IncludeMigrated: includeMigrated,
			}
			res, err := pipeline.Analyze(cmd.Context(), opts)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}

			if jsonFlag {
				return renderAnalyzeJSON(cmd, res)
			}
			renderAnalyzeHuman(cmd, res, opts)
			return nil
		},
	}
	cmd.Flags().BoolVar(&findPatterns, "find-patterns", false, "enable cross-project pattern finding")
	cmd.Flags().BoolVar(&includeMigrated, "include-migrated", false, "include migrated entries in cluster selection")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate candidates without writing frames")
	cmd.Flags().BoolVar(&explain, "explain", false, "print rejection reasons per candidate")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// buildAnalyzePipeline constructs the analyze.Pipeline from config
// and live Neo4j / Ollama / log adapters. The bridge adapter types
// live in analyze_adapters.go. The returned cleanup closes the Bolt
// client and the log writer.
func buildAnalyzePipeline() (*analyze.Pipeline, func(), error) {
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

	ollamaClient := newOllamaClient(cfg)

	// Community refresher: Leiden preferred, Louvain fallback, with
	// the Ollama-backed summariser for any regenerated summaries.
	detector := &community.Detector{
		Neo4j:        bolt,
		LeidenQuery:  neo4j.LeidenStreamQuery,
		LouvainQuery: neo4j.LouvainStreamQuery,
		TopNodeCount: 32,
	}
	refresher := &community.Refresher{
		Neo4j:      bolt,
		Summarizer: &ollamaCommunitySummarizer{client: ollamaClient},
	}
	refreshBridge := &communityRefresherBridge{
		detector:  detector,
		refresher: refresher,
		graphName: semanticGraphName,
		cfg: community.Config{
			GraphName:     semanticGraphName,
			Resolutions:   []float64{1.0, 0.5, 0.1},
			Levels:        3,
			MaxIterations: 10,
			Tolerance:     0.0001,
		},
	}

	pipeline := &analyze.Pipeline{
		Source:       &neo4jAnalyzeClusterSource{client: bolt},
		Proposer:     &ollamaFrameProposer{client: ollamaClient, source: "analyze"},
		Log:          writer,
		Community:    refreshBridge,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
	}
	return pipeline, cleanup, nil
}

func renderAnalyzeJSON(cmd *cobra.Command, res *analyze.Result) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func renderAnalyzeHuman(cmd *cobra.Command, res *analyze.Result, opts analyze.RunOptions) {
	w := cmd.OutOrStdout()
	prefix := ""
	if opts.DryRun {
		prefix = "[dry-run] "
	}
	fmt.Fprintf(w, "%s%d candidates evaluated, %d accepted\n",
		prefix, len(res.Outcomes), len(res.Accepted))
	if opts.Explain {
		for _, o := range res.Outcomes {
			status := "accepted"
			if !o.Accepted {
				status = string(o.Reason)
			}
			fmt.Fprintf(w, "  cluster=%s  %s\n", o.Cluster.ID, status)
		}
	}
	if res.CommunityRefresh {
		fmt.Fprintln(w, "Community refresh completed.")
	}
}
