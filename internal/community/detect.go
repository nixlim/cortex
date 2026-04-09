// Package community runs hierarchical community detection over the
// Cortex knowledge graph and persists the results back into Neo4j.
//
// Cortex prefers Leiden (gds.leiden.stream) and falls back to Louvain
// (gds.louvain.stream) per FR-028. The fallback decision is made by
// the caller via a ProcedureAvailability report from the neo4j
// adapter; this package does not probe on its own so the choice can
// be logged at the call site and so unit tests can drive both paths
// without touching a live database.
//
// Each resolution in the configured list produces one "level" of the
// hierarchy. The spec fixes three levels at resolutions
// [1.0, 0.5, 0.1]. The relationship between levels is membership-
// only: a node at level 0 is tagged with the communityId it lands in
// at resolution 1.0; at level 1 with the id from resolution 0.5; and
// so on. Higher levels are coarser (fewer, larger communities).
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Community Detection"
//	docs/spec/cortex-spec.md §"FR-028 Leiden preferred, Louvain fallback"
package community

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Algorithm selects which GDS procedure Detect invokes.
type Algorithm int

const (
	// AlgorithmLeiden uses gds.leiden.stream. Preferred per FR-028.
	AlgorithmLeiden Algorithm = iota
	// AlgorithmLouvain uses gds.louvain.stream. Used when Leiden is
	// unavailable in the running GDS plugin.
	AlgorithmLouvain
)

func (a Algorithm) String() string {
	switch a {
	case AlgorithmLeiden:
		return "leiden"
	case AlgorithmLouvain:
		return "louvain"
	default:
		return "unknown"
	}
}

// Config is the per-run tuning the caller hands to Detect. It mirrors
// the CommunityDetection section of ~/.cortex/config.yaml but is
// repeated here locally so this package does not import
// internal/config (config churn must not ripple into detection).
type Config struct {
	// GraphName is the GDS in-memory projection the caller has
	// already created (e.g., via gds.graph.project) before calling
	// Detect. Community detection reads from this projection; it is
	// not the name of a Neo4j database.
	GraphName string

	// Resolutions is the list of gamma values passed to Leiden or
	// (ignored parameter but used as a loop counter for) Louvain.
	// Must be strictly decreasing and its length must equal Levels.
	Resolutions []float64

	// Levels is the number of hierarchy levels to produce. Must
	// equal len(Resolutions); Detect enforces this and returns
	// ErrResolutionLevelsMismatch if they disagree.
	Levels int

	// MaxIterations caps the GDS procedure's inner loop.
	MaxIterations int

	// Tolerance is the convergence threshold handed to GDS.
	Tolerance float64
}

// ErrResolutionLevelsMismatch is returned by Detect when the caller
// passes a Config whose Resolutions list does not have exactly Levels
// entries. The spec enforces this at startup so a typo in the config
// file fails fast rather than silently producing a degraded
// hierarchy.
var ErrResolutionLevelsMismatch = errors.New("community: len(resolutions) must equal levels")

// ErrEmptyGraphName is returned when the caller forgets to set
// Config.GraphName. The GDS procedures take a projected graph name
// as their first argument; running them against "" errors inside
// Neo4j with a less useful message.
var ErrEmptyGraphName = errors.New("community: graph name is required")

// Community is a single detected community at a single level. The
// Members list contains the Neo4j internal node IDs returned by GDS;
// higher layers resolve these into Cortex entry IDs via a separate
// Cypher lookup. Summary is left empty by Detect; see Refresh for
// the LLM-driven summary path.
type Community struct {
	ID       int64   // GDS communityId within this level
	Level    int     // 0..Levels-1, matches index into Config.Resolutions
	Members  []int64 // Neo4j internal node IDs
	Summary  string  // populated by Refresh, not Detect
	TopNodes []int64 // first N members, used for prompt building
}

// Neo4jClient is the narrow subset of the neo4j adapter Detect needs.
// Keeping the interface local means we can build a fake in
// detect_test.go without dragging in the Bolt driver.
type Neo4jClient interface {
	RunGDS(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)
	WriteEntries(ctx context.Context, cypher string, params map[string]any) error
}

// GDSQueryBuilder lets the caller inject the exact Cypher fragments
// Detect should use. The neo4j package exports LeidenStreamQuery and
// LouvainStreamQuery for this purpose; tests substitute stubs so
// they don't have to match the real procedure call strings verbatim.
type GDSQueryBuilder func(graphName string) string

