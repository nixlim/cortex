package neo4j

import (
	"fmt"
)

// This file holds the small helpers that shape GDS procedure calls
// for the write- and recall-pipeline call sites. The adapter exposes
// the raw RunGDS entry point for flexibility, but the helpers below
// exist so Cortex's own call sites build consistent Cypher without
// each re-deriving the parameter names from the GDS documentation.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Community Detection" (Leiden/Louvain)
//   docs/spec/cortex-spec.md §"Recall" (PPR seeds + damping)

// PersonalizedPageRankQuery builds the Cypher for a personalised
// PageRank stream over a projected graph. The call sites in the
// recall pipeline use this to expand the top-K seed set into the
// HippoRAG-style activated node set.
//
// The returned query expects the graph projection named graphName to
// already exist (created in a prior step via gds.graph.project).
func PersonalizedPageRankQuery(graphName string) string {
	return fmt.Sprintf(
		`CALL gds.pageRank.stream('%s', {
            sourceNodes: $seeds,
            dampingFactor: $damping,
            maxIterations: $maxIterations
        }) YIELD nodeId, score
        RETURN nodeId, score ORDER BY score DESC LIMIT $limit`,
		escapeGraphName(graphName),
	)
}

// LeidenStreamQuery builds the Cypher for a Leiden community
// detection stream. Cortex uses Leiden as the primary community
// detection algorithm (FR-028) and falls back to Louvain when
// ProbeProcedures reports LeidenUnavailable.
func LeidenStreamQuery(graphName string) string {
	return fmt.Sprintf(
		`CALL gds.leiden.stream('%s', {
            maxLevels: $maxLevels,
            gamma: $resolution,
            tolerance: $tolerance,
            randomSeed: 42
        }) YIELD nodeId, communityId, intermediateCommunityIds
        RETURN nodeId, communityId, intermediateCommunityIds`,
		escapeGraphName(graphName),
	)
}

// LouvainStreamQuery builds the Cypher for the Louvain fallback.
func LouvainStreamQuery(graphName string) string {
	return fmt.Sprintf(
		`CALL gds.louvain.stream('%s', {
            maxLevels: $maxLevels,
            tolerance: $tolerance
        }) YIELD nodeId, communityId, intermediateCommunityIds
        RETURN nodeId, communityId, intermediateCommunityIds`,
		escapeGraphName(graphName),
	)
}

// CommunityLeidenStreamQuery is the Leiden stream variant used by the
// community-detection path over the derived Entry↔Entry projection.
// It differs from LeidenStreamQuery in one place only — the
// relationshipWeightProperty: 'weight' option — so Leiden multiplies
// the modularity contribution of each edge by the number of shared
// concepts that produced it. Without this, two Entries linked via 20
// shared concepts would be no more tightly coupled than two Entries
// linked via 1, collapsing the signal the projection was built to
// surface. A separate function (rather than a second argument) keeps
// the wildcard recall-time Leiden path from breaking: cortex.semantic
// does not carry a 'weight' property and GDS would error if the
// option referenced a non-existent one. See cortex-rjz.
func CommunityLeidenStreamQuery(graphName string) string {
	return fmt.Sprintf(
		`CALL gds.leiden.stream('%s', {
            maxLevels: $maxLevels,
            gamma: $resolution,
            tolerance: $tolerance,
            randomSeed: 42,
            relationshipWeightProperty: 'weight'
        }) YIELD nodeId, communityId, intermediateCommunityIds
        RETURN nodeId, communityId, intermediateCommunityIds`,
		escapeGraphName(graphName),
	)
}

// CommunityLouvainStreamQuery is the weighted-edge variant of
// LouvainStreamQuery for the community projection. Same rationale as
// CommunityLeidenStreamQuery — the cortex.community graph carries a
// 'weight' property that Leiden and Louvain must read to honour the
// shared-concept signal.
func CommunityLouvainStreamQuery(graphName string) string {
	return fmt.Sprintf(
		`CALL gds.louvain.stream('%s', {
            maxLevels: $maxLevels,
            tolerance: $tolerance,
            relationshipWeightProperty: 'weight'
        }) YIELD nodeId, communityId, intermediateCommunityIds
        RETURN nodeId, communityId, intermediateCommunityIds`,
		escapeGraphName(graphName),
	)
}

// escapeGraphName is a defence-in-depth check: graph names are
// produced internally by Cortex (e.g., "cortex.semantic") and should
// never contain characters that would break out of the single-quoted
// literal. We reject anything with a quote or backslash rather than
// attempt to escape, because the right fix is to pick a safer name
// upstream.
func escapeGraphName(name string) string {
	for _, r := range name {
		if r == '\'' || r == '\\' || r == '\n' || r == '\r' {
			panic(fmt.Sprintf("neo4j: unsafe graph name %q", name))
		}
	}
	return name
}
