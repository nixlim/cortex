// cmd/cortex/staging_backends.go is the live StagingBackends used by
// `cortex rebuild`. It replaces the no-op stubStagingBackends in
// rebuild.go (CRIT-001 + MAJ-008 fixes per
// docs/spec/cortex-spec-code-review.md).
//
// Scope of "real" today:
//
//   - Create / Cleanup hit Neo4j for real and write/clear nodes under
//     a dedicated :CortexStaging label, so the active graph (which
//     uses the live label set the write pipeline writes — :Entry,
//     :Frame, :Subject, :Trail, :Community, :PSI) is never touched
//     during the staging phase.
//
//   - ApplyDatom translates each replayed datom into a Cypher MERGE
//     against a :CortexStaging node keyed by the datom's entity id,
//     setting each datom attribute as a real property on the staging
//     node so the bag of properties accumulates per entity instead of
//     being overwritten on every call. The translation mirrors
//     internal/neo4j/applier.go (cypherProperty rewrite, OpRetract
//     REMOVEs the property) so the staging mirror is structurally
//     promotable in Swap.
//
//   - ApplyEmbedding stores a small fingerprint of the float32 vector
//     (dim, first, last) on the matching staging node so cortex doctor
//     can verify a re-embed happened. The full vector is *not* held
//     in :CortexStaging because the graph is not the right home for
//     it; a true Weaviate-staging adapter is the follow-up bead and
//     is documented in the Swap doc comment below.
//
//   - Swap promotes the :CortexStaging mirror to the live label set
//     in a deterministic order. Cypher does not allow dynamic labels
//     so the promotion walks a static (prefix → live label) map and
//     issues one (demote, promote) statement pair per prefix. The
//     demote step DETACH-deletes any live node whose id collides with
//     a staging entity for that prefix; the promote step relabels the
//     staging node in place (`SET s:Entry`, `REMOVE s:CortexStaging`,
//     and renames `entity` → `id`). After all known prefixes have
//     been promoted, a catch-all promotes any remaining staging node
//     under :CortexEntity so a malformed log row is never silently
//     dropped, and the rebuild marker is removed last so cortex doctor
//     can confirm a successful swap by absence.
//
// Atomicity caveat. The promotion plan is multi-statement; each
// WriteEntries call is its own Bolt transaction. A failure midway
// leaves the graph in a half-promoted state with both :CortexStaging
// and live nodes coexisting. The rebuild package's Cleanup is the
// recovery path: it DETACH-deletes everything still labelled
// :CortexStaging, which is the safe direction for a half-swap (the
// already-promoted live nodes survive; the still-staged ones go
// back to "needs rebuild"). A truly atomic swap requires a single
// multi-statement transaction with explicit BEGIN/COMMIT — that is
// blocked on the BoltClient gaining a transactional execution seam
// and is tracked as a follow-up bead.
//
// Weaviate-side staging is now wired via a weaviate.StagingClient
// (see internal/weaviate/staging.go, bead cortex-s84). Create calls
// EnsureStagingSchema, ApplyEmbedding also writes into the staging
// class via UpsertStaging so the rebuilt vector lands in Weaviate's
// staging namespace (not just a Neo4j fingerprint), Swap promotes
// staging to live via SwapStagingToLive (bulk copy + delete), and
// Cleanup drops the staging classes so a half-run rebuild does not
// leave stale state behind. When no StagingClient is supplied
// (construction-time failure or a test that doesn't need Weaviate)
// the Weaviate side silently no-ops and the graph-only behavior from
// before is preserved — rebuild still produces a valid Neo4j swap.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/weaviate"
)

// stagingLabel is the Neo4j label every staging node carries. The
// live write pipeline does not write nodes with this label, so the
// active graph is invisibly partitioned from staging.
const stagingLabel = "CortexStaging"

// stagingPromotions is the static (prefix → live label) map Swap
// walks. The order is deterministic so the resulting Cypher trace is
// reproducible across runs (helpful when debugging a half-swap from
// the operational logs).
var stagingPromotions = []struct {
	prefix string
	label  string
}{
	{"entry:", "Entry"},
	{"frame:", "Frame"},
	{"subject:", "Subject"},
	{"trail:", "Trail"},
	{"community:", "Community"},
	{"psi:", "PSI"},
}

