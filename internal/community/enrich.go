// internal/community/enrich.go computes per-community cohesion
// metrics (average pairwise cosine similarity, MDL ratio) and patches
// them onto the :Community nodes that Persist wrote. Without these
// properties, cortex reflect's cluster source query filters every
// community out (WHERE c.avg_cosine IS NOT NULL AND c.mdl_ratio IS
// NOT NULL) and the pipeline returns zero candidates even on a
// well-populated graph. See bead cortex-6ef.
//
// Why post-detect enrichment rather than extending Detect: Detect is
// a pure Neo4j/GDS read path and the cosine computation requires
// reading vectors from Weaviate. Adding a Weaviate dependency to
// Detector would ripple into every caller (analyze, tests) for a
// single-consumer feature. A separate EnrichLevel0 method keeps
// Detector pure and leaves the wiring to the call site that owns
// both Neo4j and Weaviate clients (cmd/cortex/communities.go).

package community

import (
	"context"
	"fmt"
	"math"
)

// VectorFetcher is the narrow Weaviate surface EnrichLevel0 needs.
// Production passes *weaviate.HTTPClient; tests build an in-memory
// fake. A nil return map is treated as "no vectors"; a nil error with
// a partial map is accepted (missing ids are skipped).
type VectorFetcher interface {
	FetchVectorsByCortexIDs(ctx context.Context, class string, cortexIDs []string) (map[string][]float32, error)
}

// EnrichmentSummary is the non-cypher-specific return shape the CLI
// uses to print "enriched N communities" after detect.
type EnrichmentSummary struct {
	CommunitiesEnriched int
	VectorsFetched      int
}

// EnrichLevel0 reads back every level-0 :Community written by
// Persist, loads the embedding for each member entry from Weaviate,
// computes the average pairwise cosine similarity and a size-aware
// MDL ratio, and writes both back as properties on the Community
// node. cortex reflect's cluster source filters on these properties,
// so this step is the fix for cortex-6ef.
//
// The MDL ratio formula is:
//
//	mdl_ratio = 1 + avg_cosine * ln(1 + member_count)
//
// This is a deliberately simple proxy for description-length
// compression: tightly cohesive groups with enough members to amortise
// a shared frame clear 1.3 (reflect.DefaultMDLRatio) while singletons
// and loose clusters stay below. Swap in a real MDL computation once
// the reflect frame summariser lands — the property contract
// (non-null, positive) is stable either way.
//
// Members without a vector in Weaviate are silently skipped.
// Communities with fewer than two valid vectors get avg_cosine=0 and
// mdl_ratio=1.0 — still non-null so reflect's IS NOT NULL predicate
// surfaces the row, still numerically sensible for any downstream
// threshold.
func (d *Detector) EnrichLevel0(ctx context.Context, fetcher VectorFetcher, weaviateClass string) (EnrichmentSummary, error) {
	if d.Neo4j == nil {
		return EnrichmentSummary{}, fmt.Errorf("community: no neo4j client configured")
	}
	if fetcher == nil {
		return EnrichmentSummary{}, fmt.Errorf("community: no vector fetcher configured")
	}
	// Read back every level-0 community and the entry_ids of its
	// members. RunGDS uses a read session which is fine for this
	// non-GDS query; it is the only read entry point on the narrow
	// Neo4jClient interface this package exposes.
	const readCypher = `
MATCH (c:Community {level: 0})<-[:IN_COMMUNITY]-(e)
WHERE e.entry_id IS NOT NULL
WITH c, collect(DISTINCT e.entry_id) AS entry_ids
RETURN c.community_id AS community_id, entry_ids
`
	rows, err := d.Neo4j.RunGDS(ctx, readCypher, map[string]any{})
	if err != nil {
		return EnrichmentSummary{}, fmt.Errorf("community: read level-0 members: %w", err)
	}
	// Collect all entry_ids up front so we can fetch their vectors in
	// a single Weaviate round-trip rather than one query per community.
	allIDs := make(map[string]struct{})
	perCommunity := make([]communityMembers, 0, len(rows))
	for _, row := range rows {
		cid, ok := rowInt64(row, "community_id")
		if !ok {
			continue
		}
		ids := toStringSlice(row["entry_ids"])
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			allIDs[id] = struct{}{}
		}
		perCommunity = append(perCommunity, communityMembers{communityID: cid, entryIDs: ids})
	}

	idBatch := make([]string, 0, len(allIDs))
	for id := range allIDs {
		idBatch = append(idBatch, id)
	}
	vectors, err := fetcher.FetchVectorsByCortexIDs(ctx, weaviateClass, idBatch)
	if err != nil {
		return EnrichmentSummary{}, fmt.Errorf("community: fetch vectors: %w", err)
	}

	const writeCypher = `
MATCH (c:Community {level: 0, community_id: $community_id})
SET c.avg_cosine = $avg_cosine, c.mdl_ratio = $mdl_ratio, c.vectors_fetched = $vectors_fetched
`
	summary := EnrichmentSummary{}
	for _, cm := range perCommunity {
		avgCos, nFetched := averagePairwiseCosine(cm.entryIDs, vectors)
		mdl := 1.0
		if nFetched >= 2 {
			mdl = 1.0 + avgCos*math.Log(1.0+float64(len(cm.entryIDs)))
		}
		if mdl < 1.0 {
			// Pairwise cosine is bounded in [-1,1]; negative average
			// would pull mdl below 1, which violates the "positive"
			// contract in the bead. Clamp to 1.0 — the reflect floor
			// rejects sub-1.3 ratios anyway.
			mdl = 1.0
		}
		params := map[string]any{
			"community_id":    cm.communityID,
			"avg_cosine":      avgCos,
			"mdl_ratio":       mdl,
			"vectors_fetched": nFetched,
		}
		if err := d.Neo4j.WriteEntries(ctx, writeCypher, params); err != nil {
			return summary, fmt.Errorf("community: write enrichment for %d: %w", cm.communityID, err)
		}
		summary.CommunitiesEnriched++
		summary.VectorsFetched += nFetched
	}
	return summary, nil
}

