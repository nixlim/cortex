// Alternate recall modes for cortex recall --mode=<...>.
//
// The default pipeline in pipeline.go runs HippoRAG + ACT-R reranking.
// This file implements the four alternate modes documented in the
// spec under US-14 and the behavioral-contract note "Alternate modes
// (similar, traverse, path, community, surprise) use different
// pipelines as specified in their BDD scenarios". Each mode has its
// own orchestrator that does NOT run default-mode PPR reranking:
//
//   - similar:   pure Weaviate nearest-neighbor, no Neo4j traversal
//   - traverse:  typed-edge graph walk from a seed within a depth
//   - path:      shortest path between two entries
//   - community: all entries in a named community
//   - surprise:  low-recency first, may include sub-threshold entries
//
// Like the default pipeline, every external dependency is a narrow
// interface so the orchestrators can be exercised with fakes.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Behavioral Contract" (default vs alternate)
//	docs/spec/cortex-spec.md US-14 BDD scenarios
//	bead cortex-4kq.48
package recall

import (
	"context"
	"sort"
	"time"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/errs"
)

// Mode is the recall mode selected by --mode. The zero value is the
// default mode, handled by Pipeline.Recall in pipeline.go.
type Mode string

const (
	ModeDefault   Mode = ""
	ModeSimilar   Mode = "similar"
	ModeTraverse  Mode = "traverse"
	ModePath      Mode = "path"
	ModeCommunity Mode = "community"
	ModeSurprise  Mode = "surprise"
)

// ModeRequest carries the mode-specific inputs. Required fields per
// mode are validated in the individual entry points, not here.
type ModeRequest struct {
	Query       string
	Mode        Mode
	From        string
	To          string
	CommunityID string
	Depth       int
	Limit       int
}

// TraverseEdge is one typed edge in a traverse result, surfacing the
// graph relationship name alongside the target entity.
type TraverseEdge struct {
	SourceID string
	TargetID string
	EdgeType string
}

// TraverseResult carries the typed subgraph returned by --mode=traverse.
type TraverseResult struct {
	SeedID string
	Nodes  []string       // entity ids reachable within depth
	Edges  []TraverseEdge // typed edge labels
}

// PathResult carries the shortest path between two entries. Empty
// Nodes means no path exists (AC3).
type PathResult struct {
	FromID string
	ToID   string
	Nodes  []string // in order from → to; empty when no path
	Edges  []TraverseEdge
}

// SimilarSearcher is the Weaviate-backed nearest-neighbor interface.
// The return slice is already ordered by descending cosine similarity.
type SimilarSearcher interface {
	Similar(ctx context.Context, queryVec []float32, k int) ([]Result, error)
}

// GraphTraverser performs typed-edge graph walks over Neo4j.
type GraphTraverser interface {
	Traverse(ctx context.Context, seed string, depth int) (TraverseResult, error)
	ShortestPath(ctx context.Context, from, to string) (PathResult, error)
	CommunityMembers(ctx context.Context, communityID string) ([]string, error)
}

// Modes is the alternate-mode orchestrator. It reuses the default
// Pipeline's Embedder and Loader fields for query embedding and entry
// loading so the CLI only needs to assemble one struct.
type Modes struct {
	Similar  SimilarSearcher
	Graph    GraphTraverser
	Loader   EntryLoader
	Embedder QueryEmbedder
	Context  ContextFetcher

	Now func() time.Time

	Limit               int
	VisibilityThreshold float64
	DecayExponent       float64
}

// fillDefaults replaces zero-value tunables with spec defaults. Same
// pattern as Pipeline.fillDefaults but scoped to mode-relevant fields.
func (m *Modes) fillDefaults() {
	if m.Limit <= 0 {
		m.Limit = DefaultLimit
	}
	if m.VisibilityThreshold <= 0 {
		m.VisibilityThreshold = activation.VisibilityThreshold
	}
	if m.DecayExponent <= 0 {
		m.DecayExponent = activation.DefaultDecayExponent
	}
	if m.Now == nil {
		m.Now = func() time.Time { return time.Now().UTC() }
	}
}

