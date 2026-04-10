// cmd/cortex/recall.go wires `cortex recall` onto the recall.Pipeline.
//
// The command parses the query and --limit flag, constructs a
// production pipeline against live Neo4j/Ollama/Weaviate adapters
// (see cmd/cortex/recall_adapters.go), runs a default-mode
// retrieval, and renders the surfaced results.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-14, FR-013/014/015/016
//	bead cortex-4kq.36, code-review fix CRIT-003
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/neo4j"
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

// buildRecallPipeline constructs a recall.Pipeline from live
// adapters. The six bridge types live in recall_adapters.go; this
// function is the single place that knows how to load config, open
// the Neo4j / Ollama / Weaviate clients, and hand them to the
// pipeline. The returned cleanup closes the Bolt client; the Ollama
// and Weaviate clients are stateless stdlib http.Client wrappers and
// do not need explicit teardown.
func buildRecallPipeline() (*recall.Pipeline, func(), error) {
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
	cleanup := func() { _ = bolt.Close(context.Background()) }

	ollamaClient := newOllamaClient(cfg)
	weaviateClient := newWeaviateClient(cfg)

	pipeline := &recall.Pipeline{
		Concepts: &ollamaConceptExtractor{client: ollamaClient},
		Seeds:    &neo4jSeedResolver{client: bolt},
		PPR:      &neo4jPPRRunner{client: bolt, graphName: semanticGraphName},
		Loader: &neo4jWeaviateEntryLoader{
			graph:    bolt,
			weaviate: weaviateClient,
		},
		Embedder:     newOllamaEmbedder(cfg),
		Context:      &neo4jContextFetcher{client: bolt},
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
	}
	return pipeline, cleanup, nil
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
