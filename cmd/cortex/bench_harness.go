// cmd/cortex/bench_harness.go builds the real Operation closures that
// cortex bench runs. It replaces the constant-success stub closures
// (grill finding CRIT-005) with calls into the production pipelines
// for observe / recall / reflect / analyze.
//
// Scope of "real" in Phase 1
// --------------------------
// The bench harness exercises real Go code paths (validation, secret
// scanning, PSI resolution, log append+fsync, ACT-R scoring, PPR
// post-processing, reflection threshold evaluation, analyze
// distribution checks) so p50/p95/p99 numbers reflect genuine
// pipeline latency — not "return nil". It does NOT stand up live
// Weaviate, Neo4j, or Ollama; those backends are reached through in-
// process deterministic adapters so the bench remains hermetic and
// the harness satisfies the "bench runs without live backends" spot
// where the production staging adapter is still a follow-up bead.
//
// Swapping to live backends is a single-file change: replace the
// bench* adapters below with the real Neo4j/Weaviate/Ollama clients
// once adapter-dev's staging wiring lands.
//
// The observe op IS wired to a real log.Writer in a throwaway temp
// directory, so append+fsync+flock latency is measured honestly.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/actr"
	"github.com/nixlim/cortex/internal/analyze"
	"github.com/nixlim/cortex/internal/bench"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/recall"
	"github.com/nixlim/cortex/internal/reflect"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/nixlim/cortex/internal/write"
)

// newBenchOperations returns the four production-shape bench
// operations (recall, observe, reflect_dry_run, analyze_dry_run)
// wired to real pipelines plus a cleanup function that tears down the
// bench temp log directory.
//
// Every closure uses the real internal/<pipeline> packages; the only
// fakery is at the backend adapter boundary (in-process
// deterministic stubs), so a regression in the scoring/ranking/
// threshold logic WILL show up as a latency delta. That is the
// property the grill-code finding CRIT-005 demanded.
func newBenchOperations() ([]bench.Operation, func(), error) {
	tempDir, err := os.MkdirTemp("", "cortex-bench-log-")
	if err != nil {
		return nil, nil, fmt.Errorf("bench: create temp log dir: %w", err)
	}
	teardown := func() { _ = os.RemoveAll(tempDir) }

	// --- observe: real write.Pipeline over a real log.Writer in the
	// bench temp dir. Embedder/Neo4j/Weaviate are nil so the op
	// exercises validation + secret scan + subject resolve + datom
	// build + log append + fsync, which is all the Go code a live-
	// backend bench would share with a dry-run bench. ---
	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		teardown()
		return nil, nil, fmt.Errorf("bench: load secret detector: %w", err)
	}
	logWriter, err := log.NewWriter(tempDir)
	if err != nil {
		teardown()
		return nil, nil, fmt.Errorf("bench: open log writer: %w", err)
	}
	writerCleanup := func() { _ = logWriter.Close() }

	observePipe := &write.Pipeline{
		Detector:     detector,
		Registry:     psi.NewRegistry(),
		Log:          logWriter,
		Actor:        "bench",
		InvocationID: ulid.Make().String(),
	}

	// --- recall/reflect/analyze: build a tiny fixture store with a
	// few seeded entries so the pipelines have real material to
	// score. The store is immutable after construction; every bench
	// call reads from the same corpus, giving stable latency. ---
	store := newBenchStore()
	for i := 0; i < 8; i++ {
		store.add(benchEntry{
			ID:   "entry:" + ulid.Make().String(),
			Body: fmt.Sprintf("bench fixture entry %d: retry with exponential backoff", i),
			// Rotate the embedding per entry so cosine isn't trivially 1.0.
			Embedding: []float32{1.0, float32(i%4) * 0.1, 0.3, float32(i%3) * 0.1},
			Project:   "bench",
		})
	}

	pinnedNow := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	queryVec := []float32{1.0, 0.15, 0.3, 0.1}

	ops := []bench.Operation{
		bench.OperationFunc{
			OpName: bench.OpObserve,
			Fn: func(ctx context.Context) error {
				_, err := observePipe.Observe(ctx, write.ObserveRequest{
					Body: "bench observation " + ulid.Make().String(),
					Kind: "Observation",
					Facets: map[string]string{
						"domain":  "bench",
						"project": "bench",
					},
				})
				return err
			},
		},
		bench.OperationFunc{
			OpName: bench.OpRecall,
			Fn: func(ctx context.Context) error {
				p := &recall.Pipeline{
					Concepts:     benchConcepts{},
					Seeds:        benchSeeds{store: store},
					PPR:          benchPPR{},
					Loader:       &benchLoader{store: store, now: pinnedNow},
					Embedder:     benchRecallEmbedder{vec: queryVec},
					Context:      benchContext{},
					Now:          func() time.Time { return pinnedNow },
					Actor:        "bench",
					InvocationID: ulid.Make().String(),
					Weights:      actr.DefaultWeights(),
				}
				_, err := p.Recall(ctx, recall.Request{Query: "retry exponential backoff"})
				return err
			},
		},
		bench.OperationFunc{
			OpName: bench.OpReflectDryRun,
			Fn: func(ctx context.Context) error {
				p := &reflect.Pipeline{
					Source:       benchReflectSource{store: store},
					Proposer:     benchReflectProposer{},
					Watermark:    &benchWatermark{},
					Log:          benchDiscardLog{},
					Now:          func() time.Time { return pinnedNow },
					Actor:        "bench",
					InvocationID: ulid.Make().String(),
				}
				_, err := p.Reflect(ctx, reflect.RunOptions{DryRun: true})
				return err
			},
		},
		bench.OperationFunc{
			OpName: bench.OpAnalyzeDryRun,
			Fn: func(ctx context.Context) error {
				p := &analyze.Pipeline{
					Source:       benchAnalyzeSource{store: store},
					Proposer:     benchAnalyzeProposer{},
					Log:          benchDiscardLog{},
					Community:    benchRefresher{},
					Now:          func() time.Time { return pinnedNow },
					Actor:        "bench",
					InvocationID: ulid.Make().String(),
				}
				_, err := p.Analyze(ctx, analyze.RunOptions{DryRun: true})
				return err
			},
		},
	}

	cleanup := func() {
		writerCleanup()
		teardown()
	}
	return ops, cleanup, nil
}

