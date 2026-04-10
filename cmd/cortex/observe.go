// cmd/cortex/observe.go wires the `cortex observe` subcommand onto
// the write.Pipeline. The command file is kept intentionally thin:
// everything that can be validated or structured lives in
// internal/write; this file only parses flags, assembles the minimum
// set of dependencies a one-shot invocation needs, and renders the
// final outcome.
//
// The subcommand is registered from commands.go — newObserveCmdReal
// replaces the stub there.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/prompts"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/nixlim/cortex/internal/write"
)

// newObserveCmdReal builds the production-ready observe command. It
// replaces the stub in commands.go once features-dev landed the
// write pipeline. The command follows the Cortex error-envelope
// convention: validation failures exit 2 via errs.Emit, operational
// failures exit 1, success prints the entry id on stdout and exits 0.
func newObserveCmdReal() *cobra.Command {
	var (
		kindFlag    string
		facetFlag   []string
		subjectFlag string
		trailFlag   string
		jsonFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "observe <body>",
		Short: "Write an observation entry through the standard pipeline",
		Long: "cortex observe validates the request, scans for secrets, resolves the " +
			"subject PSI, appends a transaction-group to the log, and applies the " +
			"datoms to Neo4j and Weaviate. The command exits 0 and prints the new " +
			"entry id on success, 2 on validation failure, and 1 on operational " +
			"failure. Backend apply errors never roll back the committed log.",
		Args: cobra.ArbitraryArgs, // empty body is a validation error, not a usage error
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")

			facets, ferr := parseFacetFlag(facetFlag)
			if ferr != nil {
				return emitAndExit(cmd, ferr, jsonFlag)
			}

			// Load config so the log.d path and timeouts match the
			// operator's environment. A missing config file is fine
			// — Load returns the defaults.
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			pipeline, cleanup, err := buildObservePipeline(segDir, cfg)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			req := write.ObserveRequest{
				Body:    body,
				Kind:    kindFlag,
				Facets:  facets,
				Subject: subjectFlag,
				TrailID: resolveTrailID(trailFlag),
			}
			res, err := pipeline.Observe(cmd.Context(), req)
			if err != nil {
				// A partial success (log committed but backend apply
				// failed) still prints the entry id so ops tooling
				// can log it. res is non-nil in that case.
				if res != nil && res.EntryID != "" {
					fmt.Fprintln(cmd.OutOrStdout(), res.EntryID)
				}
				return emitAndExit(cmd, err, jsonFlag)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.EntryID)
			return nil
		},
	}
	cmd.Flags().StringVar(&kindFlag, "kind", "", "frame type (Observation | SessionReflection | ObservedRace)")
	cmd.Flags().StringSliceVar(&facetFlag, "facets", nil, "comma-separated key:value facet list (must include domain, project)")
	cmd.Flags().StringVar(&subjectFlag, "subject", "", "optional PSI subject to attach (canonical or alias)")
	cmd.Flags().StringVar(&trailFlag, "trail", "", "optional trail id to attach this observation to")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON error envelope on failure")
	return cmd
}

// parseFacetFlag converts the --facets=key:value,key:value slice that
// cobra produced into a map. A malformed entry (missing colon, empty
// key, empty value) is surfaced as a validation error so the CLI
// exits 2 with a precise envelope rather than silently dropping the
// bad token.
func parseFacetFlag(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.Index(entry, ":")
		if idx <= 0 || idx == len(entry)-1 {
			return nil, errs.Validation("MALFORMED_FACET",
				fmt.Sprintf("facet %q must be key:value", entry),
				map[string]any{"facet": entry})
		}
		k := strings.TrimSpace(entry[:idx])
		v := strings.TrimSpace(entry[idx+1:])
		if k == "" || v == "" {
			return nil, errs.Validation("MALFORMED_FACET",
				fmt.Sprintf("facet %q has empty key or value", entry),
				map[string]any{"facet": entry})
		}
		out[k] = v
	}
	return out, nil
}

