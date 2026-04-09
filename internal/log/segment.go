package log

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// Default values mirror the spec's "Operational Defaults" table for the
// log.* settings. Callers using config.Load() should pass those values in
// via options; these constants exist so the package is usable standalone
// in tests and tooling.
const (
	defaultLockTimeout = 5 * time.Second

	segmentFileMode = 0o600
	segmentDirMode  = 0o700
)

// Errors returned by the segment writer. ErrLockTimeout lives in lock.go.
var (
	// ErrEmptyGroup is returned by Append if the transaction group is nil
	// or zero-length. A transaction group must contain at least one datom
	// because the committed tx ULID is drawn from the group itself.
	ErrEmptyGroup = errors.New("log: empty transaction group")

	// ErrMixedTx is returned by Append if the datoms in a group do not all
	// share the same tx ULID. The segment writer enforces this invariant
	// to guarantee that a transaction group is a single causal unit.
	ErrMixedTx = errors.New("log: transaction group spans multiple tx ULIDs")

	// ErrUnsealedDatom is returned by Append if any datom in the group is
	// missing its Checksum field. Datoms must be sealed before the writer
	// commits them so the per-datom SHA-256 is recorded for tail
	// validation.
	ErrUnsealedDatom = errors.New("log: datom is not sealed (empty checksum)")

	// ErrWriterClosed is returned by Append after Close has been called.
	ErrWriterClosed = errors.New("log: writer closed")
)

// Writer owns a single segment file and serialises appends to it under a
// per-segment advisory flock. A Writer is safe for concurrent use by
// multiple goroutines; the Append critical section is serialised by an
// in-process mutex *and* the flock, so cooperative writers in the same
// process cannot race on file offset or fsync ordering.
type Writer struct {
	dir             string
	path            string
	writerID        string
	lockTimeout     time.Duration
	segmentMaxBytes int64 // 0 disables rollover

	mu     sync.Mutex // serialises appends within one process
	file   *os.File
	closed bool

	// rollCount tracks how many times the writer has transparently
	// rolled to a new segment. Tests and `cortex status` can surface
	// this without needing to scan the segment directory.
	rollCount atomic.Uint64

	// fsyncFn is the test seam used by cortex-4kq.16 acceptance to assert
	// "fsync is called exactly once per Append group". Production code
	// uses syncFile; tests can swap it for a counter-only implementation
	// that does not touch the disk.
	fsyncFn    func(*os.File) error
	fsyncCount atomic.Uint64

	// size tracks the writer's view of the current segment size so rollover
	// (cortex-4kq.23) can be added without a stat syscall per append.
	size int64
}

// Option configures a Writer at construction time.
type Option func(*Writer)

// WithLockTimeout sets the per-Append advisory flock acquisition budget.
// The spec default is 5 seconds (log.lock_timeout_seconds). Tests use
// much shorter values to keep the contention case fast.
func WithLockTimeout(d time.Duration) Option {
	return func(w *Writer) { w.lockTimeout = d }
}

// WithWriterID overrides the writer identifier embedded in the segment
// filename. The default is "pid<N>" where N is os.Getpid(), which is
// stable within one process lifetime and tags the segment with the
// producing OS process for forensic purposes.
func WithWriterID(id string) Option {
	return func(w *Writer) { w.writerID = id }
}

// WithSegmentMaxBytes caps the current segment file size. When appending
// a transaction group would push the segment beyond this cap, the writer
// transparently finalises the current file (fsync + close) and opens a
// fresh <ulid>-<writer-id>.jsonl in the same directory before writing
// the group. A value of 0 (the default) disables rollover entirely.
//
// The spec (cortex-spec.md §"Segmented Datom Log") uses a default of
// 64 MB via log.segment_max_size_mb; callers should convert config MB to
// bytes and pass the result here.
//
// Rollover is atomic with respect to transaction groups: a group is
// either written entirely into the pre-roll segment (if it fits under
// the cap) or entirely into the freshly rolled segment. It never spans
// two segments. A single group larger than the cap is written into an
// empty segment rather than being split, because splitting would
// violate the "one tx per group" causal invariant.
func WithSegmentMaxBytes(n int64) Option {
	return func(w *Writer) { w.segmentMaxBytes = n }
}

// WithFsyncFn replaces the fsync implementation used after each Append.
// Production code should never call this; it exists so tests can assert
// the acceptance invariant "exactly one fsync per Append group" without
// depending on filesystem-level tracing.
func WithFsyncFn(fn func(*os.File) error) Option {
	return func(w *Writer) { w.fsyncFn = fn }
}

