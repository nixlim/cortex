// internal/neo4j/applier.go is the Cortex datom → Neo4j translator.
//
// Both the write pipeline (post-commit apply phase, internal/write/
// pipeline.go) and the startup self-heal protocol (internal/replay/
// selfheal.go) need a thing that takes a single datom and reflects it
// into Neo4j without knowing how the live graph is shaped. This file
// provides exactly that thing: BackendApplier.
//
// Translation rules — flat, deliberate, and Phase 1 minimum.
//
// Every datom carries an entity id (`d.E`) shaped like one of:
//
//	entry:<ulid>          → :Entry node
//	frame:<ulid>          → :Frame node
//	subject:<canonical>   → :Subject node
//	trail:<id>            → :Trail node
//	community:<id>        → :Community node
//	psi:<form>            → :PSI node (subject merge alias accretion)
//	<other prefix>        → :CortexEntity node (fallback so the loop
//	                         never silently drops a datom)
//
// For non-edge attributes the applier MERGEs the node by id and SETs
// `n.<attribute> = <value>`. The value is decoded from the datom's
// json.RawMessage with type preservation: strings stay strings,
// numbers become int64 or float64, bools stay bools, nulls drop the
// property entirely. Compound JSON values are stored as a JSON-encoded
// string property — Phase 1 spec does not require structural decoding.
//
// A small set of attributes denote graph edges and are translated
// into MERGE statements that materialize the relationship instead of
// (or in addition to) a property. The current edge attributes are:
//
//	"trail"          → (:Entry|:Frame)-[:IN_TRAIL]->(:Trail)
//	"subject"        → (:Entry|:Frame)-[:ABOUT]->(:Subject)
//	"derived_from"   → (:Entry|:Frame)-[:DERIVED_FROM]->(<entity>)
//	"similar_to"     → (:Entry|:Frame)-[:SIMILAR_TO]->(<entity>)
//	"supersedes"     → (:Entry|:Frame)-[:SUPERSEDES]->(<entity>)
//	"alias_of"       → (:Subject|:PSI)-[:ALIAS_OF]->(<canonical>)
//
// Adding a new edge attribute is a one-line edits to edgeAttributes;
// the writer Cypher is otherwise generic.
//
// OpRetract handling. Cortex never deletes from the graph; an
// OpRetract on the synthetic `exists` attribute writes
// `n.retracted = true` so default recall (visibility filter) hides
// the entity, while history and as-of can still observe the entity
// existed. OpRetract on any other attribute clears that property
// (REMOVE n.<attribute>) so the latest state is honest.
//
// Spec references:
//
//	docs/spec/cortex-spec.md FR-005, FR-006, FR-008 (graph edges)
//	docs/spec/cortex-spec.md §"Behavioral Contract" (visibility/retract)
//	docs/spec/cortex-spec-code-review.md MAJ-001/MAJ-007/MAJ-010
//	internal/replay/selfheal.go (Applier interface contract)
//	internal/write/pipeline.go (BackendApplier interface contract)
package neo4j

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nixlim/cortex/internal/datom"
)

// graphWriter is the subset of Client used by the applier. Pulled
// out as a private interface so unit tests can drop in a fake without
// touching the BoltClient session machinery.
type graphWriter interface {
	WriteEntries(ctx context.Context, cypher string, params map[string]any) error
}

// BackendApplier translates one datom at a time into Cypher writes
// against a live Neo4j graph. It satisfies both replay.Applier and
// write.BackendApplier (same shape: Name + Apply).
type BackendApplier struct {
	g graphWriter
}

// NewBackendApplier wraps a *BoltClient (or any compatible
// graphWriter) so the write pipeline and self-heal can route datoms
// into the live Neo4j graph. The constructor is the only public
// dependency injection point — tests use newBackendApplierFor with a
// fake.
func NewBackendApplier(client *BoltClient) *BackendApplier {
	return &BackendApplier{g: client}
}

// newBackendApplierFor is the test seam used by applier_test.go.
func newBackendApplierFor(g graphWriter) *BackendApplier {
	return &BackendApplier{g: g}
}

// Name identifies the backend in replay error messages.
func (a *BackendApplier) Name() string { return "neo4j" }

// Apply reflects one datom into Neo4j. The contract:
//
//   - Datoms with an empty entity id are no-ops (a malformed log row
//     should not abort an otherwise-clean apply pass; the rebuild
//     package guards the same way).
//
//   - Datoms whose attribute is one of edgeAttributes materialize a
//     graph edge in addition to (not instead of) the property write.
//     This makes it idempotent for replay: rerunning the apply leaves
//     both the property and the relationship in their final state.
//
//   - OpRetract on the synthetic `exists` attribute toggles
//     `n.retracted = true`. OpRetract on any other attribute clears
//     that property so subsequent reads see "no value" rather than
//     the stale assertion.
func (a *BackendApplier) Apply(ctx context.Context, d datom.Datom) error {
	if d.E == "" {
		return nil
	}
	label := labelForEntity(d.E)

	// OpRetract paths take precedence over the standard SET path so
	// the property/edge writers below never need to special-case the
	// retract semantics.
	if d.Op == datom.OpRetract {
		return a.applyRetract(ctx, label, d)
	}

	// Decode the value once. Compound types fall back to a JSON
	// string so the property write always succeeds, even for shapes
	// the spec hasn't blessed yet.
	value := decodeValue(d.V)

	// Edge-bearing attributes write the edge first, then the
	// property. The order doesn't matter for correctness but writing
	// the edge first means a partial failure leaves the property
	// out (and self-heal will rerun both on the next pass).
	if isEdgeAttribute(d.A) {
		if err := a.writeEdge(ctx, label, d, value); err != nil {
			return err
		}
	}

	// Standard property write — MERGE the node by id and set the
	// attribute. The MERGE clause uses the entity id as the merge key
	// so two datoms touching the same entity collapse to one node.
	cypher := fmt.Sprintf(
		"MERGE (n:%s {id: $id}) SET n.%s = $value, n.last_tx = $tx",
		label, cypherProperty(d.A),
	)
	return a.g.WriteEntries(ctx, cypher, map[string]any{
		"id":    d.E,
		"value": value,
		"tx":    d.Tx,
	})
}

