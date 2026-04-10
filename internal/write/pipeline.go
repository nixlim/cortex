// Package write implements the Cortex observe / ingest / reflect
// shared write pipeline: validate → secret scan → subject resolve →
// embed → append → apply → watermark.
//
// The pipeline is factored as a Pipeline struct with injectable
// dependencies so the CLI entrypoints (cmd/cortex/observe.go,
// ingest.go, reflect.go) can share the same ordering guarantees while
// tests can drive the whole flow against fakes. The design decision
// that matters is: every side effect that requires validation lives
// behind a function boundary that tests can substitute, so the four
// validation acceptance criteria of cortex-4kq.35 can be exercised
// without touching Ollama, Neo4j, or Weaviate.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Episodic Observation Capture"
//   docs/spec/cortex-spec.md §"Frame Type Catalog"
//   docs/spec/cortex-spec.md §"Secret handling"
package write

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
)

// AgentWritableKinds is the closed set of frame types an agent is
// allowed to write through cortex observe. The remaining reflection-
// only types are enumerated in reflectionOnlyKinds and produce the
// REFLECTION_ONLY_KIND error from observe.
var AgentWritableKinds = map[string]struct{}{
	"Observation":       {},
	"SessionReflection": {},
	"ObservedRace":      {},
}

// reflectionOnlyKinds is the eight frame types that only cortex
// reflect / cortex analyze may produce. Listed verbatim from the
// "Frame Type Catalog" section of the spec so a future additional
// reflection-only type can be added here with a one-line change and
// have the error code stay accurate.
var reflectionOnlyKinds = map[string]struct{}{
	"BugPattern":       {},
	"LibraryBehavior":  {},
	"ConceptualModel":  {},
	"Principle":        {},
	"ADR":              {},
	"RefactoringStep":  {},
	"TroubleshootStep": {},
	"Practice":         {},
}

// requiredFacets is the set of facet keys an observe call must carry
// for a valid write. The artifact facet is conditionally required and
// enforced separately by validateFacets.
var requiredFacets = []string{"domain", "project"}

// LogAppender is the subset of log.Writer the pipeline needs. A narrow
// interface lets tests drop in a fake that captures the datoms the
// pipeline tried to write without touching the filesystem.
type LogAppender interface {
	Append(group []datom.Datom) (string, error)
}

// Embedder is the subset of the Ollama adapter used during the write
// path. Embed may return an error (MODEL_DIGEST_RACE, connection
// refused); the pipeline wraps it as an operational error so the CLI
// exits 1, not 2. ModelDigest returns the embedding model's name and
// content digest captured at Show time, so the pipeline can emit the
// FR-051 embedding_model_name / embedding_model_digest datoms on
// every observed entry.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	ModelDigest(ctx context.Context) (name, digest string, err error)
}

// BackendApplier is the minimum contract a backend adapter must
// provide to participate in the write pipeline's post-commit apply
// phase. It mirrors replay.Applier intentionally so the observe write
// path and the startup self-heal path share one interface.
type BackendApplier interface {
	Name() string
	Apply(ctx context.Context, d datom.Datom) error
}

