// internal/weaviate/applier.go is the Cortex datom → Weaviate
// translator. It mirrors internal/neo4j/applier.go: a single concrete
// type that satisfies both replay.Applier and write.BackendApplier
// (Name + Apply), so the write pipeline's post-commit apply phase and
// the startup self-heal can both flow datoms into the live Weaviate
// vector store without knowing how the collections are shaped.
//
// Translation rules — Phase 1 minimum.
//
// Weaviate stores objects, not facts. A Cortex entity is a stream of
// many datoms (body, kind, facet.*, subject, trail, embedding model,
// timestamps, base activation), and Weaviate wants the union of those
// fields as one PUT. Two design choices follow:
//
//  1. The applier keeps a small per-entity scratch buffer in memory
//     (`pending`) so successive Apply calls can collapse onto one
//     Upsert. The buffer survives across Apply calls (it lives on the
//     applier value), so a write pipeline that calls Apply once per
//     datom in a transaction group converges on one fully populated
//     row by the time the loop finishes. The buffer is also the only
//     piece of state that persists across calls — there is no
//     transaction boundary signal in the Applier interface, so the
//     applier always re-Upserts the full bag on every datom rather
//     than waiting for an explicit Flush. This is wasteful in HTTP
//     calls but correct under any call ordering, including replay.
//
//  2. Each entity id is mapped to a Weaviate object UUID via a
//     deterministic SHA-1 derivation (see deriveUUID). Cortex stores
//     prefixed ULIDs (`entry:01ARZ…`); Weaviate object ids are RFC
//     4122 UUIDs. Deriving the UUID from the prefixed ULID by hashing
//     means the same entity always lands on the same Weaviate row,
//     which is the property the write pipeline and self-heal both
//     need.
//
// Routing.
//
//   - Datoms whose entity id starts with `entry:` route to ClassEntry.
//   - Datoms whose entity id starts with `frame:` route to ClassFrame.
//   - Other entities (subject, trail, community, psi) are not stored
//     in Weaviate at all in Phase 1 — Neo4j is the canonical home for
//     them. Apply silently skips them so the loop doesn't have to
//     special-case which datoms belong to which backend.
//
// OpRetract handling. The applier does not delete the Weaviate row
// (Cortex never deletes from the log; the row is part of history).
// Instead it sets a `retracted = true` property on the row and re-
// Upserts. Recall's visibility filter is the layer that hides
// retracted rows from default queries.
//
// Spec references:
//
//	docs/spec/cortex-spec.md FR-005, FR-051 (model digest invariant)
//	docs/spec/cortex-spec-code-review.md MAJ-001/MAJ-007/MAJ-010
//	internal/replay/selfheal.go (Applier interface contract)
//	internal/write/pipeline.go (BackendApplier interface contract)
package weaviate

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nixlim/cortex/internal/datom"
)

// objectUpserter is the subset of Client used by the applier. The
// narrow seam lets unit tests inject a fake without standing up an
// httptest server for every assertion.
type objectUpserter interface {
	Upsert(ctx context.Context, class, id string, vector []float32, properties map[string]any) error
}

// BackendApplier reflects datoms into Weaviate. It is safe for
// concurrent use across different entity ids; concurrent applies to
// the same entity id are serialized through the per-applier mutex
// because the per-entity scratch bag is not goroutine-safe on its own.
type BackendApplier struct {
	w objectUpserter

	mu      sync.Mutex
	pending map[string]map[string]any // entity id → accumulated property bag
}

// NewBackendApplier wraps an *HTTPClient (or any compatible
// objectUpserter) so the write pipeline and self-heal can route
// datoms into the live Weaviate store.
func NewBackendApplier(client *HTTPClient) *BackendApplier {
	return newBackendApplierFor(client)
}

// newBackendApplierFor is the test seam used by applier_test.go.
func newBackendApplierFor(w objectUpserter) *BackendApplier {
	return &BackendApplier{
		w:       w,
		pending: make(map[string]map[string]any),
	}
}

// Name identifies the backend in replay error messages.
func (a *BackendApplier) Name() string { return "weaviate" }

// Apply reflects one datom into Weaviate. The contract:
//
//   - Empty entity ids are no-ops (matches the rebuild package's
//     tolerance for malformed log rows).
//   - Datoms whose entity id is not prefixed with entry: or frame:
//     are silently skipped — they belong to Neo4j only.
//   - Every other datom mutates the per-entity scratch bag and re-
//     Upserts the full bag to the appropriate class. The bag carries
//     the canonical cortex_id property so recall can map a Weaviate
//     hit back to the prefixed ULID.
//   - OpRetract sets `retracted = true` rather than deleting; recall's
//     visibility filter handles default invisibility.
func (a *BackendApplier) Apply(ctx context.Context, d datom.Datom) error {
	if d.E == "" {
		return nil
	}
	class, ok := classForEntity(d.E)
	if !ok {
		return nil
	}

	a.mu.Lock()
	bag, exists := a.pending[d.E]
	if !exists {
		bag = map[string]any{
			"cortex_id": d.E,
		}
		a.pending[d.E] = bag
	}
	if d.Op == datom.OpRetract {
		bag["retracted"] = true
	} else {
		key := propertyName(d.A)
		// Drop the old value before setting so a JSON null retract
		// path (someone overwriting with null) doesn't leave stale
		// state.
		delete(bag, key)
		if v := decodeValue(d.V); v != nil {
			bag[key] = v
		}
	}
	// Snapshot the bag under the lock so the HTTP call below sees a
	// stable view even if a concurrent Apply mutates the same entity
	// after we release.
	snapshot := make(map[string]any, len(bag))
	for k, v := range bag {
		snapshot[k] = v
	}
	a.mu.Unlock()

	uuid := deriveUUID(d.E)
	// Vector is empty here: the Phase 1 write pipeline does not yet
	// hand the float32 vector to Apply (the embedding lives in the
	// pipeline's local var, see internal/write/pipeline.go:309). When
	// that wiring lands, the vector will flow through ApplyWithVector
	// below; until then the row exists with vectorizer=none and an
	// absent vector field.
	if err := a.w.Upsert(ctx, class, uuid, nil, snapshot); err != nil {
		return fmt.Errorf("weaviate: apply %s/%s: %w", d.E, d.A, err)
	}
	return nil
}

