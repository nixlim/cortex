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
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/analyze"
	"github.com/nixlim/cortex/internal/errs"
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

			// Build the pipeline. In Phase 1 the ClusterSource and
			// FrameProposer need Neo4j + Ollama adapters. When those
			// land (adapter-dev wiring), buildAnalyzePipeline returns a
			// fully functional pipeline. Until then it returns an
			// operational error so the user knows which adapter is
			// missing rather than getting a nil-pointer crash.
			pipeline, err := buildAnalyzePipeline()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}

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

// buildAnalyzePipeline constructs the analyze.Pipeline from config and
// live adapters. Phase 1 returns an operational error until the Neo4j
// cluster source and Ollama proposer adapters are wired.
func buildAnalyzePipeline() (*analyze.Pipeline, error) {
	// TODO(adapter-dev): wire Neo4j ClusterSource, Ollama FrameProposer,
	// log.Writer, and CommunityRefresher here. The pipeline shape and
	// all thresholds are ready in internal/analyze. This function will
	// mirror buildObservePipeline's pattern: load config, open log,
	// build adapters, return pipeline + cleanup.
	return nil, errs.Operational("ANALYZE_NOT_WIRED",
		"cortex analyze --find-patterns requires Neo4j and Ollama adapters "+
			"that are not yet wired in the CLI; the pipeline is fully "+
			"exercised by internal/analyze unit tests", nil)
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
