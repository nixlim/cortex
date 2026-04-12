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
