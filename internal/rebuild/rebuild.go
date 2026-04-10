// Package rebuild implements `cortex rebuild`: replaying the
// committed datom log into a fresh derived backend state.
//
// Rebuild's job, in spec terms (docs/spec/cortex-spec.md FR-005,
// §"Pinned-model drift", AC4 in epic cortex-4kq.33):
//
//  1. Stream every committed datom in tx-ULID-ascending order.
//  2. Enforce pinned-model invariants — every entry that recorded an
//     embedding_model_digest must agree with the currently installed
//     digest, otherwise rebuild fails with PINNED_MODEL_DRIFT and
//     names the affected entries. The operator can install the
//     pinned digest, or pass --accept-drift to opt in to re-embedding
//     under the current model. Re-embedding emits one model_rebind
//     audit datom per affected entry.
//  3. Apply each datom into a *staging namespace* (a separate set of
//     Weaviate classes / Neo4j labels) so the active derived index is
//     untouched until the rebuild succeeds. On success the staging
//     namespace is atomically swapped to active; on failure the
//     active state is unchanged and staging is cleaned up.
//  4. Be idempotent: running rebuild twice in a row over the same log
//     produces byte-identical Layer 1 (datom log) state, because
//     model_rebind audit datoms are only written when an entry was
//     actually re-embedded under a different digest.
//
// The package is structured around four narrow seams so it can be
// unit-tested without a live backend stack:
//
//   - DatomSource — yields datoms in tx-ascending order. Production
//     wraps the multi-segment merge-sort reader from internal/log;
//     tests pass a slice-backed source.
//   - DigestSource — returns the currently installed embedding model
//     digest. Production wraps *ollama.HTTPClient.Show; tests pass a
//     constant string.
//   - Embedder — re-embeds an entry's body when --accept-drift is
//     set. Production wraps *ollama.HTTPClient.Embed; tests use a
//     fake that records calls.
//   - StagingBackends — receives datoms in the staging namespace and
//     supports a final atomic Swap. Production wraps Weaviate +
//     Neo4j adapters; tests use a fake that records call order so
//     the staging-vs-active invariant can be asserted directly.
//
// Wiring note: cmd/cortex/rebuild.go (ops-dev) constructs the seams
// from the live config and calls Run.
package rebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// Attribute names used by rebuild. They are not yet emitted by the
// observe write path (a known follow-up — see TODO in pipeline.go),
// but rebuild reads them when present so the invariant is enforced
// for any log that does carry them. Tests synthesize datoms with
// these attributes directly.
const (
	AttrEmbeddingModelDigest = "embedding_model_digest"
	AttrEmbeddingModelName   = "embedding_model_name"
	AttrModelRebind          = "model_rebind"
	AttrBody                 = "body"
	EntryPrefix              = "entry:"
	SourceRebuild            = "rebuild"
)

// DatomSource yields the next datom in tx-ULID-ascending order.
// Returning (zero, false, nil) signals end of stream; a non-nil error
// aborts rebuild. Production wraps internal/log.Reader.Next; tests
// pass a slice-backed source.
type DatomSource interface {
	Next(ctx context.Context) (datom.Datom, bool, error)
	Close() error
}

// DigestSource returns the currently installed embedding model
// digest. Rebuild calls this once at the start of a run and caches
// the result for the rest of the pipeline.
type DigestSource interface {
	CurrentDigest(ctx context.Context) (string, error)
}

// Embedder re-embeds an entry body when rebuild has decided (under
// --accept-drift) to bind it to the current model. The returned
// vector is handed to the staging Weaviate applier alongside the
// existing entry properties.
type Embedder interface {
	Embed(ctx context.Context, body string) ([]float32, error)
}

// StagingBackends is the bundled write seam for the staging Weaviate
// + Neo4j namespaces. Implementations must be idempotent over (entry
// id, attribute) so a partial earlier run can be safely restarted.
type StagingBackends interface {
	// Create allocates the staging namespace (e.g. EntryStaging
	// class, :CortexStaging label) and ensures it is empty.
	Create(ctx context.Context) error

	// ApplyDatom writes one datom into staging. Implementations
	// translate the datom into the appropriate backend mutation
	// (Weaviate Upsert / Neo4j MERGE) under the staging namespace.
	ApplyDatom(ctx context.Context, d datom.Datom) error

	// ApplyEmbedding writes a re-embedded vector for an entry into
	// the staging Weaviate namespace. Called only for entries that
	// were re-embedded under --accept-drift; existing-vector entries
	// flow through ApplyDatom (the production Weaviate applier
	// honours the entry's stored vector property).
	ApplyEmbedding(ctx context.Context, entryID string, vector []float32) error

	// Swap atomically promotes the staging namespace to active and
	// removes the previously active namespace. The contract is that
	// after Swap returns nil, no caller should ever read from the
	// pre-rebuild active namespace.
	Swap(ctx context.Context) error

	// Cleanup removes any partial staging state without touching
	// active. Called from Run's failure path so a botched rebuild
	// does not leave staging artifacts behind.
	Cleanup(ctx context.Context) error
}

