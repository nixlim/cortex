package log

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// makeGroup builds a sealed transaction group of n datoms sharing one tx.
// The datom bodies are deliberately small and deterministic so size
// assertions in tests are easy to reason about.
func makeGroup(t *testing.T, n int) (tx string, group []datom.Datom) {
	t.Helper()
	tx = ulid.Make().String()
	group = make([]datom.Datom, n)
	for i := 0; i < n; i++ {
		d := datom.Datom{
			Tx:           tx,
			Ts:           "2026-04-10T00:00:00Z",
			Actor:        "test",
			Op:           datom.OpAdd,
			E:            "entry:test",
			A:            "body",
			V:            json.RawMessage(`"hello"`),
			Src:          "segment_test",
			InvocationID: "01HPTESTINVOCATION0000000000",
		}
		if err := d.Seal(); err != nil {
			t.Fatalf("seal datom %d: %v", i, err)
		}
		group[i] = d
	}
	return tx, group
}

// TestAppend_WritesExpectedBytes covers AC1 and AC3 of cortex-4kq.16:
// "A successful Append writes exactly len(group) lines" and "stat().Size
// equals prior size plus serialized group length".
func TestAppend_WritesExpectedBytes(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	before, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	tx, group := makeGroup(t, 3)

	// Pre-compute the serialized byte length the same way Append does
	// so we can assert file size deltas precisely.
	var want int64
	for i := range group {
		line, err := datom.Marshal(&group[i])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		want += int64(len(line))
	}

	gotTx, err := w.Append(group)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if gotTx != tx {
		t.Fatalf("tx mismatch: got %q want %q", gotTx, tx)
	}

	after, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if delta := after.Size() - before.Size(); delta != want {
		t.Fatalf("size delta: got %d want %d", delta, want)
	}

	// Count newlines in the file: must equal len(group) because Marshal
	// terminates every datom with exactly one 0x0A.
	data, err := os.ReadFile(w.Path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var lines int
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	if lines != len(group) {
		t.Fatalf("line count: got %d want %d", lines, len(group))
	}
}

// TestAppend_FsyncExactlyOnce covers AC4 of cortex-4kq.16 using the
// WithFsyncFn counter seam. A group of N datoms must trigger a single
// fsync, not one per datom.
func TestAppend_FsyncExactlyOnce(t *testing.T) {
	dir := t.TempDir()

	var calls atomic.Uint64
	w, err := NewWriter(dir, WithFsyncFn(func(*os.File) error {
		calls.Add(1)
		return nil
	}))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	_, group := makeGroup(t, 5)
	if _, err := w.Append(group); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fsync calls: got %d want 1", got)
	}
	if got := w.FsyncCount(); got != 1 {
		t.Fatalf("FsyncCount: got %d want 1", got)
	}

	// Append a second group and confirm the counter advances by one.
	_, group2 := makeGroup(t, 2)
	if _, err := w.Append(group2); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if got := w.FsyncCount(); got != 2 {
		t.Fatalf("FsyncCount after 2nd: got %d want 2", got)
	}
}

// TestAppend_LockTimeoutNoBytesWritten covers AC2 of cortex-4kq.16: a
// contending writer that holds the flock past the timeout causes Append
// to return ErrLockTimeout with zero bytes written. The spec budget is
// 5 seconds; the test uses 200ms for speed but exercises the same code
// path (poll loop + deadline + errno check).
func TestAppend_LockTimeoutNoBytesWritten(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WithLockTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Open a second file descriptor on the same segment file and take
	// the exclusive advisory lock for 400ms — long enough to outlast
	// the writer's 200ms budget.
	contender, err := os.OpenFile(w.Path(), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	defer contender.Close()
	if err := syscall.Flock(int(contender.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("contender flock: %v", err)
	}
	releaseAt := time.Now().Add(400 * time.Millisecond)
	go func() {
		time.Sleep(time.Until(releaseAt))
		_ = syscall.Flock(int(contender.Fd()), syscall.LOCK_UN)
	}()

	sizeBefore, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	_, group := makeGroup(t, 1)
	start := time.Now()
	_, err = w.Append(group)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("expected ErrLockTimeout, got %v", err)
	}
	// Sanity check the wait budget was actually enforced: the call
	// must have taken at least the lock timeout and strictly less than
	// the contender's hold window, so the timeout path fired rather
	// than the lock being acquired after the contender released.
	if elapsed < 180*time.Millisecond {
		t.Fatalf("elapsed %v too short: deadline was not honoured", elapsed)
	}
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("elapsed %v too long: expected timeout before contender release", elapsed)
	}

	sizeAfter, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if sizeAfter.Size() != sizeBefore.Size() {
		t.Fatalf("file size changed on timeout: before=%d after=%d",
			sizeBefore.Size(), sizeAfter.Size())
	}
	if got := w.FsyncCount(); got != 0 {
		t.Fatalf("fsync called on timeout path: got %d want 0", got)
	}
}

