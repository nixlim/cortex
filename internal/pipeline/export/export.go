// Package export implements cortex export per cortex-4kq.34.
//
// cortex export serializes the merged tx-sorted datom stream to stdout
// or a file, producing a format-independent JSONL backup artifact
// insulated from segment file layout. The pipeline reads all segments
// through the log.Reader k-way merge, marshals each datom to a single
// JSON line, and writes them in strict tx-ascending order.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Segmented Datom Log" (merge-sort reader)
//	cortex-4kq.34 acceptance criteria
package export

import (
	"fmt"
	"io"
	"os"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
)

// Run exports the merged datom stream from segmentPaths to w. If
// segmentPaths is empty, Run writes nothing and returns a zero count
// (AC3: empty log → zero-byte output). The returned count is the
// number of datoms written.
func Run(segmentPaths []string, w io.Writer) (int, error) {
	if len(segmentPaths) == 0 {
		return 0, nil
	}

	r, err := log.NewReader(segmentPaths)
	if err != nil {
		return 0, fmt.Errorf("export: open reader: %w", err)
	}
	defer r.Close()

	count := 0
	for {
		d, ok, err := r.Next()
		if err != nil {
			return count, fmt.Errorf("export: read datom %d: %w", count, err)
		}
		if !ok {
			break
		}
		line, err := datom.Marshal(&d)
		if err != nil {
			return count, fmt.Errorf("export: marshal datom %d: %w", count, err)
		}
		if _, err := w.Write(line); err != nil {
			return count, fmt.Errorf("export: write datom %d: %w", count, err)
		}
		count++
	}
	return count, nil
}

// ToFile exports the merged datom stream to the given path with mode
// 0600, per AC4. It creates the file (or truncates an existing one).
func ToFile(segmentPaths []string, path string) (int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, fmt.Errorf("export: create %s: %w", path, err)
	}
	defer f.Close()

	// Enforce mode in case umask weakened it.
	if err := os.Chmod(path, 0o600); err != nil {
		return 0, fmt.Errorf("export: chmod %s: %w", path, err)
	}

	n, err := Run(segmentPaths, f)
	if err != nil {
		return n, err
	}
	// fsync the backup file before returning so the data is durable.
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("export: fsync %s: %w", path, err)
	}
	return n, nil
}
