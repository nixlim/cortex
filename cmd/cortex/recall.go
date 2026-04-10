// cmd/cortex/recall.go wires `cortex recall` onto the recall.Pipeline.
//
// The command parses the query and --limit flag, constructs a
// production pipeline, runs a default-mode retrieval, and renders the
// surfaced results. In Phase 1 the six adapter interfaces the recall
// pipeline needs (ConceptExtractor, SeedResolver, PPRRunner,
// EntryLoader, QueryEmbedder, ContextFetcher) are not yet wired —
// buildRecallPipeline returns a precise operational error naming the
// missing adapter set so the failure mode is actionable instead of a
// generic "not implemented".
//
// This file owns the full CLI surface (flags, rendering, exit codes)
// so the only change required to light recall up end-to-end once the
// adapters exist is a drop-in replacement for buildRecallPipeline.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-14, FR-013/014/015/016
//	bead cortex-4kq.36
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/recall"
)

// newRecallCmdReal returns the production-wired `cortex recall`
// command. It replaces the notImplemented stub in commands.go.
func newRecallCmdReal() *cobra.Command {
	var (
		limit    int
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "recall <query>",
		Short: "Retrieve entries matching a query",
		Long: "cortex recall runs the default-mode retrieval pipeline: concept " +
			"extraction -> seed resolution -> Personalized PageRank -> entry load " +
			"-> ACT-R activation rerank -> trail/community context attachment. " +
			"Every surfaced entry is reinforced with an activation-update datom.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.TrimSpace(strings.Join(args, " "))
			if query == "" {
				return emitAndExit(cmd, errs.Validation("EMPTY_QUERY",
					"cortex recall requires a non-empty query", nil), jsonFlag)
			}
			pipeline, cleanup, err := buildRecallPipeline()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			res, err := pipeline.Recall(cmd.Context(), recall.Request{
				Query: query,
				Limit: limit,
			})
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			return renderRecallResult(cmd, res, jsonFlag)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "override retrieval.default_limit (10)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// buildRecallPipeline constructs a recall.Pipeline from live adapters.
// Phase 1 returns an operational error with a precise code until the
// six adapter interfaces (ConceptExtractor, SeedResolver, PPRRunner,
// EntryLoader, QueryEmbedder, ContextFetcher) are wired — see
// cortex-4kq.36 adapter-dev follow-ups. The pipeline itself is fully
// exercised by internal/recall/pipeline_test.go.
func buildRecallPipeline() (*recall.Pipeline, func(), error) {
	// TODO(adapter-dev): build the six adapters recall needs:
	//   - ConceptExtractor  -> Ollama Generate + prompts.NameConceptExtraction
	//   - SeedResolver      -> Neo4j concept->entry lookup
	//   - PPRRunner         -> Neo4j GDS PersonalizedPageRank (gds.go)
	//   - EntryLoader       -> Neo4j + Weaviate bulk fetch
	//   - QueryEmbedder     -> Ollama /api/embeddings (reuse observeEmbedder)
	//   - ContextFetcher    -> Neo4j trail/community lookup
	// Mirror buildObservePipeline's pattern: load config, open log,
	// build adapters, return pipeline + cleanup.
	return nil, func() {}, errs.Operational("RECALL_NOT_WIRED",
		"cortex recall requires Neo4j (ConceptExtractor, SeedResolver, "+
			"PPRRunner, EntryLoader, ContextFetcher) and Ollama "+
			"(QueryEmbedder) adapters that are not yet wired in the CLI; "+
			"the pipeline is fully exercised by internal/recall unit tests", nil)
}

// renderRecallResult prints the surfaced entries as either a
// human-readable summary or a JSON envelope. The ReinforcementDatoms
// slice is returned by the pipeline but the caller is responsible for
// appending those datoms to the log after rendering — that log write
// will be added here once the underlying pipeline produces results.
func renderRecallResult(cmd *cobra.Command, res *recall.Response, jsonFlag bool) error {
	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "results=%d\n", len(res.Results))
	for i, r := range res.Results {
		fmt.Fprintf(w, "  %d. %s  score=%.3f  base=%.3f  ppr=%.3f  sim=%.3f\n",
			i+1, r.EntryID, r.Score, r.BaseActivation, r.PPRScore, r.Similarity)
	}
	return nil
}
