package rebuild

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// ---------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------

// fakeDigest returns a fixed digest from CurrentDigest. Used to drive
// the drift check from tests.
type fakeDigest struct {
	digest string
	err    error
}

func (f fakeDigest) CurrentDigest(_ context.Context) (string, error) {
	return f.digest, f.err
}

// fakeEmbedder records every Embed call so tests can assert that
// only --accept-drift triggered re-embedding.
type fakeEmbedder struct {
	calls []string
	vec   []float32
	err   error
}

func (f *fakeEmbedder) Embed(_ context.Context, body string) ([]float32, error) {
	f.calls = append(f.calls, body)
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

// fakeAppender captures appended datom groups.
type fakeAppender struct {
	groups [][]datom.Datom
	err    error
}

func (f *fakeAppender) Append(group []datom.Datom) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return cp[0].Tx, nil
}

// fakeBackends is a StagingBackends double that records the order of
// lifecycle calls and the datoms applied. Tests assert on these
// directly to verify the staging-vs-active invariant.
type fakeBackends struct {
	createCalls  int
	swapCalls    int
	cleanupCalls int

	appliedDatoms []datom.Datom
	embeddings    map[string][]float32

	createErr error
	applyErr  error
	swapErr   error

	// callOrder records the lifecycle method names in invocation
	// order. Tests assert this never contains "Apply" before
	// "Create" and "Swap" after every "Apply".
	callOrder []string
}

func newFakeBackends() *fakeBackends {
	return &fakeBackends{embeddings: map[string][]float32{}}
}

func (f *fakeBackends) Create(_ context.Context) error {
	f.createCalls++
	f.callOrder = append(f.callOrder, "Create")
	return f.createErr
}

func (f *fakeBackends) ApplyDatom(_ context.Context, d datom.Datom) error {
	f.callOrder = append(f.callOrder, "Apply")
	if f.applyErr != nil {
		return f.applyErr
	}
	f.appliedDatoms = append(f.appliedDatoms, d)
	return nil
}

func (f *fakeBackends) ApplyEmbedding(_ context.Context, entryID string, vector []float32) error {
	f.callOrder = append(f.callOrder, "ApplyEmbedding")
	f.embeddings[entryID] = vector
	return nil
}

func (f *fakeBackends) Swap(_ context.Context) error {
	f.swapCalls++
	f.callOrder = append(f.callOrder, "Swap")
	return f.swapErr
}

