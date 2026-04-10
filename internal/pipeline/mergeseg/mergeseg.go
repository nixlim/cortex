// Package mergeseg implements cortex merge per cortex-4kq.31.
//
// cortex merge <log-file> validates checksums on an external segment
// file and renames it into the local log directory (~/.cortex/log.d/),
// producing a merged log whose tx set is the deduplicated union of
// the prior log and the external segment. The reader's k-way merge
// already deduplicates (same tx across segments yields one datom
// per unique (tx,a) pair), so the merge operation is just "validate
// then move".
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Segmented Datom Log" (merge-sort reader)
//	cortex-4kq.31 acceptance criteria
package mergeseg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
)

// Errors surfaced by Merge.
var (
	// ErrChecksumMismatch is returned when a datom in the external
	// segment fails checksum verification. The file is NOT moved into
	// log.d when this happens (AC2).
	ErrChecksumMismatch = errors.New("mergeseg: checksum mismatch in external segment")

	// ErrEmptyPath is returned when the external segment path is
	// empty.
	ErrEmptyPath = errors.New("mergeseg: external segment path is empty")

	// ErrEmptyLogDir is returned when the target log directory is
	// empty.
	ErrEmptyLogDir = errors.New("mergeseg: log directory is empty")
)

// Result is the returned summary of a Merge operation.
type Result struct {
	// SourcePath is the original external segment path.
	SourcePath string

	// DestPath is the final path inside the log directory.
	DestPath string

	// DatomCount is the number of datoms verified in the external
	// segment.
	DatomCount int

	// TxCount is the number of distinct tx ULIDs found in the
	// external segment.
	TxCount int
}

// Merge validates every datom's checksum in the external segment at
// externalPath, then moves (renames or copies) the file into logDir
// with a name that preserves the ULID ordering property. If any datom
// fails verification, the file is left untouched and ErrChecksumMismatch
// is returned.
//
// The merge produces a file in logDir whose name starts with a fresh
// ULID so it sorts after all existing segments. The reader's k-way
// merge handles deduplication of tx values that appear in both the
// existing log and the imported segment.
func Merge(externalPath, logDir string) (*Result, error) {
	if externalPath == "" {
		return nil, ErrEmptyPath
	}
	if logDir == "" {
		return nil, ErrEmptyLogDir
	}

	// --- Stage 1: validate every datom's checksum. The file is opened
	// read-only; no bytes are written until validation passes.
	datomCount, txCount, err := validateSegment(externalPath)
	if err != nil {
		return nil, err
	}

	// --- Stage 2: ensure logDir exists.
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("mergeseg: create log dir: %w", err)
	}

	// --- Stage 3: move (or copy) the file into logDir. A fresh ULID
	// prefix ensures the filename sorts after all existing segments.
	// We preserve the .jsonl extension so the reader's glob picks it
	// up.
	destName := fmt.Sprintf("%s-imported.jsonl", ulid.Make().String())
	destPath := filepath.Join(logDir, destName)

	// Try rename first (same filesystem); fall back to copy+remove
	// for cross-device moves.
	if err := os.Rename(externalPath, destPath); err != nil {
		if copyErr := copyFile(externalPath, destPath); copyErr != nil {
			return nil, fmt.Errorf("mergeseg: copy %s to %s: %w", externalPath, destPath, copyErr)
		}
		// Remove original only after a successful copy.
		_ = os.Remove(externalPath)
	}

	// Enforce owner-only perms on the imported segment.
	_ = os.Chmod(destPath, 0o600)

	return &Result{
		SourcePath: externalPath,
		DestPath:   destPath,
		DatomCount: datomCount,
		TxCount:    txCount,
	}, nil
}

// ValidateOnly checks every datom's checksum without moving the file.
// Useful for a --dry-run mode or for pre-flight checks.
func ValidateOnly(externalPath string) (int, int, error) {
	if externalPath == "" {
		return 0, 0, ErrEmptyPath
	}
	return validateSegment(externalPath)
}

// validateSegment opens the file, scans every JSONL line, unmarshals
// and verifies each datom, and returns (datomCount, txCount, err).
// On the first checksum failure, it returns ErrChecksumMismatch.
func validateSegment(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("mergeseg: open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)

	txSet := map[string]struct{}{}
	count := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		d, err := datom.Unmarshal(line)
		if err != nil {
			return count, len(txSet), fmt.Errorf("%w: line %d: %v", ErrChecksumMismatch, count+1, err)
		}
		if err := d.Verify(); err != nil {
			return count, len(txSet), fmt.Errorf("%w: line %d: %v", ErrChecksumMismatch, count+1, err)
		}
		txSet[d.Tx] = struct{}{}
		count++
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return count, len(txSet), fmt.Errorf("mergeseg: scan %s: %w", path, err)
	}
	return count, len(txSet), nil
}

// copyFile copies src to dst with mode 0600.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
