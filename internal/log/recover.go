package log

import (
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

// defaultTailWindowBytes mirrors log.tail_validation_window_bytes from
// the spec's Operational Defaults table (65536). Kept here so package
// users that don't load the full config can still call RecoverDir with
// a sane value via the zero-options path.
const defaultTailWindowBytes = 64 * 1024

// ErrUnrecoverableTail is returned in a TailReport when the entire tail
// window of a segment fails checksum validation and the window does not
// reach the start of the file. The caller must not delete the segment
// (spec constraint); it should surface the failure for quarantine in
// cortex-4kq.28.
var ErrUnrecoverableTail = errors.New("log: segment tail unrecoverable within validation window")

// TailReport captures the outcome of validating (and optionally
// truncating) the torn tail of a single segment file. One report is
// produced per segment that RecoverDir inspects.
type TailReport struct {
	// Path is the absolute segment file path.
	Path string

	// OriginalSize is the size of the file before any truncation.
	OriginalSize int64

	// FinalSize is the size of the file after validation. Equal to
	// OriginalSize when the tail was already intact.
	FinalSize int64

	// BytesRead is the number of bytes read from the file during
	// validation. Never exceeds tail_window_bytes (or OriginalSize if
	// the file is smaller than the window).
	BytesRead int64

	// DatomsValidated is the count of well-formed datoms the validator
	// parsed and checksum-verified inside the tail window. Useful for
	// ops telemetry but not part of the commit protocol.
	DatomsValidated int

	// Truncated reports whether the file was actually shortened.
	Truncated bool

	// Unrecoverable is true when the entire tail window failed
	// validation and no earlier safe boundary exists. Implies
	// !Truncated and FinalSize == OriginalSize.
	Unrecoverable bool

	// LastTx is the tx ULID of the last well-formed datom found in the
	// tail window, or the empty string if the window contained none.
	// Startup self-healing uses this to compute the log watermark
	// without re-reading the full segment.
	LastTx string
}

// ValidateTail inspects the final windowBytes of a segment file and
// repairs a *torn* tail only: the case where a crash during an append
// left the file without a terminating newline on its last line. A
// clean tail (file ends with '\n') is always left untouched so AC2
// "mtime unchanged on a clean segment" holds.
//
// ValidateTail explicitly does NOT quarantine or truncate mid-segment
// corruption. If a complete line in the tail window fails checksum
// verification the file is still left intact: middle-of-file
// corruption is the responsibility of ScanSegment + Quarantine in
// cortex-4kq.28, which moves the whole segment aside. That separation
// keeps torn-tail recovery safe on files that have both a clean tail
// and some ancient mid-segment bit rot.
//
// The function reads at most windowBytes bytes even for very large
// segments; callers that care about I/O cost can inspect BytesRead on
// the returned report.
func ValidateTail(path string, windowBytes int) (TailReport, error) {
	if windowBytes <= 0 {
		windowBytes = defaultTailWindowBytes
	}
	report := TailReport{Path: path}

	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return report, fmt.Errorf("log: recover open %s: %w", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return report, fmt.Errorf("log: recover stat %s: %w", path, err)
	}
	report.OriginalSize = st.Size()
	report.FinalSize = st.Size()
	if st.Size() == 0 {
		return report, nil
	}

	// Read only the tail window. For files smaller than windowBytes we
	// read from offset 0.
	var readFrom int64
	if st.Size() > int64(windowBytes) {
		readFrom = st.Size() - int64(windowBytes)
	}
	bufLen := st.Size() - readFrom
	buf := make([]byte, bufLen)
	n, err := f.ReadAt(buf, readFrom)
	if err != nil && !errors.Is(err, io.EOF) {
		return report, fmt.Errorf("log: recover read %s: %w", path, err)
	}
	buf = buf[:n]
	report.BytesRead = int64(n)

	// If the last byte is already a newline, the file ends on a line
	// boundary and there is nothing torn to repair. Walk the window
	// forward to populate LastTx / DatomsValidated for telemetry, but
	// do not touch the file. Unverifiable lines inside a clean tail
	// are left for ScanSegment.
	if buf[len(buf)-1] == '\n' {
		populateTelemetry(&report, buf, readFrom)
		return report, nil
	}

	// Torn tail: the last line has no terminator. The safe boundary
	// is the byte after the last '\n' in the window. If the window
	// contains no newline at all we fall back to: (a) truncate to 0
	// if the whole file fits in the window, (b) declare the file
	// unrecoverable otherwise (spec constraint: never delete).
	lastNL := -1
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			lastNL = i
			break
		}
	}
	var safeAbs int64
	if lastNL >= 0 {
		safeAbs = readFrom + int64(lastNL+1)
	} else if readFrom == 0 {
		safeAbs = 0
	} else {
		report.Unrecoverable = true
		return report, ErrUnrecoverableTail
	}

	if safeAbs == st.Size() {
		// Should not happen (we already checked the trailing byte),
		// but guard against it so a future refactor does not
		// accidentally shrink a clean file.
		return report, nil
	}
	if err := f.Truncate(safeAbs); err != nil {
		return report, fmt.Errorf("log: truncate %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return report, fmt.Errorf("log: sync %s after truncate: %w", path, err)
	}
	report.FinalSize = safeAbs
	report.Truncated = true

	// Re-read the retained region so LastTx / DatomsValidated reflect
	// the post-truncation state. This is still bounded by windowBytes.
	if safeAbs > 0 {
		tailStart := int64(0)
		if safeAbs > int64(windowBytes) {
			tailStart = safeAbs - int64(windowBytes)
		}
		telemetryBuf := make([]byte, safeAbs-tailStart)
		if _, err := f.ReadAt(telemetryBuf, tailStart); err != nil && !errors.Is(err, io.EOF) {
			// Telemetry is best-effort; do not fail recovery.
			return report, nil
		}
		populateTelemetry(&report, telemetryBuf, tailStart)
	}
	return report, nil
}