// Detector runs community detection and persists the result. It is
// stateless aside from the injected dependencies; a single Detector
// can be reused across multiple Detect calls with different configs.
type Detector struct {
	Neo4j Neo4jClient

	// LeidenQuery / LouvainQuery let callers pass in the query
	// builders from the neo4j package (or fakes from tests).
	LeidenQuery  GDSQueryBuilder
	LouvainQuery GDSQueryBuilder

	// TopNodeCount caps the number of members stored in Community.TopNodes
	// for the prompt-building path. Zero means "all members".
	TopNodeCount int
}

// Detect runs the chosen algorithm once per resolution level and
// returns the resulting hierarchy grouped by (level, communityId).
// It does NOT persist anything; callers chain Detect → Persist so
// the two concerns can be unit-tested independently.
func (d *Detector) Detect(ctx context.Context, alg Algorithm, cfg Config) ([][]Community, error) {
	if cfg.GraphName == "" {
		return nil, ErrEmptyGraphName
	}
	if cfg.Levels != len(cfg.Resolutions) {
		return nil, fmt.Errorf("%w: levels=%d resolutions=%d",
			ErrResolutionLevelsMismatch, cfg.Levels, len(cfg.Resolutions))
	}
	if d.Neo4j == nil {
		return nil, errors.New("community: no neo4j client configured")
	}

	builder := d.LeidenQuery
	if alg == AlgorithmLouvain {
		builder = d.LouvainQuery
	}
	if builder == nil {
		return nil, fmt.Errorf("community: no query builder for %s", alg)
	}

	hierarchy := make([][]Community, cfg.Levels)
	for level, resolution := range cfg.Resolutions {
		cypher := builder(cfg.GraphName)
		params := map[string]any{
			"maxLevels":  cfg.MaxIterations,
			"resolution": resolution,
			"tolerance":  cfg.Tolerance,
		}
		rows, err := d.Neo4j.RunGDS(ctx, cypher, params)
		if err != nil {
			return nil, fmt.Errorf("community: %s level %d: %w", alg, level, err)
		}
		hierarchy[level] = groupByCommunity(rows, level, d.TopNodeCount)
	}
	return hierarchy, nil
}

// groupByCommunity collapses a flat "nodeId, communityId" stream
// into one Community per communityId. The output is sorted by
// community ID so test assertions and persistence writes are
// deterministic.
func groupByCommunity(rows []map[string]any, level, topN int) []Community {
	byID := map[int64]*Community{}
	for _, row := range rows {
		cid, ok := rowInt64(row, "communityId")
		if !ok {
			continue
		}
		nid, ok := rowInt64(row, "nodeId")
		if !ok {
			continue
		}
		c, exists := byID[cid]
		if !exists {
			c = &Community{ID: cid, Level: level}
			byID[cid] = c
		}
		c.Members = append(c.Members, nid)
	}

	out := make([]Community, 0, len(byID))
	for _, c := range byID {
		if topN > 0 && len(c.Members) > topN {
			c.TopNodes = append(c.TopNodes, c.Members[:topN]...)
		} else {
			c.TopNodes = append(c.TopNodes, c.Members...)
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rowInt64 reads an integer value from a driver result row. The
// Neo4j driver decodes Cypher integers as int64, but tests may use
// plain ints, so we accept both.
func rowInt64(row map[string]any, key string) (int64, bool) {
	v, ok := row[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	default:
		return 0, false
	}
}

// Persist writes the hierarchy to Neo4j by MERGE-ing a :Community
// node per (level, communityId) and MERGE-ing :IN_COMMUNITY edges
// from each member node. The Cypher is intentionally one statement
// per community so a mid-run failure leaves earlier communities
// committed; callers that want all-or-nothing semantics should wrap
// this in a Neo4j transaction at a higher layer.
func (d *Detector) Persist(ctx context.Context, hierarchy [][]Community) error {
	if d.Neo4j == nil {
		return errors.New("community: no neo4j client configured")
	}
	const cypher = `
MERGE (c:Community {level: $level, community_id: $community_id})
SET c.member_count = $member_count, c.summary = $summary
WITH c
UNWIND $members AS nid
MATCH (n) WHERE id(n) = nid
MERGE (n)-[:IN_COMMUNITY {level: $level}]->(c)
`
	for _, level := range hierarchy {
		for _, c := range level {
			params := map[string]any{
				"level":        c.Level,
				"community_id": c.ID,
				"member_count": len(c.Members),
				"members":      c.Members,
				"summary":      c.Summary,
			}
			if err := d.Neo4j.WriteEntries(ctx, cypher, params); err != nil {
				return fmt.Errorf("community: persist level %d id %d: %w", c.Level, c.ID, err)
			}
		}
	}
	return nil
}
