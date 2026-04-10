// cmd/cortex/recall_adapters.go holds the six bridge adapter types
// the recall pipeline needs — thin wrappers that translate the
// generic internal/recall interfaces (ConceptExtractor, SeedResolver,
// PPRRunner, EntryLoader, QueryEmbedder, ContextFetcher) into
// concrete calls on internal/neo4j, internal/ollama, and
// internal/weaviate.
//
// Each adapter is deliberately small (one method each) and carries
// only the backend client(s) it needs. The Cypher queries below
// target the schema the write pipeline emits (entry_id property on
// node, IN_TRAIL / IN_COMMUNITY edges) and return empty result sets
// when the graph is cold. GDS PPR is called against the
// "cortex.semantic" projection; an absent projection surfaces as the
// underlying Neo4j error with a PPR_FAILED envelope so the operator
// knows which backend step is missing rather than getting a generic
// NOT_WIRED message.
//
// Phase 1 note: the write-path backend applier that would populate
// Entry nodes and the GDS projection is still work-in-progress. Until
// that lands these adapters will run against an empty graph and
// return empty recall results — but every call is a real network
// round trip, so an unavailable Neo4j surfaces as NEO4J_UNAVAILABLE
// rather than hiding behind RECALL_NOT_WIRED.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Behavioral Contract" (default recall flow)
//	docs/spec/cortex-spec.md §"Neo4j with Graph Data Science"
//	bead cortex-4kq.36, code-review fix CRIT-003
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/prompts"
	"github.com/nixlim/cortex/internal/recall"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/nixlim/cortex/internal/write"
)

// semanticGraphName is the GDS in-memory projection name Cortex uses
// for semantic-graph queries (PPR, community detection). The
// projection is created by cortex up / rebuild once the backend
// applier lands; until then, GDS calls against it return an error
// that this package surfaces as PPR_FAILED.
const semanticGraphName = "cortex.semantic"

// newOllamaClient builds a shared *ollama.HTTPClient from the loaded
// config. The three recall/analyze/reflect adapter files all share
// this helper so the model name and timeout budgets stay in lock-step
// with the defaults cortex up uses.
func newOllamaClient(cfg config.Config) *ollama.HTTPClient {
	return ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
		LinkDerivationTimeout: time.Duration(cfg.Timeouts.LinkDerivationSeconds) * time.Second,
		NumCtx:                cfg.Ollama.NumCtx,
	})
}

// newWeaviateClient mirrors newOllamaClient for Weaviate. The HTTP
// endpoint comes from the same EndpointsConfig block cortex up reads.
func newWeaviateClient(cfg config.Config) *weaviate.HTTPClient {
	return weaviate.NewHTTPClient(
		cfg.Endpoints.WeaviateHTTP,
		time.Duration(cfg.Timeouts.EmbeddingSeconds)*time.Second,
	)
}

// ---------------------------------------------------------------------------
// 1. ConceptExtractor — Ollama Generate + concept_extraction prompt
// ---------------------------------------------------------------------------

type ollamaConceptExtractor struct {
	client *ollama.HTTPClient
}

// Extract renders the concept-extraction prompt with the query as the
// user body, posts it to Ollama, and parses the response line-by-line
// into a concept slice. The prompt instructs the model to emit one
// concept per line, so splitting on newlines is sufficient. Empty
// lines and leading bullets/dashes are tolerated so the model has
// some freedom in its output shape.
func (e *ollamaConceptExtractor) Extract(ctx context.Context, query string) ([]string, error) {
	prompt, err := prompts.Render(prompts.NameConceptExtraction, prompts.Data{Body: query})
	if err != nil {
		return nil, fmt.Errorf("render concept_extraction prompt: %w", err)
	}
	raw, err := e.client.Generate(ctx, prompt)
	if err != nil {
		return nil, err
	}
	concepts := make([]string, 0, 8)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*• \t0123456789.)")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		concepts = append(concepts, line)
	}
	return concepts, nil
}

// ---------------------------------------------------------------------------
// 2. SeedResolver — Neo4j concept -> entry_id lookup
// ---------------------------------------------------------------------------

type neo4jSeedResolver struct {
	client neo4j.Client
}

