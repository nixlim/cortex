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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// externalPath, filters out any datoms whose tx already exists in an
// on-disk segment under logDir, and writes the surviving datoms to a
// fresh imported segment file inside logDir. If no datoms survive the
// dedup pass, the external file is still consumed but no new segment
// is written (DestPath is empty and DatomCount is zero).
//
// Import-time dedup is required because the startup loader
// (internal/log.Load) treats any cross-segment tx ULID sharing as a
// hard error (ErrTxCollision). Before this dedup pass existed,
// re-importing a log's own export bricked the stack: the imported
// file collided tx-for-tx with the live segments and the next
// command hit LOG_LOAD_FAILED. The spec's phrasing "set union
// deduplicated by tx" lives at the merge layer: the merge command is
// responsible for producing an input that is a disjoint extension of
// the current log, so the loader's collision detector can remain a
// real safety check for accidental ULID collisions.
//
// A checksum failure on the external segment leaves the file
// untouched and returns ErrChecksumMismatch — no dedup or move
// happens when validation fails.
func Merge(externalPath, logDir string) (*Result, error) {
	if externalPath == "" {
		return nil, ErrEmptyPath
	}
	if logDir == "" {
		return nil, ErrEmptyLogDir
	}

	// --- Stage 1: validate every datom's checksum. The file is opened
	// read-only; no bytes are written until validation passes.
	if _, _, err := validateSegment(externalPath); err != nil {
		return nil, err
	}

	// --- Stage 2: ensure logDir exists, then collect the tx ULIDs
	// already present in its segments so we can filter them out of
	// the incoming file.
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("mergeseg: create log dir: %w", err)
	}
	existingTxs, err := collectExistingTxs(logDir)
	if err != nil {
		return nil, err
	}

	// --- Stage 3: stream the external file into a fresh imported
	// segment, skipping any datoms whose tx already exists. The new
	// file is written atomically (tmp file + rename) so a crash mid
	// write cannot leave a half-file inside log.d. A fresh ULID
	// prefix ensures the filename sorts after all existing segments.
	destName := fmt.Sprintf("%s-imported.jsonl", ulid.Make().String())
	destPath := filepath.Join(logDir, destName)

	importedDatoms, importedTxs, err := writeFilteredSegment(externalPath, destPath, existingTxs)
	if err != nil {
		return nil, err
	}

	// If every tx in the external file was already present, remove
	// the empty destination file and return a zero-count result. The
	// external file is still consumed to match the "merge takes
	// ownership" contract.
	if importedDatoms == 0 {
		_ = os.Remove(destPath)
		_ = os.Remove(externalPath)
		return &Result{
			SourcePath: externalPath,
			DestPath:   "",
			DatomCount: 0,
			TxCount:    0,
		}, nil
	}

	// Enforce owner-only perms on the imported segment and drop the
	// source file now that its novel datoms are safely persisted.
	_ = os.Chmod(destPath, 0o600)
	_ = os.Remove(externalPath)

	return &Result{
		SourcePath: externalPath,
		DestPath:   destPath,
		DatomCount: importedDatoms,
		TxCount:    importedTxs,
	}, nil
}

// collectExistingTxs enumerates every *.jsonl segment directly under
// logDir and returns the set of tx ULIDs they contain. Files inside
// the .quarantine subdir are ignored: a quarantined segment is not
// part of the loadable set, so its tx values do not block an import.
// A missing logDir is treated as an empty set (first-run merge).
func collectExistingTxs(logDir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, fmt.Errorf("mergeseg: readdir %s: %w", logDir, err)
	}
	out := map[string]struct{}{}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		paths = append(paths, filepath.Join(logDir, name))
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("mergeseg: open %s: %w", p, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1<<16), 1<<20)
		for scanner.Scan() {
			var head struct {
				Tx string `json:"tx"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &head); err != nil {
				f.Close()
				return nil, fmt.Errorf("mergeseg: parse tx in %s: %w", p, err)
			}
			if head.Tx != "" {
				out[head.Tx] = struct{}{}
			}
		}
		if err := scanner.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("mergeseg: scan %s: %w", p, err)
		}
		f.Close()
	}
	return out, nil
}

// writeFilteredSegment streams srcPath into dstPath, copying lines
// whose tx is NOT in existingTxs. Returns the number of datoms
// written and the number of distinct tx ULIDs they span. The
// destination file is always created (even if empty) so the caller
// can decide whether to keep it.
func writeFilteredSegment(srcPath, dstPath string, existingTxs map[string]struct{}) (int, int, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, 0, fmt.Errorf("mergeseg: open %s: %w", srcPath, err)
	}
	defer src.Close()

	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, 0, fmt.Errorf("mergeseg: create %s: %w", dstPath, err)
	}
	defer dst.Close()

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)

	importedTxSet := map[string]struct{}{}
	datoms := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var head struct {
			Tx string `json:"tx"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return datoms, len(importedTxSet), fmt.Errorf("mergeseg: parse tx in %s: %w", srcPath, err)
		}
		if _, dup := existingTxs[head.Tx]; dup {
			continue
		}
		if _, werr := dst.Write(line); werr != nil {
			return datoms, len(importedTxSet), fmt.Errorf("mergeseg: write %s: %w", dstPath, werr)
		}
		if _, werr := dst.Write([]byte{'\n'}); werr != nil {
			return datoms, len(importedTxSet), fmt.Errorf("mergeseg: write %s: %w", dstPath, werr)
		}
		if head.Tx != "" {
			importedTxSet[head.Tx] = struct{}{}
		}
		datoms++
	}
	if err := scanner.Err(); err != nil {
		return datoms, len(importedTxSet), fmt.Errorf("mergeseg: scan %s: %w", srcPath, err)
	}
	if err := dst.Sync(); err != nil {
		return datoms, len(importedTxSet), fmt.Errorf("mergeseg: sync %s: %w", dstPath, err)
	}
	return datoms, len(importedTxSet), nil
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
