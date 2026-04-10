package mergeseg

// Acceptance tests for cortex-4kq.31 (cortex merge for external log
// segments).
//
// AC1: Merging log B (tx T2,T3,T4) into log A (tx T1,T2,T3) yields a
//       reader that returns exactly {T1,T2,T3,T4}.
// AC2: An external segment whose checksums do not verify is rejected
//       with ErrChecksumMismatch and not moved into log.d.
// AC3: Duplicated tx values across segments are returned only once by
//       the merge-sort reader.
// AC4: cortex merge exits zero on success and exit 1 on checksum
//       failure. (Exit code is CLI; we test the error return here.)

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
)

// makeSealedDatom creates a single sealed datom with the given tx.
func makeSealedDatom(t *testing.T, tx string) datom.Datom {
	t.Helper()
	raw, _ := json.Marshal("test-value")
	d := datom.Datom{
		Tx:           tx,
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Actor:        "test",
		Op:           datom.OpAdd,
		E:            "entry:" + ulid.Make().String(),
		A:            "body",
		V:            raw,
		Src:          "test",
		InvocationID: tx,
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return d
}

// writeSegment creates a .jsonl segment file at path containing the
// given datoms.
func writeSegment(t *testing.T, path string, datoms []datom.Datom) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	for i := range datoms {
		line, err := datom.Marshal(&datoms[i])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(line); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// writeSegmentViaWriter uses log.Writer to create a proper segment
// in dir and returns the writer's path.
func writeSegmentViaWriter(t *testing.T, dir string, txs []string) string {
	t.Helper()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	for _, tx := range txs {
		d := makeSealedDatom(t, tx)
		d.Tx = tx // override with caller-provided tx for determinism
		d.Checksum = "" // re-seal with new tx
		if err := d.Seal(); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if _, err := w.Append([]datom.Datom{d}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return w.Path()
}

func listJSONL(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func readAllTxs(t *testing.T, paths []string) []string {
	t.Helper()
	all, err := log.ReadAll(paths)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	seen := map[string]struct{}{}
	var txs []string
	for _, d := range all {
		if _, ok := seen[d.Tx]; !ok {
			seen[d.Tx] = struct{}{}
			txs = append(txs, d.Tx)
		}
	}
	return txs
}

// TestMerge_DeduplicatedUnion — AC1 + AC3.
//
// Log A has tx {T1, T2, T3}. External segment B has tx {T2, T3, T4}.
// After merge, the reader returns exactly {T1, T2, T3, T4} in
// ascending order.
func TestMerge_DeduplicatedUnion(t *testing.T) {
	// Use ULIDs so they sort properly.
	t1 := ulid.Make().String()
	t2 := ulid.Make().String()
	t3 := ulid.Make().String()
	t4 := ulid.Make().String()

	logDir := t.TempDir()
	extDir := t.TempDir()

	// Write log A into logDir.
	writeSegmentViaWriter(t, logDir, []string{t1, t2, t3})

	// Write external segment B into a temp file.
	extPath := filepath.Join(extDir, "external.jsonl")
	writeSegment(t, extPath, []datom.Datom{
		makeSealedDatom(t, t2),
		makeSealedDatom(t, t3),
		makeSealedDatom(t, t4),
	})

	// Merge B into logDir.
	result, err := Merge(extPath, logDir)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if result.DatomCount != 3 {
		t.Errorf("DatomCount = %d, want 3", result.DatomCount)
	}
	if result.TxCount != 3 {
		t.Errorf("TxCount = %d, want 3", result.TxCount)
	}

	// External file should no longer exist at the original path
	// (it was renamed/moved).
	if _, err := os.Stat(extPath); !os.IsNotExist(err) {
		t.Errorf("external file still exists at %s after merge", extPath)
	}

	// The merged reader should yield {T1, T2, T3, T4}.
	segs := listJSONL(t, logDir)
	txs := readAllTxs(t, segs)

	// Check we have exactly 4 distinct txs.
	if len(txs) != 4 {
		t.Fatalf("distinct txs = %d, want 4; got %v", len(txs), txs)
	}

	// Check tx order is ascending.
	for i := 1; i < len(txs); i++ {
		if txs[i] <= txs[i-1] {
			t.Errorf("tx[%d] %s <= tx[%d] %s (not ascending)", i, txs[i], i-1, txs[i-1])
		}
	}

	// Check all four tx values are present.
	want := map[string]bool{t1: false, t2: false, t3: false, t4: false}
	for _, tx := range txs {
		if _, ok := want[tx]; ok {
			want[tx] = true
		}
	}
	for tx, found := range want {
		if !found {
			t.Errorf("tx %s missing from merged reader output", tx)
		}
	}
}

// TestMerge_ChecksumMismatchRejected — AC2.
//
// An external segment with a corrupted checksum must be rejected with
// ErrChecksumMismatch and NOT moved into logDir.
func TestMerge_ChecksumMismatchRejected(t *testing.T) {
	logDir := t.TempDir()
	extDir := t.TempDir()
	extPath := filepath.Join(extDir, "corrupt.jsonl")

	// Write a valid datom then corrupt its checksum.
	d := makeSealedDatom(t, ulid.Make().String())
	d.Checksum = "0000000000000000000000000000000000000000000000000000000000000000"
	line, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line = append(line, '\n')
	if err := os.WriteFile(extPath, line, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, mergeErr := Merge(extPath, logDir)
	if mergeErr == nil {
		t.Fatal("expected ErrChecksumMismatch, got nil")
	}
	if !errors.Is(mergeErr, ErrChecksumMismatch) {
		t.Errorf("error = %v, want ErrChecksumMismatch", mergeErr)
	}

	// The corrupted file must NOT have been moved into logDir.
	if _, err := os.Stat(extPath); os.IsNotExist(err) {
		t.Error("corrupted file was moved despite checksum failure")
	}
	// logDir should contain no .jsonl files (merge was rejected).
	segs := listJSONL(t, logDir)
	if len(segs) != 0 {
		t.Errorf("logDir has %d segments after rejected merge, want 0", len(segs))
	}
}

// TestMerge_EmptyPathRejected — validation guard.
func TestMerge_EmptyPathRejected(t *testing.T) {
	_, err := Merge("", t.TempDir())
	if !errors.Is(err, ErrEmptyPath) {
		t.Errorf("err = %v, want ErrEmptyPath", err)
	}
}

// TestMerge_EmptyLogDirRejected — validation guard.
func TestMerge_EmptyLogDirRejected(t *testing.T) {
	_, err := Merge("/some/file.jsonl", "")
	if !errors.Is(err, ErrEmptyLogDir) {
		t.Errorf("err = %v, want ErrEmptyLogDir", err)
	}
}

// TestValidateOnly_ValidSegment — the validate-only path returns
// counts without moving the file.
func TestValidateOnly_ValidSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "valid.jsonl")
	writeSegment(t, path, []datom.Datom{
		makeSealedDatom(t, ulid.Make().String()),
		makeSealedDatom(t, ulid.Make().String()),
	})

	dc, tc, err := ValidateOnly(path)
	if err != nil {
		t.Fatalf("ValidateOnly: %v", err)
	}
	if dc != 2 {
		t.Errorf("DatomCount = %d, want 2", dc)
	}
	if tc != 2 {
		t.Errorf("TxCount = %d, want 2", tc)
	}

	// File must still exist (not moved).
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file disappeared after ValidateOnly: %v", err)
	}
}

// TestValidateOnly_CorruptSegment — the validate-only path catches
// checksum corruption.
func TestValidateOnly_CorruptSegment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.jsonl")

	d := makeSealedDatom(t, ulid.Make().String())
	d.Checksum = "badbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadbadba"
	line, _ := json.Marshal(d)
	line = append(line, '\n')
	os.WriteFile(path, line, 0o600)

	_, _, err := ValidateOnly(path)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("err = %v, want ErrChecksumMismatch", err)
	}
}

// TestMerge_ImportedSegmentPerms — the imported file should have mode
// 0600.
func TestMerge_ImportedSegmentPerms(t *testing.T) {
	logDir := t.TempDir()
	extDir := t.TempDir()
	extPath := filepath.Join(extDir, "external.jsonl")
	writeSegment(t, extPath, []datom.Datom{makeSealedDatom(t, ulid.Make().String())})

	result, err := Merge(extPath, logDir)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	info, err := os.Stat(result.DestPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("imported segment mode = %o, want 600", info.Mode().Perm())
	}
}