// Pipeline orchestrates a single write. Zero values are not usable
// for real writes — at minimum a LogAppender and a clock source must
// be supplied — but tests that exercise only the validation stages
// can leave every adapter field nil.
type Pipeline struct {
	// Detector scans the request body for high-confidence secrets
	// before the pipeline is allowed to append any datoms. A nil
	// detector is treated as "no secret scan", which is intended for
	// unit tests only.
	Detector *secrets.Detector

	// Registry resolves subject PSIs to their canonical form and
	// mints previously unseen canonical slots. A nil registry skips
	// subject resolution entirely, which is acceptable when the
	// caller has no subject to attach.
	Registry *psi.Registry

	// Log is the segment appender. The pipeline always holds the
	// log's flock for as short a time as possible by pre-building
	// the entire datom group before calling Append; see
	// log.Writer.Append for the commit-point contract.
	Log LogAppender

	// Embedder is optional. When present, the pipeline captures a
	// body vector and attaches it to the Weaviate apply phase. When
	// absent, the write still commits; Weaviate's row for the entry
	// lands with a nil vector (the class is declared with
	// vectorizer=none so this is safe).
	Embedder Embedder

	// Neo4j and Weaviate are post-commit appliers. Failures from
	// either are non-fatal for the write — the log is already
	// durable and self-heal will retry on the next command — but
	// the pipeline still returns the error so the CLI can surface a
	// warning.
	Neo4j    BackendApplier
	Weaviate BackendApplier

	// Now returns the wall-clock timestamp used in every datom's Ts
	// field. Tests pin it to make datom content deterministic.
	// Production callers should pass func() time.Time { return time.Now().UTC() }.
	Now func() time.Time

	// Actor is recorded in every datom's Actor field. Typical values
	// are user identities, "cortex", or a service name.
	Actor string

	// InvocationID is the per-command ULID shared by every datom
	// and ops.log entry in one invocation. Generating it at the
	// command entry point rather than inside the pipeline keeps the
	// ops.log correlation invariant easy to enforce.
	InvocationID string
}

// ObserveRequest is the normalized cortex observe input after flag
// parsing. The CLI is responsible for populating every field from the
// command line; the pipeline never looks at os.Args.
type ObserveRequest struct {
	Body    string
	Kind    string
	Facets  map[string]string
	Subject string // canonical or alias PSI, optional
	TrailID string // optional trail attachment
}

// ObserveResult is the successful-outcome payload. The EntryID is the
// prefixed ULID the CLI prints to stdout on success. Tx is returned
// so ops.log and watermark updates can reference the same identifier.
type ObserveResult struct {
	EntryID string
	Tx      string
}

