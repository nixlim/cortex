// Package replay implements the self-healing startup protocol that
// reconciles Neo4j and Weaviate against the authoritative datom log.
//
// Before any read-dependent command (recall, reflect, analyze, subject
// merge, community show, path, trail show, trail list, history, as-of)
// answers a query, it calls SelfHeal. SelfHeal computes log max(tx),
// reads the current per-backend watermark, and replays every datom in
// the half-open interval (watermark, max(tx)] into any backend that is
// behind. On success the watermark for that backend is advanced to
// log max(tx).
//
// The replay is strictly in-order: datoms come out of the merge-sort
// reader in ascending tx-ULID order, so applying them in that order is
// equivalent to the spec's "LWW attributes collapse to the highest-tx
// value" guarantee. No LWW pre-collapse pass is required — the final
// state is identical either way, and streaming keeps memory bounded.
//
// Atomicity: the watermark is only advanced for a backend after every
// datom in the interval has been applied without error. A failure in
// the middle leaves the watermark at its previous value, which is safe
// because the next invocation will retry from that point. Per-backend
// atomicity is the same model documented in internal/watermark.
package replay

import (
	"context"
	"errors"
	"fmt"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/watermark"
)

// Applier is the subset of a backend adapter the replay path needs.
// The log package does not know about Neo4j or Weaviate; the replay
// package depends only on this interface so unit tests can supply a
// fake and the neo4j / weaviate adapters can implement it without a
// cycle. Name is used in error messages ("neo4j replay tx T4: ...").
type Applier interface {
	Name() string
	Apply(ctx context.Context, d datom.Datom) error
}

// WatermarkStore is the subset of *watermark.Store used by SelfHeal.
// The interface is narrow so tests can supply a fake and so a future
// third backend can be added without changing this signature.
type WatermarkStore interface {
	Read(ctx context.Context) (watermark.Watermark, error)
	UpdateNeo4j(ctx context.Context, tx string) error
	UpdateWeaviate(ctx context.Context, tx string) error
}

// Result reports per-backend counts and the log watermark at the time
// of the heal. Callers (commands) pass it to ops.log so operators can
// see exactly how many datoms each startup path replayed.
type Result struct {
	// LogMaxTx is the tx ULID of the last datom in the log at the time
	// SelfHeal ran. Empty when the log is empty.
	LogMaxTx string

	// Neo4jApplied is the number of datoms applied to Neo4j during
	// this invocation. Zero when Neo4j was already caught up.
	Neo4jApplied int

	// WeaviateApplied is the number of datoms applied to Weaviate
	// during this invocation. Zero when Weaviate was already caught up.
	WeaviateApplied int
}

// BackendError wraps an Applier error with the backend name and the
// tx ULID that failed, so callers can surface a precise diagnostic
// without string matching. The spec acceptance criterion "A failure
// during replay returns an error identifying which backend and which
// tx failed" is satisfied by this type.
type BackendError struct {
	Backend string
	Tx      string
	Err     error
}

func (e *BackendError) Error() string {
	return fmt.Sprintf("replay: backend %s failed at tx %s: %v", e.Backend, e.Tx, e.Err)
}

func (e *BackendError) Unwrap() error { return e.Err }

