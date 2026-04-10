package exportlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// newDatom builds a sealed datom with the supplied tx and entity.
func newDatom(t *testing.T, tx, entity, attr string, value any) datom.Datom {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
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
	return d
}

func TestStreamProducesAscendingJSONL(t *testing.T) {
	tx1 := ulid.Make().String()
	tx2 := ulid.Make().String()
	tx3 := ulid.Make().String()
	rows := []datom.Datom{
		newDatom(t, tx1, "entry:01AAA", "body", "first"),
		newDatom(t, tx2, "entry:01BBB", "body", "second"),
		newDatom(t, tx3, "entry:01CCC", "body", "third"),
	}

	var buf bytes.Buffer
	res, err := Stream(context.Background(), NewSliceSource(rows), &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if res.DatomCount != 3 {
		t.Errorf("DatomCount = %d, want 3", res.DatomCount)
	}
	if int64(buf.Len()) != res.BytesOut {
		t.Errorf("BytesOut = %d, buf.Len = %d", res.BytesOut, buf.Len())
	}

	// Output must be three lines, tx values strictly ascending.
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3", len(lines))
	}
	var prevTx string
	for i, line := range lines {
		d, err := datom.Unmarshal(line)
		if err != nil {
			t.Fatalf("Unmarshal line %d: %v", i, err)
		}
		if prevTx != "" && d.Tx <= prevTx {
			t.Errorf("line %d tx %s not strictly ascending after %s", i, d.Tx, prevTx)
		}
		prevTx = d.Tx
	}
}

func TestStreamEmptySourceProducesZeroBytes(t *testing.T) {
	var buf bytes.Buffer
	res, err := Stream(context.Background(), NewSliceSource(nil), &buf)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("buf.Len = %d, want 0", buf.Len())
	}
	if res.DatomCount != 0 || res.BytesOut != 0 {
		t.Errorf("counters non-zero: %+v", res)
	}
}

func TestStreamRejectsOutOfOrderTx(t *testing.T) {
	tx1 := ulid.Make().String()
	tx2 := ulid.Make().String()
	// Build rows with tx2 first then tx1 — non-monotonic.
	rows := []datom.Datom{
		newDatom(t, tx2, "entry:01AAA", "body", "later"),
		newDatom(t, tx1, "entry:01BBB", "body", "earlier"),
	}
	var buf bytes.Buffer
	_, err := Stream(context.Background(), NewSliceSource(rows), &buf)
	if err == nil {
		t.Fatal("expected EXPORT_TX_OUT_OF_ORDER, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "EXPORT_TX_OUT_OF_ORDER" {
		t.Errorf("err = %v, want EXPORT_TX_OUT_OF_ORDER", err)
	}
}

func TestStreamRoundTripsThroughUnmarshal(t *testing.T) {
	// AC: round-trip via merge reaches a byte-identical Layer 1 log.
	// We can't run merge here, but we can verify that every line of
	// the export is byte-identical to a fresh datom.Marshal of the
	// same input — which is the building block merge will reuse.
	tx := ulid.Make().String()
	d := newDatom(t, tx, "entry:01AAA", "body", "round-trip")
	want, err := datom.Marshal(&d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var buf bytes.Buffer
	if _, err := Stream(context.Background(), NewSliceSource([]datom.Datom{d}), &buf); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("export bytes diverge from datom.Marshal\nexport=%q\nwant  =%q",
			buf.String(), string(want))
	}
}

func TestStreamRejectsNilSourceOrDest(t *testing.T) {
	if _, err := Stream(context.Background(), nil, &bytes.Buffer{}); err == nil {
		t.Error("nil source: expected validation error, got nil")
	}
	if _, err := Stream(context.Background(), NewSliceSource(nil), nil); err == nil {
		t.Error("nil dest: expected validation error, got nil")
	}
}

func TestStreamWriteFailurePropagates(t *testing.T) {
	tx := ulid.Make().String()
	rows := []datom.Datom{newDatom(t, tx, "entry:01AAA", "body", "x")}
	_, err := Stream(context.Background(), NewSliceSource(rows), failingWriter{})
	if err == nil {
		t.Fatal("expected EXPORT_WRITE_FAILED, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "EXPORT_WRITE_FAILED" {
		t.Errorf("err = %v, want EXPORT_WRITE_FAILED", err)
	}
}

// failingWriter returns an error on every Write so the failure path
// can be exercised without a real OS-level write error.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("disk full")
}

// Sanity-check that lines are newline terminated.
func TestStreamLinesEndWithNewline(t *testing.T) {
	tx := ulid.Make().String()
	rows := []datom.Datom{
		newDatom(t, tx, "entry:01AAA", "body", "x"),
	}
	var buf bytes.Buffer
	if _, err := Stream(context.Background(), NewSliceSource(rows), &buf); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("output does not end with newline: %q", buf.String())
	}
}
