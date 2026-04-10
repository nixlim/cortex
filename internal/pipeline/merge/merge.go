// Package merge implements the cortex subject merge write path
// described in docs/spec/cortex-spec.md §"Subject merge writes
// canonical and alias facts" and the bead acceptance criteria for
// cortex-4kq.45.
//
// A subject merge is structurally similar to retract: it is purely
// additive on the append-only log. Given two valid PSIs psi-a and
// psi-b that refer to the same subject, the pipeline emits OpAdd
// datoms recording (a) that psi-b is an alias of psi-a, (b) that
// psi-a is the canonical id, (c) the operator identity, and (d) one
// contradiction edge per facet key where both subjects asserted
// different values. No prior datom is ever mutated or deleted, so
// every original assertion on either subject remains visible to
// cortex history and cortex as-of, while default retrieval can use
// the alias edge to follow psi-b to psi-a.
//
// Contradiction detection is delegated to an injected
// SubjectFacetReader so this package stays decoupled from
// internal/neo4j and the log replay machinery. The CLI command
// (cmd/cortex/subject_merge.go, ops-dev's wiring) constructs a
// Pipeline with a reader that consults whichever index it prefers;
// tests inject deterministic in-memory fakes.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Subject merge writes canonical and alias facts"
//	docs/spec/cortex-spec.md FR-029 (PSI governance: accretive merge)
//	docs/spec/cortex-spec.md §"Operator audit identity"
package merge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/psi"
)

// Default attribute names used by the merge path. Defined as constants
// so the recall and history layers can match against the same strings
// without copy-paste drift.
const (
	// AttrAliasOf is set on the alias-side subject (psi-b). Its value
	// is the canonical PSI (psi-a). Recall's "follow alias" lookup
	// reads this attribute.
	AttrAliasOf = "subject.alias_of"

	// AttrCanonical is set on the alias-side subject as a redundant
	// pointer to the canonical PSI. Distinct from AttrAliasOf because
	// downstream consumers may want a single direct lookup.
	AttrCanonical = "subject.canonical"

	// AttrMergeActor records the operator identity for the merge.
	// Required by the spec's "Operator audit identity" section
	// (FR: subject merge records invoking identity).
	AttrMergeActor = "subject.merge.actor"

	// AttrContradictionKey is set on a contradiction-edge entity. Its
	// value is the facet key (e.g., "facet.project") where the two
	// subjects disagreed.
	AttrContradictionKey = "contradiction.key"

	// AttrContradictionA / AttrContradictionB carry the two divergent
	// values, tagged by source so a human reviewing cortex history can
	// see which subject asserted which claim.
	AttrContradictionA = "contradiction.value_a"
	AttrContradictionB = "contradiction.value_b"

	// AttrContradictionSourceA / AttrContradictionSourceB record the
	// PSI each value came from. Recorded as separate datoms so the
	// contradiction-edge entity is fully self-describing.
	AttrContradictionSourceA = "contradiction.source_a"
	AttrContradictionSourceB = "contradiction.source_b"

	// SrcMerge is the Datom.Src tag every merge-path datom carries,
	// parallel to "observe", "reflect", "retract".
	SrcMerge = "merge"

	// ContradictionPrefix is the entity-id prefix used for the
	// synthetic contradiction-edge entities the merge emits. Each
	// contradiction gets its own ULID under this prefix so cortex
	// history can render them as first-class events.
	ContradictionPrefix = "contradiction:"
)

// LogAppender is the narrow log surface this package needs. It
// matches internal/log.Writer.Append byte-for-byte but is declared
// locally so the package can be unit-tested with an in-memory fake.
type LogAppender interface {
	Append(group []datom.Datom) (tx string, err error)
}

// SubjectFacetReader returns the facet claims currently associated
// with a subject PSI. The map is key → value (e.g., "project" →
// "cortex"). A nil reader is allowed: in that case the merge skips
// contradiction detection entirely and emits only the alias datoms.
//
// The CLI implementation queries Neo4j or replays the log; tests
// inject in-memory fakes.
type SubjectFacetReader interface {
	SubjectFacets(ctx context.Context, psi string) (map[string]string, error)
}

// SubjectFacetReaderFunc adapts a bare function to SubjectFacetReader.
type SubjectFacetReaderFunc func(ctx context.Context, psi string) (map[string]string, error)

// SubjectFacets calls f.
func (f SubjectFacetReaderFunc) SubjectFacets(ctx context.Context, psi string) (map[string]string, error) {
	return f(ctx, psi)
}

