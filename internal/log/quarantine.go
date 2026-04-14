package log

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

	"github.com/nixlim/cortex/internal/datom"
)

// quarantineSubdir is the relative directory name under log.segment_dir
// where bad segment files are moved. Kept as a constant because multiple
// code paths (startup scan, cortex doctor, cortex merge) must agree on
// exactly one location per spec §"Segmented Datom Log".
const quarantineSubdir = ".quarantine"

// ErrTxCollision is returned by Load (or DetectCollisions directly)
// when two or more healthy segments contain a datom with the same tx
// ULID. Collisions are cryptographically improbable but not impossible,
// and the substrate must fail loudly rather than silently deduplicate.
var ErrTxCollision = errors.New("log: tx ULID collision across segments")

// ScanFault describes the first point at which full-file checksum
// validation failed for a segment. Line is 1-based to match ops.log
// conventions; Offset is the byte offset of the start of the failing
// line inside the segment, which `cortex doctor` surfaces verbatim.
type ScanFault struct {
	Line   int
	Offset int64
	Err    error
}

func (f *ScanFault) Error() string {
	if f == nil {
		return "<nil ScanFault>"
	}
	return fmt.Sprintf("scan fault: line %d offset %d: %v", f.Line, f.Offset, f.Err)
}

// TxCollision captures the set of segment paths sharing one tx ULID.
// All collisions detected in a single Load pass are returned together
// so `cortex doctor` can print a complete remediation list rather than
// halting at the first.
type TxCollision struct {
	Tx    string
	Paths []string
}

// QuarantineAction records the outcome of moving a single corrupt
// segment file to the quarantine subdirectory. One is produced per
// quarantined file and surfaced in LoadReport; the caller translates
// them into ops.log events via the OpsRecordFn hook.
type QuarantineAction struct {
	OriginalPath   string
	QuarantinePath string
	Fault          ScanFault
}

// LoadReport is the aggregate result of a startup scan of log.d.
// Callers use it to drive ops.log writes, print `cortex doctor` status,
// and decide whether the process may proceed.
type LoadReport struct {
	// RecoveredTails is the per-segment outcome of torn-tail
	// validation. Entries with Truncated=true had torn suffixes removed.
	RecoveredTails []TailReport

	// Quarantined lists every segment file that failed full-file
	// checksum validation and was moved into the quarantine subdir.
	Quarantined []QuarantineAction

	// Healthy is the set of segment paths that passed validation and
	// are eligible for merge-sort replay. This is the list the reader
	// (cortex-4kq.29) will consume.
	Healthy []string

	// Collisions is non-empty when DetectCollisions found at least one
	// tx ULID shared by two or more healthy segments. Load returns
	// ErrTxCollision in that case so the caller refuses to proceed.
	Collisions []TxCollision
}

// OpsRecordFn is the hook used by Load to emit ops.log events for
// quarantine actions and other operationally significant findings. The
// log package deliberately does not import the opslog package; callers
// adapt their ops.log writer to this signature so the log package
// remains self-contained and easy to test.
//
// Parameters:
//   - level: "INFO" | "WARN" | "ERROR"
//   - component: subsystem name (callers pass "log")
//   - message: human-readable description
//   - entity: the segment path, or "" if not applicable
//   - err: optional underlying error (nil for advisory events)
type OpsRecordFn func(level, component, message, entity string, err error)

// LoadOptions configures Load. A zero value is valid and uses spec
// defaults: 64 KB tail-validation window, no ops.log hook.
type LoadOptions struct {
	// TailWindowBytes overrides the torn-tail validation window.
	TailWindowBytes int

	// OpsRecord is called once per operationally significant event.
	// If nil, events are dropped (tests frequently pass nil).
	OpsRecord OpsRecordFn
}