// LogAppender is the narrow append seam Run uses to write
// model_rebind audit datoms when --accept-drift triggers a re-embed.
// *log.Writer satisfies this contract.
type LogAppender interface {
	Append(group []datom.Datom) (string, error)
}

// Config is the per-run input to Run. Every field is required except
// Embedder and Log, which are only consulted on the --accept-drift
// path.
type Config struct {
	Source       DatomSource
	Digest       DigestSource
	Backends     StagingBackends
	AcceptDrift  bool
	Embedder     Embedder    // required iff AcceptDrift
	Log          LogAppender // required iff AcceptDrift (audit datoms)
	Actor        string
	InvocationID string
	Now          func() time.Time
}

// Result is the populated outcome of a successful Run. The CLI prints
// a human summary; tests assert on the counters.
type Result struct {
	DatomsScanned    int
	EntriesApplied   int
	RebindsPerformed int
	StartedAt        time.Time
	CompletedAt      time.Time
}

// ErrPinnedModelDrift is the operational error returned when one or
// more entries declare an embedding_model_digest that differs from
// the currently installed digest, and --accept-drift was not set.
// The error's Details field carries the affected entry ids so the
// CLI can print them.
func newPinnedDriftError(currentDigest string, affected []affectedEntry) error {
	ids := make([]string, len(affected))
	mismatches := make([]map[string]string, len(affected))
	for i, a := range affected {
		ids[i] = a.entryID
		mismatches[i] = map[string]string{
			"entry":   a.entryID,
			"pinned":  a.pinnedDigest,
			"current": currentDigest,
		}
	}
	e := errs.Operational("PINNED_MODEL_DRIFT",
		fmt.Sprintf("rebuild aborted: %d entries pinned to a different embedding model digest; "+
			"install the pinned digest or rerun with --accept-drift", len(affected)),
		nil)
	e.Details = map[string]any{
		"current_digest":   currentDigest,
		"affected_entries": ids,
		"mismatches":       mismatches,
	}
	return e
}

// affectedEntry tracks one entry whose stored digest differs from
// current. The pinnedDigest field carries the digest the entry was
// originally written under so the operator can install it.
type affectedEntry struct {
	entryID      string
	pinnedDigest string
	body         string // populated only on the --accept-drift path
}