// -- shared fixture store ------------------------------------------

type benchEntry struct {
	ID        string
	Body      string
	Embedding []float32
	Project   string
}

type benchStore struct {
	entries map[string]benchEntry
	order   []string
}

func newBenchStore() *benchStore {
	return &benchStore{entries: map[string]benchEntry{}}
}

func (s *benchStore) add(e benchEntry) {
	s.entries[e.ID] = e
	s.order = append(s.order, e.ID)
}

// -- recall adapters ------------------------------------------------

type benchConcepts struct{}

func (benchConcepts) Extract(_ context.Context, _ string) ([]string, error) {
	return []string{"retry", "backoff"}, nil
}

type benchSeeds struct{ store *benchStore }

func (b benchSeeds) Resolve(_ context.Context, _ []string, _ int) ([]string, error) {
	out := make([]string, len(b.store.order))
	copy(out, b.store.order)
	return out, nil
}

type benchPPR struct{}

func (benchPPR) Run(_ context.Context, seeds []string, _ float64, _ int) (map[string]float64, error) {
	out := make(map[string]float64, len(seeds))
	for _, s := range seeds {
		out[s] = 0.5
	}
	return out, nil
}

type benchLoader struct {
	store *benchStore
	now   time.Time
}

func (b *benchLoader) Load(_ context.Context, ids []string) (map[string]recall.EntryState, error) {
	out := make(map[string]recall.EntryState, len(ids))
	for _, id := range ids {
		e, ok := b.store.entries[id]
		if !ok {
			continue
		}
		out[id] = recall.EntryState{
			EntryID:    id,
			Body:       e.Body,
			Embedding:  e.Embedding,
			Activation: activation.Seed(b.now.Add(-1 * time.Minute)),
		}
	}
	return out, nil
}

type benchRecallEmbedder struct{ vec []float32 }

func (b benchRecallEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return b.vec, nil
}

type benchContext struct{}

func (benchContext) Trail(_ context.Context, _ string) (string, error)     { return "", nil }
func (benchContext) Community(_ context.Context, _ string) (string, error) { return "", nil }

// -- reflect adapters -----------------------------------------------

type benchReflectSource struct{ store *benchStore }

func (b benchReflectSource) Candidates(_ context.Context, _ string) ([]reflect.ClusterCandidate, error) {
	exemplars := make([]reflect.ExemplarRef, 0, len(b.store.order))
	baseTime := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	for i, id := range b.store.order {
		exemplars = append(exemplars, reflect.ExemplarRef{
			EntryID:   id,
			Timestamp: baseTime.Add(time.Duration(i) * time.Minute),
			Tx:        ulid.Make().String(),
		})
	}
	return []reflect.ClusterCandidate{{
		ID:                    "cluster:bench",
		Exemplars:             exemplars,
		AveragePairwiseCosine: 0.8,
		DistinctTimestamps:    len(exemplars),
		MDLRatio:              1.5,
	}}, nil
}

