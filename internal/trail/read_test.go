package trail

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
)

// writeEntryDatom writes one observe-style entry datom whose A=trail
// V=trailID. Used by read tests to simulate observations attached to a
// trail without dragging the write pipeline into this package's tests.
func writeEntryDatom(t *testing.T, w *log.Writer, entryID, trailID string) string {
	t.Helper()
	tx := ulid.Make().String()
	raw, _ := json.Marshal(trailID)
	d := datom.Datom{
		Tx:           tx,
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Actor:        "tester",
		Op:           datom.OpAdd,
		E:            entryID,
		A:            AttrTrail,
		V:            raw,
		Src:          "observe",
		InvocationID: "inv-test",
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := w.Append([]datom.Datom{d}); err != nil {
		t.Fatalf("append entry: %v", err)
	}
	return tx
}

// healthySegments runs log.Load on dir and returns the healthy
// segment paths so tests can hand them to trail.Load / trail.List.
func healthySegments(t *testing.T, dir string) []string {
	t.Helper()
	report, err := log.Load(dir, log.LoadOptions{})
	if err != nil {
		t.Fatalf("log.Load: %v", err)
	}
	return report.Healthy
}

// ---------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------

func TestLoadReconstructsTrailWithEntriesInWriteOrder(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Begin the trail.
	trailID, err := Begin(w, "tester", "inv-1", "grill-spec", "auth review",
		fixedClock("2026-04-10T12:00:00Z"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	// Three entries appended in a known order.
	entryA := "entry:" + ulid.Make().String()
	txA := writeEntryDatom(t, w, entryA, trailID)
	entryB := "entry:" + ulid.Make().String()
	txB := writeEntryDatom(t, w, entryB, trailID)
	entryC := "entry:" + ulid.Make().String()
	txC := writeEntryDatom(t, w, entryC, trailID)

	// End the trail with a non-empty summary.
	if err := End(w, "tester", "inv-1", trailID, "narrative",
		fixedClock("2026-04-10T13:00:00Z")); err != nil {
		t.Fatalf("End: %v", err)
	}

	// Drop the writer's handle so the segment is fully readable.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	manifest, err := Load(healthySegments(t, dir), trailID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if manifest.ID != trailID {
		t.Errorf("ID = %q, want %q", manifest.ID, trailID)
	}
	if manifest.Name != "auth review" {
		t.Errorf("Name = %q, want 'auth review'", manifest.Name)
	}
	if manifest.Agent != "grill-spec" {
		t.Errorf("Agent = %q, want grill-spec", manifest.Agent)
	}
	if manifest.StartedAt == "" {
		t.Errorf("StartedAt empty")
	}
	if manifest.EndedAt == "" {
		t.Errorf("EndedAt empty")
	}
	if manifest.Summary != "narrative" {
		t.Errorf("Summary = %q, want narrative", manifest.Summary)
	}
	if len(manifest.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(manifest.Entries))
	}
	wantOrder := []EntryRef{
		{ID: entryA, Tx: txA},
		{ID: entryB, Tx: txB},
		{ID: entryC, Tx: txC},
	}
	for i, want := range wantOrder {
		if manifest.Entries[i] != want {
			t.Errorf("entries[%d] = %+v, want %+v", i, manifest.Entries[i], want)
		}
	}
}

func TestLoadReturnsTrailNotFound(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if _, err := Begin(w, "a", "inv-1", "agent", "name", nil); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = Load(healthySegments(t, dir), "trail:does-not-exist")
	if err == nil {
		t.Fatal("expected ErrTrailNotFound, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NOT_FOUND" || e.Kind != errs.KindOperational {
		t.Errorf("err = %v, want operational NOT_FOUND", err)
	}
}

func TestLoadIgnoresEntriesPointingAtOtherTrails(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	mineID, err := Begin(w, "a", "inv-1", "agent", "mine", nil)
	if err != nil {
		t.Fatalf("Begin mine: %v", err)
	}
	otherID, err := Begin(w, "a", "inv-1", "agent", "other", nil)
	if err != nil {
		t.Fatalf("Begin other: %v", err)
	}
	writeEntryDatom(t, w, "entry:"+ulid.Make().String(), mineID)
	writeEntryDatom(t, w, "entry:"+ulid.Make().String(), otherID)
	writeEntryDatom(t, w, "entry:"+ulid.Make().String(), otherID)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	m, err := Load(healthySegments(t, dir), mineID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(m.Entries) != 1 {
		t.Errorf("entries = %d, want 1 (other trail's entries leaked in)", len(m.Entries))
	}
}

// ---------------------------------------------------------------------
// List
// ---------------------------------------------------------------------

func TestListEnumeratesAllTrails(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	first, err := Begin(w, "a", "inv-1", "agent", "first",
		fixedClock("2026-04-10T10:00:00Z"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	second, err := Begin(w, "a", "inv-1", "agent", "second",
		fixedClock("2026-04-10T11:00:00Z"))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	// One entry for the first trail.
	writeEntryDatom(t, w, "entry:"+ulid.Make().String(), first)
	// Close out the second trail.
	if err := End(w, "a", "inv-1", second, "wrap-up",
		fixedClock("2026-04-10T12:00:00Z")); err != nil {
		t.Fatalf("End: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	list, err := List(healthySegments(t, dir))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2 (%+v)", len(list), list)
	}
	// In-progress trail (no EndedAt) should sort before the closed one.
	if list[0].ID != first {
		t.Errorf("list[0] = %q, want in-progress %q", list[0].ID, first)
	}
	if list[0].EntryCount != 1 {
		t.Errorf("entry count = %d, want 1", list[0].EntryCount)
	}
	if list[1].EndedAt == "" {
		t.Errorf("list[1] EndedAt empty, want closed trail second")
	}
}
