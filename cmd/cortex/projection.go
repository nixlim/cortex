// cmd/cortex/projection.go owns the lifecycle of the shared GDS
// in-memory projection that both the recall pipeline (PPR) and the
// community-detection subcommand read from. The two call sites need
// the same "is this projection present and consistent with the live
// graph?" fast path, and they need to agree on the projection's name.
//
// Placement rationale: the helper lives in cmd/cortex rather than
// internal/neo4j because the projection-name constant and the
// wildcard project-everything policy are product decisions, not part
// of the Neo4j adapter surface. internal/neo4j stays a thin
// driver-agnostic wrapper; the "which labels and rels are in our
// semantic projection" choice stays in the command layer.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Neo4j with Graph Data Science"
//   cortex-6vi (staleness regression) / cortex-3kz (bench envelope)
package main

import (
	"context"
	"fmt"

	"github.com/nixlim/cortex/internal/neo4j"
)

// ensureSemanticProjection guarantees the named GDS projection exists
// and is consistent with the live graph. The fast path checks that
// the projection exists and that its node + relationship counts match
// the live graph; when they do the existing projection is reused
// (microseconds). When they differ — or when the projection is
// absent — the slow path drops and re-creates the projection from
// scratch via gds.graph.project with wildcard label/rel selectors.
//
// Any error during the fast-path checks (an unreachable GDS, a
// missing procedure, an unexpected result shape) falls through to
// the slow path so correctness is never compromised: the worst case
// is the old always-rebuild behavior.
//
// gds.graph.drop is called with failIfMissing=false so the first
// call in a fresh database does not error. The project call uses
// wildcards ('*','*') so Phase 1 does not have to enumerate every
// node label and relationship type.
func ensureSemanticProjection(ctx context.Context, client neo4j.Client, graph string) error {
	if projectionMatchesLive(ctx, client, graph) {
		return nil
	}
	const dropQuery = `CALL gds.graph.drop($graph, false) YIELD graphName RETURN graphName`
	if _, err := client.RunGDS(ctx, dropQuery, map[string]any{"graph": graph}); err != nil {
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
	if _, err := client.RunGDS(ctx, projectQuery, nil); err != nil {
		return fmt.Errorf("gds.graph.project: %w", err)
	}
	return nil
}

// validateGraphName panics if the graph name contains characters
// that would break out of a single-quoted Cypher string literal.
// Graph names are internal constants (cortex.semantic, cortex.community)
// so in practice this is a defence-in-depth assertion, not a
// sanitiser — we mirror internal/neo4j.escapeGraphName but can't
// import that package-private symbol across the module boundary.
func validateGraphName(name string) string {
	for _, r := range name {
		if r == '\'' || r == '\\' || r == '\n' || r == '\r' {
			panic(fmt.Sprintf("cortex: unsafe graph name %q", name))
		}
	}
	return name
}

// CommunityProjectionHubDegreeCap is the maximum per-concept MENTIONS
// degree allowed into the Entry↔Entry shared-concept projection.
// Concepts mentioned by more Entries than this are dropped from the
// projection entirely.
//
// Why a cap: the raw co-mention join is O(deg^2) per concept. On the
// cortex + myagentsgigs graph the most-mentioned concept is degree
// 5,775, which would alone contribute ~16.7M Entry pairs and blow
// Neo4j's 1.4 GiB transaction memory pool (measured 2026-04-15 during
// cortex-rjz probe runs). Hub concepts are also the LEAST informative
// edges for community detection — a node mentioned by every Entry
// connects everything to everything and collapses the modularity
// score to uniform, which is the co-mention analogue of TF-IDF
// stop-word filtering.
//
// 50 was chosen empirically: at cap=50 the projection builds in under
// 700ms (5,569 Entry nodes, ~1.3M undirected relationships on the
// cortex + myagentsgigs graph) and Leiden produces ~11 non-trivial
// communities at γ=1.0. Raising the cap keeps more signal at the
// cost of memory; lowering it drops the mid-frequency concepts that
// actually discriminate between topics.
const CommunityProjectionHubDegreeCap = 50

// CommunityProjectionMinDegree drops concepts that only one Entry
// mentions. A degree-1 concept contributes zero Entry-Entry pairs
// anyway, but filtering it in the node-source subquery keeps the GDS
// planner from iterating every lone concept just to emit no edges.
const CommunityProjectionMinDegree = 2

// ensureCommunityProjection builds the GDS projection used by the
// community-detection path. Unlike the wildcard semantic projection
// (which serves PPR and is kept in sync with the live graph), the
// community projection is derived: nodes are :Entry only, and
// relationships are weighted Entry↔Entry edges where the weight is
// the number of :Concept nodes two Entries share via MENTIONS. Hub
// concepts above CommunityProjectionHubDegreeCap are filtered out to
// keep the co-mention join within Neo4j transaction memory and to
// strip the "mentioned by everything" nodes that would otherwise
// collapse Louvain/Leiden into one giant community.
//
// The projection uses the aggregation form of gds.graph.project so
// we can request undirected orientation — Leiden requires it — and
// ship the weight as a relationship property in a single Cypher
// statement. WHERE id(e1) < id(e2) deduplicates the symmetric join
// before aggregation; {undirectedRelationshipTypes: ['SHARED_CONCEPT']}
// tells GDS to treat the resulting edges as undirected so both
// Leiden and Louvain see them correctly.
//
// We always drop and rebuild rather than use a projectionMatchesLive
// fast path: the projection is derived, so a node/rel count check
// against the live graph never matches; attempting to reuse a stale
// projection would silently serve pre-ingest community state. The
// rebuild runs in ~600ms on the 5,797-entry graph, an acceptable
// one-time cost per `cortex communities detect` invocation.
//
// Placement rationale: same as ensureSemanticProjection — the
// projection's semantic shape is a product decision, not an adapter
// concern, so it stays in cmd/cortex. See cortex-rjz.
func ensureCommunityProjection(ctx context.Context, client neo4j.Client, graph string) error {
	const dropQuery = `CALL gds.graph.drop($graph, false) YIELD graphName RETURN graphName`
	if _, err := client.RunGDS(ctx, dropQuery, map[string]any{"graph": graph}); err != nil {
		return fmt.Errorf("gds.graph.drop: %w", err)
	}
	// The projection name is a literal in the subquery (cannot be
	// parameterised — it's a graph-name literal). The caller passes
	// the communityGraphName const, so there is no injection surface;
	// validateGraphName rejects anything that would break out of the
	// single-quoted literal as a defence-in-depth check. The MATCH
	// chain reads:
	//   - Filter :Concept nodes by MENTIONS degree ∈ [min, cap]
	//   - Walk (e1)-[:MENTIONS]->(c)<-[:MENTIONS]-(e2) with id(e1) <
	//     id(e2) so each unordered pair appears once
	//   - count(c) is the shared-concept weight
	//   - gds.graph.project accumulates the edges and emits the
	//     in-memory graph; undirectedRelationshipTypes turns the
	//     directed src→tgt insertion into an undirected edge for
	//     Leiden/Louvain to consume.
	projectQuery := fmt.Sprintf(
		`MATCH (c:Concept)
WITH c, size([(c)<-[:MENTIONS]-(:Entry) | 1]) AS deg
WHERE deg >= %d AND deg <= %d
MATCH (e1:Entry)-[:MENTIONS]->(c)<-[:MENTIONS]-(e2:Entry)
WHERE id(e1) < id(e2)
WITH e1, e2, count(c) AS weight
WITH gds.graph.project(
  '%s',
  e1,
  e2,
  {relationshipProperties: {weight: weight}, relationshipType: 'SHARED_CONCEPT'},
  {undirectedRelationshipTypes: ['SHARED_CONCEPT']}
) AS g
RETURN g.graphName AS graphName, g.nodeCount AS nodeCount, g.relationshipCount AS relationshipCount`,
		CommunityProjectionMinDegree,
		CommunityProjectionHubDegreeCap,
		validateGraphName(graph),
	)
	if _, err := client.RunGDS(ctx, projectQuery, nil); err != nil {
		return fmt.Errorf("gds.graph.project (community): %w", err)
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
func projectionMatchesLive(ctx context.Context, client neo4j.Client, graph string) bool {
	existsRows, err := client.RunGDS(ctx,
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
	listRows, err := client.RunGDS(ctx, listQuery, nil)
	if err != nil || len(listRows) == 0 {
		return false
	}
	projNodes, okN := rowFloat64(listRows[0], "nodeCount")
	projRels, okR := rowFloat64(listRows[0], "relationshipCount")
	if !okN || !okR {
		return false
	}
	liveRows, err := client.QueryGraph(ctx,
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