// TestNewWriter_FilePermissions verifies the spec invariants: the segment
// directory is 0700 and segment files are 0600 even if the umask would
// allow wider access.
func TestNewWriter_FilePermissions(t *testing.T) {
	// Tighten umask so the OpenFile 0600 is not masked further, then
	// restore it on exit.
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	dir := t.TempDir()
	// Loosen the tempdir first so we can prove NewWriter tightens it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod tempdir: %v", err)
	}

	w, err := NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	dinfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dinfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir perm: got %#o want 0700", perm)
	}

	finfo, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if perm := finfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file perm: got %#o want 0600", perm)
	}

	if filepath.Dir(w.Path()) != dir {
		t.Fatalf("segment not placed under dir: %s", w.Path())
	}
}

// TestAppend_ValidatesGroupInvariants covers the group-level invariants
// enforced before any bytes are written: non-empty group, shared tx,
// sealed datoms.
func TestAppend_ValidatesGroupInvariants(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Append(nil); !errors.Is(err, ErrEmptyGroup) {
		t.Fatalf("empty group: got %v want ErrEmptyGroup", err)
	}

	_, mixed := makeGroup(t, 2)
	mixed[1].Tx = ulid.Make().String()
	if err := mixed[1].Seal(); err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if _, err := w.Append(mixed); !errors.Is(err, ErrMixedTx) {
		t.Fatalf("mixed tx: got %v want ErrMixedTx", err)
	}

	_, unsealed := makeGroup(t, 1)
	unsealed[0].Checksum = ""
	if _, err := w.Append(unsealed); !errors.Is(err, ErrUnsealedDatom) {
		t.Fatalf("unsealed: got %v want ErrUnsealedDatom", err)
	}

	// None of the failed calls should have advanced fsync count or file
	// size, because every check happens before the flock is acquired.
	if got := w.FsyncCount(); got != 0 {
		t.Fatalf("fsync called on validation error: %d", got)
	}
	info, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("file not empty after validation failures: %d bytes", info.Size())
	}
}

// TestAppend_AfterClose ensures a closed writer rejects further appends
// cleanly with ErrWriterClosed rather than crashing on a nil file handle.
func TestAppend_AfterClose(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, group := makeGroup(t, 1)
	if _, err := w.Append(group); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("expected ErrWriterClosed, got %v", err)
	}
	// Double-close is a no-op.
	if err := w.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

// TestAppend_SegmentFileNamePrefix verifies the spec naming convention
// "<ulid>-<writer-id>.jsonl" so the merge-sort reader (cortex-4kq.29)
// can rely on lexicographic filename ordering as a temporal proxy.
func TestAppend_SegmentFileNamePrefix(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, WithWriterID("unit"))
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	name := filepath.Base(w.Path())
	if len(name) < 26+1+4+len(".jsonl") {
		t.Fatalf("segment name %q is shorter than <ulid>-<writer>.jsonl", name)
	}
	if name[26] != '-' {
		t.Fatalf("segment name %q missing '-' at position 26", name)
	}
	// The first 26 characters must parse as a ULID.
	if _, err := ulid.Parse(name[:26]); err != nil {
		t.Fatalf("segment name prefix %q is not a ULID: %v", name[:26], err)
	}
	if name[len(name)-len(".jsonl"):] != ".jsonl" {
		t.Fatalf("segment name %q missing .jsonl suffix", name)
	}
}