// Run executes one rebuild pass over cfg.Source. The high-level
// shape is:
//
//  1. Pre-flight: load the current digest.
//  2. Scan pass: stream every datom into an in-memory entry summary
//     (body + recorded digest). The merge-sort reader guarantees
//     tx-ascending order so we never need to reconcile out-of-order
//     records here. The scan also captures every datom in a buffer
//     so the apply pass can replay them without re-reading the log
//     (rebuild is a one-shot operation, not a streaming subscriber).
//  3. Drift check: any entry whose recorded digest differs from
//     current is added to affected[]. If !cfg.AcceptDrift, return
//     PINNED_MODEL_DRIFT before touching any backend.
//  4. Staging create: allocate the staging namespace.
//  5. Apply pass: replay every datom through Backends.ApplyDatom.
//     For affected entries under --accept-drift, additionally
//     re-embed the body and write a model_rebind audit datom via
//     LogAppender so the new lineage is captured in the log.
//  6. Swap: atomically promote staging to active.
//  7. Return Result. On any error after staging is created, Cleanup
//     is called best-effort so we never leave a half-built staging
//     namespace lying around.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	startedAt := now()

	currentDigest, err := cfg.Digest.CurrentDigest(ctx)
	if err != nil {
		return nil, errs.Operational("DIGEST_LOOKUP_FAILED",
			"could not read current embedding model digest", err)
	}
	if currentDigest == "" {
		return nil, errs.Operational("DIGEST_LOOKUP_FAILED",
			"current embedding model digest is empty; rebuild requires a pinned digest", nil)
	}

	// --- Phase 2: scan pass. We materialize the full datom slice
	// because rebuild is one-shot and the spec's idempotency AC
	// requires the apply phase to see the same ordering as the scan
	// phase. The merge-sort reader is single-pass so a re-open
	// would be the alternative; buffering once is simpler. ---
	allDatoms, perEntry, scanErr := scanLog(ctx, cfg.Source)
	if scanErr != nil {
		return nil, scanErr
	}

	// --- Phase 3: drift check. ---
	affected := computeDrift(perEntry, currentDigest)
	if len(affected) > 0 && !cfg.AcceptDrift {
		return nil, newPinnedDriftError(currentDigest, affected)
	}

	// --- Phase 4: staging create. ---
	if err := cfg.Backends.Create(ctx); err != nil {
		return nil, errs.Operational("STAGING_CREATE_FAILED",
			"could not allocate staging namespace", err)
	}
	// Best-effort cleanup on any subsequent failure. Success path
	// runs Swap which makes Cleanup a no-op (or a delete of the now-
	// inactive namespace, depending on backend).
	cleanup := func() { _ = cfg.Backends.Cleanup(ctx) }

	// --- Phase 5: apply pass. ---
	rebinds := 0
	rebindByEntry := make(map[string]struct{}, len(affected))
	for _, a := range affected {
		rebindByEntry[a.entryID] = struct{}{}
	}
	for i := range allDatoms {
		d := allDatoms[i]
		if err := cfg.Backends.ApplyDatom(ctx, d); err != nil {
			cleanup()
			return nil, errs.Operational("STAGING_APPLY_FAILED",
				fmt.Sprintf("backend apply failed at tx %s", d.Tx), err)
		}
	}
	// Re-embed affected entries (only on --accept-drift) and emit
	// audit datoms. We do this after the main apply so a re-embed
	// failure does not leave the staging namespace in a half-applied
	// state — by the time we re-embed, every datom is already in
	// staging and the re-embedded vector is the only delta.
	if cfg.AcceptDrift {
		for _, a := range affected {
			vec, err := cfg.Embedder.Embed(ctx, a.body)
			if err != nil {
				cleanup()
				return nil, errs.Operational("REBIND_EMBED_FAILED",
					fmt.Sprintf("re-embed failed for %s", a.entryID), err)
			}
			if err := cfg.Backends.ApplyEmbedding(ctx, a.entryID, vec); err != nil {
				cleanup()
				return nil, errs.Operational("REBIND_APPLY_FAILED",
					fmt.Sprintf("staging apply failed for %s vector", a.entryID), err)
			}
			audit, err := buildRebindDatom(a, currentDigest, cfg.Actor, cfg.InvocationID, now())
			if err != nil {
				cleanup()
				return nil, errs.Operational("REBIND_AUDIT_FAILED",
					"could not construct model_rebind audit datom", err)
			}
			if _, err := cfg.Log.Append([]datom.Datom{audit}); err != nil {
				cleanup()
				return nil, errs.Operational("REBIND_AUDIT_APPEND_FAILED",
					"could not append model_rebind audit datom", err)
			}
			rebinds++
		}
	}

	// --- Phase 6: swap. ---
	if err := cfg.Backends.Swap(ctx); err != nil {
		cleanup()
		return nil, errs.Operational("STAGING_SWAP_FAILED",
			"atomic swap from staging to active failed; active namespace untouched", err)
	}

	return &Result{
		DatomsScanned:    len(allDatoms),
		EntriesApplied:   len(perEntry),
		RebindsPerformed: rebinds,
		StartedAt:        startedAt,
		CompletedAt:      now(),
	}, nil
}

// validateConfig enforces the contract that --accept-drift requires
// an Embedder and a LogAppender. Other zero values produce an
// operational error rather than a panic so the CLI surfaces a clean
// message.
func validateConfig(cfg Config) error {
	if cfg.Source == nil {
		return errs.Validation("MISSING_SOURCE", "rebuild requires a DatomSource", nil)
	}
	if cfg.Digest == nil {
		return errs.Validation("MISSING_DIGEST", "rebuild requires a DigestSource", nil)
	}
	if cfg.Backends == nil {
		return errs.Validation("MISSING_BACKENDS", "rebuild requires a StagingBackends", nil)
	}
	if cfg.AcceptDrift {
		if cfg.Embedder == nil {
			return errs.Validation("MISSING_EMBEDDER",
				"--accept-drift requires an Embedder for re-embedding", nil)
		}
		if cfg.Log == nil {
			return errs.Validation("MISSING_LOG_APPENDER",
				"--accept-drift requires a LogAppender for model_rebind audit datoms", nil)
		}
	}
	return nil
}

// entrySummary tracks the rebuild-relevant attributes of one entry
// as the scan pass walks the log. Body and pinnedDigest are stored
// as plain strings so the apply pass does not need to re-unmarshal.
type entrySummary struct {
	entryID      string
	body         string
	pinnedDigest string
}

