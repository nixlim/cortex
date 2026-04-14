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
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/recall"
)

// newRecallCmdReal returns the production-wired `cortex recall`
// command. It replaces the notImplemented stub in commands.go.
func newRecallCmdReal() *cobra.Command {
	var (
		limit    int
		jsonFlag bool
		explain  bool
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
			pipeline, writer, cleanup, err := buildRecallPipeline()
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
			if err := renderRecallResult(cmd, res, jsonFlag, explain); err != nil {
				return err
			}
			// FR-015: every recall reinforces the surfaced entries.
			// The pipeline emits the reinforcement datoms unsealed; the
			// CLI is the owner of the log writer (per the recall package
			// contract that "the pipeline never opens its own log
			// handle"), so sealing + appending happens here, after the
			// response is rendered. A reinforcement-append failure does
			// NOT roll back the rendered response — the user already saw
			// the results, and self-heal will retry the apply on the
			// next command.
			if err := appendReinforcementDatoms(writer, res); err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "override retrieval.default_limit (10)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	cmd.Flags().BoolVar(&explain, "explain", false, "print per-stage gate drop counts after results (cortex-9ti)")
	return cmd
}

// buildRecallPipeline constructs a recall.Pipeline from live
// adapters. The six bridge types live in recall_adapters.go; this
// function is the single place that knows how to load config, open
// the Neo4j / Ollama / Weaviate clients, and hand them to the
// pipeline. It also opens the segment writer the CLI uses to append
// the FR-015 reinforcement datoms after the response is rendered.
// The returned cleanup closes both the Bolt client and the writer.
// The Ollama and Weaviate clients are stateless stdlib http.Client
// wrappers and do not need explicit teardown.
func buildRecallPipeline() (*recall.Pipeline, *log.Writer, func(), error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, func() {}, errs.Operational("CONFIG_LOAD_FAILED",
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
		return nil, nil, func() {}, errs.Operational("NEO4J_UNAVAILABLE",
			"could not open Neo4j Bolt client", err)
	}

	segDir := expandHome(cfg.Log.SegmentDir)
	writer, err := log.NewWriter(segDir)
	if err != nil {
		_ = bolt.Close(context.Background())
		return nil, nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}
	cleanup := func() {
		_ = writer.Close()
		_ = bolt.Close(context.Background())
	}

	generator, err := newGenerator(cfg, time.Duration(cfg.Timeouts.ConceptExtractionSeconds)*time.Second)
	if err != nil {
		_ = writer.Close()
		_ = bolt.Close(context.Background())
		return nil, nil, func() {}, errs.Operational("LLM_CONFIG_INVALID",
			"could not construct LLM generator", err)
	}
	weaviateClient := newWeaviateClient(cfg)

	pipeline := &recall.Pipeline{
		Concepts: &ollamaConceptExtractor{client: generator},
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
		// Wire activation tunables from config so the loaded
		// retrieval.forgetting.visibility_threshold and
		// retrieval.activation.decay_exponent actually take effect.
		// Without this, recall.Pipeline.fillDefaults silently falls
		// back to the package-level activation constants (0.05 / 0.5)
		// and any user override is ignored. See bead cortex-upp.
		VisibilityThreshold: cfg.Retrieval.Forgetting.VisibilityThreshold,
		DecayExponent:       cfg.Retrieval.Activation.DecayExponent,
		SeedTopK:            cfg.Retrieval.PPR.SeedTopK,
		Damping:             cfg.Retrieval.PPR.Damping,
		MaxIterations:       cfg.Retrieval.PPR.MaxIterations,
		// Layered relevance gate (cortex-y6g). Precedence:
		// explicit relevance_gate.sim_floor_strict > legacy
		// relevance_floor > spec default 0.55. The hard floor and
		// rescue alpha fall back to fillDefaults if the sub-struct
		// was omitted from config.yaml.
		SimFloorHard:   cfg.Retrieval.RelevanceGate.SimFloorHard,
		SimFloorStrict: resolveSimFloorStrict(cfg.Retrieval),
		RescueAlpha:    cfg.Retrieval.RelevanceGate.RescueAlpha,
		CompositeFloor:  cfg.Retrieval.RelevanceGate.CompositeFloor,
		GateSimWeight:   cfg.Retrieval.RelevanceGate.GateSimWeight,
		GatePPRWeight:   cfg.Retrieval.RelevanceGate.GatePPRWeight,
		PPRBaselineMinN: cfg.Retrieval.RelevanceGate.PPRBaselineMinN,
		RelevanceFloor:  cfg.Retrieval.RelevanceFloor,
		// Pipeline default limit sourced from config so
		// retrieval.default_limit actually flows through (bead
		// cortex-voa). A positive --limit flag on Request overrides
		// this value per-call.
		Limit: cfg.Retrieval.DefaultLimit,
	}
	return pipeline, writer, cleanup, nil
}

// resolveSimFloorStrict picks the effective strict floor using the
// cortex-y6g precedence: an explicit relevance_gate.sim_floor_strict
// wins; otherwise the legacy retrieval.relevance_floor is promoted.
// Both zero disables the gate — Defaults() populates both fields with
// 0.55, so callers reach the both-zero state only by explicitly
// overriding both to zero in config.yaml, which is the documented
// way to opt out of the relevance gate entirely.
func resolveSimFloorStrict(r config.RetrievalConfig) float64 {
	if r.RelevanceGate.SimFloorStrict > 0 {
		return r.RelevanceGate.SimFloorStrict
	}
	return r.RelevanceFloor
}

// appendReinforcementDatoms seals every datom in res.ReinforcementDatoms
// and appends them as a single transaction group to the supplied
// writer. The pipeline emits the datoms unsealed (so callers can
// stamp their own tx/invocation fields), so the CLI must Seal each
// one immediately before Append. A nil response or empty slice is a
// no-op.
func appendReinforcementDatoms(writer *log.Writer, res *recall.Response) error {
	if res == nil || len(res.ReinforcementDatoms) == 0 {
		return nil
	}
	group := make([]datom.Datom, 0, len(res.ReinforcementDatoms))
	for i := range res.ReinforcementDatoms {
		d := res.ReinforcementDatoms[i]
		if err := d.Seal(); err != nil {
			return errs.Operational("REINFORCEMENT_SEAL_FAILED",
				"could not seal recall reinforcement datom", err)
		}
		group = append(group, d)
	}
	if _, err := writer.Append(group); err != nil {
		return errs.Operational("REINFORCEMENT_APPEND_FAILED",
			"could not append recall reinforcement datoms", err)
	}
	return nil
}

// renderRecallResult prints the surfaced entries as either a
// human-readable summary or a JSON envelope. The ReinforcementDatoms
// slice is returned by the pipeline but the caller is responsible for
// appending those datoms to the log after rendering — that log write
// will be added here once the underlying pipeline produces results.
func renderRecallResult(cmd *cobra.Command, res *recall.Response, jsonFlag, explain bool) error {
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
		if body := strings.TrimSpace(r.Body); body != "" {
			fmt.Fprintf(w, "     %s\n", body)
		}
	}
	if explain {
		fmt.Fprintln(w, formatGateDrops(res.Diagnostics))
	}
	return nil
}

// formatGateDrops renders a one-line summary of per-stage gate drops
// for the `cortex recall --explain` footer. Stage order is fixed so
// operators can eyeball the same layout across runs. Empty or all-zero
// maps render as "gate drops: none".
func formatGateDrops(diag recall.Diagnostics) string {
	stages := []string{
		recall.StageHardSimFloor,
		recall.StageRescuePath,
		recall.StageCompositeFloor,
	}
	parts := make([]string, 0, len(stages))
	for _, st := range stages {
		if n := diag.DroppedByStage[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", st, n))
		}
	}
	if len(parts) == 0 {
		return "gate drops: none"
	}
	return "gate drops: " + strings.Join(parts, ", ")
}
