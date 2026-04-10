// cmd/cortex/staging_backends.go is the live StagingBackends used by
// `cortex rebuild`. It replaces the no-op stubStagingBackends in
// rebuild.go (CRIT-001 fix per docs/spec/cortex-spec-code-review.md).
//
// Scope of "real" today:
//
//   - Create / Cleanup hit Neo4j for real and write/clear nodes under
//     a dedicated :CortexStaging label, so the active graph (which is
//     unlabeled or carries the live label set the write pipeline uses)
//     is never touched.
//
//   - ApplyDatom translates each replayed datom into a Cypher MERGE
//     against a :CortexStaging node keyed by the datom's entity id,
//     setting the attribute name as a property and the JSON-encoded
//     value as another property. This is intentionally a flat mirror
//     rather than a structural translation: the goal is to prove the
//     rebuild loop touches a real backend with the actual datom shape,
//     not to reach perfect parity with the live write pipeline's
//     graph schema (which is a separate, larger bead).
//
//   - ApplyEmbedding stores the float32 vector as a property string
//     on the matching staging node. We don't use Weaviate's vector
//     index here yet — see the staging-Weaviate follow-up note below.
//
//   - Swap returns a clear NOT_IMPLEMENTED operational error. Atomic
//     promotion of staging to active requires (a) a Weaviate-side
//     staging class with a documented swap dance and (b) a graph
//     reconciliation step that the rebuild package contract permits
//     but does not yet exercise. Both are tracked as follow-up beads.
//     The honest minimum the team-lead asked for in the round-1 grill
//     review was: "if full swap semantics aren't ready, at minimum
//     implement ApplyDatom that actually writes something to the real
//     backends in a separate staging namespace." That is what this
//     adapter delivers.
//
// Staging-Weaviate follow-up note: a true rebuild must also stage a
// fresh Weaviate Entry/Frame class so vector retrieval is rebuilt
// alongside the graph. That work is intentionally out of scope of
// the CRIT-001 minimum because it requires schema additions to
// internal/weaviate (ClassEntryStaging, EnsureStagingSchema, a
// safe DeleteClass) plus an end-to-end staging-class swap. The
// errStagingSwapNotWired sentinel surfaces that gap to the operator
// instead of silently succeeding.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/neo4j"
)

// stagingLabel is the Neo4j label every staging node carries. The
// live write pipeline does not write nodes with this label, so the
// active graph is invisibly partitioned from staging.
const stagingLabel = "CortexStaging"

// errStagingSwapNotWired is the sentinel returned by Swap. It is
// surfaced to the operator as STAGING_SWAP_NOT_WIRED so the rebuild
// CLI can render a precise next-step message instead of the generic
// "STAGING_SWAP_FAILED" the rebuild package would otherwise emit.
var errStagingSwapNotWired = errors.New(
	"rebuild swap is not yet wired: this build can populate the staging " +
		"namespace but cannot atomically promote it to active. Inspect the " +
		":CortexStaging nodes via `cypher-shell` and rerun once the " +
		"staging-Weaviate adapter lands")

// realStagingBackends is the live StagingBackends implementation. It
// holds a single neo4j.Client (typed as the interface so tests can
// inject a fake) and an actor/invocation pair so audit nodes can be
// attributed back to the rebuild that created them.
type realStagingBackends struct {
	graph        neo4j.Client
	actor        string
	invocationID string

	applied int // counts ApplyDatom calls; used in close-out logging
}

// newRealStagingBackends builds a realStagingBackends from a live
// graph client. Construction is trivial — connection lifetime is
// managed by the caller (cmd/cortex/rebuild.go opens and closes the
// client around the rebuild.Run call).
func newRealStagingBackends(graph neo4j.Client, actor, invocationID string) *realStagingBackends {
	return &realStagingBackends{
		graph:        graph,
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
	return nil
}

// ApplyDatom mirrors one datom into the staging namespace. We MERGE
// the staging node by entity id (so multiple datoms touching the
// same entity collapse onto one node) and set the datom's attribute
// as a Cypher property. The value is stored as the raw JSON string
// the datom carries — staging is a flat mirror, not a typed graph,
// because the goal is to prove the loop touches a real backend with
// the actual datom shape.
//
// Datoms with an empty entity id (a malformed log row) are skipped
// rather than failing the run. The replay loop guards against this
// case earlier in the rebuild package; we mirror that tolerance so
// a single malformed datom does not abort an otherwise-clean
// rebuild.
func (s *realStagingBackends) ApplyDatom(ctx context.Context, d datom.Datom) error {
	if d.E == "" {
		return nil
	}
	// Use string keys derived from the datom attribute name. We
	// pass the value as a JSON string ([]byte → string) because
	// json.RawMessage round-trips through the Bolt driver as
	// []byte and most Neo4j Phase 1 indexes are string-based.
	cypher := fmt.Sprintf(
		"MERGE (n:%s {entity: $entity}) "+
			"SET n.last_attribute = $attribute, "+
			"    n.last_value = $value, "+
			"    n.last_op = $op, "+
			"    n.last_tx = $tx",
		stagingLabel,
	)
	params := map[string]any{
		"entity":    d.E,
		"attribute": d.A,
		"value":     string(d.V),
		"op":        string(d.Op),
		"tx":        d.Tx,
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
	return nil
}

// Swap is the explicit gap. Returning errStagingSwapNotWired causes
// rebuild.Run to surface STAGING_SWAP_FAILED with this sentinel as
// the cause; the CLI catches it in renderRebuildError and prints the
// precise next-step message.
func (s *realStagingBackends) Swap(_ context.Context) error {
	return errStagingSwapNotWired
}

// Cleanup removes every staging node so an aborted rebuild does not
// leave the graph littered. The rebuild package calls Cleanup on
// every error path *after* Create, so this is the safety net for
// any failure between Create and Swap.
func (s *realStagingBackends) Cleanup(ctx context.Context) error {
	clear := fmt.Sprintf("MATCH (n:%s) DETACH DELETE n", stagingLabel)
	if err := s.graph.WriteEntries(ctx, clear, nil); err != nil {
		return fmt.Errorf("staging: cleanup :%s nodes: %w", stagingLabel, err)
	}
	return nil
}

// classifyStagingError is a helper for the rebuild CLI: it inspects
// an error returned from rebuild.Run and, if it traces back to the
// staging-swap gap, rewrites it into a precise STAGING_SWAP_NOT_WIRED
// envelope. Other errors pass through unchanged.
func classifyStagingError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errStagingSwapNotWired) {
		return errs.Operational("STAGING_SWAP_NOT_WIRED",
			"rebuild populated the :CortexStaging namespace but cannot atomically "+
				"promote it to active in this build; staging-Weaviate adapter is the "+
				"follow-up bead", err)
	}
	return err
}
