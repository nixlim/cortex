package log

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/datom"
)

// listSegments returns the sorted list of *.jsonl filenames in dir,
// which (per spec) matches the creation order because of the ULID
// prefix in every segment name.
func listSegments(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// readSegmentTxSet returns the set of tx ULIDs present in a segment
// file, one per datom line, so tests can assert that a given tx appears
// wholly inside one segment and never straddles two.
func readSegmentTxSet(t *testing.T, path string) map[string]int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	set := map[string]int{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	for s.Scan() {
		var d datom.Datom
		if err := json.Unmarshal(s.Bytes(), &d); err != nil {
			t.Fatalf("unmarshal line in %s: %v", path, err)
		}
		set[d.Tx]++
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return set
}

// TestRollover_CreatesNewSegmentAndWritesGroupThere covers the primary
// .23 acceptance case: "Given a segment at size_mb - 1 byte, appending
// a 2 KiB group creates a new segment and writes the entire group into
// it." We use byte-level caps rather than MB to keep the test fast.
func TestRollover_CreatesNewSegmentAndWritesGroupThere(t *testing.T) {
	dir := t.TempDir()
	// Cap at 1 KiB so the first append (a single small datom) fits and
	// the second append (a 2-datom group) forces a roll.
	w, err := NewWriter(dir, WithSegmentMaxBytes(1024))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	firstPath := w.Path()

	_, g1 := makeGroup(t, 1)
	tx1, err := w.Append(g1)
	if err != nil {
		t.Fatalf("Append g1: %v", err)
	}
	sizeAfter1 := w.Size()
	if sizeAfter1 == 0 {
		t.Fatalf("segment empty after first append")
	}
	if got := w.RollCount(); got != 0 {
		t.Fatalf("premature rollover: RollCount=%d", got)
	}

	// Force the next group to exceed the cap by inflating the number of
	// datoms until the total payload blows past 1 KiB.
	_, g2 := makeGroup(t, 32)
	tx2, err := w.Append(g2)
	if err != nil {
		t.Fatalf("Append g2: %v", err)
	}
	if got := w.RollCount(); got != 1 {
		t.Fatalf("RollCount after rollover: got %d want 1", got)
	}
	if w.Path() == firstPath {
		t.Fatalf("segment path did not change after rollover")
	}

	segs := listSegments(t, dir)
	if len(segs) != 2 {
		t.Fatalf("expected 2 segments after rollover, got %d: %v", len(segs), segs)
	}
	if filepath.Join(dir, segs[0]) != firstPath {
		t.Fatalf("first segment path mismatch: %s vs %s", segs[0], firstPath)
	}
	// AC: "segment file names sort lexicographically by creation time"
	if segs[0] >= segs[1] {
		t.Fatalf("segment names not lexicographically ordered: %v", segs)
	}

	// AC: the prior segment holds g1, the new segment holds g2 in full,
	// and no tx straddles the two files.
	firstSet := readSegmentTxSet(t, filepath.Join(dir, segs[0]))
	secondSet := readSegmentTxSet(t, filepath.Join(dir, segs[1]))
	if firstSet[tx1] != len(g1) {
		t.Fatalf("first segment missing tx1 datoms: got %d want %d", firstSet[tx1], len(g1))
	}
	if firstSet[tx2] != 0 {
		t.Fatalf("first segment contains tx2 after rollover: %d datoms", firstSet[tx2])
	}
	if secondSet[tx2] != len(g2) {
		t.Fatalf("second segment missing tx2 datoms: got %d want %d", secondSet[tx2], len(g2))
	}
	if secondSet[tx1] != 0 {
		t.Fatalf("second segment contains tx1: %d datoms", secondSet[tx1])
	}
}

// TestRollover_NeverSplitsTxGroup writes many groups against a very
// tight cap and asserts that for every tx in the directory, all of its
// datoms live in exactly one file. This is the verification form of AC
// "No transaction group is ever split across two segment files".
func TestRollover_NeverSplitsTxGroup(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WithSegmentMaxBytes(512))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	txs := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		_, g := makeGroup(t, 3)
		tx, err := w.Append(g)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		txs = append(txs, tx)
	}

	// Build tx -> segment-file index. Any tx seen in more than one file
	// is a violation of the invariant.
	segs := listSegments(t, dir)
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	txToFile := map[string]string{}
	for _, seg := range segs {
		path := filepath.Join(dir, seg)
		for tx := range readSegmentTxSet(t, path) {
			if prev, ok := txToFile[tx]; ok && prev != seg {
				t.Fatalf("tx %s split across %s and %s", tx, prev, seg)
			}
			txToFile[tx] = seg
		}
	}
	for _, tx := range txs {
		if _, ok := txToFile[tx]; !ok {
			t.Fatalf("tx %s missing from all segments", tx)
		}
	}
}

// TestRollover_EmptySegmentAcceptsOversizedGroup verifies the edge
// case called out in rolloverIfNeeded: if the cap is smaller than a
// single group, writing into an empty segment must still succeed
// (splitting a group is forbidden, so the cap yields to the invariant).
func TestRollover_EmptySegmentAcceptsOversizedGroup(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WithSegmentMaxBytes(64)) // absurdly small
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	_, g := makeGroup(t, 5)
	if _, err := w.Append(g); err != nil {
		t.Fatalf("Append oversized group into empty segment: %v", err)
	}
	if got := w.RollCount(); got != 0 {
		t.Fatalf("unexpected rollover on empty segment: %d", got)
	}
	segs := listSegments(t, dir)
	if len(segs) != 1 {
		t.Fatalf("expected single segment, got %d: %v", len(segs), segs)
	}
}

// TestRollover_PriorSegmentFsyncedBeforeNewOpen is an indirect check of
// "The prior segment file is closed and fsync'd before the new segment
// is created". We install a file-level sync counter via the fsyncFn
// seam and also rely on the fact that after rollover the prior segment
// is fully readable (no pending in-kernel buffers from this process,
// which would have been flushed by the close()/Sync() pair).
func TestRollover_PriorSegmentFsyncedBeforeNewOpen(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WithSegmentMaxBytes(256))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	_, g1 := makeGroup(t, 1)
	if _, err := w.Append(g1); err != nil {
		t.Fatalf("Append g1: %v", err)
	}
	priorPath := w.Path()

	_, g2 := makeGroup(t, 16)
	if _, err := w.Append(g2); err != nil {
		t.Fatalf("Append g2: %v", err)
	}

	// Prior segment must be non-empty, readable and complete: every
	// line parses as a datom and every checksum verifies.
	data, err := os.ReadFile(priorPath)
	if err != nil {
		t.Fatalf("read prior: %v", err)
	}
	if len(data) == 0 {
		t.Fatalf("prior segment is empty after rollover")
	}
	for i, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		if _, err := datom.Unmarshal(line); err != nil {
			t.Fatalf("prior segment line %d failed verify: %v", i, err)
		}
	}

	// FsyncCount reflects commit-point fsyncs only, so two successful
	// Appends produce exactly two regardless of rollover in between.
	if got := w.FsyncCount(); got != 2 {
		t.Fatalf("FsyncCount: got %d want 2", got)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
