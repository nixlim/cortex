package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/datom"
)

// --- fakes -----------------------------------------------------------

type fakeLoader struct {
	states map[string]activation.State
}

func (f *fakeLoader) Load(_ context.Context, id string) (activation.State, bool, error) {
	if f.states == nil {
		return activation.State{}, false, nil
	}
	s, ok := f.states[id]
	return s, ok, nil
}

type fakeLog struct {
	groups [][]datom.Datom
}

func (f *fakeLog) Append(group []datom.Datom) (string, error) {
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	if len(group) > 0 {
		return group[0].Tx, nil
	}
	return "", nil
}

// --- helpers ---------------------------------------------------------

func newPipeline(loader StateLoader, log LogAppender) *Pipeline {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &Pipeline{
		Log:          log,
		Loader:       loader,
		Now:          func() time.Time { return now },
		Actor:        "test-operator",
		InvocationID: "01HTESTINVOCATION0000000000",
	}
}

func freshState(now time.Time) activation.State {
	return activation.Seed(now)
}

// --- tests -----------------------------------------------------------

// TestEvict_ForcesBaseToZeroAndWritesStickyMarker covers AC1: cortex
// evict on entry X forces base_activation=0.0 and writes the sticky
// evicted_at marker. The visibility absence (default recall, similar,
// traverse, path, community, surprise) is enforced by the recall
// pipelines reading State.Evicted; we verify here that the right
// datoms get written so those readers see the eviction.
func TestEvict_ForcesBaseToZeroAndWritesStickyMarker(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:X": freshState(now),
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)

	out, err := p.Evict(context.Background(), "entry:X")
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if out.NoOp {
		t.Fatalf("evict on non-evicted reported NoOp")
	}
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
	group := log.groups[0]
	if len(group) != 2 {
		t.Fatalf("evict datoms: got %d want 2", len(group))
	}
	hasBase := false
	hasEvictMarker := false
	for _, d := range group {
		switch d.A {
		case AttrBaseActivation:
			hasBase = true
			if string(d.V) != "0" {
				t.Errorf("base_activation: got %s want 0", string(d.V))
			}
		case AttrEvictedAt:
			hasEvictMarker = true
		}
		if d.Src != SrcLifecycle {
			t.Errorf("src: got %s want %s", d.Src, SrcLifecycle)
		}
		if d.Actor != "test-operator" {
			t.Errorf("actor: got %s want test-operator", d.Actor)
		}
		if d.Checksum == "" {
			t.Errorf("datom not sealed: %+v", d)
		}
		if err := d.Verify(); err != nil {
			t.Errorf("verify: %v", err)
		}
	}
	if !hasBase || !hasEvictMarker {
		t.Fatalf("missing required datoms: base=%v evicted_at=%v",
			hasBase, hasEvictMarker)
	}
	// Sanity-check that the State an updated loader would return is
	// not visible — proves recall pipelines that consult State.Visible
	// will hide this entry. This is a structural assertion against
	// activation.State.Evict, which Evict() exercises in the loader-
	// less path; we replay it directly here to mirror what a recall
	// loader applying these LWW writes would compute.
	post := freshState(now).Evict()
	if post.Visible(now, activation.DefaultDecayExponent, activation.VisibilityThreshold) {
		t.Fatalf("post-evict state still visible")
	}
}

// TestUnevict_NonEvictedIsNoOp covers AC2: cortex unevict on a non-
// evicted entry exits zero with a no-op status message and writes
// zero datoms.
func TestUnevict_NonEvictedIsNoOp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:Y": freshState(now), // not evicted
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)

	out, err := p.Unevict(context.Background(), "entry:Y")
	if err != nil {
		t.Fatalf("Unevict: %v", err)
	}
	if !out.NoOp {
		t.Fatalf("unevict on non-evicted not flagged NoOp")
	}
	if out.Tx != "" {
		t.Fatalf("NoOp should leave Tx empty, got %s", out.Tx)
	}
	if len(log.groups) != 0 {
		t.Fatalf("NoOp wrote to log: %d groups", len(log.groups))
	}
	if !strings.Contains(out.Reason, "not evicted") {
		t.Fatalf("Reason: got %q", out.Reason)
	}
}

// TestPin_OnEvictedEntryRetractsEvictionAndPins covers AC3: cortex pin
// on an evicted entry retracts evicted_at and marks the entry pinned.
// We verify both writes appear in a single tx so audit and replay
// see one atomic operation.
func TestPin_OnEvictedEntryRetractsEvictionAndPins(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evicted := freshState(now).Evict()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:Z": evicted,
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)

	out, err := p.Pin(context.Background(), "entry:Z")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if out.NoOp {
		t.Fatalf("pin-on-evicted reported NoOp")
	}
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
	group := log.groups[0]
	hasRetract := false
	hasPinned := false
	hasPinAct := false
	tx := group[0].Tx
	for _, d := range group {
		if d.Tx != tx {
			t.Fatalf("tx mismatch in pin-on-evicted group")
		}
		switch d.A {
		case AttrEvictedAtRetracted:
			hasRetract = true
		case AttrPinned:
			hasPinned = true
			if string(d.V) != "true" {
				t.Errorf("pinned value: got %s want true", string(d.V))
			}
		case AttrPinActivation:
			hasPinAct = true
		}
	}
	if !hasRetract {
		t.Fatalf("missing %s datom", AttrEvictedAtRetracted)
	}
	if !hasPinned {
		t.Fatalf("missing %s datom", AttrPinned)
	}
	if !hasPinAct {
		t.Fatalf("missing %s datom", AttrPinActivation)
	}
}

