// Package recall implements the Cortex default recall pipeline.
//
// The default mode (described in docs/spec/cortex-spec.md §"Behavioral
// Contract" and US-14) is:
//
//  1. Extract concepts from the query.
//  2. Resolve concepts to seed entry ids.
//  3. Run Personalized PageRank from the seed set over the entity graph.
//  4. Load entry state (body, activation, embedding, trail, community)
//     for every PPR candidate.
//  5. Filter out evicted and below-threshold entries (base_activation
//     strictly less than retrieval.forgetting.visibility_threshold,
//     default 0.05 — the threshold is INCLUSIVE so a value of exactly
//     0.05 is surfaced).
//  6. Rerank the visible candidates by the composite ACT-R activation
//     score computed in internal/actr, with weights 0.3/0.3/0.3/0.1
//     in the canonical base/PPR/sim/importance order.
//  7. Truncate to retrieval.default_limit (10).
//  8. Attach trail context, community context, and a "why surfaced"
//     trace to each surviving result.
//  9. Emit reinforcement datoms (base_activation, retrieval_count,
//     last_retrieved_at) for each surfaced entry. The caller is
//     responsible for appending them to the log AFTER the response
//     has been rendered — "reinforces each surfaced entity by writing
//     activation-update datoms ... after the response is assembled".
//
// The orchestrator is expressed as a Pipeline struct with narrow
// interfaces for every external dependency (concepts, seeds, PPR,
// loader, embedder, context fetcher) so unit tests can drive the
// full flow against fakes without touching Neo4j, Weaviate, or
// Ollama. Production wiring lives in cmd/cortex/recall.go (a later
// bead).
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"ACT-R Activation Formula"
//	docs/spec/cortex-spec.md §"Behavioral Contract" (default recall flow)
//	docs/spec/cortex-spec.md §"Configuration Defaults"
//	bead cortex-4kq.36
package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/actr"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// DefaultLimit matches retrieval.default_limit from the spec.
const DefaultLimit = 10

// DefaultSeedTopK matches retrieval.ppr.seed_top_k from the spec.
const DefaultSeedTopK = 5

// DefaultPPRDamping matches retrieval.ppr.damping from the spec.
const DefaultPPRDamping = 0.85

// DefaultPPRMaxIterations matches retrieval.ppr.max_iterations from the spec.
const DefaultPPRMaxIterations = 20

// Request is the parsed cortex recall default-mode input.
type Request struct {
	Query string
	Limit int // 0 → DefaultLimit
}

// Result is one entry surfaced by default recall, ready for rendering.
// Every field is populated by Pipeline.Recall before the response is
// returned — AC1 requires trail_context, community_context, and
// why_surfaced on every result.
type Result struct {
	EntryID          string
	Body             string
	Score            float64
	BaseActivation   float64
	PPRScore         float64
	Similarity       float64
	Importance       float64
	TrailContext     string
	CommunityContext string
	WhySurfaced      []string
}

// Response is the full recall output. Results is ordered by descending
// composite score and truncated to Request.Limit. ReinforcementDatoms
// carries the unsealed (use Seal() before Append) activation-update
// datoms the caller MUST append to the log after rendering. The
// caller — not this package — owns the log writer to preserve the
// "pipeline never opens its own log handle" invariant that keeps
// recall cheap on cold caches.
type Response struct {
	Results             []Result
	ReinforcementDatoms []datom.Datom
}

// ConceptExtractor maps a query string to a list of concept terms.
// Implementations typically use the Ollama adapter with the concept-
// extraction prompt from internal/prompts. A zero-length response is
// acceptable (degenerates to a random-walk style PPR kick from any
// available seed).
type ConceptExtractor interface {
	Extract(ctx context.Context, query string) ([]string, error)
}

// SeedResolver turns concept terms into seed entry ids. It is a thin
// wrapper around Neo4j lookups in production.
type SeedResolver interface {
	Resolve(ctx context.Context, concepts []string, topK int) ([]string, error)
}

// PPRRunner runs Personalized PageRank from the supplied seed set.
// The returned map is keyed by entry id and holds the PPR score in
// [0, 1]. Entries not in the map have an implied PPR score of 0.
type PPRRunner interface {
	Run(ctx context.Context, seeds []string, damping float64, maxIter int) (map[string]float64, error)
}