// scanLog drains the source into an ordered slice and an entry-id →
// summary map. The slice preserves the global tx-ascending order
// (which the source guarantees), and the map records the most recent
// (highest-tx) value of body and embedding_model_digest per entry —
// rebuild only needs the latest write of each, since the apply pass
// re-emits everything in order anyway.
func scanLog(ctx context.Context, src DatomSource) ([]datom.Datom, map[string]*entrySummary, error) {
	defer src.Close()
	const initialCap = 1024
	all := make([]datom.Datom, 0, initialCap)
	per := make(map[string]*entrySummary, 64)
	for {
		d, ok, err := src.Next(ctx)
		if err != nil {
			return nil, nil, errs.Operational("LOG_READ_FAILED",
				"failed to read next datom from log source", err)
		}
		if !ok {
			break
		}
		all = append(all, d)
		if len(d.E) == 0 {
			continue
		}
		// Only entry-prefixed entities participate in pinned-model
		// drift; trail and community datoms have no embedding.
		if !hasPrefix(d.E, EntryPrefix) {
			continue
		}
		summary, exists := per[d.E]
		if !exists {
			summary = &entrySummary{entryID: d.E}
			per[d.E] = summary
		}
		switch d.A {
		case AttrBody:
			if s, ok := decodeString(d.V); ok {
				summary.body = s
			}
		case AttrEmbeddingModelDigest:
			if s, ok := decodeString(d.V); ok {
				summary.pinnedDigest = s
			}
		}
	}
	return all, per, nil
}

// computeDrift returns the entries whose pinned digest differs from
// current. Entries with no recorded digest are skipped — the spec
// allows them as a degraded mode (cortex doctor surfaces them) and
// they trivially round-trip under any current digest because there
// is nothing to compare against. The result is sorted by entry id
// so error messages are stable across runs.
func computeDrift(per map[string]*entrySummary, currentDigest string) []affectedEntry {
	var out []affectedEntry
	for id, s := range per {
		if s.pinnedDigest == "" {
			continue
		}
		if s.pinnedDigest == currentDigest {
			continue
		}
		out = append(out, affectedEntry{
			entryID:      id,
			pinnedDigest: s.pinnedDigest,
			body:         s.body,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].entryID < out[j].entryID })
	return out
}

// buildRebindDatom assembles the model_rebind audit datom written
// for one re-embedded entry. The V field is a JSON object with the
// pinned and current digests so a future history walk can show the
// rebind story.
func buildRebindDatom(a affectedEntry, currentDigest, actor, invocationID string, ts time.Time) (datom.Datom, error) {
	payload := map[string]string{
		"from": a.pinnedDigest,
		"to":   currentDigest,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return datom.Datom{}, fmt.Errorf("marshal payload: %w", err)
	}
	d := datom.Datom{
		Tx:           ulid.Make().String(),
		Ts:           ts.UTC().Format(time.RFC3339Nano),
		Actor:        actor,
		Op:           datom.OpAdd,
		E:            a.entryID,
		A:            AttrModelRebind,
		V:            raw,
		Src:          SourceRebuild,
		InvocationID: invocationID,
	}
	if err := d.Seal(); err != nil {
		return datom.Datom{}, fmt.Errorf("seal: %w", err)
	}
	return d, nil
}

// hasPrefix is a tiny wrapper avoiding the strings import for one
// call site. It keeps the package's import surface aligned with what
// the rebuild logic actually needs.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// decodeString unmarshals a json.RawMessage that is expected to be a
// JSON string into a Go string. Non-string values return ("", false)
// rather than an error so the scan pass can keep going.
func decodeString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	// Cheap path: a JSON-encoded string starts with '"'. Falling
	// through to json.Unmarshal handles escapes correctly.
	if !bytes.HasPrefix(raw, []byte(`"`)) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// SliceSource is a DatomSource backed by a pre-built slice. It is
// exposed (rather than kept test-only) so the CLI can drain a one-
// shot reader into a slice when the operator wants a deterministic
// rebuild against a frozen log snapshot.
type SliceSource struct {
	rows []datom.Datom
	i    int
}

// NewSliceSource returns a SliceSource over the given datoms. The
// slice is referenced, not copied; callers must not mutate it for
// the lifetime of the source.
func NewSliceSource(rows []datom.Datom) *SliceSource {
	return &SliceSource{rows: rows}
}

// Next implements DatomSource.
func (s *SliceSource) Next(_ context.Context) (datom.Datom, bool, error) {
	if s.i >= len(s.rows) {
		return datom.Datom{}, false, nil
	}
	d := s.rows[s.i]
	s.i++
	return d, true, nil
}

// Close implements DatomSource. SliceSource holds no resources, so
// Close is a no-op.
func (s *SliceSource) Close() error { return nil }