// realStagingBackends is the live StagingBackends implementation. It
// holds a single neo4j.Client (typed as the interface so tests can
// inject a fake) and an actor/invocation pair so audit nodes can be
// attributed back to the rebuild that created them.
type realStagingBackends struct {
	graph        neo4j.Client
	weaviate     weaviate.StagingClient
	actor        string
	invocationID string

	applied int // counts ApplyDatom calls; used in close-out logging
}

// newRealStagingBackends builds a realStagingBackends from a live
// graph client and (optionally) a Weaviate staging client.
// Construction is trivial — connection lifetime is managed by the
// caller (cmd/cortex/rebuild.go opens and closes the clients around
// the rebuild.Run call). A nil weaviate client is legal: the Weaviate
// side of the rebuild becomes a no-op and the graph-only swap path
// from before remains in place.
func newRealStagingBackends(graph neo4j.Client, wv weaviate.StagingClient, actor, invocationID string) *realStagingBackends {
	return &realStagingBackends{
		graph:        graph,
		weaviate:     wv,
		actor:        actor,
		invocationID: invocationID,
	}
}

// Create clears any leftover staging nodes from a prior aborted
// rebuild and writes a single :CortexStaging:CortexStagingMarker
// breadcrumb so the operator can confirm out-of-band that this run
// reached the staging phase. The breadcrumb carries actor +
// invocation_id so cortex doctor can attribute orphaned staging
// state back to a specific run.
func (s *realStagingBackends) Create(ctx context.Context) error {
	// Drop any prior staging state. DETACH DELETE removes both the
	// nodes and any relationships incident to them, so a half-run
	// from yesterday cannot poison today's rebuild.
	clear := fmt.Sprintf("MATCH (n:%s) DETACH DELETE n", stagingLabel)
	if err := s.graph.WriteEntries(ctx, clear, nil); err != nil {
		return fmt.Errorf("staging: clear prior :%s nodes: %w", stagingLabel, err)
	}
	marker := fmt.Sprintf(
		"MERGE (m:%s:CortexStagingMarker {kind: 'rebuild_marker'}) "+
			"SET m.actor = $actor, m.invocation_id = $invocation_id",
		stagingLabel,
	)
	if err := s.graph.WriteEntries(ctx, marker, map[string]any{
		"actor":         s.actor,
		"invocation_id": s.invocationID,
	}); err != nil {
		return fmt.Errorf("staging: write marker: %w", err)
	}
	// Also prepare the Weaviate staging namespace if a client is
	// wired. Failure to reach Weaviate here fails the whole Create:
	// a rebuild with a half-ready staging is worse than a rebuild
	// that refuses to start.
	if s.weaviate != nil {
		// Best-effort cleanup of any stranded staging classes from a
		// prior aborted rebuild before we recreate them.
		if err := s.weaviate.CleanupStaging(ctx); err != nil {
			return fmt.Errorf("staging: clear prior weaviate staging classes: %w", err)
		}
		if err := s.weaviate.EnsureStagingSchema(ctx); err != nil {
			return fmt.Errorf("staging: ensure weaviate staging schema: %w", err)
		}
	}
	return nil
}