// buildObservePipeline constructs a write.Pipeline with the full set
// of dependencies a one-shot observe invocation requires: a log writer
// pointing at segDir, the built-in secret detector, a fresh
// per-invocation PSI registry, an Ollama-backed Embedder so the FR-051
// embedding_model_name / embedding_model_digest datoms get captured on
// every entry, and concrete Neo4j + Weaviate BackendAppliers so the
// post-commit apply phase actually mutates the live backends. A failed
// Bolt connection is non-fatal at construction time — the pipeline
// simply runs without the Neo4j applier; the log commit is still
// authoritative and the next command's self-heal replay will catch up
// the missing apply (FR-004).
func buildObservePipeline(segDir string, cfg config.Config) (*write.Pipeline, func(), error) {
	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		return nil, func() {}, errs.Operational("SECRETS_INIT_FAILED",
			"could not initialize secret detector", err)
	}
	writer, err := log.NewWriter(segDir)
	if err != nil {
		return nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}

	embedder := newOllamaEmbedder(cfg)
	ollamaClient := newOllamaClient(cfg)
	weaviateClient := newWeaviateClient(cfg)

	// Open a Bolt client for the Neo4j BackendApplier. Failure here
	// degrades to "no neo4j applier" rather than blocking the write —
	// the log commit is the authoritative step and self-heal will
	// replay the missing rows on the next command (FR-004).
	cfgPath := defaultConfigPath()
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, boltErr := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})

	cleanup := func() {
		_ = writer.Close()
		if bolt != nil {
			_ = bolt.Close(context.Background())
		}
	}

	var neoApplier write.BackendApplier
	if boltErr == nil {
		neoApplier = neo4j.NewBackendApplier(bolt)
	}
	weaviateApplier := weaviate.NewBackendApplier(weaviateClient)

	p := &write.Pipeline{
		Detector:     detector,
		Registry:     psi.NewRegistry(),
		Log:          writer,
		Embedder:     embedder,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
		Neo4j:        neoApplier,
		Weaviate:     weaviateApplier,
		Neighbors:    &weaviateNeighborFinder{client: weaviateClient},
		LinkProposer: &ollamaLinkProposer{client: ollamaClient},
		LinkConfig: write.LinkDerivationConfig{
			ConfidenceFloor:    cfg.LinkDerivation.ConfidenceFloor,
			SimilarCosineFloor: cfg.LinkDerivation.SimilarToCosineFloor,
		},
		LinkTopK:             5,
		ConceptsEnabled:      true,
		ExpectedEmbeddingDim: cfg.Ollama.EmbeddingVectorDim,
	}
	return p, cleanup, nil
}

// weaviateNeighborFinder bridges write.NeighborFinder onto a live
// Weaviate client by calling NearestNeighbors against the Entry class
// with the just-embedded body vector. The bridge maps each search hit's
// cortex_id property and recovered cosine similarity into a
// write.LinkCandidate. A nil client or a Weaviate failure surfaces as
// an empty candidate set so the link derivation pass becomes a no-op
// rather than blocking the write — link derivation is best-effort and
// must never roll back the source entry.
type weaviateNeighborFinder struct {
	client weaviate.Client
}

func (w *weaviateNeighborFinder) Neighbors(ctx context.Context, vector []float32, k int) ([]write.LinkCandidate, error) {
	if w.client == nil || k <= 0 || len(vector) == 0 {
		return nil, nil
	}
	hits, err := w.client.NearestNeighbors(ctx, weaviate.ClassEntry, vector, k, 0)
	if err != nil {
		return nil, err
	}
	out := make([]write.LinkCandidate, 0, len(hits))
	for _, h := range hits {
		cortexID, _ := h.Properties["cortex_id"].(string)
		if cortexID == "" {
			continue
		}
		out = append(out, write.LinkCandidate{
			TargetEntryID:    cortexID,
			CosineSimilarity: h.CosineSimilarity,
		})
	}
	return out, nil
}

// ollamaLinkProposer bridges write.LinkProposer onto a live Ollama
// generation client. It renders the link_derivation prompt with the
// source body and a serialized candidate block, calls Generate, and
// parses the response as JSON of the shape {"links": [{target, type,
// confidence}]}. Per the LinkProposer contract (and the bead's AC3),
// any unparseable response is treated as "no proposals" — the source
// entry is already committed and link derivation must not fail it.
type ollamaLinkProposer struct {
	client *ollama.HTTPClient
}