// NewWriter opens (or creates) a new segment file in dir, acquires no lock,
// and returns a Writer ready for Append calls. The segment directory is
// created with mode 0700 if missing, and the segment file itself is
// created with mode 0600. If dir already exists with looser permissions
// NewWriter tightens it so the spec invariant "~/.cortex/ is owner-only"
// cannot be accidentally weakened by a caller.
//
// Each NewWriter call produces a fresh segment file named
// <ulid>-<writer-id>.jsonl where the ULID prefix encodes the creation
// time so lexicographic ordering of segments roughly matches temporal
// order, as required by the spec.
func NewWriter(dir string, opts ...Option) (*Writer, error) {
	w := &Writer{
		dir:         dir,
		writerID:    defaultWriterID(),
		lockTimeout: defaultLockTimeout,
		fsyncFn:     syncFile,
	}
	for _, opt := range opts {
		opt(w)
	}

	// Ensure the segment directory exists with the required mode. MkdirAll
	// is a no-op if dir already exists, but it does not tighten existing
	// permissions, so we follow it with an explicit Chmod to honour the
	// spec even on pre-existing directories.
	if err := os.MkdirAll(dir, segmentDirMode); err != nil {
		return nil, fmt.Errorf("log: create segment dir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, segmentDirMode); err != nil {
		return nil, fmt.Errorf("log: chmod segment dir %s: %w", dir, err)
	}

	// Build the segment path. The ULID is generated once per Writer
	// construction so every process/invocation gets its own segment file.
	name := fmt.Sprintf("%s-%s.jsonl", newSegmentULID(), w.writerID)
	w.path = filepath.Join(dir, name)

	// O_APPEND guarantees the kernel atomically advances the file offset
	// to EOF on every write so concurrent writers (even absent the flock)
	// never overwrite each other's bytes. The flock in Append is therefore
	// purely for fsync/transaction-group ordering, not for offset safety.
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, segmentFileMode)
	if err != nil {
		return nil, fmt.Errorf("log: open segment %s: %w", w.path, err)
	}
	// OpenFile honours umask on create; enforce the spec-mandated 0600
	// explicitly so a permissive umask cannot leak read access.
	if err := os.Chmod(w.path, segmentFileMode); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("log: chmod segment %s: %w", w.path, err)
	}
	// Record the initial size (zero for a freshly-created file) so later
	// Append calls can maintain the size counter without stat syscalls.
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("log: stat segment %s: %w", w.path, err)
	}
	w.file = f
	w.size = st.Size()
	return w, nil
}

// Path returns the absolute path of the segment file this writer owns.
// Tests use it to simulate lock contention by opening the same file from
// another file descriptor.
func (w *Writer) Path() string { return w.path }

// FsyncCount returns the number of successful fsync calls issued by this
// writer since construction. The acceptance criterion for cortex-4kq.16
// asserts that every successful Append increments the counter by exactly
// one, regardless of how many datoms the group contains.
func (w *Writer) FsyncCount() uint64 { return w.fsyncCount.Load() }

// Size returns the writer's view of the current segment size in bytes.
// This is updated after each successful Append and is intended for
// segment rollover (cortex-4kq.23); callers that need the authoritative
// value should os.Stat the file.
func (w *Writer) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// Close releases file handles held by the writer. It does not finalise
// the segment (segment finalisation happens transparently on rollover in
// cortex-4kq.23). Calling Close twice returns nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

