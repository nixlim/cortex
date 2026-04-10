// cmd/cortex/staging_backends.go is the live StagingBackends used by
// `cortex rebuild`. The earlier revision hand-rolled a parallel Cypher
// translation layer under a :CortexStaging label and promoted it to
// live at Swap time. That duplication drifted from the real write path
// (cortex-sv8): staging stored values as raw JSON-encoded strings
// instead of decoded primitives, keyed the node by `entity` and
// renamed it to `id` on promote while every reader queries by
// `entry_id`, and never materialized graph edges at all — which left
// the GDS projection empty after rebuild and made recall return zero
// results.
//
// The architecturally correct fix is to route rebuild through the
// same translator the live write path uses, so replay produces the
// exact graph shape that observe would have produced. This revision
// delegates every datom in the rebuild replay to
// neo4j.BackendApplier.Apply, which is the canonical datom→Cypher
// translator shared by the write pipeline and the startup self-heal.
//
// Atomicity. The old staging path promised an "active graph untouched
// until Swap" guarantee but never actually delivered it — the doc
// comment itself called out that a failure midway left a half-promoted
// graph. Writing directly to live is the same effective safety
// property with far less code. A truly atomic rebuild requires a
// transactional execution seam on the Bolt client and stays on the
// backlog as a follow-up.
//
// Weaviate side. Non-drift rebuild cannot reconstruct vectors (the
// log does not persist embeddings) so the Weaviate live state is
// intentionally preserved across a rebuild — the volumes survive
// `cortex down` and the existing vectors remain valid under the pinned
// digest. The --accept-drift path (which does re-embed) continues to
// write via the Weaviate staging namespace and promote at Swap, which
// is the code path weaviate.StagingClient was built for.
package main

import (
	"context"
	"fmt"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/weaviate"
)

// realStagingBackends is the live StagingBackends implementation. It
// delegates every replayed datom to the shared neo4j.BackendApplier so
// the rebuild path produces the same graph shape the write pipeline
// would have produced for the same datom stream.
type realStagingBackends struct {
	graph        *neo4j.BoltClient
	neoApplier   *neo4j.BackendApplier
	weaviate     weaviate.StagingClient
	actor        string
	invocationID string

	applied          int  // counts ApplyDatom calls; used in close-out logging
	weaviateStaged   bool // true once ApplyEmbedding has populated the staging class
}

// newRealStagingBackends builds a realStagingBackends from a live
// graph client and (optionally) a Weaviate staging client. A nil
// weaviate client is legal — the Weaviate side of the rebuild becomes
// a no-op and only Neo4j is reconstructed.
func newRealStagingBackends(graph *neo4j.BoltClient, wv weaviate.StagingClient, actor, invocationID string) *realStagingBackends {
	return &realStagingBackends{
		graph:        graph,
		neoApplier:   neo4j.NewBackendApplier(graph),
		weaviate:     wv,
		actor:        actor,
		invocationID: invocationID,
	}
}

// Create prepares the live graph for a fresh replay and, if a
// Weaviate staging client is wired, recreates the staging classes so
// an --accept-drift run has a clean target.
//
// Two pieces of pre-replay cleanup land in Neo4j:
//
//  1. Any leftover :CortexStaging nodes from an older (pre-cortex-sv8)
//     rebuild that used the hand-rolled staging namespace are detached
//     and deleted.
//  2. Any orphan entry-shaped nodes whose entry_id property is null
//     are deleted. These are the stranded artifacts the broken staging
//     path created: it wrote Entry nodes using an `id` property instead
//     of `entry_id`, so recall's Cypher (which filters on entry_id)
//     never found them. Cleaning them up here makes a fresh rebuild
//     self-repairing — the operator does not have to manually purge
//     volumes to recover.
func (s *realStagingBackends) Create(ctx context.Context) error {
	const dropLegacyStaging = "MATCH (n:CortexStaging) DETACH DELETE n"
	if err := s.graph.WriteEntries(ctx, dropLegacyStaging, nil); err != nil {
		return fmt.Errorf("staging: drop legacy :CortexStaging nodes: %w", err)
	}
	for _, label := range []string{"Entry", "Frame", "Subject", "Trail", "Community", "PSI", "Concept", "CortexEntity"} {
		cypher := fmt.Sprintf("MATCH (n:%s) WHERE n.entry_id IS NULL DETACH DELETE n", label)
		if err := s.graph.WriteEntries(ctx, cypher, nil); err != nil {
			return fmt.Errorf("staging: purge orphan :%s nodes: %w", label, err)
		}
	}
	if s.weaviate != nil {
		if err := s.weaviate.CleanupStaging(ctx); err != nil {
			return fmt.Errorf("staging: clear prior weaviate staging classes: %w", err)
		}
		if err := s.weaviate.EnsureStagingSchema(ctx); err != nil {
			return fmt.Errorf("staging: ensure weaviate staging schema: %w", err)
		}
	}
	return nil
}