// Load runs the full startup log-layer protocol for a segment
// directory:
//
//  1. Torn-tail validation and truncation (cortex-4kq.24).
//  2. Full-file checksum validation of every segment.
//  3. Quarantine (by rename) of any segment that fails validation,
//     with an ops.log event naming the segment and failing offset.
//  4. Multi-segment tx ULID collision detection across the survivors.
//
// Load returns a LoadReport even when it also returns an error, so the
// caller can log the partial outcome. The only hard-failure case is
// ErrTxCollision: when two segments contain the same tx, Load refuses
// to hand back a "Healthy" set and the caller must abort startup.
//
// If dir does not exist the function returns a zero-value report with
// no error: a brand-new Cortex installation has an empty log directory.
func Load(dir string, opts LoadOptions) (LoadReport, error) {
	var report LoadReport

	// Step 0: does the directory exist? Missing is fine (first-run).
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return report, nil
		}
		return report, fmt.Errorf("log: load stat %s: %w", dir, err)
	}

	// Step 1: torn-tail validation.
	tails, err := RecoverDir(dir, opts.TailWindowBytes)
	if err != nil && !errors.Is(err, ErrUnrecoverableTail) {
		return report, err
	}
	report.RecoveredTails = tails
	if opts.OpsRecord != nil {
		for i := range tails {
			r := tails[i]
			if r.Truncated {
				opts.OpsRecord("WARN", "log",
					fmt.Sprintf("torn tail truncated: %d bytes removed", r.OriginalSize-r.FinalSize),
					r.Path, nil)
			}
			if r.Unrecoverable {
				opts.OpsRecord("ERROR", "log",
					"tail window unrecoverable; segment left intact",
					r.Path, ErrUnrecoverableTail)
			}
		}
	}

	// Step 2: enumerate segment files in lexicographic (ULID) order.
	segments, err := listDirSegments(dir)
	if err != nil {
		return report, err
	}

	// Step 3: full-file scan + quarantine.
	for _, path := range segments {
		fault, err := ScanSegment(path)
		if err != nil {
			// A filesystem error during scan is surfaced but does
			// not prevent other segments from loading.
			if opts.OpsRecord != nil {
				opts.OpsRecord("ERROR", "log", "scan failed", path, err)
			}
			continue
		}
		if fault == nil {
			report.Healthy = append(report.Healthy, path)
			continue
		}
		// Quarantine the bad segment.
		destPath, qerr := Quarantine(path, dir)
		if qerr != nil {
			if opts.OpsRecord != nil {
				opts.OpsRecord("ERROR", "log", "quarantine failed", path, qerr)
			}
			continue
		}
		report.Quarantined = append(report.Quarantined, QuarantineAction{
			OriginalPath:   path,
			QuarantinePath: destPath,
			Fault:          *fault,
		})
		if opts.OpsRecord != nil {
			opts.OpsRecord("WARN", "log",
				fmt.Sprintf("segment quarantined: line %d offset %d: %v",
					fault.Line, fault.Offset, fault.Err),
				path, fault.Err)
		}
	}

	// Step 4: tx ULID collision detection across the survivors.
	collisions, err := DetectCollisions(report.Healthy)
	if err != nil {
		return report, err
	}
	report.Collisions = collisions
	if len(collisions) > 0 {
		if opts.OpsRecord != nil {
			for _, c := range collisions {
				opts.OpsRecord("ERROR", "log",
					fmt.Sprintf("tx ULID collision: %s in %d segments", c.Tx, len(c.Paths)),
					strings.Join(c.Paths, ","), ErrTxCollision)
			}
		}
		return report, ErrTxCollision
	}

	return report, nil
}