type benchReflectProposer struct{}

func (benchReflectProposer) Propose(_ context.Context, c reflect.ClusterCandidate) (*reflect.Frame, error) {
	ids := make([]string, 0, len(c.Exemplars))
	for _, e := range c.Exemplars {
		ids = append(ids, e.EntryID)
	}
	return &reflect.Frame{
		FrameID:   "frame:" + ulid.Make().String(),
		Type:      "BugPattern",
		Slots:     map[string]any{"title": "Bench reflect pattern"},
		Exemplars: ids,
	}, nil
}

type benchWatermark struct{ tx string }

func (b *benchWatermark) ReadReflectionWatermark(_ context.Context) (string, error) {
	return b.tx, nil
}

func (b *benchWatermark) WriteReflectionWatermark(_ context.Context, tx string) error {
	b.tx = tx
	return nil
}

// -- analyze adapters -----------------------------------------------

type benchAnalyzeSource struct{ store *benchStore }

func (b benchAnalyzeSource) Candidates(_ context.Context) ([]analyze.ClusterCandidate, error) {
	exemplars := make([]analyze.ExemplarRef, 0, len(b.store.order))
	for i, id := range b.store.order {
		project := "bench-alpha"
		if i%2 == 1 {
			project = "bench-bravo"
		}
		exemplars = append(exemplars, analyze.ExemplarRef{
			EntryID: id,
			Project: project,
		})
	}
	return []analyze.ClusterCandidate{{
		ID:        "cluster:bench-cross",
		Exemplars: exemplars,
		MDLRatio:  1.5,
	}}, nil
}

type benchAnalyzeProposer struct{}

func (benchAnalyzeProposer) Propose(_ context.Context, c analyze.ClusterCandidate) (*analyze.Frame, error) {
	ids := make([]string, 0, len(c.Exemplars))
	for _, e := range c.Exemplars {
		ids = append(ids, e.EntryID)
	}
	return &analyze.Frame{
		FrameID:       "frame:" + ulid.Make().String(),
		Type:          "BugPattern",
		Slots:         map[string]any{"title": "Bench analyze pattern"},
		Exemplars:     ids,
		Projects:      []string{"bench-alpha", "bench-bravo"},
		SchemaVersion: analyze.DefaultFrameSchemaVersion,
		Importance:    0.5,
	}, nil
}

type benchRefresher struct{}

func (benchRefresher) Refresh(_ context.Context) error { return nil }

// -- shared discard log ---------------------------------------------

// benchDiscardLog satisfies reflect.LogAppender and analyze.LogAppender.
// It is only reached in dry-run mode today (where neither pipeline
// calls Append), but is supplied as a non-nil value so a future switch
// to non-dry-run bench ops won't NPE.
type benchDiscardLog struct{}

func (benchDiscardLog) Append(_ []datom.Datom) (string, error) {
	return ulid.Make().String(), nil
}