// populateTelemetry walks the buffer forward, verifying every complete
// line inside the in-window region and updating the report's LastTx
// and DatomsValidated counters. A verify failure stops the walk for
// telemetry purposes only — it does not propagate to truncation or
// quarantine decisions.
func populateTelemetry(report *TailReport, buf []byte, readFrom int64) {
	lineEnds := findLineEnds(buf)
	firstStart := -1
	if readFrom == 0 {
		firstStart = 0
	} else if len(lineEnds) > 0 {
		firstStart = lineEnds[0] + 1
	}
	if firstStart < 0 {
		return
	}
	cursor := firstStart
	for _, nl := range lineEnds {
		if nl < cursor {
			continue
		}
		line := buf[cursor:nl]
		d, verr := datom.Unmarshal(line)
		if verr != nil {
			// Mid-window bit rot: stop counting but do not flag the
			// segment as torn. Quarantine owns this.
			return
		}
		report.DatomsValidated++
		report.LastTx = d.Tx
		cursor = nl + 1
	}
}

// findLineEnds returns the byte offsets (inside buf) of every '\n'.
// A small dedicated helper keeps ValidateTail readable and makes the
// scanning cost obvious: one linear pass over the tail window.
func findLineEnds(buf []byte) []int {
	var out []int
	for i, c := range buf {
		if c == '\n' {
			out = append(out, i)
		}
	}
	return out
}

// RecoverDir runs ValidateTail on every *.jsonl segment under dir,
// sorted by filename (which matches creation order under the ULID
// naming scheme). Segments that produce ErrUnrecoverableTail are
// reported but not deleted and not quarantined here — quarantine is
// the responsibility of cortex-4kq.28 and is deliberately decoupled
// so recovery and quarantine can be tested independently.
//
// Returns one TailReport per segment file plus an aggregated error
// list for anything the caller needs to escalate.
func RecoverDir(dir string, windowBytes int) ([]TailReport, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("log: recover readdir %s: %w", dir, err)
	}

	// Filter to *.jsonl and sort lexicographically so the caller sees
	// segments in ULID (i.e. roughly temporal) order.
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	reports := make([]TailReport, 0, len(names))
	var firstErr error
	for _, name := range names {
		path := filepath.Join(dir, name)
		report, err := ValidateTail(path, windowBytes)
		reports = append(reports, report)
		if err != nil && !errors.Is(err, ErrUnrecoverableTail) && firstErr == nil {
			firstErr = err
		}
	}
	return reports, firstErr
}

// BuildRecoveredDatom assembles a sealed `log.recovered` audit datom
// describing the outcome of a single truncation. Call sites that want
// to persist the audit trail pass the returned datom to a Writer's
// Append (as part of a transaction group with the caller-provided tx
// ULID, ts, actor, and invocation id).
//
// The value is a JSON object with the path, the original size, the
// final size, and the number of bytes removed. Keeping all the
// recovery context in the datom value means a replay over the log
// alone can reconstruct what happened without relying on ops.log.
func BuildRecoveredDatom(tx, ts, actor, invocationID string, r TailReport) (datom.Datom, error) {
	body := map[string]any{
		"path":             r.Path,
		"original_size":    r.OriginalSize,
		"final_size":       r.FinalSize,
		"bytes_removed":    r.OriginalSize - r.FinalSize,
		"datoms_validated": r.DatomsValidated,
		"last_tx":          r.LastTx,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return datom.Datom{}, fmt.Errorf("log: marshal recovered datom: %w", err)
	}
	d := datom.Datom{
		Tx:           tx,
		Ts:           ts,
		Actor:        actor,
		Op:           datom.OpAdd,
		E:            "log:" + filepath.Base(r.Path),
		A:            "log.recovered",
		V:            raw,
		Src:          "recover",
		InvocationID: invocationID,
	}
	if err := d.Seal(); err != nil {
		return datom.Datom{}, err
	}
	return d, nil
}

// lineScanBudget is a safety check used by tests to ensure the
// validator never walks past the window. It is declared here (rather
// than inlined) so the test can import the same constant and assert
// the I/O counter stays within it.
func lineScanBudget(fileSize int64, windowBytes int) int64 {
	if fileSize < int64(windowBytes) {
		return fileSize
	}
	return int64(windowBytes)
}