// TestHistoryStillVisible covers AC4 by structural reasoning: the
// lifecycle pipeline only ever appends datoms, never retracts (the
// "_retracted" attributes are themselves OpAdd writes carrying a
// timestamp value). Cortex history walks the per-entity datom log
// in append order, so the lineage of an evicted entry is the entire
// pre-eviction sequence + the eviction marker. The assertion here is
// that Op is OpAdd on every emitted datom — the log-append rule is
// what guarantees history visibility.
func TestEvictDatomsAreOpAddNotRetract(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:X": freshState(now),
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)
	if _, err := p.Evict(context.Background(), "entry:X"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for _, d := range log.groups[0] {
		if d.Op != datom.OpAdd {
			t.Fatalf("evict emitted non-OpAdd datom %+v — would corrupt history", d)
		}
	}
}

// --- additional behaviour tests for the four commands ---------------

// TestPin_AlreadyPinnedIsNoOp confirms an idempotent re-pin writes
// nothing. (Symmetric to the unevict-on-non-evicted contract.)
func TestPin_AlreadyPinnedIsNoOp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	pinned := freshState(now).Pin(now, activation.DefaultDecayExponent)
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:P": pinned,
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)
	out, err := p.Pin(context.Background(), "entry:P")
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if !out.NoOp {
		t.Fatalf("re-pin not flagged NoOp")
	}
	if len(log.groups) != 0 {
		t.Fatalf("re-pin wrote datoms: %d", len(log.groups))
	}
}

// TestEvict_AlreadyEvictedIsNoOp confirms idempotent eviction.
func TestEvict_AlreadyEvictedIsNoOp(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:E": freshState(now).Evict(),
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)
	out, err := p.Evict(context.Background(), "entry:E")
	if err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if !out.NoOp {
		t.Fatalf("re-evict not flagged NoOp")
	}
	if len(log.groups) != 0 {
		t.Fatalf("re-evict wrote datoms: %d", len(log.groups))
	}
}

// TestUnpin_PinnedEntryWritesFlipDatoms confirms unpin emits two
// datoms (pinned=false, pin_activation=0) sharing one tx.
func TestUnpin_PinnedEntryWritesFlipDatoms(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	pinned := freshState(now).Pin(now, activation.DefaultDecayExponent)
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:P": pinned,
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)
	out, err := p.Unpin(context.Background(), "entry:P")
	if err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	if out.NoOp {
		t.Fatalf("unpin reported NoOp on pinned entry")
	}
	if len(log.groups) != 1 || len(log.groups[0]) != 2 {
		t.Fatalf("unpin datoms: got %v", log.groups)
	}
	tx := log.groups[0][0].Tx
	for _, d := range log.groups[0] {
		if d.Tx != tx {
			t.Fatalf("tx mismatch in unpin group")
		}
	}
}

// TestUnevict_EvictedEntryWritesRetractionMarker covers the happy path
// counterpart to TestUnevict_NonEvictedIsNoOp.
func TestUnevict_EvictedEntryWritesRetractionMarker(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{states: map[string]activation.State{
		"entry:E": freshState(now).Evict(),
	}}
	log := &fakeLog{}
	p := newPipeline(loader, log)
	out, err := p.Unevict(context.Background(), "entry:E")
	if err != nil {
		t.Fatalf("Unevict: %v", err)
	}
	if out.NoOp {
		t.Fatalf("unevict on evicted reported NoOp")
	}
	if len(log.groups) != 1 || len(log.groups[0]) != 1 {
		t.Fatalf("unevict datoms: got %v", log.groups)
	}
	if log.groups[0][0].A != AttrEvictedAtRetracted {
		t.Fatalf("unevict attr: got %s want %s",
			log.groups[0][0].A, AttrEvictedAtRetracted)
	}
}

// TestMissingEntityIDRejected covers the precondition.
func TestMissingEntityIDRejected(t *testing.T) {
	loader := &fakeLoader{states: map[string]activation.State{}}
	p := newPipeline(loader, &fakeLog{})
	for name, fn := range map[string]func() error{
		"pin":     func() error { _, e := p.Pin(context.Background(), ""); return e },
		"unpin":   func() error { _, e := p.Unpin(context.Background(), ""); return e },
		"evict":   func() error { _, e := p.Evict(context.Background(), ""); return e },
		"unevict": func() error { _, e := p.Unevict(context.Background(), ""); return e },
	} {
		if err := fn(); err == nil || !strings.Contains(err.Error(), "MISSING_ENTITY_ID") {
			t.Errorf("%s: want MISSING_ENTITY_ID, got %v", name, err)
		}
	}
}

// TestEntryNotFoundRejected covers the loader-miss path.
func TestEntryNotFoundRejected(t *testing.T) {
	loader := &fakeLoader{states: map[string]activation.State{}}
	p := newPipeline(loader, &fakeLog{})
	_, err := p.Evict(context.Background(), "entry:UNKNOWN")
	if err == nil || !strings.Contains(err.Error(), "ENTRY_NOT_FOUND") {
		t.Fatalf("want ENTRY_NOT_FOUND, got %v", err)
	}
}
