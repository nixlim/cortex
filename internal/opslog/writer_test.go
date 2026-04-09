package opslog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const testInvID = "01J000000000000000000000IV"

func newTestWriter(t *testing.T, maxMB int) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.log")
	w, err := New(Options{Path: path, InvocationID: testInvID, MaxSizeMB: maxMB})
	if err != nil {
		t.Fatal(err)
	}
	return w, path
}

func TestEventHasAllEightRequiredFields(t *testing.T) {
	w, path := newTestWriter(t, 1)
	if err := w.Write(Event{
		Component: "observe",
		Tx:        "01J000000000000000000000TX",
		EntityIDs: []string{"entry:1", "entry:2"},
		Message:   "wrote entry",
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &m); err != nil {
		t.Fatalf("line is not valid JSON: %v", err)
	}
	required := []string{"timestamp", "level", "invocation_id", "component", "tx", "entity_ids", "message", "error"}
	for _, f := range required {
		if _, ok := m[f]; !ok {
			t.Errorf("missing required field %q", f)
		}
	}
	if m["invocation_id"] != testInvID {
		t.Errorf("invocation_id: got %v want %s", m["invocation_id"], testInvID)
	}
}

func TestEveryLineIsValidJSON(t *testing.T) {
	w, path := newTestWriter(t, 1)
	for i := 0; i < 5; i++ {
		if err := w.Write(Event{Component: "test", Message: "hello"}); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("line %d invalid JSON: %v", i, err)
		}
	}
}

func TestRotationOnOversizeWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.log")
	// Use a small maxBytes by constructing Writer directly so we don't
	// have to produce 50 MB of test data.
	w, err := New(Options{Path: path, InvocationID: testInvID, MaxSizeMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.maxBytes = 256 // tiny

	// First line fits.
	if err := w.Write(Event{Component: "c", Message: strings.Repeat("a", 100)}); err != nil {
		t.Fatal(err)
	}
	// Second line pushes past the cap.
	if err := w.Write(Event{Component: "c", Message: strings.Repeat("b", 200)}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated file at %s.1: %v", path, err)
	}
	// Active file should contain exactly one line (the write that
	// triggered rotation).
	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), "\n"); n != 1 {
		t.Errorf("active file should have 1 line post-rotate, got %d", n)
	}
}

func TestConcurrentWritersExactlyNLines(t *testing.T) {
	w, path := newTestWriter(t, 50)
	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			_ = w.Write(Event{Component: "c", Message: "m"})
		}(i)
	}
	wg.Wait()
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != N {
		t.Fatalf("expected %d lines, got %d", N, len(lines))
	}
	// Every line parses cleanly — proving atomic append, no interleave.
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("line %d corrupted by interleave: %v", i, err)
		}
	}
}

func TestFileModeIs0600(t *testing.T) {
	w, path := newTestWriter(t, 1)
	if err := w.Write(Event{Component: "c", Message: "m"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file mode: got %v want 0600", info.Mode().Perm())
	}
}