// Append commits one transaction group to the segment. It acquires the
// per-segment advisory flock (waiting up to lockTimeout), writes every
// datom in the group in a single write syscall, fsyncs the file exactly
// once, releases the lock, and returns the committed tx ULID.
//
// Semantics:
//   - The flock scope is minimal: only the write and fsync happen under
//     the lock. Callers perform all backend writes, LLM calls, and
//     watermark updates outside this method.
//   - fsync is the authoritative commit point per spec §"Lock Scope".
//   - On ErrLockTimeout, no bytes have been written and the file size
//     is unchanged.
//   - On a failed write after the lock is held (very rare: disk full,
//     etc.), the writer returns the underlying error; the append may
//     have partially written, and torn-tail recovery (cortex-4kq.24)
//     will truncate any half-written datom on the next startup.
func (w *Writer) Append(group []datom.Datom) (string, error) {
	if len(group) == 0 {
		return "", ErrEmptyGroup
	}

	// Validate invariants and pre-serialise the entire group *before*
	// acquiring the flock. Serialisation can allocate and hash, and none
	// of that needs to happen inside the critical section. Doing it
	// outside keeps the per-segment lock hold time dominated by the
	// fsync itself.
	tx := group[0].Tx
	if tx == "" {
		return "", fmt.Errorf("log: datom has empty tx")
	}
	payload := make([]byte, 0, len(group)*256)
	for i := range group {
		d := &group[i]
		if d.Tx != tx {
			return "", ErrMixedTx
		}
		if d.Checksum == "" {
			return "", ErrUnsealedDatom
		}
		line, err := datom.Marshal(d)
		if err != nil {
			return "", fmt.Errorf("log: marshal datom %d: %w", i, err)
		}
		payload = append(payload, line...)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", ErrWriterClosed
	}

	// Roll to a fresh segment *before* acquiring the flock on the new
	// file. Rollover finalises the current segment durably (fsync +
	// close) so a crash mid-rollover leaves an intact prior segment
	// that torn-tail recovery can load without special-casing. We pass
	// the serialized payload length so rolloverIfNeeded can decide
	// whether this specific group would exceed the cap.
	if err := w.rolloverIfNeeded(int64(len(payload))); err != nil {
		return "", err
	}

	// Acquire the per-segment flock. Any timeout here must leave the
	// file byte-identical to its pre-Append state, so we branch on the
	// lock result before any Write call.
	fd := int(w.file.Fd())
	if err := acquireFlock(fd, w.lockTimeout); err != nil {
		return "", err
	}
	// Release is deferred so that even a panic in the write path clears
	// the flock. The release error is surfaced only if the primary
	// operation succeeded, so a successful append is never masked by a
	// spurious unlock failure.
	var unlockErr error
	defer func() {
		if err := releaseFlock(fd); err != nil && unlockErr == nil {
			unlockErr = err
		}
	}()

	// Single Write call: with O_APPEND the kernel moves the offset to
	// EOF atomically, so one Write with the concatenated payload gives
	// us an atomic append of the whole group relative to other
	// cooperative writers on the same file.
	n, err := w.file.Write(payload)
	if err != nil {
		return "", fmt.Errorf("log: write segment: %w", err)
	}
	if n != len(payload) {
		return "", fmt.Errorf("log: short write: %d of %d bytes", n, len(payload))
	}

	// fsync is the commit point per spec: returning success from this
	// call is what makes the datoms durable. The counter is incremented
	// after a successful call so the acceptance test can assert "exactly
	// one fsync per Append group".
	if err := w.fsyncFn(w.file); err != nil {
		return "", fmt.Errorf("log: fsync segment: %w", err)
	}
	w.fsyncCount.Add(1)
	w.size += int64(n)

	if unlockErr != nil {
		return tx, fmt.Errorf("log: release flock: %w", unlockErr)
	}
	return tx, nil
}

// RollCount reports how many times this writer has transparently rolled
// to a new segment since construction. Intended for tests and `cortex
// status`-style reporting; not part of any ordering guarantee.
func (w *Writer) RollCount() uint64 { return w.rollCount.Load() }

// rolloverIfNeeded finalises the current segment and opens a fresh one
// if and only if writing `payloadLen` more bytes would push the current
// segment above segmentMaxBytes. Rollover on an empty segment is
// suppressed even for oversized payloads, because splitting a tx group
// across two files is forbidden and the only way to honour both "never
// split a group" and "respect the cap" for a single oversized group is
// to write it into an empty segment.
//
// The caller must hold w.mu. The caller must NOT hold the flock yet:
// rollover swaps the file handle, and the new segment needs to be
// locked on its own fd.
func (w *Writer) rolloverIfNeeded(payloadLen int64) error {
	if w.segmentMaxBytes <= 0 {
		return nil
	}
	if w.size == 0 {
		return nil
	}
	if w.size+payloadLen <= w.segmentMaxBytes {
		return nil
	}

	// Finalise the current segment. The fsync here is an internal
	// durability step, not a "commit point" for an Append group, so we
	// call file.Sync() directly rather than the counter-seamed fsyncFn.
	// The public FsyncCount is reserved for Append commits so its
	// semantics ("exactly one fsync per Append group") remain stable
	// regardless of how often the writer rolls underneath.
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("log: rollover fsync: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("log: rollover close: %w", err)
	}

	// Open the next segment. A fresh ULID prefix guarantees that the
	// new filename sorts *after* the one we just closed, so the merge-
	// sort reader's filename-order heuristic (cortex-4kq.29) continues
	// to match temporal order even for the same writer id.
	name := fmt.Sprintf("%s-%s.jsonl", newSegmentULID(), w.writerID)
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, segmentFileMode)
	if err != nil {
		return fmt.Errorf("log: rollover open %s: %w", path, err)
	}
	if err := os.Chmod(path, segmentFileMode); err != nil {
		_ = f.Close()
		return fmt.Errorf("log: rollover chmod %s: %w", path, err)
	}
	w.file = f
	w.path = path
	w.size = 0
	w.rollCount.Add(1)
	return nil
}

// syncFile is the production fsync implementation. It is a thin wrapper
// around (*os.File).Sync so that WithFsyncFn can substitute a counting
// stub in tests.
func syncFile(f *os.File) error { return f.Sync() }

// defaultWriterID returns a stable per-process writer identifier used as
// the suffix of the segment filename. "pid<N>" is easy to match against
// ops.log entries for forensic purposes.
func defaultWriterID() string {
	return "pid" + strconv.Itoa(os.Getpid())
}

// newSegmentULID produces a fresh ULID encoded as 26 Crockford base32
// characters. It uses the default monotonic entropy source from the
// oklog/ulid library so segment files created in the same millisecond
// still sort in creation order.
func newSegmentULID() string {
	return ulid.Make().String()
}