// ApplyWithVector is the variant the write pipeline calls when it
// has a fresh embedding for the entity (the body datom is the
// canonical trigger). It is intentionally additive — features-dev can
// switch the pipeline to call this method without breaking the
// generic Apply seam used by self-heal.
func (a *BackendApplier) ApplyWithVector(ctx context.Context, d datom.Datom, vector []float32) error {
	if d.E == "" {
		return nil
	}
	class, ok := classForEntity(d.E)
	if !ok {
		return nil
	}
	a.mu.Lock()
	bag, exists := a.pending[d.E]
	if !exists {
		bag = map[string]any{"cortex_id": d.E}
		a.pending[d.E] = bag
	}
	key := propertyName(d.A)
	delete(bag, key)
	if v := decodeValue(d.V); v != nil {
		bag[key] = v
	}
	snapshot := make(map[string]any, len(bag))
	for k, v := range bag {
		snapshot[k] = v
	}
	a.mu.Unlock()

	uuid := deriveUUID(d.E)
	if err := a.w.Upsert(ctx, class, uuid, vector, snapshot); err != nil {
		return fmt.Errorf("weaviate: apply %s/%s: %w", d.E, d.A, err)
	}
	return nil
}

// Forget drops the per-entity scratch bag for an entity id. Long-
// running processes can call this to bound the applier's memory
// footprint after they know an entity has reached a stable state.
// Tests use it to assert isolation across runs.
func (a *BackendApplier) Forget(entityID string) {
	a.mu.Lock()
	delete(a.pending, entityID)
	a.mu.Unlock()
}

// classForEntity returns the Weaviate class an entity id belongs to,
// or ("", false) if the id should not be reflected into Weaviate at
// all (subjects, trails, communities live in Neo4j only).
func classForEntity(id string) (string, bool) {
	switch {
	case strings.HasPrefix(id, "entry:"):
		return ClassEntry, true
	case strings.HasPrefix(id, "frame:"):
		return ClassFrame, true
	default:
		return "", false
	}
}

// propertyName rewrites a datom attribute name into a Weaviate
// property name. Weaviate property names must match the pattern
// `[a-zA-Z][a-zA-Z0-9_]*`; Cortex attributes use dot-separated
// segments (`facet.domain`), so we replace dots and any other
// non-allowed character with underscores. The first character is
// forced to a letter via a `p_` prefix when it isn't already one.
func propertyName(a string) string {
	if a == "" {
		return "value"
	}
	var b strings.Builder
	b.Grow(len(a) + 2)
	for i, r := range a {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case (r >= '0' && r <= '9') || r == '_':
			if i == 0 {
				b.WriteByte('p')
				b.WriteByte('_')
			}
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// deriveUUID maps a Cortex prefixed ULID to a stable RFC 4122-shaped
// id Weaviate will accept. We compute SHA-1 of the prefixed id, take
// the first 16 bytes, set the version (5) and variant (RFC 4122) bits
// per the UUIDv5 algorithm, and format as the canonical 8-4-4-4-12
// hex string.
//
// The reason for SHA-1 over a more modern hash is that UUIDv5 is the
// IETF-blessed deterministic-from-name UUID flavour and Weaviate
// validates against UUID syntax, not against the namespace; matching
// the spec keeps us defensible against future Weaviate validation
// tightening.
func deriveUUID(id string) string {
	h := sha1.Sum([]byte("cortex:" + id))
	// Bytes 6 and 8 carry version and variant per RFC 4122.
	h[6] = (h[6] & 0x0f) | 0x50 // version 5
	h[8] = (h[8] & 0x3f) | 0x80 // RFC 4122 variant
	hexs := hex.EncodeToString(h[:16])
	return strings.Join([]string{
		hexs[0:8],
		hexs[8:12],
		hexs[12:16],
		hexs[16:20],
		hexs[20:32],
	}, "-")
}

// decodeValue mirrors the Neo4j applier's decoder so the two backends
// agree on type fidelity. Compound JSON values fall back to a
// JSON-encoded string because Weaviate primitive properties don't
// support nested objects directly (a struct property would require a
// matching schema declaration).
func decodeValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	switch t := v.(type) {
	case nil:
		return nil
	case bool, string:
		return t
	case float64:
		if float64(int64(t)) == t {
			return int64(t)
		}
		return t
	case []any, map[string]any:
		buf, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(buf)
	default:
		return fmt.Sprintf("%v", t)
	}
}