type communityMembers struct {
	communityID int64
	entryIDs    []string
}

// averagePairwiseCosine computes the average cosine similarity over
// all distinct unordered pairs of member entries that have a known
// vector. The second return value is the count of members whose
// vector was actually resolved — callers use it to pick a sensible
// MDL ratio fallback for sparse communities.
//
// Returns (0, n) when there are fewer than two resolved vectors;
// returns the average otherwise, clamped to [0, 1] to respect the
// reflect schema's cosine-in-[0,1] expectation (cortex-6ef AC).
func averagePairwiseCosine(entryIDs []string, vectors map[string][]float32) (float64, int) {
	resolved := make([][]float32, 0, len(entryIDs))
	for _, id := range entryIDs {
		if v, ok := vectors[id]; ok && len(v) > 0 {
			resolved = append(resolved, v)
		}
	}
	if len(resolved) < 2 {
		return 0, len(resolved)
	}
	var sum float64
	var pairs int
	for i := 0; i < len(resolved); i++ {
		for j := i + 1; j < len(resolved); j++ {
			sum += cosine32(resolved[i], resolved[j])
			pairs++
		}
	}
	if pairs == 0 {
		return 0, len(resolved)
	}
	avg := sum / float64(pairs)
	if avg < 0 {
		avg = 0
	}
	if avg > 1 {
		avg = 1
	}
	return avg, len(resolved)
}

// cosine32 returns the cosine similarity of two equal-length float32
// vectors. Mismatched lengths or zero-norm inputs produce 0.
func cosine32(a, b []float32) float64 {
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

// toStringSlice decodes a Cypher list column into []string. Neo4j
// returns []any of strings in production; tests use []string directly.
func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