// Errors surfaced by Pipeline.Merge.
var (
	// ErrEmptyPSI is returned when either side of the merge is empty.
	ErrEmptyPSI = errors.New("merge: psi is empty")

	// ErrSamePSI is returned when psi-a equals psi-b. Merging a
	// subject with itself is meaningless and almost certainly an
	// operator error worth surfacing.
	ErrSamePSI = errors.New("merge: psi-a and psi-b are identical")

	// ErrEmptyActor is returned when the pipeline has no actor
	// configured. Audit datoms must always carry an identity.
	ErrEmptyActor = errors.New("merge: actor is empty")

	// ErrNoLog is returned when the pipeline has no LogAppender.
	ErrNoLog = errors.New("merge: pipeline has no log appender")

	// ErrInvalidPSI wraps a psi.Validate failure with the side that
	// failed (a or b) so the operator gets a clear error.
	ErrInvalidPSI = errors.New("merge: invalid psi")
)

// Pipeline is the subject merge write path. Construct one per command
// invocation. Zero values are not usable for real writes (Log must be
// set; Actor and InvocationID should be set by the CLI before calling
// Merge).
type Pipeline struct {
	// Log is the segment appender. Required.
	Log LogAppender

	// Reader supplies the facet claims for each subject so the
	// pipeline can detect contradictions before writing the merge.
	// Optional: nil disables contradiction detection.
	Reader SubjectFacetReader

	// Now returns the wall-clock timestamp recorded in every emitted
	// datom. Tests pin it for determinism. Production callers pass
	// func() time.Time { return time.Now().UTC() }.
	Now func() time.Time

	// Actor is recorded in every datom's Actor field AND in the
	// subject.merge.actor audit datom. The CLI fills this from
	// CORTEX_AGENT or USER (see spec FR audit identity).
	Actor string

	// InvocationID is the per-command ULID shared by every datom and
	// ops.log entry in this invocation.
	InvocationID string
}

// Request is the normalized cortex subject merge input after flag
// parsing. PsiA is the canonical side; PsiB is recorded as an alias.
type Request struct {
	PsiA string
	PsiB string
}

// Result is the returned summary. Tx is the single tx ULID assigned
// to the merge group. ContradictionCount is the number of synthetic
// contradiction-edge entities emitted (one per disagreeing facet
// key); zero means the two subjects' facet claims were compatible.
type Result struct {
	Tx                 string
	ContradictionCount int
	// Canonical and Alias echo back the validated canonical PSI
	// strings so the CLI can print them in the success message
	// without re-running psi.Validate.
	Canonical string
	Alias     string
}

// Merge executes the cortex subject merge command for one request.
// The returned Result names the tx and the number of contradictions
// emitted alongside the alias datoms.
//
// Atomicity note: every datom in a single merge — alias edges, audit,
// and every contradiction edge — lives in one transaction group and
// hits the log in a single Append call, so a partial merge is
// structurally impossible. If the contradiction reader fails, the
// merge aborts BEFORE any datom is emitted, so the operator can
// retry without leaving the log in a half-merged state.
func (p *Pipeline) Merge(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.PsiA) == "" || strings.TrimSpace(req.PsiB) == "" {
		return nil, ErrEmptyPSI
	}
	if req.PsiA == req.PsiB {
		return nil, ErrSamePSI
	}
	if p.Log == nil {
		return nil, ErrNoLog
	}
	if strings.TrimSpace(p.Actor) == "" {
		return nil, ErrEmptyActor
	}

	// --- Stage 1: validate both PSIs through the same governor that
	// observe and ingest use. A bad PSI is a validation error, not an
	// operational one — the operator typo'd the argument.
	canonA, err := psi.Validate(req.PsiA)
	if err != nil {
		return nil, fmt.Errorf("%w (psi-a): %v", ErrInvalidPSI, err)
	}
	canonB, err := psi.Validate(req.PsiB)
	if err != nil {
		return nil, fmt.Errorf("%w (psi-b): %v", ErrInvalidPSI, err)
	}

	// --- Stage 2: gather contradictions BEFORE any write. If the
	// reader fails, no datoms have been emitted and the operator can
	// retry without churn on the log.
	var contradictions []contradiction
	if p.Reader != nil {
		facetsA, err := p.Reader.SubjectFacets(ctx, canonA.CanonicalForm)
		if err != nil {
			return nil, fmt.Errorf("merge: read facets of %s: %w", canonA.CanonicalForm, err)
		}
		facetsB, err := p.Reader.SubjectFacets(ctx, canonB.CanonicalForm)
		if err != nil {
			return nil, fmt.Errorf("merge: read facets of %s: %w", canonB.CanonicalForm, err)
		}
		contradictions = diffFacets(canonA.CanonicalForm, canonB.CanonicalForm, facetsA, facetsB)
	}

	// --- Stage 3: build the single transaction group and append.
	now := p.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	tx := ulid.Make().String()
	ts := now().UTC().Format(time.RFC3339Nano)

	group, err := p.buildMergeGroup(tx, ts, canonA.CanonicalForm, canonB.CanonicalForm, contradictions)
	if err != nil {
		return nil, fmt.Errorf("merge: build group: %w", err)
	}
	committedTx, err := p.Log.Append(group)
	if err != nil {
		return nil, fmt.Errorf("merge: append %s: %w", canonB.CanonicalForm, err)
	}
	if committedTx != tx {
		return nil, fmt.Errorf("merge: log returned tx %s, expected %s", committedTx, tx)
	}

	return &Result{
		Tx:                 tx,
		ContradictionCount: len(contradictions),
		Canonical:          canonA.CanonicalForm,
		Alias:              canonB.CanonicalForm,
	}, nil
}