// ApplyDatom mirrors one datom into the staging namespace. We MERGE
// the staging node by entity id (so multiple datoms touching the same
// entity collapse onto one node) and set the datom attribute as a
// real Cypher property — not a flat last_attribute slot — so the
// staging bag accumulates the full per-entity property set the way
// the live BackendApplier would. Swap relies on this shape: a
// promotion that just relabels the node lifts the entire property
// bag in one step.
//
// OpRetract REMOVEs the property to match the live applier semantics
// (Cortex never deletes from history, but the live state should not
// observe a stale assertion).
//
// Datoms with an empty entity id (a malformed log row) are skipped
// rather than failing the run. The replay loop guards against this
// case earlier in the rebuild package; we mirror that tolerance so
// a single malformed datom does not abort an otherwise-clean rebuild.
func (s *realStagingBackends) ApplyDatom(ctx context.Context, d datom.Datom) error {
	if d.E == "" {
		return nil
	}
	prop := stagingProperty(d.A)
	if d.Op == datom.OpRetract {
		cypher := fmt.Sprintf(
			"MERGE (n:%s {entity: $entity}) "+
				"REMOVE n.%s "+
				"SET n.last_op = $op, n.last_tx = $tx",
			stagingLabel, prop,
		)
		if err := s.graph.WriteEntries(ctx, cypher, map[string]any{
			"entity": d.E,
			"op":     string(d.Op),
			"tx":     d.Tx,
		}); err != nil {
			return fmt.Errorf("staging: retract %s/%s: %w", d.E, d.A, err)
		}
		s.applied++
		return nil
	}
	cypher := fmt.Sprintf(
		"MERGE (n:%s {entity: $entity}) "+
			"SET n.%s = $value, n.last_op = $op, n.last_tx = $tx",
		stagingLabel, prop,
	)
	params := map[string]any{
		"entity": d.E,
		"value":  string(d.V),
		"op":     string(d.Op),
		"tx":     d.Tx,
	}
	if err := s.graph.WriteEntries(ctx, cypher, params); err != nil {
		return fmt.Errorf("staging: apply datom %s/%s: %w", d.E, d.A, err)
	}
	s.applied++
	return nil
}

// ApplyEmbedding writes a re-embedded vector for one entry into the
// staging namespace. We persist the dimension and a small JSON
// fingerprint of the vector so cortex doctor can verify a re-embed
// happened, without bloating Neo4j with the full vector (a true
// staging-Weaviate adapter is the right home for the float32 array).
func (s *realStagingBackends) ApplyEmbedding(ctx context.Context, entryID string, vector []float32) error {
	if entryID == "" {
		return nil
	}
	cypher := fmt.Sprintf(
		"MERGE (n:%s {entity: $entity}) "+
			"SET n.embedding_dim = $dim, "+
			"    n.embedding_first = $first, "+
			"    n.embedding_last = $last",
		stagingLabel,
	)
	var first, last float64
	if len(vector) > 0 {
		first = float64(vector[0])
		last = float64(vector[len(vector)-1])
	}
	if err := s.graph.WriteEntries(ctx, cypher, map[string]any{
		"entity": entryID,
		"dim":    int64(len(vector)),
		"first":  first,
		"last":   last,
	}); err != nil {
		return fmt.Errorf("staging: apply embedding %s: %w", entryID, err)
	}
	// Mirror the full vector into the Weaviate staging namespace so
	// the subsequent Swap can copy it straight into the live Entry
	// class. Callers that wire this up pass entry:<ulid> strings;
	// anything else (frame:, subject:, …) is skipped because the
	// rebuild embedder only re-embeds entry bodies.
	if s.weaviate != nil && strings.HasPrefix(entryID, "entry:") {
		props := map[string]any{"cortex_id": entryID}
		if err := s.weaviate.UpsertStaging(ctx, weaviate.ClassEntry, entryID, vector, props); err != nil {
			return fmt.Errorf("staging: upsert weaviate staging %s: %w", entryID, err)
		}
	}
	return nil
}