// EntryState is the scoring and rendering view of one entry. The
// loader stitches together datom-sourced body/subject/facet metadata,
// the persisted activation.State, and the Weaviate-held embedding.
type EntryState struct {
	EntryID      string
	Body         string
	Embedding    []float32
	Activation   activation.State
	TrailID      string
	CommunityID  string
	CrossProject bool
}

// EntryLoader bulk-loads EntryState for a candidate set. Returning
// fewer entries than requested is not an error — the orchestrator
// silently drops missing ids (possible if a candidate was retracted
// between PPR and load).
type EntryLoader interface {
	Load(ctx context.Context, entryIDs []string) (map[string]EntryState, error)
}

// QueryEmbedder produces the query vector used for cosine similarity.
type QueryEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ContextFetcher retrieves the human-readable trail and community
// context strings attached to a result. Both lookups are allowed to
// return ("", nil) for entries that have no attachment; an error
// promotes to an operational failure.
type ContextFetcher interface {
	Trail(ctx context.Context, trailID string) (string, error)
	Community(ctx context.Context, communityID string) (string, error)
}

// Pipeline is the default-recall orchestrator. Zero values are not
// usable; at minimum every interface field must be set. The
// threshold, weights, and timing knobs have sensible zero-value
// fallbacks filled in on the first Recall call.
type Pipeline struct {
	Concepts ConceptExtractor
	Seeds    SeedResolver
	PPR      PPRRunner
	Loader   EntryLoader
	Embedder QueryEmbedder
	Context  ContextFetcher

	Now          func() time.Time
	Actor        string
	InvocationID string

	// Tunables. Zero values are replaced with spec defaults.
	SeedTopK            int
	Damping             float64
	MaxIterations       int
	Limit               int
	VisibilityThreshold float64
	DecayExponent       float64
	Weights             actr.Weights

	// RelevanceFloor is the minimum cosine similarity a candidate must
	// carry to survive reranking. Without it, the composite is propped
	// up by w_base*B(e)=0.3 for every fresh entry and queries with no
	// real semantic match still return 10 results at composite ~0.3
	// (see bead cortex-7y4).
	//
	// Why similarity alone, not max(similarity, ppr): PPR from seeds
	// unavoidably touches graph-connected neighbors with nonzero score
	// even when the query has no semantic match, so gating on max(sim,
	// ppr) lets the PPR term alone keep irrelevant entries alive. The
	// cortex-7y4 fix originally used max; the deep-eval verification
	// at commit 4d34969 showed the max formulation did not fire on
	// any of the five negative queries because ppr~0.15 is typical
	// for any connected candidate. Requiring cosine similarity to
	// clear the floor makes the gate a pure semantic relevance check.
	//
	// Tuning: 0.55 is calibrated against nomic-embed-text distributions
	// measured in deep-eval dump 20260412T225007Z, where real positive
	// rank-1 hits sat at sim 0.65-0.70 and negative rank-1 hits sat
	// at sim 0.53-0.60. Conservative starting point — tune downward
	// if legitimate queries are being dropped, upward if negatives
	// still leak through. Re-run eval/deep/run_deep_eval.sh after a
	// change and compare .retrievals[0].hits[0].similarity for
	// negatives vs positives. Zero disables (preserves pre-cortex-7y4
	// behavior for benchmarks and e2e tests that construct a pipeline
	// directly). Production wiring in cmd/cortex/recall.go sources
	// the value from retrieval.relevance_floor.
	RelevanceFloor float64
}