// ScanSegment reads every line of a segment file and verifies the
// per-datom SHA-256 checksum. It returns nil when every datom is
// well-formed, or a *ScanFault describing the first failure. An error
// return indicates a filesystem-level problem (unreadable file,
// unexpected EOF), not a content failure.
//
// A torn suffix that does not end in '\n' is treated as a scan fault.
// In normal operation RecoverDir will have already truncated such
// tails, so hitting this path from inside Load implies either a
// concurrent modification or a segment that was merged in via
// cortex merge without going through the recovery pass.
func ScanSegment(path string) (*ScanFault, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("log: scan open %s: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 1<<16)
	var offset int64
	var prevTx string
	line := 0
	for {
		line++
		// ReadBytes returns the line including the terminator.
		// A final line without a newline is reported as io.EOF *with*
		// content — we treat that as a torn suffix fault below.
		buf, err := r.ReadBytes('\n')
		if len(buf) == 0 && errors.Is(err, io.EOF) {
			// Clean EOF: done.
			return nil, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("log: scan read %s: %w", path, err)
		}
		// Torn suffix: final line has content but no newline.
		if !endsWithNewline(buf) {
			return &ScanFault{
				Line:   line,
				Offset: offset,
				Err:    errors.New("line missing terminating newline"),
			}, nil
		}
		d, verr := datom.Unmarshal(buf)
		if verr != nil {
			return &ScanFault{
				Line:   line,
				Offset: offset,
				Err:    verr,
			}, nil
		}
		// Enforce tx ULID monotonicity. A segment whose datoms are not in
		// strictly non-decreasing tx order indicates concurrent-write
		// interleaving or log corruption. The merge-sort reader detects the
		// same violation at replay time, so catching it here lets log.Load
		// quarantine the segment before it ever reaches the reader.
		if prevTx != "" && d.Tx < prevTx {
			return &ScanFault{
				Line:   line,
				Offset: offset,
				Err:    fmt.Errorf("tx order violated: %s < %s", d.Tx, prevTx),
			}, nil
		}
		prevTx = d.Tx
		offset += int64(len(buf))
		if errors.Is(err, io.EOF) {
			// Final line had a newline and parsed cleanly.
			return nil, nil
		}
	}
}

// endsWithNewline reports whether a buffer ends in 0x0A without
// allocating: bufio.ReadBytes returns the terminator inside the slice
// on success so we can test the final byte directly.
func endsWithNewline(buf []byte) bool {
	return len(buf) > 0 && buf[len(buf)-1] == '\n'
}

// Quarantine moves segmentPath into segmentDir/.quarantine/, creating
// the quarantine subdir (mode 0700) if needed. Quarantine never
// deletes files; on a name conflict inside .quarantine/ the function
// appends a numeric suffix so prior quarantines are preserved.
//
// The rename is atomic on same-filesystem moves, which is always the
// case under log.segment_dir. The returned path is the final
// destination.
func Quarantine(segmentPath, segmentDir string) (string, error) {
	qdir := filepath.Join(segmentDir, quarantineSubdir)
	if err := os.MkdirAll(qdir, segmentDirMode); err != nil {
		return "", fmt.Errorf("log: quarantine mkdir %s: %w", qdir, err)
	}
	if err := os.Chmod(qdir, segmentDirMode); err != nil {
		return "", fmt.Errorf("log: quarantine chmod %s: %w", qdir, err)
	}

	base := filepath.Base(segmentPath)
	dest := filepath.Join(qdir, base)
	// Avoid clobber: if dest exists, pick <base>.N where N is the
	// smallest non-negative integer with no existing file.
	for n := 1; ; n++ {
		if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
			break
		}
		dest = filepath.Join(qdir, fmt.Sprintf("%s.%d", base, n))
	}
	if err := os.Rename(segmentPath, dest); err != nil {
		return "", fmt.Errorf("log: quarantine rename %s -> %s: %w", segmentPath, dest, err)
	}
	return dest, nil
}

// DetectCollisions reads every segment in paths and returns the set of
// tx ULIDs that appear in more than one segment. Within a single
// segment a tx may appear many times (one per datom in the group);
// only *cross-segment* sharing is a collision.
//
// The scan is a single pass per segment and builds a map from tx ULID
// to the set of segment paths containing it. Memory usage is O(unique
// tx count + segments-per-tx).
func DetectCollisions(paths []string) ([]TxCollision, error) {
	txSegments := map[string]map[string]struct{}{}
	for _, p := range paths {
		seen, err := collectTxs(p)
		if err != nil {
			return nil, err
		}
		for tx := range seen {
			set, ok := txSegments[tx]
			if !ok {
				set = map[string]struct{}{}
				txSegments[tx] = set
			}
			set[p] = struct{}{}
		}
	}
	var out []TxCollision
	for tx, segs := range txSegments {
		if len(segs) < 2 {
			continue
		}
		list := make([]string, 0, len(segs))
		for p := range segs {
			list = append(list, p)
		}
		sort.Strings(list)
		out = append(out, TxCollision{Tx: tx, Paths: list})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tx < out[j].Tx })
	return out, nil
}

// collectTxs returns the set of tx ULIDs found in a single segment.
// Lines are only parsed enough to extract the tx field; full checksum
// verification has already been done by ScanSegment for healthy
// segments, so re-verifying here would be wasted work.
func collectTxs(path string) (map[string]struct{}, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("log: collect txs open %s: %w", path, err)
	}
	defer f.Close()
	out := map[string]struct{}{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	for s.Scan() {
		// Minimal parse: we only need the tx field. A tiny struct
		// avoids the allocation/unmarshal cost of the full Datom.
		var head struct {
			Tx string `json:"tx"`
		}
		if err := json.Unmarshal(s.Bytes(), &head); err != nil {
			return nil, fmt.Errorf("log: collect txs parse %s: %w", path, err)
		}
		if head.Tx != "" {
			out[head.Tx] = struct{}{}
		}
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("log: collect txs scan %s: %w", path, err)
	}
	return out, nil
}

// listDirSegments returns the lexicographically sorted absolute paths
// of every *.jsonl segment file directly under dir, excluding files
// inside the quarantine subdirectory. Used by Load and tests alike.
func listDirSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("log: listsegments readdir %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}