// ApplyDatom delegates to the shared neo4j.BackendApplier, which is
// the same translator the write pipeline and self-heal use. Every
// datom therefore produces the same graph shape a fresh observe would
// have produced: node keyed by entry_id, decoded primitive values,
// and materialized relationships for the edge attributes (trail,
// subject, derived_from, similar_to, supersedes, alias_of,
// mentions_concept).
func (s *realStagingBackends) ApplyDatom(ctx context.Context, d datom.Datom) error {
	if err := s.neoApplier.Apply(ctx, d); err != nil {
		return fmt.Errorf("staging: apply datom %s/%s: %w", d.E, d.A, err)
	}
	s.applied++
	return nil
}

// ApplyEmbedding writes a re-embedded vector for one entry into the
// Weaviate staging namespace so the subsequent Swap can promote it.
// Called only from the --accept-drift path; non-drift rebuilds never
// reach this method because rebuild.Run only invokes ApplyEmbedding
// for entries flagged as affected by drift.
func (s *realStagingBackends) ApplyEmbedding(ctx context.Context, entryID string, vector []float32) error {
	if entryID == "" || s.weaviate == nil {
		return nil
	}
	props := map[string]any{"cortex_id": entryID}
	if err := s.weaviate.UpsertStaging(ctx, weaviate.ClassEntry, entryID, vector, props); err != nil {
		return fmt.Errorf("staging: upsert weaviate staging %s: %w", entryID, err)
	}
	s.weaviateStaged = true
	return nil
}

// Swap promotes the Weaviate staging namespace to live, but only
// when --accept-drift actually populated it (tracked via
// weaviateStaged). SwapStagingToLive drops and recreates the live
// classes before the bulk copy, so calling it on an empty staging
// namespace would wipe the live vectors — exactly the regression
// cortex-sv8 round-2 hit while fixing the graph side. Non-drift
// rebuilds therefore skip the Weaviate path entirely; the live
// vectors from the prior observes survive untouched. The Neo4j side
// has already been written straight to live during ApplyDatom so
// there is nothing to promote on the graph — the rebuild is complete
// by the time Swap is called.
func (s *realStagingBackends) Swap(ctx context.Context) error {
	if s.weaviate != nil && s.weaviateStaged {
		if err := s.weaviate.SwapStagingToLive(ctx); err != nil {
			return fmt.Errorf("staging: weaviate swap: %w", err)
		}
	}
	return nil
}

// Cleanup is the failure-path hook rebuild.Run calls when apply or
// swap has errored out after Create succeeded. We drop the Weaviate
// staging classes so a botched --accept-drift run does not leave
// stale EntryStaging / FrameStaging objects behind. The Neo4j live
// state is intentionally left where it is: partial writes from a
// failed rebuild are still valid datoms (MERGE is idempotent), and a
// subsequent retry replays the full log on top without producing
// duplicates.
func (s *realStagingBackends) Cleanup(ctx context.Context) error {
	if s.weaviate != nil {
		if err := s.weaviate.CleanupStaging(ctx); err != nil {
			return fmt.Errorf("staging: cleanup weaviate staging: %w", err)
		}
	}
	return nil
}
