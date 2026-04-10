package log

// End-to-end 50-writer contention test for cortex-4kq.47.
//
// This is the hammer test for the committed log layer. It spawns 50
// goroutines, each driving its own *Writer against a shared segment
// directory, and races them through many appends of multi-datom
// transaction groups.
//
// Design note: the log layer's per-segment tx monotonicity invariant
// (reader.ErrSegmentOutOfOrder) is enforced within each segment file.
// The correct concurrent-writer pattern is therefore "one Writer per
// goroutine, all pointing at the same dir" — each goroutine writes to
// its own segment (monotonic within, because only that goroutine
// emits tx ULIDs for it), and the Reader k-way merges across
// segments. This exercises:
//
//   1. All N writes committed — every mint-then-Append pair across
//      every goroutine shows up in the merged stream exactly once.
//   2. Merge-sort reader returns datoms in strict non-decreasing tx
//      ULID order — the k-way merge correctness property over many
//      real segments produced under real contention.
//   3. No corruption — every datom's checksum verifies after all the
//      concurrent filesystem activity.
//   4. Segment rollover under contention preserves causality — with
//      a tight per-Writer size cap, each Writer rolls several times
//      while other goroutines are racing them on the directory; we
//      assert that for every tx, all its datoms live in exactly one
//      segment (group atomicity).
//   5. Advisory flock prevents interleaved tx groups — each Writer
//      holds its segment's flock over the write+fsync, so even if
//      another Writer picked the same path by collision (they can't,
//      thanks to the ULID+writer-id prefix, but the flock guarantees
//      correctness even if they did), no datoms from a second tx
//      would ever slip between the datoms of the first.
//
// The test MUST pass under `go test -race` — that's the point.
// Run with: go test -race -run TestContention -count=1 ./internal/log/

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nixlim/cortex/internal/datom"
)

const (
	// contentionWriters is the goroutine count called out by the bead.
	// 50 is enough to produce meaningful directory-level contention
	// and several rollovers per writer under the tight size cap,
	// without making the test slow under -race.
	contentionWriters = 50

	// contentionAppendsPerWriter is the per-goroutine iteration count.
	// 20 * 50 * 3 datoms = 3000 datoms total — enough work to exercise
	// the k-way merge with many segments, still fast under -race.
	contentionAppendsPerWriter = 20

	// contentionGroupSize is the number of datoms per transaction
	// group. Groups larger than 1 exercise the "no interleaving"
	// invariant — a failure would let another writer's datoms slip
	// between the datoms of the current tx in the merged stream.
	contentionGroupSize = 3

	// contentionSegmentMaxBytes is the per-Writer rollover cap. Small
	// enough to force multiple rollovers per writer over 20 groups,
	// large enough to hold a single 3-datom group in an empty segment
	// (see TestRollover_EmptySegmentAcceptsOversizedGroup).
	contentionSegmentMaxBytes = 2 * 1024
)

