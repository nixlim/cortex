package log

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// writeDatomLine appends one sealed datom line (with trailing newline)
// to a file opened for append. Used to build synthetic segment fixtures
// without going through Writer.Append, so tests can craft exact byte
// layouts including torn tails.
func writeDatomLine(t *testing.T, path string, line []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func marshalDatom(t *testing.T, tx string) []byte {
	t.Helper()
	d := datom.Datom{
		Tx:           tx,
		Ts:           "2026-04-10T00:00:00Z",
		Actor:        "test",
		Op:           datom.OpAdd,
		E:            "entry:recover",
		A:            "body",
		V:            json.RawMessage(`"durable"`),
		Src:          "recover_test",
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

// TestValidateTail_TruncatesHalfWrittenLine covers AC1 of cortex-4kq.24:
// a segment whose final bytes are a half-written datom must be
// truncated to the byte offset of the last valid (checksummed) datom.
func TestValidateTail_TruncatesHalfWrittenLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01HVALIDTEST00000000000000-pid1.jsonl")

	// Two valid datoms followed by a garbage partial line (no newline,
	// not a valid JSON datom).
	l1 := marshalDatom(t, ulid.Make().String())
	l2 := marshalDatom(t, ulid.Make().String())
	writeDatomLine(t, path, l1)
	writeDatomLine(t, path, l2)
	expectedSafe, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	writeDatomLine(t, path, []byte(`{"tx":"torn","ts":"2026`)) // no newline

	report, err := ValidateTail(path, 0)
	if err != nil {
		t.Fatalf("ValidateTail: %v", err)
	}
	if !report.Truncated {
		t.Fatalf("expected Truncated=true, got report %+v", report)
	}
	if report.FinalSize != expectedSafe.Size() {
		t.Fatalf("FinalSize: got %d want %d", report.FinalSize, expectedSafe.Size())
	}
	if report.DatomsValidated != 2 {
		t.Fatalf("DatomsValidated: got %d want 2", report.DatomsValidated)
	}

	// On disk: size matches, file ends in '\n', last complete line
	// still verifies.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("post-stat: %v", err)
	}
	if info.Size() != expectedSafe.Size() {
		t.Fatalf("file size after truncate: got %d want %d", info.Size(), expectedSafe.Size())
	}
	data, _ := os.ReadFile(path)
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatalf("truncated file does not end in newline")
	}
}