// applyRetract handles the OpRetract branch.
func (a *BackendApplier) applyRetract(ctx context.Context, label string, d datom.Datom) error {
	if d.A == "exists" {
		cypher := fmt.Sprintf(
			"MERGE (n:%s {id: $id}) SET n.retracted = true, n.last_tx = $tx",
			label,
		)
		return a.g.WriteEntries(ctx, cypher, map[string]any{
			"id": d.E,
			"tx": d.Tx,
		})
	}
	cypher := fmt.Sprintf(
		"MERGE (n:%s {id: $id}) REMOVE n.%s SET n.last_tx = $tx",
		label, cypherProperty(d.A),
	)
	return a.g.WriteEntries(ctx, cypher, map[string]any{
		"id": d.E,
		"tx": d.Tx,
	})
}

// writeEdge materializes a relationship from the datom's source
// entity to a target entity inferred from the value. The relationship
// type is the upper-snake-case form of the attribute name; the target
// label is inferred from the target entity id prefix.
func (a *BackendApplier) writeEdge(ctx context.Context, srcLabel string, d datom.Datom, value any) error {
	target, ok := value.(string)
	if !ok || target == "" {
		// Edge datoms whose value isn't a target id (or is empty)
		// degrade to a plain property write. This keeps the edge
		// writer tolerant of upstream shape drift without dropping
		// the data.
		return nil
	}
	tgtLabel := labelForEntity(target)
	rel := relationshipName(d.A)
	cypher := fmt.Sprintf(
		"MERGE (s:%s {id: $src}) "+
			"MERGE (t:%s {id: $tgt}) "+
			"MERGE (s)-[r:%s]->(t) "+
			"SET r.last_tx = $tx",
		srcLabel, tgtLabel, rel,
	)
	return a.g.WriteEntries(ctx, cypher, map[string]any{
		"src": d.E,
		"tgt": target,
		"tx":  d.Tx,
	})
}

// edgeAttributes maps the datom attribute names that materialize as
// graph relationships to their Cypher relationship type. Adding a new
// edge attribute is a one-line addition here.
var edgeAttributes = map[string]string{
	"trail":        "IN_TRAIL",
	"subject":      "ABOUT",
	"derived_from": "DERIVED_FROM",
	"similar_to":   "SIMILAR_TO",
	"supersedes":   "SUPERSEDES",
	"alias_of":     "ALIAS_OF",
}

func isEdgeAttribute(a string) bool {
	_, ok := edgeAttributes[a]
	return ok
}

func relationshipName(a string) string {
	if rel, ok := edgeAttributes[a]; ok {
		return rel
	}
	return strings.ToUpper(cypherProperty(a))
}

// labelForEntity maps a Cortex prefixed id to a Neo4j label. The
// prefix is the segment before the first colon; the fallback label
// guarantees the apply loop never silently drops a datom.
func labelForEntity(id string) string {
	idx := strings.Index(id, ":")
	if idx <= 0 {
		return "CortexEntity"
	}
	switch id[:idx] {
	case "entry":
		return "Entry"
	case "frame":
		return "Frame"
	case "subject":
		return "Subject"
	case "trail":
		return "Trail"
	case "community":
		return "Community"
	case "psi":
		return "PSI"
	default:
		return "CortexEntity"
	}
}

// cypherProperty rewrites a datom attribute name into a safe Cypher
// property identifier. Cortex attributes are dot-separated
// (`facet.domain`); Cypher property names cannot contain dots, so we
// replace them with underscores. Any other non-alphanumeric character
// is also collapsed to underscore as defense-in-depth.
func cypherProperty(a string) string {
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

// decodeValue turns a datom's json.RawMessage into a Go value with
// type fidelity preserved as far as Neo4j accepts. JSON null returns
// nil; numbers become int64 if integer, float64 otherwise; strings
// stay strings; bools stay bools; arrays and objects fall back to
// their JSON-encoded string form because Neo4j primitive properties
// don't support nested maps.
func decodeValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		// Unparseable values are stored verbatim as strings; the apply
		// loop should never panic on a single bad datom.
		return string(raw)
	}
	switch t := v.(type) {
	case nil:
		return nil
	case bool, string:
		return t
	case float64:
		// JSON numbers decode as float64; recover int64 when the
		// fraction is zero so Neo4j stores them as integers.
		if float64(int64(t)) == t {
			return int64(t)
		}
		return t
	case []any, map[string]any:
		// Compound shapes serialize as JSON. The applier doesn't
		// attempt structural decomposition because Phase 1 spec
		// neither requires it nor blesses a target schema.
		buf, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(buf)
	default:
		return fmt.Sprintf("%v", t)
	}
}
