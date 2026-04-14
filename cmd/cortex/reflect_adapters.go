// cmd/cortex/reflect_adapters.go holds the bridge adapters the
// reflect pipeline needs. Unlike analyze, reflect has a per-frame
// watermark so an interrupted run can resume without reprocessing
// already-written frames (AC3 of US-3).
//
// Three adapters:
//
//   - neo4jReflectClusterSource   : Cypher enumeration of episodic
//     clusters whose youngest exemplar tx is strictly greater than
//     the stored reflection watermark.
//   - neo4jReflectionWatermarkStore : read/write of the :Reflection
//     watermark node.
//   - reflectFrameProposerBridge  : adapts the shared
//     ollamaFrameProposer (analyze-shaped) to the reflect-shaped
//     FrameProposer interface.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-3 (reflection BDD scenarios)
//	bead cortex-4kq.44, code-review fix CRIT-003
package main

import (
	"context"
	"time"

	"github.com/nixlim/cortex/internal/analyze"
	"github.com/nixlim/cortex/internal/llm"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/reflect"
)

// ---------------------------------------------------------------------------
// 1. ClusterSource — Neo4j episodic cluster enumeration since watermark
// ---------------------------------------------------------------------------

type neo4jReflectClusterSource struct {
	client neo4j.Client
}

// Candidates returns episodic clusters whose youngest exemplar tx is
// strictly greater than sinceTx. Each row carries the exemplar list
// (entry_id, timestamp, tx), the pre-computed average pairwise
// cosine similarity, the number of distinct day-granularity
// timestamps, and the MDL ratio.
//
// The query assumes the write pipeline attaches :IN_COMMUNITY edges
// to entries with a level-0 Community node carrying avg_cosine /
// mdl_ratio properties. Until the write-side applier populates
// those properties this returns empty; the threshold filter in the
// reflect pipeline drops each candidate as BELOW_COSINE_FLOOR or
// similar, which is the correct "no work to do" behaviour.
func (s *neo4jReflectClusterSource) Candidates(ctx context.Context, sinceTx string) ([]reflect.ClusterCandidate, error) {
	const cypher = `
MATCH (c:Community {level: 0})<-[:IN_COMMUNITY]-(e)
WHERE e.entry_id IS NOT NULL
WITH c, collect({
  entry_id: e.entry_id,
  ts:       coalesce(e.encoding_at,''),
  tx:       coalesce(e.tx,'')
}) AS members
WITH c, members,
     [m IN members WHERE m.tx > $sinceTx | m] AS newMembers
WHERE size(newMembers) > 0
RETURN
  'C' + toString(c.community_id)                AS id,
  members,
  coalesce(c.avg_cosine,0.0)                    AS avg_cosine,
  coalesce(c.distinct_timestamps,0)             AS distinct_ts,
  coalesce(c.mdl_ratio,0.0)                     AS mdl_ratio
`
	rows, err := s.client.QueryGraph(ctx, cypher, map[string]any{
		"sinceTx": sinceTx,
	})
	if err != nil {
		return nil, err
	}
	out := make([]reflect.ClusterCandidate, 0, len(rows))
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		exemplars := parseReflectExemplars(row["members"])
		avgCos, _ := rowFloat64(row, "avg_cosine")
		mdl, _ := rowFloat64(row, "mdl_ratio")
		distinctTS := 0
		if n, ok := row["distinct_ts"].(int64); ok {
			distinctTS = int(n)
		}
		out = append(out, reflect.ClusterCandidate{
			ID:                    id,
			Exemplars:             exemplars,
			AveragePairwiseCosine: avgCos,
			DistinctTimestamps:    distinctTS,
			MDLRatio:              mdl,
		})
	}
	return out, nil
}

// parseReflectExemplars decodes the collect(map) column into
// reflect.ExemplarRef values. Timestamps arrive as RFC3339 strings
// the write path emits; malformed entries collapse to zero time.
func parseReflectExemplars(raw any) []reflect.ExemplarRef {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]reflect.ExemplarRef, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["entry_id"].(string)
		if id == "" {
			continue
		}
		tsStr, _ := m["ts"].(string)
		var ts time.Time
		if tsStr != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
				ts = parsed
			} else if parsed, err := time.Parse(time.RFC3339, tsStr); err == nil {
				ts = parsed
			}
		}
		tx, _ := m["tx"].(string)
		out = append(out, reflect.ExemplarRef{
			EntryID:   id,
			Timestamp: ts,
			Tx:        tx,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 2. WatermarkStore — :Reflection node read/write
// ---------------------------------------------------------------------------

type neo4jReflectionWatermarkStore struct {
	client neo4j.Client
}

// ReadReflectionWatermark fetches the persisted watermark. A missing
// node returns the empty string, which the reflect pipeline treats
// as "from the beginning".
func (w *neo4jReflectionWatermarkStore) ReadReflectionWatermark(ctx context.Context) (string, error) {
	const cypher = `MATCH (r:Reflection {id: 'singleton'}) RETURN coalesce(r.tx,'') AS tx LIMIT 1`
	rows, err := w.client.QueryGraph(ctx, cypher, nil)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	tx, _ := rows[0]["tx"].(string)
	return tx, nil
}

// WriteReflectionWatermark upserts the singleton :Reflection node.
// The pipeline advances the watermark per accepted frame, so this
// method is called in a hot loop and must be idempotent.
func (w *neo4jReflectionWatermarkStore) WriteReflectionWatermark(ctx context.Context, tx string) error {
	const cypher = `
MERGE (r:Reflection {id: 'singleton'})
SET r.tx = $tx, r.updated_at = $updated_at
`
	return w.client.WriteEntries(ctx, cypher, map[string]any{
		"tx":         tx,
		"updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// ---------------------------------------------------------------------------
// 3. FrameProposer — delegates to the shared ollamaFrameProposer
// ---------------------------------------------------------------------------

// reflectFrameProposerBridge adapts the analyze-shaped
// ollamaFrameProposer to the reflect.FrameProposer interface. Both
// Propose methods take an (id, exemplars) cluster and return a frame
// with (type, slots, exemplars); the reflect shape drops the
// cross-project Projects/Importance fields, so a direct field copy
// is sufficient.
type reflectFrameProposerBridge struct {
	client llm.Generator
}

func (p *reflectFrameProposerBridge) Propose(ctx context.Context, cluster reflect.ClusterCandidate) (*reflect.Frame, error) {
	// Project the reflect cluster into the analyze shape so we can
	// reuse the shared prompt + JSON-parse path. Project/Migrated are
	// irrelevant for reflection and left empty.
	analyzeCluster := analyze.ClusterCandidate{
		ID:       cluster.ID,
		MDLRatio: cluster.MDLRatio,
	}
	for _, ex := range cluster.Exemplars {
		analyzeCluster.Exemplars = append(analyzeCluster.Exemplars, analyze.ExemplarRef{
			EntryID: ex.EntryID,
		})
	}
	proposer := &ollamaFrameProposer{client: p.client, source: "reflect"}
	frame, err := proposer.Propose(ctx, analyzeCluster)
	if err != nil {
		return nil, err
	}
	if frame == nil {
		return nil, nil
	}
	exemplarIDs := make([]string, 0, len(cluster.Exemplars))
	for _, ex := range cluster.Exemplars {
		exemplarIDs = append(exemplarIDs, ex.EntryID)
	}
	return &reflect.Frame{
		Type:          frame.Type,
		Slots:         frame.Slots,
		Exemplars:     exemplarIDs,
		SchemaVersion: reflect.FrameSchemaVersion,
	}, nil
}