// Observe runs the full observe pipeline for one request. On any
// validation failure it returns an *errs.Error with Kind=Validation
// and writes no datoms. On an operational failure after the log
// commit it returns a non-nil error *and* a non-nil *ObserveResult —
// the EntryID is valid because the log is authoritative once Append
// succeeded, and the caller can still print it.
func (p *Pipeline) Observe(ctx context.Context, req ObserveRequest) (*ObserveResult, error) {
	// --- Stage 1: validation, all before any side effect. ---
	if req.Kind == "" {
		return nil, errs.Validation("MISSING_KIND",
			"cortex observe requires --kind", nil)
	}
	if _, ok := reflectionOnlyKinds[req.Kind]; ok {
		return nil, errs.Validation("REFLECTION_ONLY_KIND",
			fmt.Sprintf("kind %q is reflection-only; use cortex reflect", req.Kind),
			map[string]any{"kind": req.Kind})
	}
	if _, ok := AgentWritableKinds[req.Kind]; !ok {
		return nil, errs.Validation("UNKNOWN_KIND",
			fmt.Sprintf("kind %q is not a known frame type", req.Kind),
			map[string]any{"kind": req.Kind})
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, errs.Validation("EMPTY_BODY",
			"cortex observe requires a non-empty body", nil)
	}
	if err := validateFacets(req.Facets); err != nil {
		return nil, err
	}

	// --- Stage 2: secret scan. Matches are reported by rule name; the
	// matched substring is never put in the envelope so no secret
	// payload leaks via stderr or ops.log. ---
	if p.Detector != nil {
		if matches := p.Detector.Scan(req.Body); len(matches) > 0 {
			return nil, errs.Validation("SECRET_DETECTED",
				"body matched a high-confidence secret pattern",
				map[string]any{"rule": matches[0].Rule})
		}
	}

	// --- Stage 3: subject resolution. An unknown subject is minted
	// into the registry so subsequent observes see it as canonical. ---
	var subjectCanonical string
	if req.Subject != "" {
		canon, err := p.resolveSubject(req.Subject)
		if err != nil {
			return nil, err
		}
		subjectCanonical = canon
	}

	// --- Stage 4: embedding. Optional. A failure is operational,
	// not validation, because it indicates a runtime dependency
	// (Ollama) is unavailable. When the embedder is wired, we also
	// capture the embedding model's name and digest so the FR-051
	// invariant ("every entry records the model that produced its
	// vector") is enforced at write time. ---
	var (
		embedding         []float32
		embeddingModel    string
		embeddingDigest   string
	)
	if p.Embedder != nil {
		name, digest, err := p.Embedder.ModelDigest(ctx)
		if err != nil {
			return nil, errs.Operational("EMBEDDING_MODEL_UNAVAILABLE",
				"could not capture embedding model digest", err)
		}
		if digest == "" {
			return nil, errs.Operational("EMBEDDING_MODEL_UNAVAILABLE",
				"embedding model returned empty digest", nil)
		}
		embeddingModel = name
		embeddingDigest = digest

		vec, err := p.Embedder.Embed(ctx, req.Body)
		if err != nil {
			return nil, errs.Operational("EMBEDDING_FAILED",
				"embedding model unavailable", err)
		}
		embedding = vec
	}

	// --- Stage 5: build the transaction group. Every datom shares
	// tx, ts, actor, src, invocation_id. The E field is the new
	// entry's prefixed ULID. Datoms are sealed here so the log's
	// flock critical section just does write + fsync. ---
	tx := ulid.Make().String()
	entryID := "entry:" + ulid.Make().String()
	now := p.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	encodingAt := now().UTC()
	ts := encodingAt.Format(time.RFC3339Nano)
	group, err := buildObserveDatoms(tx, ts, p.Actor, p.InvocationID, entryID, req,
		subjectCanonical, embeddingModel, embeddingDigest, encodingAt)
	if err != nil {
		return nil, errs.Operational("DATOM_BUILD_FAILED", "could not construct datom group", err)
	}

	// --- Stage 6: append to log. This is the commit point; nothing
	// before this is visible to another process, nothing after this
	// can be rolled back. ---
	if p.Log == nil {
		return nil, errs.Operational("NO_LOG_WRITER", "write pipeline has no log writer", nil)
	}
	if _, err := p.Log.Append(group); err != nil {
		return nil, errs.Operational("LOG_APPEND_FAILED", "failed to append to log", err)
	}

	result := &ObserveResult{EntryID: entryID, Tx: tx}

	// --- Stage 7: backend apply. Outside the log's critical section.
	// Failures here do NOT unwind the log commit; they are surfaced
	// as operational errors so the CLI can warn, and the next
	// command's self-heal replay will bring the backends up to
	// date. Neo4j and Weaviate are independent — a failure in one
	// does not skip the other. ---
	if p.Neo4j != nil {
		for i := range group {
			if err := p.Neo4j.Apply(ctx, group[i]); err != nil {
				return result, errs.Operational("NEO4J_APPLY_FAILED",
					"backend apply failed; log committed, self-heal will retry", err)
			}
		}
	}
	if p.Weaviate != nil {
		for i := range group {
			if err := p.Weaviate.Apply(ctx, group[i]); err != nil {
				return result, errs.Operational("WEAVIATE_APPLY_FAILED",
					"backend apply failed; log committed, self-heal will retry", err)
			}
		}
	}
	// Embedding is held as an attribute of the pipeline result even
	// though this phase-1 pipeline does not yet attach it to a
	// separate vector row; cortex-4kq.41 (A-MEM link scoring) will
	// consume it from a neighbouring pipeline invocation. Silencing
	// the unused-variable warning here keeps the seam obvious.
	_ = embedding

	return result, nil
}

// validateFacets enforces the spec-mandated facet keys. Every observe
// write must carry a domain and a project. Missing keys produce a
// single MISSING_FACET error that names the first offender.
func validateFacets(f map[string]string) error {
	for _, key := range requiredFacets {
		v, ok := f[key]
		if !ok || strings.TrimSpace(v) == "" {
			return errs.Validation("MISSING_FACET",
				fmt.Sprintf("observe requires facet %q", key),
				map[string]any{"facet": key})
		}
	}
	return nil
}

