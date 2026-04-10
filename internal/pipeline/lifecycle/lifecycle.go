// Package lifecycle implements the cortex pin / unpin / evict /
// unevict write paths.
//
// All four commands are activation-state transitions that mutate a
// small set of LWW attributes on an existing entry. The package owns
// the rules for "which attributes change for which command", but it
// delegates two things:
//
//   - StateLoader reads the entry's current activation state. The CLI
//     wires this to the same Neo4j read path the recall pipeline uses;
//     tests inject a fake.
//   - LogAppender appends the produced datom group exactly the way
//     observe / retract / reflect do, so audit and replay invariants
//     are preserved.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-12 (pin/unpin/evict/unevict scenarios)
//	docs/spec/cortex-spec.md §"Boundary conditions" (visibility floor)
//	bead cortex-4kq.50
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// Attribute names emitted by the lifecycle pipeline. Defined as
// constants so the recall layer's "hide evicted" filter, the activation
// state loader, and any future history rendering can match the same
// strings without copy-paste drift.
const (
	AttrBaseActivation     = "base_activation"
	AttrPinned             = "pinned"
	AttrPinActivation      = "pin_activation"
	AttrEvictedAt          = "evicted_at"
	AttrEvictedAtRetracted = "evicted_at_retracted"

	// SrcLifecycle is the Datom.Src tag used by every lifecycle datom,
	// parallel to "observe", "retract", "reflect", "analyze", "recall".
	SrcLifecycle = "lifecycle"
)

// LogAppender is the narrow log surface this package needs.
type LogAppender interface {
	Append(group []datom.Datom) (tx string, err error)
}

// StateLoader returns the current persisted activation state of an
// entry. ok=false means the entry id is not known to the loader; the
// pipeline turns that into ENTRY_NOT_FOUND. The loader is read-only.
type StateLoader interface {
	Load(ctx context.Context, entityID string) (state activation.State, ok bool, err error)
}

// Errors surfaced by the lifecycle pipeline.
var (
	ErrEmptyEntityID = errors.New("lifecycle: entity id is empty")
	ErrEmptyActor    = errors.New("lifecycle: actor is empty")
	ErrNoLog         = errors.New("lifecycle: pipeline has no log appender")
	ErrNoLoader      = errors.New("lifecycle: pipeline has no state loader")
)

// Pipeline is the activation lifecycle write path. Construct one per
// command invocation.
type Pipeline struct {
	Log    LogAppender
	Loader StateLoader

	// DecayExponent is forwarded to activation.State.Pin so the
	// PinActivation floor reflects the entry's current decayed value.
	// Zero → activation.DefaultDecayExponent.
	DecayExponent float64

	// Now returns the wall-clock timestamp recorded in every emitted
	// datom. Tests pin it for determinism.
	Now func() time.Time

	// Actor is the operator identity recorded on every datom. The CLI
	// fills this from $CORTEX_AGENT, $USER, or an explicit --actor flag.
	Actor string

	// InvocationID is the per-command ULID shared by every datom and
	// ops.log entry in this invocation.
	InvocationID string
}

// Outcome describes one lifecycle command's result. NoOp is true when
// the command was valid but the entry was already in the requested
// state, so zero datoms were written (AC2 path for unevict-on-non-
// evicted, and the equivalent paths for unpin/pin/evict).
type Outcome struct {
	EntityID string
	Tx       string // empty when NoOp
	NoOp     bool
	Reason   string // human-readable explanation, populated for NoOp
	Datoms   []datom.Datom
}

