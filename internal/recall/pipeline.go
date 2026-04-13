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

// Stage labels for the layered relevance gate (cortex-9ti). Used as
// keys in Response.Diagnostics.DroppedByStage so an operator can tell
// which stage is doing the work and which knob to move.
const (
	StageHardSimFloor   = "HARD_SIM_FLOOR"
	StageRescuePath     = "RESCUE_PATH"
	StageCompositeFloor = "COMPOSITE_FLOOR"
)

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
	Diagnostics         Diagnostics
}

// Diagnostics carries per-stage drop counts from the layered relevance
// gate (cortex-9ti). DroppedByStage is keyed by StageHardSimFloor /
// StageRescuePath / StageCompositeFloor. Empty when no gate stage fired.
type Diagnostics struct {
	DroppedByStage map[string]int `json:"dropped_by_stage,omitempty"`
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

	// Layered relevance gate (cortex-y6g, Stage 1+2a).
	//
	// The old single-floor gate (RelevanceFloor) dropped any candidate
	// with sim below one threshold. That was too strict on borderline
	// positives — a sim=0.50 hit with strong PPR support was silently
	// discarded — and too loose at the hard end because there was no
	// absolute "never surface" bound. The layered gate replaces it
	// with two conditions evaluated per candidate:
	//
	//  1. sim < SimFloorHard      → DROP unconditionally (below hard floor).
	//  2. sim >= SimFloorStrict
	//     OR sim >= SimFloorHard - RescueAlpha*ppr → KEEP.
	//
	// The second clause is the smooth rescue: between the hard and
	// strict floors, PPR strength can pull a candidate back into the
	// result set, but only proportionally (alpha ~0.15) and only so
	// far as the hard floor allows. A zero PPR gives no rescue; a PPR
	// of ~0.40 buys ~0.06 of slack on sim. Tuned against deep-eval
	// dump 20260412T225007Z: positives sit at sim 0.65-0.70, negatives
	// at sim 0.53-0.60. Strict=0.55 preserves the prior default for
	// the upper path; hard=0.40 is well below any observed negative.
	//
	// RelevanceFloor is retained as a back-compat alias for
	// SimFloorStrict so existing callers (bench harness, older
	// config files, tests) keep working without churn. fillDefaults
	// copies RelevanceFloor into SimFloorStrict when only the legacy
	// field is set. Production wiring in cmd/cortex/recall.go now
	// sources the gate knobs from retrieval.relevance_gate.*.
	SimFloorHard   float64
	SimFloorStrict float64
	RescueAlpha    float64

	// Stage 3 composite floor (cortex-2sg). After a candidate clears
	// Stage 1+2a it must also clear a simple weighted gate
	// GateSimWeight*sim + GatePPRWeight*ppr >= CompositeFloor. Weak
	// queries therefore return fewer than Limit results — adaptive
	// truncation communicates confidence to downstream consumers.
	// CompositeFloor == 0 disables the gate entirely.
	CompositeFloor float64
	GateSimWeight  float64
	GatePPRWeight  float64

	// PPRBaselineMinN is the minimum per-query candidate count required
	// to use the Stage 2b quantile-baseline rescue (cortex-5mp). When
	// len(pprScores) >= PPRBaselineMinN, a borderline candidate (sim
	// between the hard and strict floors) is rescued only if its PPR
	// score is a strict upper-quartile outlier in this query's PPR
	// distribution (ppr > p75). Below the threshold, quantile estimation
	// over so few samples is unstable so the gate falls back to the
	// Stage 2a Option-1 formula sim >= SimFloorHard - RescueAlpha*ppr.
	// Zero means "always use Option-1 fallback" (quantile disabled).
	PPRBaselineMinN int

	// RelevanceFloor is a back-compat alias for SimFloorStrict. Zero
	// disables the gate entirely. When set WITHOUT any of the new
	// RelevanceGate fields (SimFloorHard, SimFloorStrict, RescueAlpha,
	// CompositeFloor, GateSimWeight, GatePPRWeight, PPRBaselineMinN),
	// fillDefaults reproduces the pre-cortex-9uc single-floor gate
	// exactly: drop iff sim < RelevanceFloor, no rescue, no Stage 3,
	// no quantile baseline. Setting ANY RelevanceGate field alongside
	// RelevanceFloor opts into the layered defaults described above.
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
	// Per-call diagnostics (cortex-9ti). Tracked in a local map rather
	// than on Pipeline so concurrent Recall callers don't trample each
	// other's counters.
	diag := Diagnostics{DroppedByStage: map[string]int{}}
	// Stage 2b (cortex-5mp): compute the per-query PPR p75 once up
	// front. Below PPRBaselineMinN samples the quantile is too noisy,
	// so we fall back to the Stage 2a Option-1 rescue formula.
	useQuantile := p.PPRBaselineMinN > 0 && len(pprScores) >= p.PPRBaselineMinN
	var pprP75 float64
	if useQuantile {
		pprValues := make([]float64, 0, len(pprScores))
		for _, v := range pprScores {
			pprValues = append(pprValues, v)
		}
		pprP75 = quantile(pprValues, 0.75)
	}
	for _, e := range visible {
		base := e.Activation.Current(now, p.DecayExponent)
		ppr := pprScores[e.EntryID]
		sim := cosine(queryVec, e.Embedding)
		// Layered relevance gate (cortex-y6g + cortex-5mp). See the
		// field comment on Pipeline for the rationale. Gate here
		// rather than after ranking so suppressed candidates produce
		// neither results nor reinforcement datoms.
		if p.SimFloorStrict > 0 {
			if sim < p.SimFloorHard {
				diag.DroppedByStage[StageHardSimFloor]++
				continue
			}
			if sim < p.SimFloorStrict {
				rescued := false
				if useQuantile {
					rescued = (ppr - pprP75) > 0
				} else {
					rescued = sim >= p.SimFloorHard-p.RescueAlpha*ppr
				}
				if !rescued {
					diag.DroppedByStage[StageRescuePath]++
					continue
				}
			}
		}
		// Stage 3 composite floor (cortex-2sg). Runs AFTER Stage 1+2a so
		// rescued borderline candidates still get a shot; a weighted
		// sum gate-out here yields adaptive truncation (fewer than Limit
		// results for weak queries).
		if p.CompositeFloor > 0 {
			gateSig := p.GateSimWeight*sim + p.GatePPRWeight*ppr
			if gateSig < p.CompositeFloor {
				diag.DroppedByStage[StageCompositeFloor]++
				continue
			}
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
		Diagnostics:         diag,
	}, nil
}

// fillDefaults applies spec defaults to zero-valued tunables. Called
// at the top of Recall so tests that only care about one knob don't
// have to set the others.
//
// Caveat: fillDefaults mutates the receiver permanently. After the
// first Recall call, fields zeroed by the caller between calls will
// NOT revert to defaults — the stamped values persist. In particular,
// setting SimFloorStrict back to 0 after a prior Recall leaves
// CompositeFloor / SimFloorHard populated from the first pass.
// Callers that need to toggle the gate should construct a fresh
// Pipeline rather than mutating fields in place.
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
	// Layered gate back-compat: legacy RelevanceFloor is an alias for
	// SimFloorStrict. To preserve byte-for-byte pre-cortex-9uc behavior,
	// callers that set ONLY the legacy alias (none of the new
	// RelevanceGate fields) get the old single-floor gate — no hard
	// floor below strict, no rescue, no Stage 3, no quantile baseline.
	// The new layered behavior is opt-in: setting ANY RelevanceGate
	// field triggers the full layered defaults.
	legacyOnly := p.RelevanceFloor > 0 &&
		p.SimFloorHard == 0 &&
		p.SimFloorStrict == 0 &&
		p.RescueAlpha == 0 &&
		p.CompositeFloor == 0 &&
		p.GateSimWeight == 0 &&
		p.GatePPRWeight == 0 &&
		p.PPRBaselineMinN == 0
	if p.SimFloorStrict == 0 && p.RelevanceFloor > 0 {
		p.SimFloorStrict = p.RelevanceFloor
	}
	if legacyOnly {
		p.SimFloorHard = p.SimFloorStrict
		return
	}
	if p.SimFloorStrict > 0 {
		if p.SimFloorHard == 0 {
			p.SimFloorHard = 0.40
		}
		if p.RescueAlpha == 0 {
			p.RescueAlpha = 0.15
		}
		if p.CompositeFloor == 0 {
			p.CompositeFloor = 0.45
		}
	}
	if p.GateSimWeight == 0 {
		p.GateSimWeight = 0.7
	}
	if p.GatePPRWeight == 0 {
		p.GatePPRWeight = 0.3
	}
	if p.PPRBaselineMinN <= 0 && p.SimFloorStrict > 0 {
		p.PPRBaselineMinN = 25
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

// quantile returns the q-th quantile of values using a simple
// index-based percentile (idx = int(q*len)). Linear interpolation is
// deliberately skipped because cortex-5mp only needs a "typical-vs-
// outlier" cutoff for the Stage 2b rescue and we call this once per
// query on a few hundred floats at most. Empty slice → 0; single
// value → that value. Mutates its input by sorting in place.
func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	if len(values) == 1 {
		return values[0]
	}
	sort.Float64s(values)
	idx := int(q * float64(len(values)))
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return values[idx]
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