// Resolve matches :Concept nodes by prefixed entity id (concept:<token>),
// follows any MENTIONS edge to an entry-tagged node, and returns the
// top-K distinct entry_id values. The Cypher deliberately keeps the
// legacy REFERENCES/IN edge types in the relationship list so
// pre-existing graphs populated by other ingest paths still resolve.
//
// The concept id shape MUST match what the write pipeline lays down
// in internal/write/concepts.go (ConceptEntityID). The resolver also
// re-tokenizes the raw concepts slice through write.ExtractConceptTokens
// so LLM-flavored concept phrases ("round-3 regression entry") are
// broken into the same lexical tokens the write side used. Without
// that symmetric tokenization, seed lookups would miss every time the
// concept extractor returned multi-word phrases that the write side
// had lexically split.
//
// When no concepts were supplied (an empty-query degenerate path)
// Resolve returns a nil slice so the PPR stage degenerates to a
// random walk.
func (s *neo4jSeedResolver) Resolve(ctx context.Context, concepts []string, topK int) ([]string, error) {
	if len(concepts) == 0 || topK <= 0 {
		return nil, nil
	}
	// Re-tokenize each incoming concept the same way the write path
	// did, so a concept like "cortex-roundtrip-token" lands as the
	// single id "concept:cortex-roundtrip-token" and a phrase like
	// "persistent memory" becomes ["concept:persistent","concept:memory"].
	seen := make(map[string]struct{}, len(concepts)*2)
	ids := make([]string, 0, len(concepts)*2)
	for _, c := range concepts {
		for _, tok := range write.ExtractConceptTokens(c) {
			id := write.ConceptEntityID(tok)
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	const cypher = `
MATCH (c:Concept)
WHERE c.entry_id IN $conceptIds
MATCH (c)-[:MENTIONS|REFERENCES|IN]-(e)
WHERE e.entry_id IS NOT NULL
RETURN DISTINCT e.entry_id AS id
LIMIT $topK
`
	rows, err := s.client.QueryGraph(ctx, cypher, map[string]any{
		"conceptIds": ids,
		"topK":       int64(topK),
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if id, ok := row["id"].(string); ok && id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 3. PPRRunner — Neo4j GDS Personalized PageRank
// ---------------------------------------------------------------------------

type neo4jPPRRunner struct {
	client    neo4j.Client
	graphName string
}

// Run converts the entry_id seeds into Neo4j internal node IDs,
// invokes gds.pageRank.stream against the configured projection, and
// maps the result back onto entry_id strings. An empty seed set
// returns an empty map without hitting the database — PPR from no
// seeds is a no-op by construction.
//
// The GDS named projection is rebuilt from scratch on every Run via
// refreshProjection. GDS projections are in-memory snapshots, so any
// entries observed after the projection was first created would
// otherwise be invisible to PageRank and trigger a "source node not
// in graph" failure. Phase 1 does not own a centralized projection
// bootstrap step; a follow-up bead should move the refresh to a
// write-path hook so recall doesn't pay the cost on every call.
func (p *neo4jPPRRunner) Run(ctx context.Context, seeds []string, damping float64, maxIter int) (map[string]float64, error) {
	if len(seeds) == 0 {
		return map[string]float64{}, nil
	}
	graph := p.graphName
	if graph == "" {
		graph = semanticGraphName
	}
	if err := p.refreshProjection(ctx, graph); err != nil {
		return nil, err
	}
	cypher := fmt.Sprintf(`
MATCH (seed) WHERE seed.entry_id IN $seeds
WITH collect(id(seed)) AS sourceNodes
CALL gds.pageRank.stream('%s', {
    sourceNodes: sourceNodes,
    dampingFactor: $damping,
    maxIterations: $maxIterations
}) YIELD nodeId, score
MATCH (n) WHERE id(n) = nodeId AND n.entry_id IS NOT NULL AND n.entry_id STARTS WITH 'entry:'
RETURN n.entry_id AS id, score
`, graph)
	rows, err := p.client.RunGDS(ctx, cypher, map[string]any{
		"seeds":         seeds,
		"damping":       damping,
		"maxIterations": int64(maxIter),
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		score, _ := rowFloat64(row, "score")
		out[id] = score
	}
	return out, nil
}

// refreshProjection ensures the named GDS projection is present and
// consistent with the live graph before the PPR stage runs. The fast
// path checks that the projection already exists and that its node +
// relationship counts match the live graph; when they do, the
// existing projection is reused (microseconds). When they differ, or
// when the projection is absent, the slow path drops and re-creates
// the projection from scratch.
//
// This replaces the old "drop + project on every call" strategy,
// which was correct under single-shot recall but blew the bench
// envelope at n=200 (cortex-3kz, p95=11s). Under steady state (bench
// observe phase followed by recall phase, or production recall
// between write bursts) the fast path is taken on every call after
// the first and the projection cost disappears from the hot path.
//
// Any error during the fast-path checks (an unreachable GDS, a
// missing procedure, an unexpected result shape) falls through to
// the slow path so correctness is never compromised — the worst case
// is the old behavior.
//
// gds.graph.drop is called with failIfMissing=false so the first
// Run in a fresh database does not error. The project call uses
// wildcards so Phase 1 doesn't have to enumerate every node label
// and relationship type.
func (p *neo4jPPRRunner) refreshProjection(ctx context.Context, graph string) error {
	if p.projectionMatchesLive(ctx, graph) {
		return nil
	}
	const dropQuery = `CALL gds.graph.drop($graph, false) YIELD graphName RETURN graphName`
	if _, err := p.client.RunGDS(ctx, dropQuery, map[string]any{"graph": graph}); err != nil {
		return fmt.Errorf("gds.graph.drop: %w", err)
	}
	// gds.graph.project must be invoked with a fmt.Sprintf'd graph
	// name because the first argument cannot be parameterised (it's
	// a name literal, not a value). escapeGraphName in internal/neo4j
	// sanitises the graph name so this is safe.
	projectQuery := fmt.Sprintf(
		"CALL gds.graph.project('%s', '*', '*') YIELD graphName RETURN graphName",
		graph,
	)
	if _, err := p.client.RunGDS(ctx, projectQuery, nil); err != nil {
		return fmt.Errorf("gds.graph.project: %w", err)
	}
	return nil
}

// projectionMatchesLive returns true when the named GDS projection
// already exists and its node + relationship counts match the live
// graph's counts. Any error during the checks returns false so the
// caller falls through to a full drop + reproject (correctness-first
// behavior). The two counts are compared together because either
// delta is sufficient to invalidate a projection: an added node that
// no edges touch would still flip nodeCount, and an added edge would
// flip relationshipCount.
func (p *neo4jPPRRunner) projectionMatchesLive(ctx context.Context, graph string) bool {
	existsRows, err := p.client.RunGDS(ctx,
		"CALL gds.graph.exists($graph) YIELD exists RETURN exists",
		map[string]any{"graph": graph})
	if err != nil || len(existsRows) == 0 {
		return false
	}
	exists, _ := existsRows[0]["exists"].(bool)
	if !exists {
		return false
	}
	listQuery := fmt.Sprintf(
		"CALL gds.graph.list('%s') YIELD nodeCount, relationshipCount "+
			"RETURN nodeCount, relationshipCount",
		graph,
	)
	listRows, err := p.client.RunGDS(ctx, listQuery, nil)
	if err != nil || len(listRows) == 0 {
		return false
	}
	projNodes, okN := rowFloat64(listRows[0], "nodeCount")
	projRels, okR := rowFloat64(listRows[0], "relationshipCount")
	if !okN || !okR {
		return false
	}
	liveRows, err := p.client.QueryGraph(ctx,
		"MATCH (n) WITH count(n) AS nodes OPTIONAL MATCH ()-[r]->() "+
			"RETURN nodes, count(r) AS rels",
		nil)
	if err != nil || len(liveRows) == 0 {
		return false
	}
	liveNodes, okN := rowFloat64(liveRows[0], "nodes")
	liveRels, okR := rowFloat64(liveRows[0], "rels")
	if !okN || !okR {
		return false
	}
	return int64(projNodes) == int64(liveNodes) && int64(projRels) == int64(liveRels)
}

// rowFloat64 handles the handful of numeric types the Neo4j driver may
// return for a score column (float64, int64, json.Number variants).
func rowFloat64(row map[string]any, key string) (float64, bool) {
	switch v := row[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// 4. EntryLoader — Neo4j bulk fetch + Weaviate vector lookup
// ---------------------------------------------------------------------------

type neo4jWeaviateEntryLoader struct {
	graph    neo4j.Client
	weaviate weaviate.Client
}

// Load bulk-reads the entry metadata for a candidate set. One Cypher
// query collects body, trail id, community id, cross-project flag,
// and the activation snapshot the write path mirrored onto each
// entry node (base_activation, encoding_at, last_retrieved_at,
// retrieval_count, evicted, pinned, pin_activation). One GraphQL
// round-trip against Weaviate then pulls the embedding vectors keyed
// by cortex_id. Candidate entries missing from Neo4j are simply
// absent from the output map, matching the contract in
// internal/recall/pipeline.go.
//
// A Weaviate fetch failure is non-fatal: the loader still returns
// the Neo4j-resolved metadata with Embedding=nil, so cosine rerank
// silently degrades to a 0 contribution rather than failing the
// whole recall. Cosine is one of four ACT-R weights and the spec
// permits the lexical fallback for cold caches (FR-014).
func (l *neo4jWeaviateEntryLoader) Load(ctx context.Context, entryIDs []string) (map[string]recall.EntryState, error) {
	if len(entryIDs) == 0 {
		return map[string]recall.EntryState{}, nil
	}
	const cypher = `
MATCH (e) WHERE e.entry_id IN $ids
OPTIONAL MATCH (e)-[:IN_TRAIL]->(t:Trail)
OPTIONAL MATCH (e)-[:IN_COMMUNITY]->(c:Community)
RETURN
  e.entry_id          AS id,
  coalesce(e.body,'') AS body,
  t.trail_id          AS trail_id,
  c.community_id      AS community_id,
  coalesce(e.cross_project,false)    AS cross_project,
  coalesce(e.base_activation,1.0)    AS base_activation,
  e.encoding_at                      AS encoding_at,
  e.last_retrieved_at                AS last_retrieved_at,
  coalesce(e.retrieval_count,0)      AS retrieval_count,
  coalesce(e.evicted,false)          AS evicted,
  coalesce(e.pinned,false)           AS pinned,
  coalesce(e.pin_activation,0.0)     AS pin_activation
`
	rows, err := l.graph.QueryGraph(ctx, cypher, map[string]any{"ids": entryIDs})
	if err != nil {
		return nil, err
	}

	// Pull embeddings in a single GraphQL round-trip. A Weaviate
	// failure here is intentionally swallowed: cosine rerank degrades
	// to a 0 contribution but the rest of the ACT-R formula (base /
	// PPR / importance) is unaffected, and a cold Weaviate must not
	// kill recall on its hot path.
	vectors := map[string][]float32{}
	if l.weaviate != nil {
		if vs, verr := l.weaviate.FetchVectorsByCortexIDs(ctx, weaviate.ClassEntry, entryIDs); verr == nil {
			vectors = vs
		}
	}

	out := make(map[string]recall.EntryState, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		body, _ := row["body"].(string)
		trailID, _ := row["trail_id"].(string)
		communityID := fmt.Sprint(row["community_id"])
		if row["community_id"] == nil {
			communityID = ""
		}
		crossProject, _ := row["cross_project"].(bool)

		baseAct, _ := rowFloat64(row, "base_activation")
		retrievalCount := 0
		if n, ok := row["retrieval_count"].(int64); ok {
			retrievalCount = int(n)
		}
		evicted, _ := row["evicted"].(bool)
		pinned, _ := row["pinned"].(bool)
		pinAct, _ := rowFloat64(row, "pin_activation")

		state := activation.State{
			EncodingAt:      parseRowTime(row["encoding_at"]),
			BaseActivation:  baseAct,
			RetrievalCount:  retrievalCount,
			LastRetrievedAt: parseRowTime(row["last_retrieved_at"]),
			Pinned:          pinned,
			PinActivation:   pinAct,
			Evicted:         evicted,
		}
		out[id] = recall.EntryState{
			EntryID:      id,
			Body:         body,
			Embedding:    vectors[id],
			Activation:   state,
			TrailID:      trailID,
			CommunityID:  communityID,
			CrossProject: crossProject,
		}
	}
	return out, nil
}

// parseRowTime accepts the handful of time-ish shapes the Neo4j
// driver returns: an RFC3339 string (the shape the write pipeline
// emits), a time.Time value (for drivers that pre-parse), or nil.
// Anything else collapses to the zero Time so the downstream decay
// math treats the entry as freshly encoded.
func parseRowTime(v any) time.Time {
	switch t := v.(type) {
	case nil:
		return time.Time{}
	case time.Time:
		return t
	case string:
		if t == "" {
			return time.Time{}
		}
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return parsed
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 5. QueryEmbedder — already satisfied by observeEmbedder from observe.go
// ---------------------------------------------------------------------------
//
// The existing observeEmbedder type in observe.go implements both
// write.Embedder AND recall.QueryEmbedder (both require the same
// Embed(ctx, text) -> []float32 method). buildRecallPipeline reuses
// it directly instead of defining a second type.

// ---------------------------------------------------------------------------
// 6. ContextFetcher — Neo4j trail + community summary lookups
// ---------------------------------------------------------------------------

type neo4jContextFetcher struct {
	client neo4j.Client
}

// Trail returns the persisted summary for a trail, or ("", nil) if
// the id is empty or no matching Trail node exists. Empty is not an
// error because entries may legitimately have no trail attachment.
func (f *neo4jContextFetcher) Trail(ctx context.Context, trailID string) (string, error) {
	if trailID == "" {
		return "", nil
	}
	rows, err := f.client.QueryGraph(ctx,
		`MATCH (t:Trail {trail_id: $id}) RETURN coalesce(t.summary,'') AS summary LIMIT 1`,
		map[string]any{"id": trailID})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	s, _ := rows[0]["summary"].(string)
	return s, nil
}

// Community mirrors Trail for Community nodes. The schema matches the
// one in internal/community/list.go: Community nodes keyed by
// community_id with a summary property populated by the refresher.
func (f *neo4jContextFetcher) Community(ctx context.Context, communityID string) (string, error) {
	if communityID == "" {
		return "", nil
	}
	rows, err := f.client.QueryGraph(ctx,
		`MATCH (c:Community {community_id: $id}) RETURN coalesce(c.summary,'') AS summary LIMIT 1`,
		map[string]any{"id": communityID})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	s, _ := rows[0]["summary"].(string)
	return s, nil
}