func (o *ollamaLinkProposer) Propose(ctx context.Context, sourceBody string, candidates []write.LinkCandidate) ([]write.LinkProposal, error) {
	if o.client == nil || len(candidates) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	for i, c := range candidates {
		fmt.Fprintf(&sb, "%d. id=%s cosine=%.3f\n", i+1, c.TargetEntryID, c.CosineSimilarity)
	}
	prompt, err := prompts.Render(prompts.NameLinkDerivation, prompts.Data{
		Body:       sourceBody,
		Candidates: sb.String(),
	})
	if err != nil {
		return nil, nil
	}
	raw, err := o.client.Generate(ctx, prompt)
	if err != nil {
		return nil, nil
	}
	// The model is instructed to return JSON only, but real models
	// occasionally wrap the object in stray prose; trim to the first
	// '{' and last '}' before unmarshaling.
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}
	var parsed struct {
		Links []struct {
			Target     string  `json:"target"`
			Type       string  `json:"type"`
			Confidence float64 `json:"confidence"`
		} `json:"links"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, nil
	}
	out := make([]write.LinkProposal, 0, len(parsed.Links))
	for _, l := range parsed.Links {
		out = append(out, write.LinkProposal{
			TargetEntryID: l.Target,
			LinkType:      l.Type,
			Confidence:    l.Confidence,
		})
	}
	return out, nil
}

// observeEmbedder adapts an *ollama.HTTPClient to the write.Embedder
// interface. The adapter exists for two reasons:
//
//  1. write.Embedder requires ModelDigest(ctx) (name, digest, err) so
//     the pipeline can emit FR-051 datoms; ollama.HTTPClient exposes
//     this through its Show(ctx) ModelInfo accessor instead.
//  2. The adapter pins the embedding model name from the same
//     defaults `cortex up` uses, so the digest captured here is for
//     the same model the readiness probe ensures is loaded.
type observeEmbedder struct {
	c     *ollama.HTTPClient
	model string
}

// newOllamaEmbedder builds an Embedder around the configured Ollama
// endpoint. The model name comes from the same Phase-1 default as
// `cortex up` uses (defaultEmbeddingModel) so digest pinning stays
// consistent across the readiness probe and the write path.
func newOllamaEmbedder(cfg config.Config) *observeEmbedder {
	client := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
		LinkDerivationTimeout: time.Duration(cfg.Timeouts.LinkDerivationSeconds) * time.Second,
		NumCtx:                cfg.Ollama.NumCtx,
	})
	return &observeEmbedder{c: client, model: defaultEmbeddingModel}
}

func (e *observeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return e.c.Embed(ctx, text)
}

// ModelDigest returns the embedding model's name and content digest.
// The first call performs a single /api/show round trip; subsequent
// calls return the cached value (the underlying HTTPClient enforces
// at-most-once Show semantics via sync.Once).
func (e *observeEmbedder) ModelDigest(ctx context.Context) (string, string, error) {
	info, err := e.c.Show(ctx)
	if err != nil {
		return "", "", err
	}
	name := info.Name
	if name == "" {
		name = e.model
	}
	return name, info.Digest, nil
}

// emitAndExit writes err via the errs package (so the envelope shape
// and stderr scrubbing rules are applied uniformly) and returns a
// SilentError that cobra will surface with the right exit code. We
// lean on cobra's Execute -> os.Exit dance by wrapping the exit code
// in a custom error type the main loop checks.
func emitAndExit(cmd *cobra.Command, err error, jsonMode bool) error {
	code := errs.Emit(cmd.ErrOrStderr(), err, jsonMode)
	return &exitCodeErr{code: code}
}

// exitCodeErr is a sentinel error that tells main() which process
// exit code to return. cobra's default behaviour would map any non-
// nil RunE error to exit 1, which is wrong for validation failures
// (exit 2). Handling it in main() keeps the observe command in
// charge of its own exit semantics.
type exitCodeErr struct{ code int }

func (e *exitCodeErr) Error() string { return fmt.Sprintf("exit %d", e.code) }

// exitCodeFromError extracts the desired process exit code from an
// error returned by a subcommand's RunE. Called from main.go before
// os.Exit.
func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var e *exitCodeErr
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

// defaultConfigPath returns ~/.cortex/config.yaml. A helper rather
// than a constant because os.UserHomeDir can fail on unusual systems
// and we want to degrade gracefully.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".cortex/config.yaml"
	}
	return filepath.Join(home, ".cortex", "config.yaml")
}

// expandHome rewrites a leading "~" in cfg paths to the user's home
// directory. The config layer stores paths in their raw "~/.cortex/..."
// form so the on-disk config file is portable across machines.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[2:])
}

// resolveTrailID returns the explicit --trail flag value when set,
// otherwise falls back to the CORTEX_TRAIL_ID environment variable so
// agents that chain `cortex observe` inside a trail session do not
// need to thread --trail through every invocation (spec integration
// surface; grill-code round 4 follow-up cortex-0ci).
func resolveTrailID(flag string) string {
	if flag != "" {
		return flag
	}
	return strings.TrimSpace(os.Getenv("CORTEX_TRAIL_ID"))
}

// defaultActor returns the best available identity for the Actor
// field of a datom: $USER if set, otherwise "cortex".
func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cortex"
}