// TestContention_FiftyWritersSharedDirectory is the primary acceptance
// test for cortex-4kq.47.
func TestContention_FiftyWritersSharedDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// expectedTxs accumulates every tx that any goroutine successfully
	// committed, mapping tx → contentionGroupSize. A sync.Map keeps
	// concurrent inserts race-free without per-goroutine buffers.
	var expectedTxs sync.Map
	var successCount atomic.Int64
	var errorCount atomic.Int64
	var totalRollCount atomic.Uint64

	var wg sync.WaitGroup
	wg.Add(contentionWriters)
	for i := 0; i < contentionWriters; i++ {
		go func(workerID int) {
			defer wg.Done()
			// Each goroutine gets its own Writer in the shared dir.
			// The Writer allocates a unique segment filename from a
			// fresh ULID prefix, so two goroutines never collide on
			// a path — but they still race on directory entry
			// creation, fsync scheduling, and every other kernel
			// resource the filesystem serializes.
			w, err := NewWriter(dir, WithSegmentMaxBytes(contentionSegmentMaxBytes))
			if err != nil {
				t.Errorf("worker %d: NewWriter: %v", workerID, err)
				errorCount.Add(1)
				return
			}
			defer func() {
				totalRollCount.Add(w.RollCount())
				if cerr := w.Close(); cerr != nil {
					t.Errorf("worker %d: Close: %v", workerID, cerr)
					errorCount.Add(1)
				}
			}()

			for j := 0; j < contentionAppendsPerWriter; j++ {
				tx, group := makeGroup(t, contentionGroupSize)
				committedTx, err := w.Append(group)
				if err != nil {
					errorCount.Add(1)
					t.Errorf("worker %d append %d: %v", workerID, j, err)
					continue
				}
				if committedTx != tx {
					errorCount.Add(1)
					t.Errorf("worker %d append %d: committedTx %s != tx %s", workerID, j, committedTx, tx)
					continue
				}
				expectedTxs.Store(tx, contentionGroupSize)
				successCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if errorCount.Load() > 0 {
		t.Fatalf("contention run had %d errors (see preceding lines)", errorCount.Load())
	}
	wantGroups := int64(contentionWriters * contentionAppendsPerWriter)
	if successCount.Load() != wantGroups {
		t.Fatalf("committed groups: got %d want %d", successCount.Load(), wantGroups)
	}

	segs := listSegments(t, dir)
	// With 50 writers each producing at least one segment, plus a
	// handful of rollovers per writer under the tight cap, we expect
	// well over 50 segments on disk. Guarding against "0 rollovers"
	// catches a regression where the size cap is silently ignored.
	if len(segs) < contentionWriters {
		t.Fatalf("expected at least %d segments (one per writer), got %d", contentionWriters, len(segs))
	}
	if totalRollCount.Load() == 0 {
		t.Fatalf("no rollovers fired; contentionSegmentMaxBytes too generous for this workload")
	}
	t.Logf("observed %d segments across %d writers (total rollovers: %d)",
		len(segs), contentionWriters, totalRollCount.Load())

	// --- AC4: rollover preserves causality. For every tx, all its
	// datoms must live inside exactly one segment file. A violation
	// here means Append split a group across the rollover boundary —
	// the specific failure mode the bead calls out.
	txToSeg := map[string]string{}
	datomsPerTx := map[string]int{}
	for _, seg := range segs {
		path := filepath.Join(dir, seg)
		txs := readSegmentTxSet(t, path)
		for tx, count := range txs {
			if prev, ok := txToSeg[tx]; ok && prev != seg {
				t.Fatalf("tx %s straddles segments %s and %s", tx, prev, seg)
			}
			txToSeg[tx] = seg
			datomsPerTx[tx] += count
		}
	}

	// --- AC1: every expected tx landed in exactly one segment with
	// the full group size. Missing or short-counted txs are logged
	// by explicit name so the failure is easy to triage.
	expected := expectedTxsMap(&expectedTxs)
	for tx, want := range expected {
		got, ok := datomsPerTx[tx]
		if !ok {
			t.Errorf("tx %s missing from every segment in dir", tx)
			continue
		}
		if got != want {
			t.Errorf("tx %s datom count: got %d want %d", tx, got, want)
		}
	}
	if len(datomsPerTx) != len(expected) {
		t.Errorf("distinct txs on disk: got %d want %d (orphan txs present?)", len(datomsPerTx), len(expected))
	}

	// --- AC2 + AC3 + AC5: drain every segment through the merge-sort
	// reader and assert global tx-ULID order, per-datom checksum
	// validity, and per-tx contiguity in the merged stream.
	segPaths := make([]string, 0, len(segs))
	for _, seg := range segs {
		segPaths = append(segPaths, filepath.Join(dir, seg))
	}
	all, err := ReadAll(segPaths)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	wantDatoms := int(wantGroups) * contentionGroupSize
	if len(all) != wantDatoms {
		t.Fatalf("reader datom total: got %d want %d", len(all), wantDatoms)
	}
	assertReaderInvariants(t, all, expected)
}

// TestContention_MergeSortAcrossManySegments is a narrower stress test
// aimed squarely at the reader's k-way merge. Even if the primary test
// above passes, this one fails fast if the merge comparator regresses
// in a way that only shows up when the heap holds a large number of
// roughly-equal-rank streams.
func TestContention_MergeSortAcrossManySegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Very tight per-writer cap forces each append onto a fresh
	// segment (see TestRollover_EmptySegmentAcceptsOversizedGroup for
	// the "oversized group into empty segment" guarantee). Combined
	// with many independent writers, this produces a fan-in fan-out
	// workload the merge-sort has to handle cleanly.
	const perGoroutine = 10
	var wg sync.WaitGroup
	wg.Add(contentionWriters)
	for i := 0; i < contentionWriters; i++ {
		go func() {
			defer wg.Done()
			w, err := NewWriter(dir, WithSegmentMaxBytes(128))
			if err != nil {
				t.Errorf("NewWriter: %v", err)
				return
			}
			defer w.Close()
			for j := 0; j < perGoroutine; j++ {
				_, g := makeGroup(t, 2)
				if _, err := w.Append(g); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	segs := listSegments(t, dir)
	if len(segs) < contentionWriters*perGoroutine/2 {
		// We expect roughly one segment per Append under the tight
		// cap; allow some slack for the first Append of each Writer
		// (which never rolls).
		t.Logf("note: only %d segments from %d expected appends (cap may be too loose)",
			len(segs), contentionWriters*perGoroutine)
	}

	paths := make([]string, 0, len(segs))
	for _, s := range segs {
		paths = append(paths, filepath.Join(dir, s))
	}
	merged, err := ReadAll(paths)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if want := contentionWriters * perGoroutine * 2; len(merged) != want {
		t.Fatalf("merged datom total: got %d want %d", len(merged), want)
	}

	// Checksum + global tx-ascending invariant from the reader.
	var lastTx string
	for i, d := range merged {
		if err := d.Verify(); err != nil {
			t.Errorf("merged[%d] verify: %v", i, err)
		}
		if d.Tx < lastTx {
			t.Errorf("merged[%d] tx %s < previous %s (reader not tx-sorted)", i, d.Tx, lastTx)
		}
		lastTx = d.Tx
	}
}

// assertReaderInvariants is the shared verifier for the merged output
// of a contention run. It enforces:
//
//   - Every datom's checksum verifies (AC3).
//   - The stream is globally tx-ULID sorted (AC2).
//   - Each tx's datoms are contiguous in the stream — they arrived as
//     one atomic group and must leave the merge in one uninterrupted
//     run (AC5).
//   - The total distinct-tx count and per-tx datom count match the
//     expected map (AC1).
func assertReaderInvariants(t *testing.T, datoms []datom.Datom, expected map[string]int) {
	t.Helper()

	// Checksum + tx-ascending.
	var lastTx string
	for i, d := range datoms {
		if err := d.Verify(); err != nil {
			t.Errorf("datom %d checksum verify: %v", i, err)
		}
		if d.Tx < lastTx {
			t.Errorf("datom %d tx %s < previous %s (reader out of order)", i, d.Tx, lastTx)
		}
		lastTx = d.Tx
	}

	// Per-tx contiguity. Walk the stream and record the first index
	// at which each tx appears; a later reappearance of a tx we've
	// already seen means another tx slipped between the two halves
	// of its group — an atomicity violation.
	seen := map[string]bool{}
	closed := map[string]bool{}
	var runTx string
	countsByTx := map[string]int{}
	for _, d := range datoms {
		if d.Tx != runTx {
			if runTx != "" {
				closed[runTx] = true
			}
			if closed[d.Tx] {
				t.Errorf("tx %s appears in multiple non-contiguous runs (interleaved group)", d.Tx)
			}
			seen[d.Tx] = true
			runTx = d.Tx
		}
		countsByTx[d.Tx]++
	}

	// Every expected tx landed with the right datom count.
	for tx, want := range expected {
		if got := countsByTx[tx]; got != want {
			t.Errorf("tx %s final count: got %d want %d", tx, got, want)
		}
	}
	if len(countsByTx) != len(expected) {
		t.Errorf("distinct txs in reader: got %d want %d", len(countsByTx), len(expected))
	}
}

// expectedTxsMap drains a sync.Map into a plain map[string]int so the
// invariant check can iterate deterministically.
func expectedTxsMap(m *sync.Map) map[string]int {
	out := map[string]int{}
	m.Range(func(k, v interface{}) bool {
		out[k.(string)] = v.(int)
		return true
	})
	return out
}
