// Package exportlog implements the read-side helper behind
// `cortex export`. The export streams every committed datom from the
// segmented log in tx-ULID-ascending order and serialises each one
// using datom.Marshal so the output is byte-identical to a single
// merged log segment.
//
// Spec references:
//
//	docs/spec/cortex-spec.md FR-006 / cortex-4kq.34
//	  ("cortex export produces a JSONL stream whose tx values are
//	   strictly ascending")
//
// The package is structured as a single Stream function so the CLI
// can pipe directly to stdout or to a file without an intermediate
// buffer. A small DatomSource interface lets tests substitute a
// slice-backed source instead of opening real segment files.
package exportlog

import (
	"context"
	"io"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// DatomSource yields the next datom in tx-ULID-ascending order. The
// production wrapper around *log.Reader is constructed by the CLI;
// tests pass a slice-backed source so the export logic can be
// exercised without segment files on disk.
type DatomSource interface {
	Next(ctx context.Context) (datom.Datom, bool, error)
	Close() error
}

// Result summarises a completed export. The CLI prints DatomCount
// alongside the output destination so operators can confirm a
// non-empty stream landed where they expected.
type Result struct {
	DatomCount int64
	BytesOut   int64
}

// Stream drains src and writes each datom as canonical JSONL to dst.
// The contract:
//
//   - Datoms are written in the order src yields them. Because the
//     production source wraps the merge-sort reader (which guarantees
//     tx-ULID-ascending order), the AC "tx values are strictly
//     ascending" holds. We additionally verify monotonicity here so
//     a buggy source surfaces a clear error rather than producing a
//     malformed export.
//   - An empty source produces zero bytes and a Result with
//     DatomCount=0, satisfying AC "Export of an empty log produces a
//     zero-byte output with exit zero".
//   - Each datom is sealed by datom.Marshal if it isn't already, so
//     the output is byte-identical to a fresh segment write.
//
// Stream calls src.Close on the way out so callers don't need to
// double-close on error. The error returned (if any) is wrapped in
// an operational errs.Error so the CLI can route it through
// emitAndExit unchanged.
func Stream(ctx context.Context, src DatomSource, dst io.Writer) (*Result, error) {
	if src == nil {
		return nil, errs.Validation("MISSING_SOURCE",
			"export requires a DatomSource", nil)
	}
	if dst == nil {
		return nil, errs.Validation("MISSING_DEST",
			"export requires an output writer", nil)
	}
	defer src.Close()

	res := &Result{}
	var prevTx string
	for {
		if err := ctx.Err(); err != nil {
			return res, errs.Operational("EXPORT_CANCELLED",
				"export interrupted by context cancellation", err)
		}
		d, ok, err := src.Next(ctx)
		if err != nil {
			return res, errs.Operational("LOG_READ_FAILED",
				"failed to read next datom for export", err)
		}
		if !ok {
			return res, nil
		}
		if prevTx != "" && d.Tx < prevTx {
			return res, errs.Operational("EXPORT_TX_OUT_OF_ORDER",
				"source yielded a datom with a tx ULID lower than its predecessor; "+
					"export refuses to write a non-monotonic stream", nil)
		}
		prevTx = d.Tx

		line, err := datom.Marshal(&d)
		if err != nil {
			return res, errs.Operational("DATOM_MARSHAL_FAILED",
				"could not serialise datom for export", err)
		}
		n, err := dst.Write(line)
		if err != nil {
			return res, errs.Operational("EXPORT_WRITE_FAILED",
				"failed to write datom to export destination", err)
		}
		res.BytesOut += int64(n)
		res.DatomCount++
	}
}

// SliceSource is a DatomSource backed by a pre-built slice. Exposed
// (rather than test-only) so the CLI can pre-load segments when the
// operator wants a frozen snapshot, and so other packages can reuse
// the same shape without redefining it.
type SliceSource struct {
	rows []datom.Datom
	i    int
}

// NewSliceSource returns a SliceSource over the given datoms.
func NewSliceSource(rows []datom.Datom) *SliceSource {
	return &SliceSource{rows: rows}
}

// Next implements DatomSource.
func (s *SliceSource) Next(_ context.Context) (datom.Datom, bool, error) {
	if s.i >= len(s.rows) {
		return datom.Datom{}, false, nil
	}
	d := s.rows[s.i]
	s.i++
	return d, true, nil
}

// Close implements DatomSource. SliceSource holds no resources.
func (s *SliceSource) Close() error { return nil }