// Pin marks an entry sticky pinned. If the entry is currently evicted,
// Pin also retracts the eviction (AC3: "cortex pin on an evicted entry
// retracts evicted_at and marks the entry pinned").
func (p *Pipeline) Pin(ctx context.Context, entityID string) (*Outcome, error) {
	state, err := p.preflight(ctx, entityID)
	if err != nil {
		return nil, err
	}

	now := p.now()
	decay := p.decay()

	// AC3 path: pin auto-unevicts. Compose the unevict transition into
	// the same datom group so the operator gets one tx, one audit row.
	wasEvicted := state.Evicted
	if wasEvicted {
		state = state.Unevict()
	}

	if state.Pinned {
		// Already pinned and (post-unevict) not evicted → genuine no-op.
		if !wasEvicted {
			return &Outcome{
				EntityID: entityID,
				NoOp:     true,
				Reason:   "entry is already pinned",
			}, nil
		}
		// Was evicted-and-pinned: we still need to write the
		// retraction even though the pinned flag does not change.
	}

	state = state.Pin(now, decay)

	tx := ulid.Make().String()
	ts := now.UTC().Format(time.RFC3339Nano)
	group := make([]datom.Datom, 0, 4)

	if wasEvicted {
		// Unevict half: retract the eviction marker.
		d, err := p.makeDatom(tx, ts, entityID, AttrEvictedAtRetracted, ts)
		if err != nil {
			return nil, err
		}
		group = append(group, d)
	}
	pinDatom, err := p.makeDatom(tx, ts, entityID, AttrPinned, true)
	if err != nil {
		return nil, err
	}
	group = append(group, pinDatom)

	pinActDatom, err := p.makeDatom(tx, ts, entityID, AttrPinActivation, state.PinActivation)
	if err != nil {
		return nil, err
	}
	group = append(group, pinActDatom)

	committedTx, err := p.Log.Append(group)
	if err != nil {
		return nil, errs.Operational("PIN_APPEND_FAILED",
			"could not append pin datoms", err)
	}
	return &Outcome{EntityID: entityID, Tx: committedTx, Datoms: group}, nil
}

// Unpin removes the sticky pin. A non-pinned entry is a no-op (zero
// datoms written) — symmetric with the unevict-on-non-evicted contract.
func (p *Pipeline) Unpin(ctx context.Context, entityID string) (*Outcome, error) {
	state, err := p.preflight(ctx, entityID)
	if err != nil {
		return nil, err
	}
	if !state.Pinned {
		return &Outcome{
			EntityID: entityID,
			NoOp:     true,
			Reason:   "entry is not pinned",
		}, nil
	}

	now := p.now()
	tx := ulid.Make().String()
	ts := now.UTC().Format(time.RFC3339Nano)

	// Two LWW writes: pinned=false and pin_activation=0. Recall reads
	// the latest value, so a single tx fully flips the entry back to
	// natural decay behavior.
	pinnedDatom, err := p.makeDatom(tx, ts, entityID, AttrPinned, false)
	if err != nil {
		return nil, err
	}
	pinActDatom, err := p.makeDatom(tx, ts, entityID, AttrPinActivation, 0.0)
	if err != nil {
		return nil, err
	}
	group := []datom.Datom{pinnedDatom, pinActDatom}

	committedTx, err := p.Log.Append(group)
	if err != nil {
		return nil, errs.Operational("UNPIN_APPEND_FAILED",
			"could not append unpin datoms", err)
	}
	return &Outcome{EntityID: entityID, Tx: committedTx, Datoms: group}, nil
}

// Evict forces the entry's base_activation to 0.0 and writes a sticky
// evicted_at marker. AC1: an evicted entry is absent from default
// recall and from every alternate retrieval mode. The visibility
// guarantee comes from activation.State.Visible (which already returns
// false when Evicted is set) and from the recall pipelines that load
// the latest evicted_at / evicted_at_retracted attribute pair.
//
// Evicting an already-evicted entry is a no-op.
func (p *Pipeline) Evict(ctx context.Context, entityID string) (*Outcome, error) {
	state, err := p.preflight(ctx, entityID)
	if err != nil {
		return nil, err
	}
	if state.Evicted {
		return &Outcome{
			EntityID: entityID,
			NoOp:     true,
			Reason:   "entry is already evicted",
		}, nil
	}

	now := p.now()
	tx := ulid.Make().String()
	ts := now.UTC().Format(time.RFC3339Nano)

	// Three LWW writes per spec:
	//   base_activation = 0.0
	//   evicted_at      = <ts>
	// (and the existing pinned flag, if any, is left intact — the spec
	// only requires base_activation forced to 0.0 and the sticky marker;
	// pin clamps are made moot by the Visible() short-circuit on
	// Evicted, so we don't need an extra unpin write here.)
	baseDatom, err := p.makeDatom(tx, ts, entityID, AttrBaseActivation, 0.0)
	if err != nil {
		return nil, err
	}
	evictDatom, err := p.makeDatom(tx, ts, entityID, AttrEvictedAt, ts)
	if err != nil {
		return nil, err
	}
	group := []datom.Datom{baseDatom, evictDatom}

	committedTx, err := p.Log.Append(group)
	if err != nil {
		return nil, errs.Operational("EVICT_APPEND_FAILED",
			"could not append evict datoms", err)
	}
	return &Outcome{EntityID: entityID, Tx: committedTx, Datoms: group}, nil
}