func (f *fakeBackends) Cleanup(_ context.Context) error {
	f.cleanupCalls++
	f.callOrder = append(f.callOrder, "Cleanup")
	return nil
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

// newDatom builds a sealed datom with the given fields. Tests use it
// to synthesize a fake log without going through the real write path
// (the real write path doesn't yet emit embedding_model_digest).
func newDatom(t *testing.T, tx, entryID, attr string, value any) datom.Datom {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	d := datom.Datom{
		Tx:           tx,
		Ts:           time.Now().UTC().Format(time.RFC3339Nano),
		Actor:        "tester",
		Op:           datom.OpAdd,
		E:            entryID,
		A:            attr,
		V:            raw,
		Src:          "observe",
		InvocationID: "inv-test",
	}
	if err := d.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return d
}

// makeEntry returns a body + digest pair of datoms with monotonically
// increasing tx ULIDs so the slice mimics a real merge-sort stream.
func makeEntry(t *testing.T, entryID, body, digest string) []datom.Datom {
	t.Helper()
	tx1 := ulid.Make().String()
	tx2 := ulid.Make().String()
	return []datom.Datom{
		newDatom(t, tx1, entryID, AttrBody, body),
		newDatom(t, tx2, entryID, AttrEmbeddingModelDigest, digest),
	}
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

func TestRunSucceedsOnMatchingDigests(t *testing.T) {
	rows := makeEntry(t, EntryPrefix+"01AAA", "hello", "sha256:aaa")
	rows = append(rows, makeEntry(t, EntryPrefix+"01BBB", "world", "sha256:aaa")...)

	be := newFakeBackends()
	res, err := Run(context.Background(), Config{
		Source:       NewSliceSource(rows),
		Digest:       fakeDigest{digest: "sha256:aaa"},
		Backends:     be,
		Actor:        "tester",
		InvocationID: "inv-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DatomsScanned != len(rows) {
		t.Errorf("scanned = %d, want %d", res.DatomsScanned, len(rows))
	}
	if res.EntriesApplied != 2 {
		t.Errorf("entries = %d, want 2", res.EntriesApplied)
	}
	if res.RebindsPerformed != 0 {
		t.Errorf("rebinds = %d, want 0", res.RebindsPerformed)
	}
	if be.createCalls != 1 || be.swapCalls != 1 {
		t.Errorf("lifecycle: create=%d swap=%d, want 1/1", be.createCalls, be.swapCalls)
	}
	if len(be.appliedDatoms) != len(rows) {
		t.Errorf("applied = %d, want %d", len(be.appliedDatoms), len(rows))
	}
}

func TestRunFailsWithPinnedDriftWhenDigestsMismatch(t *testing.T) {
	rows := makeEntry(t, EntryPrefix+"01AAA", "hello", "sha256:aaa")
	rows = append(rows, makeEntry(t, EntryPrefix+"01BBB", "world", "sha256:aaa")...)

	be := newFakeBackends()
	_, err := Run(context.Background(), Config{
		Source:   NewSliceSource(rows),
		Digest:   fakeDigest{digest: "sha256:bbb"},
		Backends: be,
	})
	if err == nil {
		t.Fatal("expected PINNED_MODEL_DRIFT, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "PINNED_MODEL_DRIFT" || e.Kind != errs.KindOperational {
		t.Fatalf("err = %v, want operational PINNED_MODEL_DRIFT", err)
	}
	if e.Details["current_digest"] != "sha256:bbb" {
		t.Errorf("current_digest = %v, want sha256:bbb", e.Details["current_digest"])
	}
	affected, _ := e.Details["affected_entries"].([]string)
	if len(affected) != 2 {
		t.Errorf("affected len = %d, want 2", len(affected))
	}
	// Backends must be untouched: AC3 says "leaves the active
	// derived index untouched until the rebuild succeeds".
	if be.createCalls != 0 {
		t.Errorf("create called %d times before drift error", be.createCalls)
	}
	if len(be.appliedDatoms) != 0 {
		t.Errorf("backends got %d apply calls before drift error", len(be.appliedDatoms))
	}
	if be.swapCalls != 0 {
		t.Error("swap was called despite drift error")
	}
}

func TestRunAcceptDriftReembedsAndWritesAuditDatoms(t *testing.T) {
	rows := makeEntry(t, EntryPrefix+"01AAA", "hello", "sha256:aaa")
	rows = append(rows, makeEntry(t, EntryPrefix+"01BBB", "world", "sha256:aaa")...)
	// Add a third entry that already matches current digest — it
	// must NOT be re-embedded or audited.
	rows = append(rows, makeEntry(t, EntryPrefix+"01CCC", "matched", "sha256:bbb")...)

	be := newFakeBackends()
	emb := &fakeEmbedder{vec: []float32{0.1, 0.2}}
	app := &fakeAppender{}

	res, err := Run(context.Background(), Config{
		Source:       NewSliceSource(rows),
		Digest:       fakeDigest{digest: "sha256:bbb"},
		Backends:     be,
		AcceptDrift:  true,
		Embedder:     emb,
		Log:          app,
		Actor:        "tester",
		InvocationID: "inv-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.RebindsPerformed != 2 {
		t.Errorf("rebinds = %d, want 2", res.RebindsPerformed)
	}
	if len(emb.calls) != 2 {
		t.Errorf("embedder calls = %d, want 2 (one per drifted entry)", len(emb.calls))
	}
	if len(app.groups) != 2 {
		t.Errorf("audit datom groups = %d, want 2", len(app.groups))
	}
	// Verify the audit datoms have the model_rebind attribute and
	// the right from/to digests.
	for _, g := range app.groups {
		if len(g) != 1 {
			t.Errorf("audit group len = %d, want 1", len(g))
			continue
		}
		d := g[0]
		if d.A != AttrModelRebind {
			t.Errorf("audit attr = %q, want %q", d.A, AttrModelRebind)
		}
		var payload map[string]string
		if err := json.Unmarshal(d.V, &payload); err != nil {
			t.Errorf("audit value not a string map: %v", err)
			continue
		}
		if payload["from"] != "sha256:aaa" || payload["to"] != "sha256:bbb" {
			t.Errorf("audit payload = %v, want from sha256:aaa to sha256:bbb", payload)
		}
		if d.Src != SourceRebuild {
			t.Errorf("audit src = %q, want %q", d.Src, SourceRebuild)
		}
	}
	if be.swapCalls != 1 {
		t.Error("swap not called on success path")
	}
	// The matched entry must not be in the embeddings map.
	if _, ok := be.embeddings[EntryPrefix+"01CCC"]; ok {
		t.Error("matched entry was unexpectedly re-embedded")
	}
}

func TestRunIsIdempotent(t *testing.T) {
	rows := makeEntry(t, EntryPrefix+"01AAA", "hello", "sha256:aaa")

	// First run.
	be1 := newFakeBackends()
	res1, err := Run(context.Background(), Config{
		Source:   NewSliceSource(rows),
		Digest:   fakeDigest{digest: "sha256:aaa"},
		Backends: be1,
	})
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}

	// Second run over the same source.
	be2 := newFakeBackends()
	res2, err := Run(context.Background(), Config{
		Source:   NewSliceSource(rows),
		Digest:   fakeDigest{digest: "sha256:aaa"},
		Backends: be2,
	})
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if res1.DatomsScanned != res2.DatomsScanned {
		t.Errorf("scanned1=%d scanned2=%d", res1.DatomsScanned, res2.DatomsScanned)
	}
	if res1.EntriesApplied != res2.EntriesApplied {
		t.Errorf("applied1=%d applied2=%d", res1.EntriesApplied, res2.EntriesApplied)
	}
	if res1.RebindsPerformed != 0 || res2.RebindsPerformed != 0 {
		t.Errorf("rebinds = %d/%d, want 0/0 (no audit datoms emitted on idempotent runs)",
			res1.RebindsPerformed, res2.RebindsPerformed)
	}
}

func TestRunCleansUpStagingOnApplyFailure(t *testing.T) {
	rows := makeEntry(t, EntryPrefix+"01AAA", "hello", "sha256:aaa")

	be := newFakeBackends()
	be.applyErr = errors.New("boom")
	_, err := Run(context.Background(), Config{
		Source:   NewSliceSource(rows),
		Digest:   fakeDigest{digest: "sha256:aaa"},
		Backends: be,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "STAGING_APPLY_FAILED" {
		t.Errorf("err = %v, want STAGING_APPLY_FAILED", err)
	}
	if be.cleanupCalls != 1 {
		t.Errorf("cleanup calls = %d, want 1", be.cleanupCalls)
	}
	if be.swapCalls != 0 {
		t.Error("swap was called on apply failure")
	}
}

func TestRunValidationRejectsAcceptDriftWithoutEmbedder(t *testing.T) {
	_, err := Run(context.Background(), Config{
		Source:      NewSliceSource(nil),
		Digest:      fakeDigest{digest: "sha256:aaa"},
		Backends:    newFakeBackends(),
		AcceptDrift: true,
	})
	var e *errs.Error
	if !errors.As(err, &e) || e.Kind != errs.KindValidation || e.Code != "MISSING_EMBEDDER" {
		t.Errorf("err = %v, want validation MISSING_EMBEDDER", err)
	}
}

func TestRunRejectsEmptyCurrentDigest(t *testing.T) {
	_, err := Run(context.Background(), Config{
		Source:   NewSliceSource(nil),
		Digest:   fakeDigest{digest: ""},
		Backends: newFakeBackends(),
	})
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "DIGEST_LOOKUP_FAILED" {
		t.Errorf("err = %v, want DIGEST_LOOKUP_FAILED", err)
	}
}

func TestRunIgnoresEntriesWithoutPinnedDigest(t *testing.T) {
	// One entry has no embedding_model_digest datom — it must be
	// scanned and applied without triggering a drift error.
	tx := ulid.Make().String()
	rows := []datom.Datom{
		newDatom(t, tx, EntryPrefix+"01AAA", AttrBody, "no-digest"),
	}
	be := newFakeBackends()
	_, err := Run(context.Background(), Config{
		Source:   NewSliceSource(rows),
		Digest:   fakeDigest{digest: "sha256:aaa"},
		Backends: be,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if be.swapCalls != 1 {
		t.Errorf("swap calls = %d, want 1", be.swapCalls)
	}
}