// Swap promotes the :CortexStaging mirror to the live label set. See
// the file-level doc comment for the atomicity caveat and the per-
// prefix walk rationale.
func (s *realStagingBackends) Swap(ctx context.Context) error {
	for _, p := range stagingPromotions {
		if err := s.demoteLive(ctx, p.prefix, p.label); err != nil {
			return err
		}
		if err := s.promoteStaging(ctx, p.prefix, p.label); err != nil {
			return err
		}
	}
	// Catch-all: any remaining :CortexStaging node that didn't match
	// a known prefix is promoted under :CortexEntity so the loop
	// never silently drops a row.
	catchAll := fmt.Sprintf(
		"MATCH (s:%s) "+
			"WHERE NOT s:CortexStagingMarker "+
			"SET s:CortexEntity, s.id = s.entity "+
			"REMOVE s:%s, s.entity",
		stagingLabel, stagingLabel,
	)
	if err := s.graph.WriteEntries(ctx, catchAll, nil); err != nil {
		return fmt.Errorf("staging: promote fallback :CortexEntity: %w", err)
	}
	// Drop the rebuild marker last so doctor can confirm a successful
	// swap by absence of any :CortexStagingMarker node.
	dropMarker := "MATCH (m:CortexStagingMarker) DETACH DELETE m"
	if err := s.graph.WriteEntries(ctx, dropMarker, nil); err != nil {
		return fmt.Errorf("staging: drop marker: %w", err)
	}
	// Promote the Weaviate staging namespace to live via bulk copy
	// + delete. A nil client leaves the live Weaviate state as the
	// graph-only rebuild found it.
	if s.weaviate != nil {
		if err := s.weaviate.SwapStagingToLive(ctx); err != nil {
			return fmt.Errorf("staging: weaviate swap: %w", err)
		}
	}
	return nil
}

// demoteLive removes any live nodes whose id collides with a staging
// entity for the given prefix, so the subsequent promotion does not
// produce a duplicate id. We delete the old live node rather than
// merging properties because the rebuild semantics are "the staging
// mirror is the new authoritative state" — anything in the live node
// that wasn't reproduced by replay is by definition divergent and
// should not survive the swap.
func (s *realStagingBackends) demoteLive(ctx context.Context, prefix, label string) error {
	cypher := fmt.Sprintf(
		"MATCH (s:%s) "+
			"WHERE s.entity STARTS WITH $prefix AND NOT s:CortexStagingMarker "+
			"WITH collect(s.entity) AS ids "+
			"MATCH (live:%s) WHERE live.id IN ids "+
			"DETACH DELETE live",
		stagingLabel, label,
	)
	if err := s.graph.WriteEntries(ctx, cypher, map[string]any{"prefix": prefix}); err != nil {
		return fmt.Errorf("staging: demote live :%s: %w", label, err)
	}
	return nil
}

// promoteStaging relabels matching staging nodes in place: it adds
// the live label, drops the staging label, and renames the `entity`
// property to `id` so the live read path (which keys on `id`) finds
// them. Cypher does not allow dynamic labels so the live label is
// formatted into the Cypher string at construction time.
func (s *realStagingBackends) promoteStaging(ctx context.Context, prefix, label string) error {
	cypher := fmt.Sprintf(
		"MATCH (s:%s) "+
			"WHERE s.entity STARTS WITH $prefix AND NOT s:CortexStagingMarker "+
			"SET s:%s, s.id = s.entity "+
			"REMOVE s:%s, s.entity",
		stagingLabel, label, stagingLabel,
	)
	if err := s.graph.WriteEntries(ctx, cypher, map[string]any{"prefix": prefix}); err != nil {
		return fmt.Errorf("staging: promote staging :%s: %w", label, err)
	}
	return nil
}

// Cleanup removes every staging node so an aborted rebuild does not
// leave the graph littered. The rebuild package calls Cleanup on
// every error path *after* Create, so this is the safety net for
// any failure between Create and Swap. After a successful Swap, the
// promote step has already removed the :CortexStaging label from
// every node we created, so this query is a no-op.
func (s *realStagingBackends) Cleanup(ctx context.Context) error {
	clear := fmt.Sprintf("MATCH (n:%s) DETACH DELETE n", stagingLabel)
	if err := s.graph.WriteEntries(ctx, clear, nil); err != nil {
		return fmt.Errorf("staging: cleanup :%s nodes: %w", stagingLabel, err)
	}
	if s.weaviate != nil {
		if err := s.weaviate.CleanupStaging(ctx); err != nil {
			return fmt.Errorf("staging: cleanup weaviate staging: %w", err)
		}
	}
	return nil
}

// stagingProperty rewrites a datom attribute name into a safe Cypher
// property identifier. Mirrors internal/neo4j/applier.go's
// cypherProperty so the promote step doesn't need a translation
// table at swap time.
func stagingProperty(a string) string {
	if a == "" {
		return "value"
	}
	var b strings.Builder
	b.Grow(len(a))
	for i, r := range a {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			if i == 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