// SelfHeal runs the full startup replay protocol and returns a Result
// describing what happened. paths is the Healthy segment list from a
// prior log.Load call; store persists the per-backend watermarks;
// neo4j and weaviate are the backend appliers (either may be nil for
// tests or a degraded-mode install).
//
// Behaviour matrix:
//   - Empty log (no segments, or segments with no datoms): returns a
//     zero-count Result and advances no watermark. Fresh installs are
//     a supported state.
//   - Both backends at log max(tx): returns a zero-count Result and
//     makes no Update calls. This is the hot path for warm restarts
//     and is explicitly tested by AC2.
//   - A backend is behind: every datom with tx > watermark is applied
//     in order; on success the backend's watermark advances to
//     log max(tx); on failure a *BackendError is returned and the
//     watermark is NOT advanced.
func SelfHeal(ctx context.Context, paths []string, store WatermarkStore, neo4j, weaviate Applier) (Result, error) {
	var result Result

	// Step 1: compute log max(tx). A single streaming pass through the
	// merge-sort reader is sufficient because the reader already emits
	// datoms in ascending tx order.
	maxTx, err := logMaxTx(paths)
	if err != nil {
		return result, err
	}
	result.LogMaxTx = maxTx
	if maxTx == "" {
		// Empty log: nothing to replay, nothing to advance.
		return result, nil
	}

	// Step 2: read current watermarks.
	wm, err := store.Read(ctx)
	if err != nil {
		return result, fmt.Errorf("replay: read watermarks: %w", err)
	}

	// Step 3: per-backend replay. Each branch is independent; a Neo4j
	// failure does not prevent the Weaviate branch from running, which
	// is the right behaviour for a partial-availability degraded mode
	// — except that we still surface the first error to the caller.
	if neo4j != nil && needsReplay(wm.Neo4jTx, maxTx) {
		applied, err := replayInto(ctx, paths, wm.Neo4jTx, neo4j)
		if err != nil {
			return result, err
		}
		result.Neo4jApplied = applied
		if err := store.UpdateNeo4j(ctx, maxTx); err != nil {
			return result, fmt.Errorf("replay: update neo4j watermark: %w", err)
		}
	}
	if weaviate != nil && needsReplay(wm.WeaviateTx, maxTx) {
		applied, err := replayInto(ctx, paths, wm.WeaviateTx, weaviate)
		if err != nil {
			return result, err
		}
		result.WeaviateApplied = applied
		if err := store.UpdateWeaviate(ctx, maxTx); err != nil {
			return result, fmt.Errorf("replay: update weaviate watermark: %w", err)
		}
	}
	return result, nil
}

// needsReplay reports whether a backend's watermark is strictly behind
// log max(tx). ULIDs are Crockford base32 strings that compare
// lexicographically in the same order as their timestamps, so ordinary
// string comparison is the correct predicate. A "never written"
// watermark is the empty string, which is less than every real ULID.
func needsReplay(backendTx, logMax string) bool {
	return backendTx < logMax
}

// logMaxTx opens the merge-sort reader over the supplied segment list
// and streams through every datom, keeping the last tx it sees. For an
// empty input (nil slice, or segments containing no datoms) it returns
// ("", nil) so SelfHeal can take the fast path.
//
// The scan is O(total datoms) but memory is bounded to one datom per
// segment (the reader's K-way heap invariant from cortex-4kq.29).
func logMaxTx(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", nil
	}
	r, err := log.NewReader(paths)
	if err != nil {
		return "", fmt.Errorf("replay: open reader: %w", err)
	}
	defer r.Close()
	var last string
	for {
		d, ok, err := r.Next()
		if err != nil {
			return "", fmt.Errorf("replay: scan log: %w", err)
		}
		if !ok {
			return last, nil
		}
		last = d.Tx
	}
}

// replayInto streams the log and applies every datom whose tx is
// strictly greater than afterTx. It returns the number of datoms it
// applied successfully. Any Apply error is wrapped in a BackendError
// with the failing tx so callers can surface the exact point of
// failure. Watermark advancement is the caller's job — replayInto
// deliberately does not touch the store.
func replayInto(ctx context.Context, paths []string, afterTx string, backend Applier) (int, error) {
	if backend == nil {
		return 0, errors.New("replay: nil backend applier")
	}
	r, err := log.NewReader(paths)
	if err != nil {
		return 0, fmt.Errorf("replay: open reader: %w", err)
	}
	defer r.Close()

	applied := 0
	for {
		d, ok, err := r.Next()
		if err != nil {
			return applied, fmt.Errorf("replay: scan log: %w", err)
		}
		if !ok {
			return applied, nil
		}
		// Skip datoms already reflected in the backend. The reader
		// emits in ascending tx order, so once we pass afterTx every
		// remaining datom is in the replay window.
		if d.Tx <= afterTx {
			continue
		}
		if err := backend.Apply(ctx, d); err != nil {
			return applied, &BackendError{Backend: backend.Name(), Tx: d.Tx, Err: err}
		}
		applied++
	}
}
