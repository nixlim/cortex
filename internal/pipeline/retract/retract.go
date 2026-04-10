// Package retract implements the cortex retract write path described in
// docs/spec/cortex-spec.md §"Retraction" and the bead's acceptance
// criteria for cortex-4kq.43.
//
// A retraction is structurally different from an observation: it
// emits an OpRetract datom whose role is the tombstone marker, plus
// one or more OpAdd audit datoms that record the operator identity
// and the human reason. The log is append-only — no prior datom is
// ever mutated or deleted — so the retraction event becomes part of
// the entity's lineage and remains visible to cortex history and
// cortex as-of, while default retrieval skips any entity that carries
// a retract.exists tombstone.
//
// Cascade is implemented via an injected ChildResolver, so this
// package stays decoupled from internal/neo4j and can be unit-tested
// without standing up a graph backend. The CLI command (cmd/cortex/
// retract.go, ops-dev's wiring) constructs a Pipeline with a
// resolver that walks DERIVED_FROM children in the graph adapter.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Retraction" (cortex retract semantics)
//	docs/spec/cortex-spec.md §"Lineage Visibility" (history vs default
//	  recall behaviour)
package retract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// Default attribute names used by the retract path. Defined as
// constants so the recall layer's "hide retracted entities" filter
// (when it lands) can match against the same strings without copy-
// paste drift.
const (
	// AttrExists is the attribute the OpRetract datom retracts. The
	// implicit assertion is "this entity exists"; the retraction
	// removes it from the default-visibility set. Recall reads
	// AttrExists to decide whether to hide a candidate.
	AttrExists = "exists"

	// AttrRetractReason carries the operator-supplied reason. Empty
	// reasons are allowed (the field still appears, with an empty
	// value, so the audit trail is complete).
	AttrRetractReason = "retract.reason"

	// AttrRetractActor carries the operator identity. The CLI fills
	// this from $USER or an explicit --actor flag.
	AttrRetractActor = "retract.actor"

	// AttrRetractCascadeSource is set on cascaded children. Its value
	// is the entity id that started the cascade, so cortex history
	// can render the cascade as a single user action across many
	// entities.
	AttrRetractCascadeSource = "retract.cascade_source"

	// SrcRetract is the Datom.Src tag every retraction-path datom
	// carries, parallel to "observe" and "reflect".
	SrcRetract = "retract"
)

// LogAppender is the narrow log surface this package needs. It
// matches internal/log.Writer.Append byte-for-byte but is declared
// locally so the package can be unit-tested with an in-memory fake.
type LogAppender interface {
	Append(group []datom.Datom) (tx string, err error)
}

// ChildResolver returns the entity ids that depend on parentID and
// must be cascaded when --cascade is set. The CLI implementation
// walks DERIVED_FROM edges in Neo4j; tests inject deterministic fakes.
//
// A nil resolver is allowed: cascade requests with a nil resolver
// return ErrNoResolver so the operator gets a clear error rather than
// a silent partial retraction.
type ChildResolver interface {
	Children(ctx context.Context, parentID string) ([]string, error)
}

// ChildResolverFunc adapts a bare function to ChildResolver.
type ChildResolverFunc func(ctx context.Context, parentID string) ([]string, error)

// Children calls f.
func (f ChildResolverFunc) Children(ctx context.Context, parentID string) ([]string, error) {
	return f(ctx, parentID)
}

// Errors surfaced by Pipeline.Retract.
var (
	// ErrEmptyEntityID is returned when the request carries no
	// entity id. The CLI catches this at flag-parse time, but the
	// pipeline guards against direct callers as well.
	ErrEmptyEntityID = errors.New("retract: entity id is empty")

	// ErrEmptyActor is returned when the pipeline has no actor
	// configured. Audit datoms must always carry an identity.
	ErrEmptyActor = errors.New("retract: actor is empty")

	// ErrNoResolver is returned when --cascade is requested but the
	// pipeline has no ChildResolver wired.
	ErrNoResolver = errors.New("retract: cascade requested but no child resolver configured")

	// ErrNoLog is returned when the pipeline has no LogAppender.
	ErrNoLog = errors.New("retract: pipeline has no log appender")
)

// Pipeline is the retract write path. Construct one per command
// invocation. Zero values are not usable for real writes (Log must
// be set; Actor and InvocationID should be set by the CLI before
// calling Retract).
type Pipeline struct {
	// Log is the segment appender. Required.
	Log LogAppender

	// Resolver walks DERIVED_FROM children for cascade. Required only
	// when callers may pass Cascade=true; nil is fine for non-cascade
	// retractions.
	Resolver ChildResolver

	// Now returns the wall-clock timestamp recorded in every emitted
	// datom. Tests pin it for determinism. Production callers pass
	// func() time.Time { return time.Now().UTC() }.
	Now func() time.Time

	// Actor is recorded in every datom's Actor field AND in the
	// retract.actor audit datom. The CLI fills this from $USER (see
	// cmd/cortex/observe.go's defaultActor pattern) or an explicit
	// --actor flag.
	Actor string

	// InvocationID is the per-command ULID shared by every datom and
	// ops.log entry in this invocation. Generating it at the command
	// entry point keeps the ops.log correlation invariant easy.
	InvocationID string
}

// Request is the normalized cortex retract input after flag parsing.
type Request struct {
	// EntityID is the target the operator wants to retract. May be
	// any prefixed id: entry:<ulid>, frame:<ulid>, trail:<ulid>,
	// subject:<canonical>, community:<id>.
	EntityID string

	// Reason is the operator-supplied audit string. Optional.
	Reason string

	// Cascade enables walking DERIVED_FROM children. When true the
	// pipeline expects Resolver to be non-nil.
	Cascade bool
}

