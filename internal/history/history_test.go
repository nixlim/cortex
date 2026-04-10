package history

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

// writeDatom is a tiny test helper that builds + seals a single
// observe-style datom and appends it to a writer. Returns the tx so
// the caller can correlate against later assertions.
func writeDatom(t *testing.T, w *log.Writer, entity, attr string, value any) string {
	t.Helper()
	tx := ulid.Make().String()
	raw, _ := json.Marshal(value)
	d := datom.Datom{
		Tx:           tx,
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Actor:        "tester",
		Op:           datom.OpAdd,
		E:            entity,
		A:            attr,
		V:            raw,
		Src:          "observe",
		InvocationID: "inv-test",
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := w.Append([]datom.Datom{d}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return tx
}

func writeRetraction(t *testing.T, w *log.Writer, entity, attr string) string {
	t.Helper()
	tx := ulid.Make().String()
	raw, _ := json.Marshal(nil)
	d := datom.Datom{
		Tx:           tx,
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Actor:        "tester",
		Op:           datom.OpRetract,
		E:            entity,
		A:            attr,
		V:            raw,
		Src:          "retract",
		InvocationID: "inv-test",
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := w.Append([]datom.Datom{d}); err != nil {
		t.Fatalf("append: %v", err)
	}
	return tx
}

func healthySegments(t *testing.T, dir string) []string {
	t.Helper()
	report, err := log.Load(dir, log.LoadOptions{})
	if err != nil {
		t.Fatalf("log.Load: %v", err)
	}
	return report.Healthy
}

// ---------------------------------------------------------------------
// History
// ---------------------------------------------------------------------

func TestHistoryReturnsAllReinforcementDatomsNoLWW(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	entryID := EntryPrefix + ulid.Make().String()
	writeDatom(t, w, entryID, "body", "hello")
	// Five reinforcement updates to base_activation — these would
	// collapse to one datom under LWW, but history must preserve all
	// five because the AC explicitly says "no LWW collapse".
	var reinforcementTxs []string
	for i := 0; i < 5; i++ {
		reinforcementTxs = append(reinforcementTxs, writeDatom(t, w, entryID, "base_activation", 0.1*float64(i+1)))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := History(healthySegments(t, dir), entryID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	// 1 body + 5 base_activation = 6 datoms.
	if len(got) != 6 {
		t.Fatalf("len = %d, want 6", len(got))
	}
	// All five activation datoms must be present in tx-ascending order.
	var seenTxs []string
	for _, d := range got {
		if d.A == "base_activation" {
			seenTxs = append(seenTxs, d.Tx)
		}
	}
	if len(seenTxs) != 5 {
		t.Fatalf("base_activation count = %d, want 5", len(seenTxs))
	}
	for i, want := range reinforcementTxs {
		if seenTxs[i] != want {
			t.Errorf("seenTxs[%d] = %q, want %q", i, seenTxs[i], want)
		}
	}
}

func TestHistoryIncludesRetractionInOrder(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	entryID := EntryPrefix + ulid.Make().String()
	writeDatom(t, w, entryID, "body", "hello")
	writeDatom(t, w, entryID, "base_activation", 0.5)
	retractionTx := writeRetraction(t, w, entryID, "base_activation")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := History(healthySegments(t, dir), entryID)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	last := got[len(got)-1]
	if last.Op != datom.OpRetract {
		t.Errorf("last.Op = %q, want retract", last.Op)
	}
	if last.Tx != retractionTx {
		t.Errorf("last.Tx = %q, want %q", last.Tx, retractionTx)
	}
}

func TestHistoryEmptyResultIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	writeDatom(t, w, "entry:other", "body", "hi")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := History(healthySegments(t, dir), "entry:nope")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestHistoryRejectsEmptyEntityID(t *testing.T) {
	_, err := History(nil, "")
	requireValidation(t, err, "MISSING_ENTITY_ID")
}

// ---------------------------------------------------------------------
// AsOf
// ---------------------------------------------------------------------

func TestAsOfFiltersFutureFacts(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	entryA := EntryPrefix + ulid.Make().String()
	txA := writeDatom(t, w, entryA, "body", "first")
	entryB := EntryPrefix + ulid.Make().String()
	writeDatom(t, w, entryB, "body", "second")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := AsOf(healthySegments(t, dir), txA)
	if err != nil {
		t.Fatalf("AsOf: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (only entry A visible at txA)", len(got))
	}
	if got[0].Entity != entryA {
		t.Errorf("entity = %q, want %q", got[0].Entity, entryA)
	}
}

func TestAsOfUnknownTxReturnsNotFound(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	writeDatom(t, w, EntryPrefix+ulid.Make().String(), "body", "hi")
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err = AsOf(healthySegments(t, dir), "01ZZZZZZZZZZZZZZZZZZZZZZZZ")
	if err == nil {
		t.Fatal("expected NOT_FOUND, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NOT_FOUND" || e.Kind != errs.KindOperational {
		t.Errorf("err = %v, want operational NOT_FOUND", err)
	}
}

func TestAsOfRejectsEmptyTx(t *testing.T) {
	_, err := AsOf(nil, "")
	requireValidation(t, err, "MISSING_TX")
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func requireValidation(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected validation %s, got nil", code)
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.KindValidation || e.Code != code {
		t.Fatalf("err = %v, want validation %s", err, code)
	}
}