// Unevict retracts the sticky eviction marker. AC2: unevict on a
// non-evicted entry exits zero with a no-op status message and writes
// zero datoms. Reinforcement is re-enabled the moment the next recall
// touches the entry — the unevict path itself does NOT push
// base_activation back above the threshold. That is intentional: the
// spec language is "reinforcement re-enabled", not "automatically
// restored to visibility".
func (p *Pipeline) Unevict(ctx context.Context, entityID string) (*Outcome, error) {
	state, err := p.preflight(ctx, entityID)
	if err != nil {
		return nil, err
	}
	if !state.Evicted {
		return &Outcome{
			EntityID: entityID,
			NoOp:     true,
			Reason:   "entry is not evicted",
		}, nil
	}

	now := p.now()
	tx := ulid.Make().String()
	ts := now.UTC().Format(time.RFC3339Nano)

	d, err := p.makeDatom(tx, ts, entityID, AttrEvictedAtRetracted, ts)
	if err != nil {
		return nil, err
	}
	group := []datom.Datom{d}

	committedTx, err := p.Log.Append(group)
	if err != nil {
		return nil, errs.Operational("UNEVICT_APPEND_FAILED",
			"could not append unevict datom", err)
	}
	return &Outcome{EntityID: entityID, Tx: committedTx, Datoms: group}, nil
}

// preflight validates the entity id, the pipeline configuration, and
// loads the current state. It is called from every command entry point
// so the four public methods stay short and parallel.
func (p *Pipeline) preflight(ctx context.Context, entityID string) (activation.State, error) {
	if strings.TrimSpace(entityID) == "" {
		return activation.State{}, errs.Validation("MISSING_ENTITY_ID",
			"lifecycle command requires an entity id", nil)
	}
	if p.Log == nil {
		return activation.State{}, ErrNoLog
	}
	if p.Loader == nil {
		return activation.State{}, ErrNoLoader
	}
	if strings.TrimSpace(p.Actor) == "" {
		return activation.State{}, ErrEmptyActor
	}
	state, ok, err := p.Loader.Load(ctx, entityID)
	if err != nil {
		return activation.State{}, errs.Operational("STATE_LOAD_FAILED",
			"could not load entry state", err)
	}
	if !ok {
		return activation.State{}, errs.Validation("ENTRY_NOT_FOUND",
			fmt.Sprintf("entry %q is not known", entityID),
			map[string]any{"entity_id": entityID})
	}
	return state, nil
}

func (p *Pipeline) now() time.Time {
	if p.Now == nil {
		return time.Now().UTC()
	}
	return p.Now().UTC()
}

func (p *Pipeline) decay() float64 {
	if p.DecayExponent <= 0 {
		return activation.DefaultDecayExponent
	}
	return p.DecayExponent
}

// makeDatom builds and seals one datom for an LWW lifecycle attribute.
func (p *Pipeline) makeDatom(tx, ts, entityID, attr string, value any) (datom.Datom, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return datom.Datom{}, fmt.Errorf("marshal %s: %w", attr, err)
	}
	d := datom.Datom{
		Tx:           tx,
		Ts:           ts,
		Actor:        p.Actor,
		Op:           datom.OpAdd,
		E:            entityID,
		A:            attr,
		V:            raw,
		Src:          SrcLifecycle,
		InvocationID: p.InvocationID,
	}
	if err := d.Seal(); err != nil {
		return datom.Datom{}, fmt.Errorf("seal %s: %w", attr, err)
	}
	return d, nil
}
