package log

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// writeSegmentFromLines constructs a segment file from pre-serialized
// JSONL byte slices. It does not go through Writer.Append so tests can
// inject corrupt middle lines without tripping any validation in the
// happy-path code.
func writeSegmentFromLines(t *testing.T, path string, lines [][]byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	for _, ln := range lines {
		if _, err := f.Write(ln); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func sealedLine(t *testing.T, tx string) []byte {
	t.Helper()
	d := datom.Datom{
		Tx:           tx,
		Ts:           "2026-04-10T00:00:00Z",
		Actor:        "test",
		Op:           datom.OpAdd,
		E:            "entry:quarantine",
		A:            "body",
		V:            json.RawMessage(`"ok"`),
		Src:          "quarantine_test",
		InvocationID: "01HPTESTINVOCATION0000000000",
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	b, err := datom.Marshal(&d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// corruptedLine produces a line that parses as JSON but has a
// deliberately wrong checksum. This is the classic "corrupt middle
// datom" scenario from AC1 of cortex-4kq.28.
func corruptedLine(t *testing.T) []byte {
	t.Helper()
	good := sealedLine(t, ulid.Make().String())
	// Flip one byte inside the JSON value portion. We locate the
	// `"v":"ok"` substring and change it, which will fail the
	// checksum but still parse as JSON.
	idx := strings.Index(string(good), `"v":"ok"`)
	if idx < 0 {
		t.Fatalf("bad fixture: cannot find v field in %q", good)
	}
	bad := make([]byte, len(good))
	copy(bad, good)
	// Replace "ok" with "OK" — valid JSON, invalid checksum.
	bad[idx+len(`"v":"`)] = 'O'
	return bad
}

// TestLoad_QuarantinesCorruptSegment covers AC1 + AC3 of cortex-4kq.28:
// a segment with a corrupt middle datom is moved into .quarantine/,
// the remaining segments still load, and an ops.log entry is produced
// naming the segment and the failing offset.
func TestLoad_QuarantinesCorruptSegment(t *testing.T) {
	dir := t.TempDir()

	healthyPath := filepath.Join(dir, "01HHEALTHY00000000000000000-pid1.jsonl")
	writeSegmentFromLines(t, healthyPath, [][]byte{
		sealedLine(t, ulid.Make().String()),
		sealedLine(t, ulid.Make().String()),
	})

	corruptPath := filepath.Join(dir, "01HCORRUPT00000000000000000-pid1.jsonl")
	writeSegmentFromLines(t, corruptPath, [][]byte{
		sealedLine(t, ulid.Make().String()),
		corruptedLine(t),
		sealedLine(t, ulid.Make().String()),
	})

	type event struct {
		level, component, message, entity string
		err                                error
	}
	var events []event
	rec := func(level, component, message, entity string, err error) {
		events = append(events, event{level, component, message, entity, err})
	}

	report, err := Load(dir, LoadOptions{OpsRecord: rec})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(report.Quarantined) != 1 {
		t.Fatalf("quarantine count: got %d want 1", len(report.Quarantined))
	}
	if report.Quarantined[0].OriginalPath != corruptPath {
		t.Fatalf("quarantined wrong segment: %s", report.Quarantined[0].OriginalPath)
	}
	if report.Quarantined[0].Fault.Line != 2 {
		t.Fatalf("fault line: got %d want 2", report.Quarantined[0].Fault.Line)
	}
	if report.Quarantined[0].Fault.Offset <= 0 {
		t.Fatalf("fault offset: got %d want >0", report.Quarantined[0].Fault.Offset)
	}

	// Healthy segment survived.
	if len(report.Healthy) != 1 || report.Healthy[0] != healthyPath {
		t.Fatalf("healthy: %v", report.Healthy)
	}
	if _, err := os.Stat(healthyPath); err != nil {
		t.Fatalf("healthy segment disappeared: %v", err)
	}

	// Corrupt segment moved, not deleted.
	if _, err := os.Stat(corruptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt segment still in place: %v", err)
	}
	if _, err := os.Stat(report.Quarantined[0].QuarantinePath); err != nil {
		t.Fatalf("quarantined file missing: %v", err)
	}
	if !strings.Contains(report.Quarantined[0].QuarantinePath, ".quarantine") {
		t.Fatalf("quarantine path wrong: %s", report.Quarantined[0].QuarantinePath)
	}

	// Ops.log entry emitted for the quarantine action naming the
	// segment and a nonzero offset.
	found := false
	for _, e := range events {
		if e.level == "WARN" && strings.Contains(e.message, "segment quarantined") &&
			e.entity == corruptPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no quarantine ops.log event; events=%+v", events)
	}
}

// TestLoad_NoQuarantineOnHealthyStartup covers AC4: a healthy startup
// produces zero quarantine actions and no ops.log WARN events.
func TestLoad_NoQuarantineOnHealthyStartup(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		path := filepath.Join(dir, "01HH"+strings.Repeat("A", 19+i)+"-pid1.jsonl")
		writeSegmentFromLines(t, path, [][]byte{sealedLine(t, ulid.Make().String())})
	}

	var events []string
	rec := func(level, _, message, _ string, _ error) {
		events = append(events, level+":"+message)
	}
	report, err := Load(dir, LoadOptions{OpsRecord: rec})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(report.Quarantined) != 0 {
		t.Fatalf("unexpected quarantine on healthy dir: %+v", report.Quarantined)
	}
	if len(report.Healthy) != 3 {
		t.Fatalf("healthy count: got %d want 3", len(report.Healthy))
	}
	for _, ev := range events {
		if strings.HasPrefix(ev, "WARN") || strings.HasPrefix(ev, "ERROR") {
			t.Fatalf("unexpected ops event: %s", ev)
		}
	}
}

// TestLoad_TxCollisionReturnsError covers AC2: two segments each
// containing the same tx ULID cause Load to return ErrTxCollision and
// the report lists both segment paths under that tx.
func TestLoad_TxCollisionReturnsError(t *testing.T) {
	dir := t.TempDir()
	sharedTx := ulid.Make().String()

	a := filepath.Join(dir, "01HAAAA00000000000000000000-pid1.jsonl")
	b := filepath.Join(dir, "01HBBBB00000000000000000000-pid2.jsonl")
	writeSegmentFromLines(t, a, [][]byte{sealedLine(t, sharedTx)})
	writeSegmentFromLines(t, b, [][]byte{sealedLine(t, sharedTx)})

	report, err := Load(dir, LoadOptions{})
	if !errors.Is(err, ErrTxCollision) {
		t.Fatalf("expected ErrTxCollision, got %v", err)
	}
	if len(report.Collisions) != 1 {
		t.Fatalf("collisions: got %d want 1", len(report.Collisions))
	}
	if report.Collisions[0].Tx != sharedTx {
		t.Fatalf("collision tx: got %q want %q", report.Collisions[0].Tx, sharedTx)
	}
	paths := report.Collisions[0].Paths
	if len(paths) != 2 {
		t.Fatalf("collision paths: got %d want 2: %v", len(paths), paths)
	}
	// Both paths are present (order is stable, lex sort).
	if paths[0] != a || paths[1] != b {
		t.Fatalf("collision paths mismatch: %v", paths)
	}
}

// TestScanSegment_CleanSegmentReturnsNil is a narrow unit test for the
// per-file scanner used inside Load.
func TestScanSegment_CleanSegmentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clean.jsonl")
	writeSegmentFromLines(t, path, [][]byte{
		sealedLine(t, ulid.Make().String()),
		sealedLine(t, ulid.Make().String()),
	})
	fault, err := ScanSegment(path)
	if err != nil {
		t.Fatalf("ScanSegment: %v", err)
	}
	if fault != nil {
		t.Fatalf("expected nil fault, got %v", fault)
	}
}

// TestScanSegment_ReportsFirstFault exercises ScanSegment on a file
// with a valid first line and a corrupt second line, asserting the
// returned fault names line 2 and a positive offset.
func TestScanSegment_ReportsFirstFault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	good := sealedLine(t, ulid.Make().String())
	writeSegmentFromLines(t, path, [][]byte{good, corruptedLine(t)})
	fault, err := ScanSegment(path)
	if err != nil {
		t.Fatalf("ScanSegment: %v", err)
	}
	if fault == nil {
		t.Fatalf("expected fault, got nil")
	}
	if fault.Line != 2 {
		t.Fatalf("fault line: got %d want 2", fault.Line)
	}
	if fault.Offset != int64(len(good)) {
		t.Fatalf("fault offset: got %d want %d", fault.Offset, len(good))
	}
}

// TestScanSegment_OutOfOrderTxReturnsFault verifies that a segment with
// valid per-datom checksums but decreasing tx ULIDs is flagged as a
// ScanFault. This is the class of corruption that caused real-world
// SELFHEAL_FAILED: the merge-sort reader detects the violation inside
// replay but ScanSegment (called by log.Load) did not, so the bad
// segment passed quarantine and reached the reader.
func TestScanSegment_OutOfOrderTxReturnsFault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ooo.jsonl")

	tx1 := ulid.Make().String() // smaller (older)
	tx2 := ulid.Make().String() // larger (newer); tx1 < tx2 by construction

	// Write tx2 first, then tx1 — strictly out of order.
	writeSegmentFromLines(t, path, [][]byte{
		sealedLine(t, tx2),
		sealedLine(t, tx1),
	})

	fault, err := ScanSegment(path)
	if err != nil {
		t.Fatalf("ScanSegment: %v", err)
	}
	if fault == nil {
		t.Fatal("expected ScanFault for out-of-order tx, got nil")
	}
	if fault.Line != 2 {
		t.Fatalf("fault line: got %d want 2", fault.Line)
	}
	if !strings.Contains(fault.Err.Error(), "tx") {
		t.Fatalf("fault error should mention tx ordering: %v", fault.Err)
	}
}

// TestLoad_QuarantinesOutOfOrderSegment verifies that log.Load moves a
// segment with valid checksums but decreasing tx ULIDs into quarantine,
// preventing it from entering the Healthy list and reaching the reader.
func TestLoad_QuarantinesOutOfOrderSegment(t *testing.T) {
	dir := t.TempDir()

	healthyPath := filepath.Join(dir, "01HHEALTHY00000000000000001-pid1.jsonl")
	writeSegmentFromLines(t, healthyPath, [][]byte{
		sealedLine(t, ulid.Make().String()),
		sealedLine(t, ulid.Make().String()),
	})

	oooPath := filepath.Join(dir, "01HHEALTHY00000000000000002-pid2.jsonl")
	tx1 := ulid.Make().String() // smaller
	tx2 := ulid.Make().String() // larger; write tx2 first → out of order
	writeSegmentFromLines(t, oooPath, [][]byte{
		sealedLine(t, tx2),
		sealedLine(t, tx1),
	})

	type opsEvent struct{ level, message, entity string }
	var events []opsEvent
	rec := func(level, _, message, entity string, _ error) {
		events = append(events, opsEvent{level, message, entity})
	}

	report, err := Load(dir, LoadOptions{OpsRecord: rec})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(report.Quarantined) != 1 {
		t.Fatalf("quarantine count: got %d want 1 (quarantined=%v healthy=%v)",
			len(report.Quarantined), report.Quarantined, report.Healthy)
	}
	if report.Quarantined[0].OriginalPath != oooPath {
		t.Fatalf("quarantined wrong segment: got %s want %s",
			report.Quarantined[0].OriginalPath, oooPath)
	}
	if report.Quarantined[0].Fault.Line != 2 {
		t.Fatalf("fault line: got %d want 2", report.Quarantined[0].Fault.Line)
	}

	if len(report.Healthy) != 1 || report.Healthy[0] != healthyPath {
		t.Fatalf("healthy: got %v want [%s]", report.Healthy, healthyPath)
	}

	found := false
	for _, e := range events {
		if e.level == "WARN" && strings.Contains(e.message, "segment quarantined") && e.entity == oooPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no quarantine ops event for out-of-order segment; events=%+v", events)
	}
}

// TestQuarantine_PreservesPriorQuarantinedFiles ensures the name
// collision handling does not clobber a pre-existing quarantined file.
func TestQuarantine_PreservesPriorQuarantinedFiles(t *testing.T) {
	dir := t.TempDir()
	// Prepare an existing quarantine subdir with a file of the same
	// base name we're about to move.
	qdir := filepath.Join(dir, ".quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatalf("mkdir quarantine: %v", err)
	}
	existing := filepath.Join(qdir, "collide.jsonl")
	if err := os.WriteFile(existing, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	src := filepath.Join(dir, "collide.jsonl")
	if err := os.WriteFile(src, []byte("new-bad-content"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}

	dest, err := Quarantine(src, dir)
	if err != nil {
		t.Fatalf("Quarantine: %v", err)
	}
	if dest == existing {
		t.Fatalf("clobbered existing quarantined file")
	}
	// Sentinel untouched.
	b, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if string(b) != "sentinel" {
		t.Fatalf("sentinel overwritten: %q", string(b))
	}
	// Source is gone.
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source still in place: %v", err)
	}
}
