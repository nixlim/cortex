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

// VectorApplier is an optional capability a BackendApplier may also
// implement when it can persist a fresh embedding vector alongside the
// usual property bag (Weaviate's BackendApplier does this via
// ApplyWithVector). The pipeline type-asserts on this interface for
// the entry-body datom in Stage 7 so the just-embedded float32 slice
// reaches Weaviate without forcing self-heal (which has no vector
// available) to grow a new method on the shared BackendApplier
// interface. Adapters that do not need the vector simply ignore it by
// not implementing this interface; the pipeline silently falls back to
// plain Apply for them.
type VectorApplier interface {
	ApplyWithVector(ctx context.Context, d datom.Datom, vector []float32) error
}

// NeighborFinder is the narrow interface the pipeline uses to pull
// candidate neighbors for A-MEM link derivation. Production wraps a
// Weaviate NearestNeighbors call against the just-embedded body
// vector; tests supply a fake. The candidates returned must already
// carry the cortex_id and cosine similarity in the shape DeriveLinks
// expects.
type NeighborFinder interface {
	Neighbors(ctx context.Context, vector []float32, k int) ([]LinkCandidate, error)
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

	// Neighbors is the optional nearest-neighbor source used during
	// A-MEM link derivation (FR-011 / cortex-4kq.41). When nil, the
	// pipeline skips the link derivation pass entirely. Production
	// wires this to a Weaviate-backed adapter; tests can drop in a
	// fake.
	Neighbors NeighborFinder

	// LinkProposer is the LLM-backed link scorer. When nil, the
	// pipeline skips link derivation regardless of Neighbors. The
	// proposer's Propose method is called outside the log flock so
	// AC4 ("link derivation runs as a separate transaction group")
	// holds by construction.
	LinkProposer LinkProposer

	// LinkConfig holds the confidence and cosine floors enforced by
	// DeriveLinks. The zero value is intentionally strict (nothing
	// passes) so a caller that forgets to populate it emits zero
	// links rather than flooding the graph.
	LinkConfig LinkDerivationConfig

	// ExpectedEmbeddingDim is the Weaviate class's declared vector
	// dimension. When > 0, the pipeline asserts that every vector the
	// Embedder returns has exactly this length before handing it to
	// the backend apply phase. A mismatch is reported as
	// EMBEDDING_DIM_MISMATCH — an operational error that the CLI surfaces
	// with a clear remediation ("embedder model was changed without
	// rebuild") rather than letting the failure surface inside the
	// Weaviate HTTP layer with a generic schema error. Zero disables
	// the check, which is the right default for tests that wire a fake
	// embedder. See cortex-06p.
	ExpectedEmbeddingDim int

	// LinkTopK is the number of nearest neighbors to ask the
	// NeighborFinder for. A zero or negative value disables link
	// derivation. Production callers pass 5 per the A-MEM defaults.
	LinkTopK int

	// ConceptsEnabled toggles the post-commit concept extraction pass
	// that populates :Concept nodes and :MENTIONS edges in Neo4j so
	// the recall pipeline's Stage 2 seed resolver has something to
	// match against. When false, the pass is skipped — which is the
	// right default for unit tests that don't wire a Neo4j applier.
	// Production callers set it to true; the extractor itself is the
	// lexical tokenizer in concepts.go, so no adapter plumbing is
	// needed. Phase 1 architectural follow-up to cortex-jw6.
	ConceptsEnabled bool

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

	// InitialBaseActivation overrides the seeded base_activation for
	// this entry. Zero means "use activation.InitialBaseActivation"
	// (1.0) — the FR-031 default that makes ingest and replay-path
	// writes immediately visible. Non-zero is used by the cortex
	// observe CLI to seed session observations at a lower value
	// (e.g. 0.3) so they do not compete with ingest content at
	// rank-1. See bead cortex-7y4 / CORTEX_EVALUATION_2026-04-13.md
	// R3. Must be in (0, 1]; out-of-range values are treated as
	// zero and fall back to the default.
	InitialBaseActivation float64
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
		// cortex-06p: validate vector dimension at the pipeline layer
		// rather than letting a re-configured embedder surface a less
		// actionable error inside Weaviate's HTTP response. A zero
		// ExpectedEmbeddingDim disables the check (tests / bootstrap).
		if p.ExpectedEmbeddingDim > 0 && len(vec) != p.ExpectedEmbeddingDim {
			return nil, errs.Operational("EMBEDDING_DIM_MISMATCH",
				fmt.Sprintf(
					"embedder returned %d-dim vector; Weaviate class expects %d. "+
						"The embedding model was likely changed without `cortex rebuild`.",
					len(vec), p.ExpectedEmbeddingDim),
				nil)
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
	// Failures here do NOT unwind the log commit and they MUST NOT
	// short-circuit the rest of the apply pass: a transient hiccup
	// on datom 3 of 9 in the Neo4j loop must still let datoms 4..9
	// land, must still let the Weaviate loop run, and must still let
	// Stage 8 (link derivation) execute. The log is the source of
	// truth, the next command's self-heal replay will bring any
	// dropped backend writes back into line (FR-004), and rendering
	// a hard error here for one bad datom would silently corrupt the
	// observe outcome (log committed but partial backend state
	// invisible to the user). Errors are collected and returned as a
	// single best-effort report at the end of the function. Neo4j
	// and Weaviate are independent — a failure in one does not skip
	// the other. ---
	var applyErrs []error
	if p.Neo4j != nil {
		for i := range group {
			if err := p.Neo4j.Apply(ctx, group[i]); err != nil {
				applyErrs = append(applyErrs, fmt.Errorf("neo4j apply %s: %w", group[i].A, err))
			}
		}
	}
	if p.Weaviate != nil {
		// If the Weaviate applier supports the optional VectorApplier
		// capability AND the pipeline has a fresh embedding to hand
		// over, route the entry-body datom through ApplyWithVector so
		// the float32 vector reaches Weaviate as part of the same
		// Upsert that lands the body. Self-heal still uses plain
		// Apply (it has no vector), so the per-entity scratch bag the
		// applier maintains converges on the same property set.
		vecApplier, vectorCapable := p.Weaviate.(VectorApplier)
		for i := range group {
			d := group[i]
			var err error
			if vectorCapable && len(embedding) > 0 && d.A == "body" {
				err = vecApplier.ApplyWithVector(ctx, d, embedding)
			} else {
				err = p.Weaviate.Apply(ctx, d)
			}
			if err != nil {
				applyErrs = append(applyErrs, fmt.Errorf("weaviate apply %s: %w", d.A, err))
			}
		}
	}
	// --- Stage 8: A-MEM link derivation (FR-011 / cortex-4kq.41).
	// This stage runs ONLY after the main append flock has been
	// released (Stage 6) and after the backend apply phase, so the
	// links.go AC4 invariant ("link derivation executes outside the
	// log flock") holds by construction. Every failure mode here is
	// swallowed: a missing neighbor source, an embedding-less write,
	// a Weaviate hiccup, an unparseable LLM response, or even a
	// failed second log append must NOT roll back the source entry
	// the user already saw committed.
	if p.LinkProposer != nil && p.Neighbors != nil && p.LinkTopK > 0 && len(embedding) > 0 {
		p.deriveAndAppendLinks(ctx, entryID, req.Body, embedding, now)
	}

	// --- Stage 8b: concept extraction + mentions-edge materialization.
	// This pass is the architectural counterpart to link derivation:
	// it populates :Concept nodes and (:Entry)-[:MENTIONS]->(:Concept)
	// edges in Neo4j so the recall pipeline's Stage 2 seed resolver
	// has something to match against. Without it, recall returns
	// zero hits because the semantic graph has no entry points. The
	// extractor is the lexical tokenizer in concepts.go — no LLM
	// call, no inline latency, deterministic across observe/recall.
	// Failures are swallowed: like link derivation, concept writing
	// is best-effort and must never roll back the source entry.
	if p.ConceptsEnabled && p.Neo4j != nil {
		p.deriveAndAppendConcepts(ctx, entryID, req.Body, now)
	}

	// If any backend apply hiccupped during Stage 7 we surface it
	// here so the CLI can warn the operator (and ops.log can record
	// the drift), but the result pointer is non-nil so the caller
	// still prints the entry id — the log commit is authoritative.
	// FR-004 self-heal will replay any dropped rows on the next
	// command. The first error is wrapped in the envelope; remaining
	// errors are joined into the message so a single ops.log line
	// captures the whole batch.
	if len(applyErrs) > 0 {
		return result, errs.Operational("BACKEND_APPLY_PARTIAL",
			fmt.Sprintf("backend apply reported %d failure(s); log committed, self-heal will retry: %v",
				len(applyErrs), joinErrors(applyErrs)),
			applyErrs[0])
	}

	return result, nil
}

// joinErrors stringifies a slice of errors into a single semicolon-
// separated message. errors.Join would be cleaner but it stores the
// underlying errors flat and the message format is too noisy for an
// operator-facing CLI envelope; this helper keeps the rendered text
// short.
func joinErrors(errs []error) string {
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "; ")
}