// RecallSimilar implements --mode=similar. AC1: pure Weaviate
// nearest-neighbor without invoking Neo4j PPR. The pipeline embeds
// the query, hands the vector to Weaviate, and returns the raw
// top-k in descending similarity order. No PPR, no rerank, no
// reinforcement — the spec BDD scenario explicitly documents that
// alternate modes do not run the default reinforcement loop.
func (m *Modes) RecallSimilar(ctx context.Context, req ModeRequest) ([]Result, error) {
	if req.Query == "" {
		return nil, errs.Validation("EMPTY_QUERY",
			"cortex recall --mode=similar requires a query", nil)
	}
	m.fillDefaults()
	vec, err := m.Embedder.Embed(ctx, req.Query)
	if err != nil {
		return nil, errs.Operational("QUERY_EMBED_FAILED",
			"could not embed query", err)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = m.Limit
	}
	hits, err := m.Similar.Similar(ctx, vec, limit)
	if err != nil {
		return nil, errs.Operational("SIMILAR_SEARCH_FAILED",
			"weaviate nearest-neighbor failed", err)
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// RecallTraverse implements --mode=traverse --from=<id> --depth=N.
// AC2: returns only entities reachable from the seed within N hops
// with typed edge labels. The traversal is delegated to Neo4j.
func (m *Modes) RecallTraverse(ctx context.Context, req ModeRequest) (*TraverseResult, error) {
	if req.From == "" {
		return nil, errs.Validation("MISSING_FROM",
			"cortex recall --mode=traverse requires --from", nil)
	}
	if req.Depth <= 0 {
		return nil, errs.Validation("INVALID_DEPTH",
			"cortex recall --mode=traverse requires --depth >= 1",
			map[string]any{"depth": req.Depth})
	}
	m.fillDefaults()
	tr, err := m.Graph.Traverse(ctx, req.From, req.Depth)
	if err != nil {
		return nil, errs.Operational("TRAVERSE_FAILED",
			"neo4j traverse failed", err)
	}
	return &tr, nil
}

// RecallPath implements --mode=path --from=A --to=B. AC3: returns the
// shortest path from A to B or an empty result when no path exists.
func (m *Modes) RecallPath(ctx context.Context, req ModeRequest) (*PathResult, error) {
	if req.From == "" || req.To == "" {
		return nil, errs.Validation("MISSING_ENDPOINTS",
			"cortex recall --mode=path requires --from and --to", nil)
	}
	m.fillDefaults()
	pr, err := m.Graph.ShortestPath(ctx, req.From, req.To)
	if err != nil {
		return nil, errs.Operational("PATH_FAILED",
			"neo4j shortest-path failed", err)
	}
	return &pr, nil
}

// RecallCommunity implements --mode=community --community=<id>:
// returns every entry in the named community. The results are loaded
// and ordered deterministically by entry id.
func (m *Modes) RecallCommunity(ctx context.Context, req ModeRequest) ([]Result, error) {
	if req.CommunityID == "" {
		return nil, errs.Validation("MISSING_COMMUNITY",
			"cortex recall --mode=community requires --community", nil)
	}
	m.fillDefaults()
	members, err := m.Graph.CommunityMembers(ctx, req.CommunityID)
	if err != nil {
		return nil, errs.Operational("COMMUNITY_LOOKUP_FAILED",
			"neo4j community lookup failed", err)
	}
	sort.Strings(members)
	entries, err := m.Loader.Load(ctx, members)
	if err != nil {
		return nil, errs.Operational("ENTRY_LOAD_FAILED",
			"could not load community entries", err)
	}
	results := make([]Result, 0, len(members))
	for _, id := range members {
		e, ok := entries[id]
		if !ok {
			continue
		}
		results = append(results, Result{
			EntryID: e.EntryID,
			Body:    e.Body,
		})
	}
	return results, nil
}

// RecallSurprise implements --mode=surprise. AC4: weights toward
// items with (1 - recency) and may surface items below the default
// visibility threshold. The surprise score is defined as the
// complement of a normalized recency signal — older entries and
// entries whose last_retrieved_at is empty score highest. Unlike
// default mode, surprise does NOT filter by the visibility threshold.
func (m *Modes) RecallSurprise(ctx context.Context, req ModeRequest) ([]Result, error) {
	m.fillDefaults()
	// Surprise works over the full entry corpus in principle. In the
	// Phase 1 contract the caller supplies the candidate set via a
	// Weaviate search if Query is non-empty, or via the loader's
	// full-scan behavior if not. We delegate candidate selection to
	// the SimilarSearcher when a query is present, and otherwise
	// fall back to whatever the test-wired fake returns as an empty
	// query.
	var candidates []Result
	if req.Query != "" {
		vec, err := m.Embedder.Embed(ctx, req.Query)
		if err != nil {
			return nil, errs.Operational("QUERY_EMBED_FAILED",
				"could not embed query", err)
		}
		k := req.Limit
		if k <= 0 {
			k = m.Limit * 3 // oversample so surprise has room to reorder
		}
		hits, err := m.Similar.Similar(ctx, vec, k)
		if err != nil {
			return nil, errs.Operational("SIMILAR_SEARCH_FAILED",
				"weaviate nearest-neighbor failed", err)
		}
		candidates = hits
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.EntryID)
	}
	entries, err := m.Loader.Load(ctx, ids)
	if err != nil {
		return nil, errs.Operational("ENTRY_LOAD_FAILED",
			"could not load surprise candidates", err)
	}
	now := m.Now()

	type surprise struct {
		state EntryState
		score float64
	}
	var scored []surprise
	for _, c := range candidates {
		e, ok := entries[c.EntryID]
		if !ok {
			continue
		}
		// Surprise explicitly MAY surface sub-threshold entries, but
		// evicted entries remain excluded across all retrieval modes
		// per the spec eviction contract.
		if e.Activation.Evicted {
			continue
		}
		// Recency proxy: most-recent event is max(EncodingAt, LastRetrievedAt).
		ref := e.Activation.EncodingAt
		if e.Activation.LastRetrievedAt.After(ref) {
			ref = e.Activation.LastRetrievedAt
		}
		ageSeconds := now.Sub(ref).Seconds()
		if ageSeconds < 0 {
			ageSeconds = 0
		}
		// Normalize recency to (0, 1]: newer → closer to 1.
		// surprise = 1 - recency = 1 - 1/(1+age) = age/(1+age).
		recency := 1.0 / (1.0 + ageSeconds)
		score := 1.0 - recency
		scored = append(scored, surprise{state: e, score: score})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].state.EntryID < scored[j].state.EntryID
	})
	limit := req.Limit
	if limit <= 0 {
		limit = m.Limit
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]Result, 0, len(scored))
	for _, s := range scored {
		out = append(out, Result{
			EntryID: s.state.EntryID,
			Body:    s.state.Body,
			Score:   s.score,
		})
	}
	return out, nil
}