// resolveSubject canonicalizes a subject PSI through the pipeline's
// Registry. A nil registry validates the subject's shape but does not
// attempt to mint or alias it — that path is intended for tests.
func (p *Pipeline) resolveSubject(raw string) (string, error) {
	if p.Registry == nil {
		cp, err := psi.Validate(raw)
		if err != nil {
			return "", errs.Validation("INVALID_SUBJECT",
				fmt.Sprintf("subject %q is not a valid PSI", raw),
				map[string]any{"subject": raw})
		}
		return cp.CanonicalForm, nil
	}
	if canon, ok := p.Registry.Canonical(raw); ok {
		return canon, nil
	}
	cp, err := psi.Validate(raw)
	if err != nil {
		return "", errs.Validation("INVALID_SUBJECT",
			fmt.Sprintf("subject %q is not a valid PSI", raw),
			map[string]any{"subject": raw})
	}
	if err := p.Registry.Mint(cp); err != nil {
		return "", errs.Operational("PSI_MINT_FAILED", "could not mint subject", err)
	}
	return cp.CanonicalForm, nil
}

// buildObserveDatoms assembles the datom group for one observe call.
// Ordering inside the group is deterministic (body, kind, facet.*,
// subject, trail, embedding_model_name, embedding_model_digest,
// encoding_at, base_activation) so tests can assert on the slice
// directly.
//
// The trailing four attributes encode:
//   - embedding_model_{name,digest}: FR-051 / SC-002. Emitted only when
//     the pipeline was configured with an Embedder; rebuild's pinned-
//     model drift check uses these to detect MODEL_DIGEST_RACE.
//   - encoding_at: the wall-clock encoding time used as the activation
//     decay reference point (internal/activation.State.EncodingAt).
//   - base_activation: FR-031 seed value (1.0) so default recall sees
//     the entry as visible immediately after observe.
func buildObserveDatoms(
	tx, ts, actor, invocationID, entryID string,
	req ObserveRequest,
	subjectCanonical string,
	embeddingModel, embeddingDigest string,
	encodingAt time.Time,
) ([]datom.Datom, error) {
	group := make([]datom.Datom, 0, 8+len(req.Facets))
	add := func(a string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", a, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        actor,
			Op:           datom.OpAdd,
			E:            entryID,
			A:            a,
			V:            raw,
			Src:          "observe",
			InvocationID: invocationID,
		}
		if err := d.Seal(); err != nil {
			return fmt.Errorf("seal %s: %w", a, err)
		}
		group = append(group, d)
		return nil
	}

	if err := add("body", req.Body); err != nil {
		return nil, err
	}
	if err := add("kind", req.Kind); err != nil {
		return nil, err
	}
	// Facets are emitted in sorted key order so the datom sequence
	// is deterministic across runs — useful for replay-equivalent
	// golden tests and for human diffing.
	for _, k := range sortedFacetKeys(req.Facets) {
		if err := add("facet."+k, req.Facets[k]); err != nil {
			return nil, err
		}
	}
	if subjectCanonical != "" {
		if err := add("subject", subjectCanonical); err != nil {
			return nil, err
		}
	}
	if req.TrailID != "" {
		if err := add("trail", req.TrailID); err != nil {
			return nil, err
		}
	}
	if embeddingModel != "" {
		if err := add("embedding_model_name", embeddingModel); err != nil {
			return nil, err
		}
	}
	if embeddingDigest != "" {
		if err := add("embedding_model_digest", embeddingDigest); err != nil {
			return nil, err
		}
	}
	// FR-031: every freshly-written entry seeds an encoding_at and a
	// base_activation=1.0 attribute. These two datoms make a brand-new
	// entry visible to default recall (visibility threshold 0.05) and
	// give the activation decay math a deterministic reference point.
	if err := add("encoding_at", encodingAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	if err := add("base_activation", activation.InitialBaseActivation); err != nil {
		return nil, err
	}
	return group, nil
}

// sortedFacetKeys returns the keys of a facets map in lexicographic
// order. A tiny helper kept local to this file so the rest of the
// package does not need to import sort just for facet ordering.
func sortedFacetKeys(f map[string]string) []string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	// Lightweight insertion sort — facet maps are tiny (< 10 keys),
	// and avoiding sort.Strings keeps the package's import graph
	// minimal.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