// TestValidateTail_CleanTailUntouched covers AC2: a segment whose tail
// already verifies must not be modified, including its mtime.
func TestValidateTail_CleanTailUntouched(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01HVALIDCLEAN00000000000000-pid1.jsonl")
	writeDatomLine(t, path, marshalDatom(t, ulid.Make().String()))
	writeDatomLine(t, path, marshalDatom(t, ulid.Make().String()))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Backdate mtime so we can reliably detect an inadvertent rewrite
	// (most filesystems have sub-second mtime resolution now, but a
	// full second of cushion is bulletproof on all of them).
	pastMtime := before.ModTime().Add(-5 * time.Second)
	if err := os.Chtimes(path, pastMtime, pastMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	report, err := ValidateTail(path, 0)
	if err != nil {
		t.Fatalf("ValidateTail: %v", err)
	}
	if report.Truncated {
		t.Fatalf("expected Truncated=false on clean tail, got %+v", report)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("size changed: got %d want %d", after.Size(), before.Size())
	}
	if !after.ModTime().Equal(pastMtime) {
		t.Fatalf("mtime changed: got %v want %v", after.ModTime(), pastMtime)
	}
}

// TestValidateTail_RespectsWindowBudget covers AC4: validation of a
// large segment touches at most windowBytes bytes, proved by the
// BytesRead counter on the report.
func TestValidateTail_RespectsWindowBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01HVALIDBUDGET00000000000000-pid1.jsonl")

	// Write many datoms so the segment size dwarfs the validation
	// window. The goal here is not bytes-per-se but that BytesRead on
	// the report equals the window, not the whole file size.
	for i := 0; i < 2000; i++ {
		writeDatomLine(t, path, marshalDatom(t, ulid.Make().String()))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	const window = 4096
	if info.Size() <= window*4 {
		t.Fatalf("test fixture too small: %d bytes for window %d", info.Size(), window)
	}

	report, err := ValidateTail(path, window)
	if err != nil {
		t.Fatalf("ValidateTail: %v", err)
	}
	if report.Truncated {
		t.Fatalf("unexpected truncation of clean file")
	}
	if report.BytesRead > window {
		t.Fatalf("read %d bytes, window was %d", report.BytesRead, window)
	}
	if report.BytesRead == 0 {
		t.Fatalf("no bytes read; validator did not inspect tail")
	}
}

// TestBuildRecoveredDatom covers AC3: an audit datom describing the
// segment path and byte range removed is constructible from the
// TailReport and verifies cleanly.
func TestBuildRecoveredDatom(t *testing.T) {
	report := TailReport{
		Path:            "/tmp/seg.jsonl",
		OriginalSize:    4096,
		FinalSize:       4000,
		Truncated:       true,
		DatomsValidated: 12,
		LastTx:          ulid.Make().String(),
	}
	d, err := BuildRecoveredDatom(ulid.Make().String(), "2026-04-10T00:00:00Z",
		"cortex", "01HPTESTINVOCATION0000000000", report)
	if err != nil {
		t.Fatalf("BuildRecoveredDatom: %v", err)
	}
	if d.A != "log.recovered" {
		t.Fatalf("attribute: got %q want log.recovered", d.A)
	}
	if d.Src != "recover" {
		t.Fatalf("src: got %q want recover", d.Src)
	}
	if err := d.Verify(); err != nil {
		t.Fatalf("recovered datom failed verify: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(d.V, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got, _ := body["bytes_removed"].(float64); got != 96 {
		t.Fatalf("bytes_removed: got %v want 96", body["bytes_removed"])
	}
	if got, _ := body["path"].(string); got != "/tmp/seg.jsonl" {
		t.Fatalf("path: got %v want /tmp/seg.jsonl", body["path"])
	}
}

// TestRecoverDir_ReturnsSortedReports checks that RecoverDir visits
// every *.jsonl segment in lexicographic (ULID) order and produces one
// report per file. Segments that are already clean return
// Truncated=false; segments with a torn tail come back truncated and
// shrunken.
func TestRecoverDir_ReturnsSortedReports(t *testing.T) {
	dir := t.TempDir()

	// Two clean segments and one with a torn tail. Filenames chosen
	// so lexicographic order is deterministic.
	clean1 := filepath.Join(dir, "01HAAA000000000000000000000-pid1.jsonl")
	clean2 := filepath.Join(dir, "01HBBB000000000000000000000-pid1.jsonl")
	torn := filepath.Join(dir, "01HCCC000000000000000000000-pid1.jsonl")

	writeDatomLine(t, clean1, marshalDatom(t, ulid.Make().String()))
	writeDatomLine(t, clean2, marshalDatom(t, ulid.Make().String()))
	writeDatomLine(t, torn, marshalDatom(t, ulid.Make().String()))
	writeDatomLine(t, torn, []byte(`not-a-datom`)) // torn suffix

	reports, err := RecoverDir(dir, 0)
	if err != nil {
		t.Fatalf("RecoverDir: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("report count: got %d want 3", len(reports))
	}
	if filepath.Base(reports[0].Path) >= filepath.Base(reports[1].Path) ||
		filepath.Base(reports[1].Path) >= filepath.Base(reports[2].Path) {
		t.Fatalf("reports not sorted: %s %s %s",
			reports[0].Path, reports[1].Path, reports[2].Path)
	}
	if reports[0].Truncated || reports[1].Truncated {
		t.Fatalf("clean segments flagged as truncated")
	}
	if !reports[2].Truncated {
		t.Fatalf("torn segment not truncated")
	}
}

// TestValidateTail_UnrecoverableLeavesFileIntact exercises the spec
// constraint "Must not delete the segment file even if the entire tail
// is unreadable": when the validation window contains no verifiable
// datoms and the file is larger than the window, we return the
// unrecoverable sentinel without touching a single byte.
func TestValidateTail_UnrecoverableLeavesFileIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "01HUNRECOVERABLE0000000000-pid1.jsonl")

	// Write valid content larger than the tiny window we'll use, then
	// append a big garbage tail that fills the window. The window
	// sees only garbage, so validation must fail unrecoverable.
	for i := 0; i < 20; i++ {
		writeDatomLine(t, path, marshalDatom(t, ulid.Make().String()))
	}
	garbage := make([]byte, 512)
	for i := range garbage {
		garbage[i] = 'x'
	}
	writeDatomLine(t, path, garbage) // no newline in the middle; line has no valid structure

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	_, err = ValidateTail(path, 256)
	if !errors.Is(err, ErrUnrecoverableTail) {
		t.Fatalf("expected ErrUnrecoverableTail, got %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("file size changed on unrecoverable: %d -> %d", before.Size(), after.Size())
	}
}

// TestLineScanBudget is a trivial sanity check on the helper used by
// tests and production alike to describe the I/O budget.
func TestLineScanBudget(t *testing.T) {
	if got := lineScanBudget(100, 4096); got != 100 {
		t.Fatalf("small file: got %d want 100", got)
	}
	if got := lineScanBudget(1<<20, 4096); got != 4096 {
		t.Fatalf("large file: got %d want 4096", got)
	}
}