// Recall runs the full default-mode pipeline. See the package doc for
// the step-by-step contract. Returns an operational error if any
// external dependency fails; validation errors are limited to
// blank-query rejection.
func (p *Pipeline) Recall(ctx context.Context, req Request) (*Response, error) {
	if req.Query == "" {
		return nil, errs.Validation("EMPTY_QUERY",
			"cortex recall requires a non-empty query", nil)
	}
	p.fillDefaults()
	// Per-request limit override. A positive req.Limit beats the
	// pipeline default so `cortex recall --limit N` actually takes
	// effect (bead cortex-voa). Non-positive values fall through to
	// p.Limit (populated by fillDefaults from retrieval.default_limit).
	effectiveLimit := p.Limit
	if req.Limit > 0 {
		effectiveLimit = req.Limit
	}

	// Stage 1: concept extraction.
	concepts, err := p.Concepts.Extract(ctx, req.Query)
	if err != nil {
		return nil, errs.Operational("CONCEPT_EXTRACTION_FAILED",
			"could not extract query concepts", err)
	}

	// Stage 2: seed resolution.
	seeds, err := p.Seeds.Resolve(ctx, concepts, p.SeedTopK)
	if err != nil {
		return nil, errs.Operational("SEED_RESOLUTION_FAILED",
			"could not resolve seeds", err)
	}

	// Stage 3: PPR.
	pprScores, err := p.PPR.Run(ctx, seeds, p.Damping, p.MaxIterations)
	if err != nil {
		return nil, errs.Operational("PPR_FAILED",
			"personalized pagerank failed", err)
	}

	// Stage 4: entry load.
	ids := make([]string, 0, len(pprScores))
	for id := range pprScores {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic load order for tests
	entries, err := p.Loader.Load(ctx, ids)
	if err != nil {
		return nil, errs.Operational("ENTRY_LOAD_FAILED",
			"could not load candidate entries", err)
	}

	// Stage 5: visibility filter.
	now := p.Now()
	var visible []EntryState
	for _, id := range ids {
		e, ok := entries[id]
		if !ok {
			continue
		}
		if !e.Activation.Visible(now, p.DecayExponent, p.VisibilityThreshold) {
			continue
		}
		visible = append(visible, e)
	}

	// Stage 6: query embedding.
	queryVec, err := p.Embedder.Embed(ctx, req.Query)
	if err != nil {
		return nil, errs.Operational("QUERY_EMBED_FAILED",
			"could not embed query", err)
	}

	// Stage 7: rerank by composite ACT-R score.
	type scored struct {
		state     EntryState
		composite float64
		base      float64
		ppr       float64
		sim       float64
		imp       float64
	}
	scoredAll := make([]scored, 0, len(visible))
	for _, e := range visible {
		base := e.Activation.Current(now, p.DecayExponent)
		ppr := pprScores[e.EntryID]
		sim := cosine(queryVec, e.Embedding)
		// Relevance floor: without a semantic-match signal the
		// composite collapses to w_base*B(e) — a constant ~0.3 for
		// any fresh entry — so irrelevant queries still return 10
		// results (cortex-7y4). The gate uses sim alone, not max(sim,
		// ppr), because PPR touches graph-connected neighbors with
		// nonzero score regardless of semantic match. Gate here
		// rather than after ranking so suppressed candidates produce
		// neither results nor reinforcement datoms.
		if p.RelevanceFloor > 0 && sim < p.RelevanceFloor {
			continue
		}
		imp := actr.ImportanceScore(actr.Importance{CrossProject: e.CrossProject})
		composite := actr.Activation(actr.Inputs{
			Base:       base,
			PPR:        ppr,
			Similarity: sim,
			Importance: imp,
		}, p.Weights)
		scoredAll = append(scoredAll, scored{
			state: e, composite: composite,
			base: base, ppr: ppr, sim: sim, imp: imp,
		})
	}
	sort.SliceStable(scoredAll, func(i, j int) bool {
		if scoredAll[i].composite != scoredAll[j].composite {
			return scoredAll[i].composite > scoredAll[j].composite
		}
		// Stable tiebreaker: lexicographic entry id.
		return scoredAll[i].state.EntryID < scoredAll[j].state.EntryID
	})

	// Stage 8: truncate to limit.
	if len(scoredAll) > effectiveLimit {
		scoredAll = scoredAll[:effectiveLimit]
	}

	// Stage 9: attach context + why-surfaced trace.
	results := make([]Result, 0, len(scoredAll))
	for _, s := range scoredAll {
		trailCtx, err := p.Context.Trail(ctx, s.state.TrailID)
		if err != nil {
			return nil, errs.Operational("TRAIL_CONTEXT_FAILED",
				"could not fetch trail context", err)
		}
		commCtx, err := p.Context.Community(ctx, s.state.CommunityID)
		if err != nil {
			return nil, errs.Operational("COMMUNITY_CONTEXT_FAILED",
				"could not fetch community context", err)
		}
		results = append(results, Result{
			EntryID:          s.state.EntryID,
			Body:             s.state.Body,
			Score:            s.composite,
			BaseActivation:   s.base,
			PPRScore:         s.ppr,
			Similarity:       s.sim,
			Importance:       s.imp,
			TrailContext:     trailCtx,
			CommunityContext: commCtx,
			WhySurfaced:      buildWhySurfaced(s.base, s.ppr, s.sim, s.imp, p.Weights),
		})
	}

	// Stage 10: reinforcement datom emission. The datoms are NOT
	// sealed here because the caller typically wants to set its own
	// tx/invocation fields and seal in a single pass just before the
	// log Append call.
	reinforcements := make([]datom.Datom, 0, 3*len(results))
	tx := ulid.Make().String()
	ts := now.UTC().Format(time.RFC3339Nano)
	for _, r := range results {
		// Compute the new activation state for the reinforcement datom.
		newState := entries[r.EntryID].Activation.Reinforce(now)
		ds, err := buildReinforcementDatoms(r.EntryID, newState, tx, ts, p.Actor, p.InvocationID)
		if err != nil {
			return nil, errs.Operational("REINFORCEMENT_EMIT_FAILED",
				"could not build reinforcement datoms", err)
		}
		reinforcements = append(reinforcements, ds...)
	}

	return &Response{
		Results:             results,
		ReinforcementDatoms: reinforcements,
	}, nil
}

// fillDefaults applies spec defaults to zero-valued tunables. Called
// at the top of Recall so tests that only care about one knob don't
// have to set the others.
func (p *Pipeline) fillDefaults() {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	if p.SeedTopK <= 0 {
		p.SeedTopK = DefaultSeedTopK
	}
	if p.Damping <= 0 {
		p.Damping = DefaultPPRDamping
	}
	if p.MaxIterations <= 0 {
		p.MaxIterations = DefaultPPRMaxIterations
	}
	if p.VisibilityThreshold <= 0 {
		p.VisibilityThreshold = activation.VisibilityThreshold
	}
	if p.DecayExponent <= 0 {
		p.DecayExponent = activation.DefaultDecayExponent
	}
	if p.Weights == (actr.Weights{}) {
		p.Weights = actr.DefaultWeights()
	}
	if p.Now == nil {
		p.Now = func() time.Time { return time.Now().UTC() }
	}
}

// cosine returns the cosine similarity of two equal-length vectors.
// Mismatched lengths or zero-norm inputs produce 0.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// buildWhySurfaced assembles a short human-readable trace describing
// which terms of the composite formula dominated a result's score.
// The output is intended for display in the recall CLI and in the
// JSON envelope; the exact format is unconstrained by AC.
func buildWhySurfaced(base, ppr, sim, imp float64, w actr.Weights) []string {
	return []string{
		fmt.Sprintf("base=%.4f (w=%.2f, contrib=%.4f)", base, w.Base, w.Base*base),
		fmt.Sprintf("ppr=%.4f (w=%.2f, contrib=%.4f)", ppr, w.PPR, w.PPR*ppr),
		fmt.Sprintf("sim=%.4f (w=%.2f, contrib=%.4f)", sim, w.Similarity, w.Similarity*sim),
		fmt.Sprintf("imp=%.4f (w=%.2f, contrib=%.4f)", imp, w.Importance, w.Importance*imp),
	}
}

// buildReinforcementDatoms emits exactly three datoms per surfaced
// entry: base_activation, retrieval_count, and last_retrieved_at.
// The datom attribute names are the LWW-replay attributes listed in
// the spec's replay section.
func buildReinforcementDatoms(entryID string, state activation.State, tx, ts, actor, invocationID string) ([]datom.Datom, error) {
	make3 := func(attr string, v any) (datom.Datom, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return datom.Datom{}, fmt.Errorf("marshal %s: %w", attr, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        actor,
			Op:           datom.OpAdd,
			E:            entryID,
			A:            attr,
			V:            raw,
			Src:          "recall",
			InvocationID: invocationID,
		}
		return d, nil
	}
	ds := make([]datom.Datom, 0, 3)
	for _, spec := range []struct {
		attr string
		val  any
	}{
		{"base_activation", state.BaseActivation},
		{"retrieval_count", state.RetrievalCount},
		{"last_retrieved_at", state.LastRetrievedAt.UTC().Format(time.RFC3339Nano)},
	} {
		d, err := make3(spec.attr, spec.val)
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, nil
}