// deriveAndAppendLinks runs the post-commit A-MEM scoring pass and
// appends accepted link datoms as a SEPARATE transaction group. All
// errors are swallowed: link derivation is best-effort and must never
// affect the source entry's committed state. The function still takes
// p.Log for the second Append, but the call happens after Stage 6 has
// returned, so the flock has already been released.
func (p *Pipeline) deriveAndAppendLinks(ctx context.Context, entryID, sourceBody string, embedding []float32, now func() time.Time) {
	candidates, err := p.Neighbors.Neighbors(ctx, embedding, p.LinkTopK)
	if err != nil || len(candidates) == 0 {
		return
	}
	// Filter the just-written entry out of its own neighbor set —
	// Weaviate will happily return it as the top hit otherwise.
	filtered := candidates[:0]
	for _, c := range candidates {
		if c.TargetEntryID == "" || c.TargetEntryID == entryID {
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return
	}
	accepted := DeriveLinks(ctx, p.LinkProposer, sourceBody, filtered, p.LinkConfig)
	if len(accepted) == 0 {
		return
	}
	linkTx := ulid.Make().String()
	linkTs := now().UTC().Format(time.RFC3339Nano)
	linkGroup, err := BuildLinkDatoms(entryID, linkTx, linkTs, p.Actor, p.InvocationID, accepted)
	if err != nil || len(linkGroup) == 0 {
		return
	}
	if p.Log == nil {
		return
	}
	_, _ = p.Log.Append(linkGroup)
}

// deriveAndAppendConcepts runs lexical concept extraction on the
// observation body, builds a mentions_concept edge datom per unique
// token, appends the new datoms to the log as a separate transaction
// group (so the flock contract is preserved), and applies them to
// the Neo4j backend inline. Inline application is necessary because
// the very next command — typically `cortex recall` — needs to find
// :Concept nodes in the graph immediately; self-heal cannot help the
// first observe→recall round trip because self-heal runs at command
// start, not at command end.
//
// All errors are swallowed. Concept extraction is a post-commit
// augmentation and must never roll back the source entry the user
// already saw committed.
func (p *Pipeline) deriveAndAppendConcepts(ctx context.Context, entryID, body string, now func() time.Time) {
	tokens := ExtractConceptTokens(body)
	if len(tokens) == 0 {
		return
	}
	conceptTx := ulid.Make().String()
	conceptTs := now().UTC().Format(time.RFC3339Nano)

	group := make([]datom.Datom, 0, len(tokens))
	for _, tok := range tokens {
		targetID := ConceptEntityID(tok)
		raw, err := json.Marshal(targetID)
		if err != nil {
			continue
		}
		d := datom.Datom{
			Tx:           conceptTx,
			Ts:           conceptTs,
			Actor:        p.Actor,
			Op:           datom.OpAdd,
			E:            entryID,
			A:            "mentions_concept",
			V:            raw,
			Src:          "observe",
			InvocationID: p.InvocationID,
		}
		if err := d.Seal(); err != nil {
			continue
		}
		group = append(group, d)
	}
	if len(group) == 0 {
		return
	}
	if p.Log != nil {
		_, _ = p.Log.Append(group)
	}
	// Apply inline so the first observe→recall round trip works
	// without waiting for self-heal on the next command.
	for i := range group {
		_ = p.Neo4j.Apply(ctx, group[i])
	}
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
	// base_activation attribute. These two datoms make a brand-new
	// entry visible to default recall and give the activation decay
	// math a deterministic reference point. The default seed is
	// activation.InitialBaseActivation (1.0); req.InitialBaseActivation
	// lets callers (e.g. cortex observe for session observations) seed
	// a lower value so they do not compete with ingest content at
	// rank-1. See bead cortex-7y4 / CORTEX_EVALUATION_2026-04-13.md R3.
	if err := add("encoding_at", encodingAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	seed := activation.InitialBaseActivation
	if req.InitialBaseActivation > 0 && req.InitialBaseActivation <= 1 {
		seed = req.InitialBaseActivation
	}
	if err := add("base_activation", seed); err != nil {
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