// contradiction is a single facet key on which the two subjects
// disagreed. Recorded with both values and the source PSI of each so
// the emitted contradiction-edge entity is fully self-describing.
type contradiction struct {
	Key     string
	ValueA  string
	ValueB  string
	SourceA string
	SourceB string
}

// diffFacets returns the set of facet keys where a and b both have a
// claim but the claims differ. Keys present in only one side are not
// contradictions — the merge simply unifies them. The output is
// sorted by key so the emitted datoms are deterministic across runs.
func diffFacets(sourceA, sourceB string, a, b map[string]string) []contradiction {
	keys := make([]string, 0)
	for k, va := range a {
		vb, ok := b[k]
		if !ok || va == vb {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]contradiction, 0, len(keys))
	for _, k := range keys {
		out = append(out, contradiction{
			Key:     k,
			ValueA:  a[k],
			ValueB:  b[k],
			SourceA: sourceA,
			SourceB: sourceB,
		})
	}
	return out
}

// buildMergeGroup constructs the per-merge datom group. Every group
// contains, in this order:
//
//  1. subject.alias_of on psi-b → psi-a
//  2. subject.canonical on psi-b → psi-a
//  3. subject.merge.actor on psi-b → actor
//  4. for each contradiction:
//     contradiction.key, contradiction.value_a, contradiction.value_b,
//     contradiction.source_a, contradiction.source_b — all on a fresh
//     contradiction:<ulid> entity.
//
// The contradiction edges live on their own entities (rather than
// being attached to either subject) so cortex history can render them
// as first-class events without polluting the subject's lineage.
func (p *Pipeline) buildMergeGroup(tx, ts, canonA, canonB string, contradictions []contradiction) ([]datom.Datom, error) {
	group := make([]datom.Datom, 0, 3+5*len(contradictions))

	add := func(entity string, op datom.Op, attr string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", attr, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        p.Actor,
			Op:           op,
			E:            entity,
			A:            attr,
			V:            raw,
			Src:          SrcMerge,
			InvocationID: p.InvocationID,
		}
		if err := d.Seal(); err != nil {
			return fmt.Errorf("seal %s: %w", attr, err)
		}
		group = append(group, d)
		return nil
	}

	// 1-3: alias + canonical pointer + actor on the alias-side subject.
	if err := add(canonB, datom.OpAdd, AttrAliasOf, canonA); err != nil {
		return nil, err
	}
	if err := add(canonB, datom.OpAdd, AttrCanonical, canonA); err != nil {
		return nil, err
	}
	if err := add(canonB, datom.OpAdd, AttrMergeActor, p.Actor); err != nil {
		return nil, err
	}

	// 4: contradiction edges, one synthetic entity per disagreement.
	for _, c := range contradictions {
		entity := ContradictionPrefix + ulid.Make().String()
		if err := add(entity, datom.OpAdd, AttrContradictionKey, c.Key); err != nil {
			return nil, err
		}
		if err := add(entity, datom.OpAdd, AttrContradictionA, c.ValueA); err != nil {
			return nil, err
		}
		if err := add(entity, datom.OpAdd, AttrContradictionB, c.ValueB); err != nil {
			return nil, err
		}
		if err := add(entity, datom.OpAdd, AttrContradictionSourceA, c.SourceA); err != nil {
			return nil, err
		}
		if err := add(entity, datom.OpAdd, AttrContradictionSourceB, c.SourceB); err != nil {
			return nil, err
		}
	}

	return group, nil
}
