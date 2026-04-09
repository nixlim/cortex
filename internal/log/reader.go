package log

import (
	"bufio"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/nixlim/cortex/internal/datom"
)

// ErrSegmentOutOfOrder is returned by the Reader when a segment file
// yields two datoms whose tx ULIDs are not monotonically non-decreasing
// within that segment. Phase 1 writers always produce monotonic tx
// within a segment, so an out-of-order segment indicates either a
// corrupt import or a bug; the reader fails loudly rather than
// silently producing a stream that is not globally sorted.
var ErrSegmentOutOfOrder = errors.New("log: segment tx order violated")

// Reader streams datoms from a set of segment files in strict
// tx-ULID-ascending order. The implementation is a K-way merge over
// per-segment bufio.Scanner instances: memory stays bounded to one
// datom per segment regardless of total corpus size, satisfying AC2
// "memory stays bounded as segments grow".
//
// Readers are single-pass and not safe for concurrent use. Close the
// reader when finished; Close releases all open segment file handles.
type Reader struct {
	streams []*segmentStream
	h       *mergeHeap
	err     error
	closed  bool
}

// segmentStream wraps a single segment file with a buffered scanner
// and a one-datom look-ahead. The head field holds the next datom to
// emit; when depleted, the stream is removed from the heap.
type segmentStream struct {
	path     string
	file     *os.File
	scanner  *bufio.Scanner
	head     datom.Datom
	hasHead  bool
	prevTx   string
	depleted bool
}

// mergeHeap is a min-heap ordered by the head datom's tx ULID.
// ULIDs are Crockford base32 strings that compare byte-wise as
// timestamps, so ordinary string comparison is the correct sort key.
type mergeHeap struct {
	items []*segmentStream
}

func (h *mergeHeap) Len() int { return len(h.items) }
func (h *mergeHeap) Less(i, j int) bool {
	return h.items[i].head.Tx < h.items[j].head.Tx
}
func (h *mergeHeap) Swap(i, j int) { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap) Push(x any)    { h.items = append(h.items, x.(*segmentStream)) }
func (h *mergeHeap) Pop() any {
	n := len(h.items)
	x := h.items[n-1]
	h.items = h.items[:n-1]
	return x
}

// NewReader opens every segment path in paths, seeds each with a
// single-datom look-ahead, and returns a Reader ready for Next. If any
// segment fails to open the already-opened files are closed before
// returning the error.
//
// Callers should pass only paths that have already cleared torn-tail
// recovery and full-file checksum validation (i.e. the Healthy list
// from a LoadReport). The reader verifies per-line checksums on every
// Next call as defence-in-depth, but relies on the caller to have
// weeded out quarantined segments.
func NewReader(paths []string) (*Reader, error) {
	r := &Reader{}
	for _, p := range paths {
		stream, err := openStream(p)
		if err != nil {
			_ = r.Close()
			return nil, err
		}
		// Prime the look-ahead with the first datom in the segment.
		// Empty segments are allowed and simply do not enter the heap.
		if err := advanceStream(stream); err != nil {
			_ = r.Close()
			return nil, err
		}
		r.streams = append(r.streams, stream)
	}

	h := &mergeHeap{}
	for _, s := range r.streams {
		if s.hasHead {
			h.items = append(h.items, s)
		}
	}
	heap.Init(h)
	r.h = h
	return r, nil
}

// openStream opens one segment file for reading and wraps it in a
// buffered scanner sized for the spec's maximum line. The scanner
// buffer (1 MiB) is generous enough for any realistic datom value
// while staying within the bounded-memory guarantee of the reader.
func openStream(path string) (*segmentStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("log: reader open %s: %w", path, err)
	}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	return &segmentStream{path: path, file: f, scanner: s}, nil
}

// advanceStream reads the next line from a segment, parses and
// verifies it, and populates head. On EOF it flips depleted and
// clears hasHead so the stream is never pushed back onto the heap.
// A monotonicity violation within one segment returns
// ErrSegmentOutOfOrder because the K-way merge assumes per-stream
// sorted input.
func advanceStream(s *segmentStream) error {
	if s.depleted {
		s.hasHead = false
		return nil
	}
	if !s.scanner.Scan() {
		if err := s.scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("log: reader scan %s: %w", s.path, err)
		}
		s.depleted = true
		s.hasHead = false
		return nil
	}
	d, err := datom.Unmarshal(s.scanner.Bytes())
	if err != nil {
		return fmt.Errorf("log: reader parse %s: %w", s.path, err)
	}
	if s.prevTx != "" && d.Tx < s.prevTx {
		return fmt.Errorf("%w: %s: %s < %s", ErrSegmentOutOfOrder, s.path, d.Tx, s.prevTx)
	}
	s.head = *d
	s.hasHead = true
	s.prevTx = d.Tx
	return nil
}

// Next returns the next datom in global tx-ULID-ascending order.
// The second return is false when the stream is exhausted. A non-nil
// error terminates the stream for all subsequent calls.
func (r *Reader) Next() (datom.Datom, bool, error) {
	if r.err != nil {
		return datom.Datom{}, false, r.err
	}
	if r.closed {
		return datom.Datom{}, false, errors.New("log: reader closed")
	}
	if r.h.Len() == 0 {
		return datom.Datom{}, false, nil
	}
	top := heap.Pop(r.h).(*segmentStream)
	out := top.head
	if err := advanceStream(top); err != nil {
		r.err = err
		return datom.Datom{}, false, err
	}
	if top.hasHead {
		heap.Push(r.h, top)
	}
	return out, true, nil
}

// Close releases all open segment file handles. Calling Close twice
// returns nil. It is safe to Close a Reader that is still mid-stream.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	var firstErr error
	for _, s := range r.streams {
		if s.file != nil {
			if err := s.file.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ReadAll is a convenience wrapper that drains a Reader into a slice.
// It exists so tests and small tools (e.g. cortex export) can get the
// merged stream as a slice in one call. Production code paths that
// care about memory should use Next directly.
func ReadAll(paths []string) ([]datom.Datom, error) {
	r, err := NewReader(paths)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []datom.Datom
	for {
		d, ok, err := r.Next()
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, d)
	}
}
