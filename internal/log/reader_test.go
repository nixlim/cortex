package log

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// writeReaderSegment builds a segment file from a slice of tx ULIDs,
// sealing each datom so the reader's per-line checksum verification
// passes. Tests use this to craft exact segment layouts without going
// through Writer.
func writeReaderSegment(t *testing.T, path string, txs []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	for _, tx := range txs {
		d := datom.Datom{
			Tx:           tx,
			Ts:           "2026-04-10T00:00:00Z",
			Actor:        "test",
			Op:           datom.OpAdd,
			E:            "entry:reader",
			A:            "body",
			V:            json.RawMessage(`"ok"`),
			Src:          "reader_test",
			InvocationID: "01HPTESTINVOCATION0000000000",
		}
		if err := d.Seal(); err != nil {
			t.Fatalf("seal: %v", err)
		}
		b, err := datom.Marshal(&d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// TestReader_InterleavedSegmentsYieldAscending covers AC1: three
// segments whose tx ULIDs are interleaved must come out strictly
// ascending across the merged stream.
func TestReader_InterleavedSegmentsYieldAscending(t *testing.T) {
	dir := t.TempDir()

	// Pre-generate nine ULIDs and sort them so we know the expected
	// global order up front. Then deal them across three segments in
	// a strided pattern so each segment is individually sorted but the
	// segments interleave.
	all := make([]string, 9)
	for i := range all {
		all[i] = ulid.Make().String()
	}
	sort.Strings(all)

	segA := []string{all[0], all[3], all[6]}
	segB := []string{all[1], all[4], all[7]}
	segC := []string{all[2], all[5], all[8]}

	pA := filepath.Join(dir, "01HAAA000000000000000000000-pid1.jsonl")
	pB := filepath.Join(dir, "01HBBB000000000000000000000-pid1.jsonl")
	pC := filepath.Join(dir, "01HCCC000000000000000000000-pid1.jsonl")
	writeReaderSegment(t, pA, segA)
	writeReaderSegment(t, pB, segB)
	writeReaderSegment(t, pC, segC)

	got, err := ReadAll([]string{pA, pB, pC})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(all) {
		t.Fatalf("count: got %d want %d", len(got), len(all))
	}
	for i, d := range got {
		if d.Tx != all[i] {
			t.Fatalf("index %d: got %s want %s", i, d.Tx, all[i])
		}
	}
}

// TestReader_EmptyDirectoryEmptyIterator covers AC3: no paths → Next
// returns (_, false, nil) immediately and Close is a no-op.
func TestReader_EmptyDirectoryEmptyIterator(t *testing.T) {
	r, err := NewReader(nil)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	_, ok, err := r.Next()
	if err != nil {
		t.Fatalf("Next err: %v", err)
	}
	if ok {
		t.Fatalf("expected ok=false on empty reader")
	}
}

// TestReader_NonOverlappingSegmentsYieldInTurn covers AC4: segments
// whose tx ranges do not overlap drain one at a time in order.
func TestReader_NonOverlappingSegmentsYieldInTurn(t *testing.T) {
	dir := t.TempDir()

	// Build two ranges where every tx in segA precedes every tx in segB.
	first := make([]string, 3)
	second := make([]string, 3)
	for i := range first {
		first[i] = ulid.Make().String()
	}
	for i := range second {
		second[i] = ulid.Make().String()
	}
	sort.Strings(first)
	sort.Strings(second)
	// Sanity: second entirely > first.
	if first[len(first)-1] >= second[0] {
		t.Fatalf("fixture ranges overlap: %v %v", first, second)
	}

	pA := filepath.Join(dir, "01HAAA000000000000000000000-pid1.jsonl")
	pB := filepath.Join(dir, "01HBBB000000000000000000000-pid1.jsonl")
	writeReaderSegment(t, pA, first)
	writeReaderSegment(t, pB, second)

	got, err := ReadAll([]string{pA, pB})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := append(append([]string{}, first...), second...)
	if len(got) != len(want) {
		t.Fatalf("count: got %d want %d", len(got), len(want))
	}
	for i, d := range got {
		if d.Tx != want[i] {
			t.Fatalf("index %d: got %s want %s", i, d.Tx, want[i])
		}
	}
}

// TestReader_BoundedMemory covers AC2 indirectly: after NewReader the
// heap contains exactly one entry per non-empty segment, regardless of
// how many datoms each segment holds.
func TestReader_BoundedMemory(t *testing.T) {
	dir := t.TempDir()

	// Two segments, 50 datoms each.
	var segA, segB []string
	for i := 0; i < 50; i++ {
		segA = append(segA, ulid.Make().String())
		segB = append(segB, ulid.Make().String())
	}
	sort.Strings(segA)
	sort.Strings(segB)

	pA := filepath.Join(dir, "01HAAA000000000000000000000-pid1.jsonl")
	pB := filepath.Join(dir, "01HBBB000000000000000000000-pid1.jsonl")
	writeReaderSegment(t, pA, segA)
	writeReaderSegment(t, pB, segB)

	r, err := NewReader([]string{pA, pB})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	if r.h.Len() != 2 {
		t.Fatalf("heap size after init: got %d want 2", r.h.Len())
	}
	// Drain and assert the heap size never exceeds the number of
	// segments — a direct witness of bounded memory use.
	count := 0
	for {
		_, ok, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		count++
		if r.h.Len() > 2 {
			t.Fatalf("heap grew beyond segment count: %d", r.h.Len())
		}
	}
	if count != 100 {
		t.Fatalf("drained count: got %d want 100", count)
	}
}

// TestReader_EmptySegmentSkipped verifies that a segment file with no
// content does not appear in the heap and does not stall the merge.
func TestReader_EmptySegmentSkipped(t *testing.T) {
	dir := t.TempDir()
	pEmpty := filepath.Join(dir, "01HEMPTY0000000000000000000-pid1.jsonl")
	if err := os.WriteFile(pEmpty, nil, 0o600); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	pFull := filepath.Join(dir, "01HFULL0000000000000000000-pid1.jsonl")
	txs := []string{ulid.Make().String(), ulid.Make().String()}
	sort.Strings(txs)
	writeReaderSegment(t, pFull, txs)

	got, err := ReadAll([]string{pEmpty, pFull})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: got %d want 2", len(got))
	}
	if got[0].Tx != txs[0] || got[1].Tx != txs[1] {
		t.Fatalf("order: got %v want %v", []string{got[0].Tx, got[1].Tx}, txs)
	}
}

// TestReader_RejectsOutOfOrderSegment exercises the per-segment
// monotonicity guard: if a segment has a tx earlier than its previous
// tx, Next must return ErrSegmentOutOfOrder.
func TestReader_RejectsOutOfOrderSegment(t *testing.T) {
	dir := t.TempDir()
	txs := []string{ulid.Make().String(), ulid.Make().String()}
	sort.Strings(txs)
	// Reverse order inside the segment.
	bad := []string{txs[1], txs[0]}
	p := filepath.Join(dir, "01HBAD0000000000000000000000-pid1.jsonl")
	writeReaderSegment(t, p, bad)

	r, err := NewReader([]string{p})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	// Next pops the primed (larger) head, then advances the stream to
	// read the (smaller) second line, which trips the monotonicity
	// guard. The reader surfaces the error rather than silently
	// emitting a reordered stream.
	_, _, err = r.Next()
	if !errors.Is(err, ErrSegmentOutOfOrder) {
		t.Fatalf("expected ErrSegmentOutOfOrder, got %v", err)
	}
	// Subsequent calls keep returning the error.
	_, _, err = r.Next()
	if !errors.Is(err, ErrSegmentOutOfOrder) {
		t.Fatalf("sticky err: got %v", err)
	}
}

// TestReader_CloseIsIdempotent checks that Close can be called twice
// without error and that using a closed reader fails cleanly.
func TestReader_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "01HCLOSE0000000000000000000-pid1.jsonl")
	writeReaderSegment(t, p, []string{ulid.Make().String()})
	r, err := NewReader([]string{p})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, _, err := r.Next(); err == nil {
		t.Fatalf("Next on closed reader: expected error")
	}
}
