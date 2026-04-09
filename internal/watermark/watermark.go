// Package watermark persists and reads the per-backend transaction
// watermarks Cortex uses to detect when Neo4j or Weaviate is behind
// the datom log.
//
// The watermark is the highest tx ULID successfully applied to a
// given backend. On startup, the self-healing replay path (see
// internal/replay) compares log max(tx) against these watermarks and
// replays missing committed datoms into any lagging backend before
// answering a query.
//
// Atomicity semantics:
//
//	Update is not cross-backend atomic. Each backend's watermark is
//	written in its own single transaction (Cypher MERGE for Neo4j,
//	Weaviate REST upsert for the CortexMeta collection). If the
//	process is interrupted between the Neo4j and Weaviate writes,
//	each backend is left in a valid prior state — the worst case
//	is that the next startup replays a handful of datoms that were
//	actually already applied, which is safe because LWW attributes
//	collapse to the highest-tx value.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Self-healing startup"
//   docs/spec/cortex-spec.md §"Log and Apply Layer" (watermarks)
package watermark

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The Neo4j :CortexMeta node is a singleton keyed by id="watermark".
// Using MERGE ensures Update is idempotent across restarts.
const (
	neo4jNodeLabel = "CortexMeta"
	neo4jNodeID    = "watermark"
)

// Weaviate stores its watermark in a dedicated CortexMeta class. We
// use a fixed UUID so Read and Update always target the same object.
// The class name must start with an uppercase letter (Weaviate
// GraphQL type identifier rule).
const (
	weaviateClass    = "CortexMeta"
	weaviateObjectID = "00000000-0000-0000-0000-00000000c0de"
)

// Watermark is the value the store reads and writes. Two bool fields
// distinguish "never written" from "written to the zero ULID"; the
// acceptance criterion "The returned struct distinguishes a
// never-written watermark from one explicitly set to the zero ULID"
// relies on this distinction.
type Watermark struct {
	Neo4jTx         string
	WeaviateTx      string
	Neo4jWritten    bool
	WeaviateWritten bool
	UpdatedAt       time.Time
}

// Neo4jClient is the subset of the neo4j adapter we need. Declaring
// it here (rather than importing internal/neo4j.Client directly)
// lets unit tests substitute a fake without pulling in the driver
// and keeps the dependency direction one-way.
type Neo4jClient interface {
	WriteEntries(ctx context.Context, cypher string, params map[string]any) error
	QueryGraph(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error)
}

// WeaviateClient is the subset of the weaviate adapter we need.
type WeaviateClient interface {
	EnsureSchema(ctx context.Context) error
	Upsert(ctx context.Context, class, id string, vector []float32, properties map[string]any) error
	GetObject(ctx context.Context, class, id string) (map[string]any, error)
}

// Store is the concrete watermark persistence handle.
type Store struct {
	Neo4j    Neo4jClient
	Weaviate WeaviateClient

	// now is a seam so tests can pin the UpdatedAt timestamp. In
	// production it defaults to time.Now().UTC().
	now func() time.Time
}

// NewStore wires a Store over the two backend clients. Pass a nil
// client for any backend that is intentionally absent (e.g., in a
// partial-availability degraded mode); Read and Update will then
// report that backend as "never written" and skip it respectively.
func NewStore(n Neo4jClient, w WeaviateClient) *Store {
	return &Store{
		Neo4j:    n,
		Weaviate: w,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// Read returns the current watermark for both backends. Missing
// watermark records (fresh install, no replay ever run) are reported
// as Neo4jWritten=false / WeaviateWritten=false with zero-valued tx
// strings, rather than as errors, so callers can use the struct
// directly without a special-case "first run" branch.
func (s *Store) Read(ctx context.Context) (Watermark, error) {
	var out Watermark
	out.UpdatedAt = s.now()

	if s.Neo4j != nil {
		tx, ok, err := s.readNeo4j(ctx)
		if err != nil {
			return out, fmt.Errorf("watermark: read neo4j: %w", err)
		}
		out.Neo4jTx = tx
		out.Neo4jWritten = ok
	}
	if s.Weaviate != nil {
		tx, ok, err := s.readWeaviate(ctx)
		if err != nil {
			return out, fmt.Errorf("watermark: read weaviate: %w", err)
		}
		out.WeaviateTx = tx
		out.WeaviateWritten = ok
	}
	return out, nil
}

// readNeo4j runs a MATCH for the singleton :CortexMeta node and
// returns (tx, true, nil) if present, ("", false, nil) if absent, or
// an error if the query itself failed.
func (s *Store) readNeo4j(ctx context.Context) (string, bool, error) {
	rows, err := s.Neo4j.QueryGraph(ctx,
		`MATCH (m:`+neo4jNodeLabel+` {id: $id}) RETURN m.tx AS tx`,
		map[string]any{"id": neo4jNodeID},
	)
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	tx, _ := rows[0]["tx"].(string)
	return tx, true, nil
}

// readWeaviate fetches the singleton CortexMeta object and decodes
// the tx property. A 404 from Weaviate is translated into "not
// written" rather than an error; the weaviate.HTTPClient.GetObject
// surface maps 404 to (nil, nil) so we just check for a nil map.
func (s *Store) readWeaviate(ctx context.Context) (string, bool, error) {
	props, err := s.Weaviate.GetObject(ctx, weaviateClass, weaviateObjectID)
	if err != nil {
		return "", false, err
	}
	if props == nil {
		return "", false, nil
	}
	tx, _ := props["tx"].(string)
	return tx, true, nil
}

// UpdateNeo4j writes a new Neo4j watermark. The MERGE pattern keeps
// the node a singleton: first call creates it, subsequent calls
// update it in place. The acceptance criterion "Writing tx T5 via
// Update then calling Read returns T5 for the same backend" is
// covered by the round-trip tests over a fake Neo4jClient.
func (s *Store) UpdateNeo4j(ctx context.Context, tx string) error {
	if s.Neo4j == nil {
		return errors.New("watermark: no neo4j client configured")
	}
	return s.Neo4j.WriteEntries(ctx,
		`MERGE (m:`+neo4jNodeLabel+` {id: $id})
         SET m.tx = $tx, m.updated_at = $updated_at`,
		map[string]any{
			"id":         neo4jNodeID,
			"tx":         tx,
			"updated_at": s.now().Format(time.RFC3339Nano),
		},
	)
}

// UpdateWeaviate writes a new Weaviate watermark via Upsert with an
// empty vector. The CortexMeta class is declared with vectorizer
// none (see EnsureCortexMetaSchema); no HNSW lookup is needed for
// watermark reads because GetObject hits the REST path directly.
func (s *Store) UpdateWeaviate(ctx context.Context, tx string) error {
	if s.Weaviate == nil {
		return errors.New("watermark: no weaviate client configured")
	}
	return s.Weaviate.Upsert(ctx, weaviateClass, weaviateObjectID, nil, map[string]any{
		"tx":         tx,
		"updated_at": s.now().Format(time.RFC3339Nano),
	})
}

// Update writes the watermark to both backends. It is a convenience
// wrapper — calling it is equivalent to UpdateNeo4j followed by
// UpdateWeaviate, with the caveat that an error from the Neo4j
// write short-circuits and the Weaviate write is not attempted.
// This matches the spec's "atomic per backend" semantics: each
// backend's watermark is independently valid after any prefix of
// the update sequence.
func (s *Store) Update(ctx context.Context, tx string) error {
	if s.Neo4j != nil {
		if err := s.UpdateNeo4j(ctx, tx); err != nil {
			return err
		}
	}
	if s.Weaviate != nil {
		if err := s.UpdateWeaviate(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}