// Result is the returned summary. EntityIDs lists every id that
// received a retract group, in retraction order (target first, then
// any cascaded children). TxIDs is parallel to EntityIDs.
type Result struct {
	EntityIDs []string
	TxIDs     []string
}

// Retract executes the cortex retract command for one request. The
// returned Result lists every entity that was retracted (target plus
// cascade children) along with the tx ULID assigned to each one.
//
// Atomicity note: every retracted entity gets its own append (its own
// tx), in target-first order. A cascade resolver failure aborts the
// run BEFORE writing the target's retraction so a partial cascade
// never leaves the log in a half-retracted state. After the resolver
// returns, every Append failure also aborts the run, but earlier
// successful appends remain in the log because the log is append-
// only — that is the spec's "no rollback" guarantee.
func (p *Pipeline) Retract(ctx context.Context, req Request) (*Result, error) {
	if strings.TrimSpace(req.EntityID) == "" {
		return nil, ErrEmptyEntityID
	}
	if p.Log == nil {
		return nil, ErrNoLog
	}
	if strings.TrimSpace(p.Actor) == "" {
		return nil, ErrEmptyActor
	}

	// --- Stage 1: gather the cascade set BEFORE any write. If the
	// resolver fails, no datoms have been emitted and the operator
	// can retry without leaving a half-cascaded log.
	targets := []string{req.EntityID}
	if req.Cascade {
		if p.Resolver == nil {
			return nil, ErrNoResolver
		}
		children, err := p.Resolver.Children(ctx, req.EntityID)
		if err != nil {
			return nil, fmt.Errorf("retract: resolve children of %s: %w", req.EntityID, err)
		}
		// Children may legitimately be empty (a leaf frame); that
		// just means the cascade collapses to a single retraction.
		// We dedupe in case the resolver returns the parent in its
		// own child list (defensive — a buggy graph query could).
		seen := map[string]struct{}{req.EntityID: {}}
		for _, c := range children {
			if c == "" {
				continue
			}
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			targets = append(targets, c)
		}
	}

	// --- Stage 2: emit one retraction group per target. The first
	// group is the operator-named target; subsequent groups are
	// cascaded children, each carrying retract.cascade_source pointing
	// at the original target so cortex history can render the cascade
	// as one event.
	now := p.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	result := &Result{
		EntityIDs: make([]string, 0, len(targets)),
		TxIDs:     make([]string, 0, len(targets)),
	}
	for i, entityID := range targets {
		group, tx, err := p.buildRetractGroup(entityID, req, now(), i > 0)
		if err != nil {
			return result, fmt.Errorf("retract: build group for %s: %w", entityID, err)
		}
		committedTx, err := p.Log.Append(group)
		if err != nil {
			return result, fmt.Errorf("retract: append %s: %w", entityID, err)
		}
		if committedTx != tx {
			return result, fmt.Errorf("retract: log returned tx %s, expected %s", committedTx, tx)
		}
		result.EntityIDs = append(result.EntityIDs, entityID)
		result.TxIDs = append(result.TxIDs, tx)
	}
	return result, nil
}

// buildRetractGroup constructs the per-entity retraction datom group.
// Every group contains, in this order:
//
//  1. The OpRetract tombstone against (entity, AttrExists).
//  2. retract.reason audit datom (always emitted, even if reason is
//     empty, so the audit trail is byte-shape stable).
//  3. retract.actor audit datom.
//  4. retract.cascade_source audit datom (only when isCascadeChild
//     is true).
//
// The cascade-source datom is omitted on the operator-named target
// because that target IS the cascade source — emitting it on the
// target would make every retraction look like a cascaded child.
func (p *Pipeline) buildRetractGroup(entityID string, req Request, ts time.Time, isCascadeChild bool) ([]datom.Datom, string, error) {
	tx := ulid.Make().String()
	tsStr := ts.UTC().Format(time.RFC3339Nano)
	group := make([]datom.Datom, 0, 4)

	add := func(op datom.Op, attr string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", attr, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           tsStr,
			Actor:        p.Actor,
			Op:           op,
			E:            entityID,
			A:            attr,
			V:            raw,
			Src:          SrcRetract,
			InvocationID: p.InvocationID,
		}
		if err := d.Seal(); err != nil {
			return fmt.Errorf("seal %s: %w", attr, err)
		}
		group = append(group, d)
		return nil
	}

	// 1. The tombstone. Op=retract, value=null is the canonical "this
	// prior assertion no longer holds" pattern from the Datomic-style
	// log model. Default-recall callers scan for this datom and skip
	// the entity from their candidate set.
	if err := add(datom.OpRetract, AttrExists, nil); err != nil {
		return nil, "", err
	}

	// 2. Reason — always emitted so audit shape is stable.
	if err := add(datom.OpAdd, AttrRetractReason, req.Reason); err != nil {
		return nil, "", err
	}

	// 3. Actor — operator identity.
	if err := add(datom.OpAdd, AttrRetractActor, p.Actor); err != nil {
		return nil, "", err
	}

	// 4. Cascade source — only on cascaded children.
	if isCascadeChild {
		if err := add(datom.OpAdd, AttrRetractCascadeSource, req.EntityID); err != nil {
			return nil, "", err
		}
	}

	return group, tx, nil
}
