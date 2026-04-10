package export

// Acceptance tests for cortex-4kq.34 (cortex export).
//
// AC1: cortex export produces a JSONL stream whose tx values are
//       strictly ascending.
// AC2: A round-trip (export, fresh install, import via merge) reaches
//       a byte-identical Layer 1 log. (Deferred to .39 integration.)
// AC3: Export of an empty log produces a zero-byte output with exit 0.
// AC4: cortex export --output=/tmp/backup.jsonl writes the stream to
//       that file with mode 0600.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
)

// seedSegment writes n sealed datoms into a fresh segment in dir and
// returns the segment path. Each datom gets its own tx (ascending
// within the segment by construction).
func seedSegment(t *testing.T, dir string, n int) string {
	t.Helper()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	for i := 0; i < n; i++ {
		tx := ulid.Make().String()
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		raw, _ := json.Marshal("value")
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        "seed",
			Op:           datom.OpAdd,
			E:            "entry:" + ulid.Make().String(),
			A:            "body",
			V:            raw,
			Src:          "test",
			InvocationID: tx,
		}
		if err := d.Seal(); err != nil {
			t.Fatalf("Seal: %v", err)
		}
		if _, err := w.Append([]datom.Datom{d}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	return w.Path()
}

func listSegments(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestExport_TxAscending — AC1: the exported JSONL stream has
// strictly ascending tx values.
func TestExport_TxAscending(t *testing.T) {
	dir := t.TempDir()
	seedSegment(t, dir, 10)
	segs := listSegments(t, dir)

	var buf bytes.Buffer
	n, err := Run(segs, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 10 {
		t.Fatalf("count = %d, want 10", n)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 10 {
		t.Fatalf("output lines = %d, want 10", len(lines))
	}
	var prevTx string
	for i, line := range lines {
		var d datom.Datom
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %d unmarshal: %v", i, err)
		}
		if err := d.Verify(); err != nil {
			t.Errorf("line %d checksum: %v", i, err)
		}
		if d.Tx <= prevTx && prevTx != "" {
			t.Errorf("line %d tx %s <= previous %s (not ascending)", i, d.Tx, prevTx)
		}
		prevTx = d.Tx
	}
}

// TestExport_EmptyLog — AC3: empty log → zero-byte output, no error.
func TestExport_EmptyLog(t *testing.T) {
	var buf bytes.Buffer
	n, err := Run(nil, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("output length = %d, want 0", buf.Len())
	}
}

// TestExport_ToFileMode0600 — AC4: file written with mode 0600.
func TestExport_ToFileMode0600(t *testing.T) {
	dir := t.TempDir()
	seedSegment(t, dir, 3)
	segs := listSegments(t, dir)

	out := filepath.Join(t.TempDir(), "backup.jsonl")
	n, err := ToFile(segs, out)
	if err != nil {
		t.Fatalf("ToFile: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 600", info.Mode().Perm())
	}

	// Verify the file content is valid JSONL with ascending tx.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("file lines = %d, want 3", len(lines))
	}
	var prevTx string
	for i, line := range lines {
		var d datom.Datom
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if d.Tx <= prevTx && prevTx != "" {
			t.Errorf("line %d tx not ascending", i)
		}
		prevTx = d.Tx
	}
}

// TestExport_MultipleSegments — verifies the merge across multiple
// segments. Two separate Writers produce interleaved tx ULIDs; the
// export must interleave them into a single ascending stream.
func TestExport_MultipleSegments(t *testing.T) {
	dir := t.TempDir()
	// Two writers → two segments in same dir.
	seedSegment(t, dir, 5)
	seedSegment(t, dir, 5)
	segs := listSegments(t, dir)
	if len(segs) < 2 {
		t.Fatalf("expected at least 2 segments, got %d", len(segs))
	}

	var buf bytes.Buffer
	n, err := Run(segs, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 10 {
		t.Fatalf("count = %d, want 10", n)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var prevTx string
	for i, line := range lines {
		var d datom.Datom
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if d.Tx <= prevTx && prevTx != "" {
			t.Errorf("line %d tx %s <= %s", i, d.Tx, prevTx)
		}
		prevTx = d.Tx
	}
}

// TestExport_EmptyDirectory — an empty segment dir (no .jsonl files)
// should behave like an empty log.
func TestExport_EmptyDirectory(t *testing.T) {
	var buf bytes.Buffer
	n, err := Run([]string{}, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("expected zero output for empty segment list")
	}
}