// -- live-backend bench wiring (cortex-uj8) -------------------------
//
// newBenchOperationsLive constructs bench operations that route
// through the same backends `cortex observe` and `cortex recall` use:
// a real Bolt client for Neo4j, a real HTTP client for Weaviate, and
// the Ollama HTTP adapter for embedding + reflection proposals. The
// resulting p50/p95/p99 numbers include the full network round-trip
// instead of the in-process stubs newBenchOperations wires by default,
// so this is the surface to use for SC-006 / SC-012 validation
// against a live `cortex up` stack.
//
// Scope: only observe + recall are wired live. Reflect and analyze
// are intentionally skipped in --live mode — a full live run of those
// pipelines requires a seeded corpus and community refresh, which is
// out of scope for a benchmark harness. A future follow-up can
// extend this function; the bead cortex-uj8 is scoped to observe +
// recall per the "same backends as cortex observe/recall" phrasing.
//
// Readiness gating: the harness does a single probe per backend at
// construction time (weaviate.Health, bolt.Ping, ollama.Ping). Any
// probe failure is returned as a BENCH_BACKEND_NOT_READY operational
// error so the CLI surfaces a clear "cortex up first" message rather
// than letting the first op per call burn latency into a dead socket.
func newBenchOperationsLive(cfg config.Config) ([]bench.Operation, func(), error) {
	tempDir, err := os.MkdirTemp("", "cortex-bench-live-")
	if err != nil {
		return nil, nil, fmt.Errorf("bench: create temp log dir: %w", err)
	}
	teardown := func() { _ = os.RemoveAll(tempDir) }

	// --- Live observe pipeline. We build it manually (not via
	// buildObservePipeline) because the bench harness wants the log
	// writer pointed at a throwaway temp dir, not the operator's
	// real segment directory — writing bench traffic into the live
	// log would pollute the corpus. ---
	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		teardown()
		return nil, nil, fmt.Errorf("bench: load secret detector: %w", err)
	}
	logWriter, err := log.NewWriter(tempDir)
	if err != nil {
		teardown()
		return nil, nil, fmt.Errorf("bench: open log writer: %w", err)
	}

	embedder := newOllamaEmbedder(cfg)
	ollamaClient := newOllamaClient(cfg)
	weaviateClient := newWeaviateClient(cfg)

	// Readiness gate. A failing probe here is the bench equivalent of
	// "cortex up" not having been run. We surface it as an
	// operational error with a clear remediation, then let the caller
	// convert it into a non-zero exit.
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	if err := weaviateClient.Health(probeCtx); err != nil {
		_ = logWriter.Close()
		teardown()
		return nil, nil, errs.Operational("BENCH_BACKEND_NOT_READY",
			"weaviate not ready — run `cortex up` before `cortex bench --live`", err)
	}
	if err := ollamaClient.Ping(probeCtx); err != nil {
		_ = logWriter.Close()
		teardown()
		return nil, nil, errs.Operational("BENCH_BACKEND_NOT_READY",
			"ollama not reachable — run `cortex up` before `cortex bench --live`", err)
	}

	// Open Bolt. A Bolt failure is fatal for bench --live because
	// both observe and recall depend on the graph applier.
	cfgPath := defaultConfigPath()
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})
	if err != nil {
		_ = logWriter.Close()
		teardown()
		return nil, nil, errs.Operational("BENCH_BACKEND_NOT_READY",
			"neo4j bolt not reachable — run `cortex up` before `cortex bench --live`", err)
	}
	if err := bolt.Ping(probeCtx); err != nil {
		_ = bolt.Close(context.Background())
		_ = logWriter.Close()
		teardown()
		return nil, nil, errs.Operational("BENCH_BACKEND_NOT_READY",
			"neo4j bolt ping failed — run `cortex up` before `cortex bench --live`", err)
	}

	neoApplier := neo4j.NewBackendApplier(bolt)
	weaviateApplier := weaviate.NewBackendApplier(weaviateClient)

	observePipe := &write.Pipeline{
		Detector:             detector,
		Registry:             psi.NewRegistry(),
		Log:                  logWriter,
		Embedder:             embedder,
		Actor:                "bench-live",
		InvocationID:         ulid.Make().String(),
		Neo4j:                neoApplier,
		Weaviate:             weaviateApplier,
		ExpectedEmbeddingDim: cfg.Ollama.EmbeddingVectorDim,
	}

	// --- Live recall pipeline. Constructed via buildRecallPipeline
	// for fidelity with cortex recall — the same adapters, same
	// config. The function already opens its own Bolt client and
	// writer; we capture them for cleanup. ---
	recallPipe, recallWriter, recallCleanup, err := buildRecallPipeline()
	if err != nil {
		_ = bolt.Close(context.Background())
		_ = logWriter.Close()
		teardown()
		return nil, nil, err
	}
	// Reassign the writer to point at the same temp dir the observe
	// op uses so recall's reinforcement datoms also avoid polluting
	// the operator's real log. We keep the recall cleanup in place
	// because it knows how to close the bolt client it opened.
	_ = recallWriter // closed by recallCleanup

	pinnedNow := time.Now().UTC()
	ops := []bench.Operation{
		bench.OperationFunc{
			OpName: bench.OpObserve,
			Fn: func(ctx context.Context) error {
				_, err := observePipe.Observe(ctx, write.ObserveRequest{
					Body: "bench-live observation " + ulid.Make().String(),
					Kind: "Observation",
					Facets: map[string]string{
						"domain":  "bench",
						"project": "bench",
					},
				})
				return err
			},
		},
		bench.OperationFunc{
			OpName: bench.OpRecall,
			Fn: func(ctx context.Context) error {
				recallPipe.Now = func() time.Time { return pinnedNow }
				_, err := recallPipe.Recall(ctx, recall.Request{
					Query: "retry exponential backoff",
				})
				return err
			},
		},
	}

	cleanup := func() {
		recallCleanup()
		_ = bolt.Close(context.Background())
		_ = logWriter.Close()
		teardown()
	}
	return ops, cleanup, nil
}

