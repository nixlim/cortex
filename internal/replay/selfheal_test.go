package replay

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/watermark"
)

// fakeApplier records every Apply call so tests can assert on the
// exact datoms a backend saw, and optionally fails on a nominated tx
// so AC4 (error naming the backend and tx) can be exercised.
type fakeApplier struct {
	name    string
	seen    []string
	failOn  string
	failErr error
}

func (f *fakeApplier) Name() string { return f.name }
func (f *fakeApplier) Apply(_ context.Context, d datom.Datom) error {
	if f.failOn != "" && d.Tx == f.failOn {
		return f.failErr
	}
	f.seen = append(f.seen, d.Tx)
	return nil
}

// fakeStore is an in-memory WatermarkStore that lets tests pin the
// initial watermark and observe Update calls.
type fakeStore struct {
	wm           watermark.Watermark
	neo4jWrites  []string
	weavWrites   []string
	readErr      error
	neo4jErr     error
	weavErr      error
}

func (s *fakeStore) Read(_ context.Context) (watermark.Watermark, error) {
	return s.wm, s.readErr
}
func (s *fakeStore) UpdateNeo4j(_ context.Context, tx string) error {
	s.neo4jWrites = append(s.neo4jWrites, tx)
	return s.neo4jErr
}
func (s *fakeStore) UpdateWeaviate(_ context.Context, tx string) error {
	s.weavWrites = append(s.weavWrites, tx)
	return s.weavErr
}

// writeSegment builds a segment file containing sealed datoms for the
// supplied tx ULIDs. Tests craft exact log layouts without going
// through the real Writer.
func writeSegment(t *testing.T, path string, txs []string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	for _, tx := range txs {
		d := datom.Datom{
			Tx:           tx,
			Ts:           "2026-04-10T00:00:00Z",
			Actor:        "test",
			Op:           datom.OpAdd,
			E:            "entry:replay",
			A:            "body",
			V:            json.RawMessage(`"ok"`),
			Src:          "replay_test",
			InvocationID: "01HPTESTINVOCATION0000000000",
		}
		if err := d.Seal(); err != nil {
			t.Fatalf("seal: %v", err)
		}
		b, err := datom.Marshal(&d)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := f.Write(b); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

// sortedTxs returns five ULIDs in ascending order so test fixtures can
// reason about T1..T5 as stable labels.
func sortedTxs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = ulid.Make().String()
	}
	sort.Strings(out)
	return out
}

// TestSelfHeal_AppliesOnlyMissingDatoms covers AC1: log max(tx)=T5 and
// Neo4j watermark=T3 results in exactly T4 and T5 being applied.
func TestSelfHeal_AppliesOnlyMissingDatoms(t *testing.T) {
	dir := t.TempDir()
	txs := sortedTxs(5) // T1..T5
	path := filepath.Join(dir, "01HREPLAY000000000000000000-pid1.jsonl")
	writeSegment(t, path, txs)

	store := &fakeStore{wm: watermark.Watermark{
		Neo4jTx:         txs[2], // T3
		Neo4jWritten:    true,
		WeaviateTx:      txs[4], // already caught up
		WeaviateWritten: true,
	}}
	neo := &fakeApplier{name: "neo4j"}
	weav := &fakeApplier{name: "weaviate"}

	res, err := SelfHeal(context.Background(), []string{path}, store, neo, weav)
	if err != nil {
		t.Fatalf("SelfHeal: %v", err)
	}
	if res.LogMaxTx != txs[4] {
		t.Fatalf("LogMaxTx: got %s want %s", res.LogMaxTx, txs[4])
	}
	if res.Neo4jApplied != 2 {
		t.Fatalf("Neo4jApplied: got %d want 2", res.Neo4jApplied)
	}
	if res.WeaviateApplied != 0 {
		t.Fatalf("WeaviateApplied: got %d want 0", res.WeaviateApplied)
	}
	if len(neo.seen) != 2 || neo.seen[0] != txs[3] || neo.seen[1] != txs[4] {
		t.Fatalf("neo4j saw wrong txs: %v want [%s %s]", neo.seen, txs[3], txs[4])
	}
	if len(weav.seen) != 0 {
		t.Fatalf("weaviate was touched: %v", weav.seen)
	}
	// Watermark advanced to log max for the lagging backend only.
	if len(store.neo4jWrites) != 1 || store.neo4jWrites[0] != txs[4] {
		t.Fatalf("neo4j watermark write: %v want [%s]", store.neo4jWrites, txs[4])
	}
	if len(store.weavWrites) != 0 {
		t.Fatalf("weaviate watermark touched: %v", store.weavWrites)
	}
}

// TestSelfHeal_BothCaughtUpIsNoop covers AC2: matching watermarks on
// both backends produce zero Apply calls and zero Update calls.
func TestSelfHeal_BothCaughtUpIsNoop(t *testing.T) {
	dir := t.TempDir()
	txs := sortedTxs(3)
	path := filepath.Join(dir, "01HREPLAY000000000000000000-pid1.jsonl")
	writeSegment(t, path, txs)

	store := &fakeStore{wm: watermark.Watermark{
		Neo4jTx: txs[2], Neo4jWritten: true,
		WeaviateTx: txs[2], WeaviateWritten: true,
	}}
	neo := &fakeApplier{name: "neo4j"}
	weav := &fakeApplier{name: "weaviate"}

	res, err := SelfHeal(context.Background(), []string{path}, store, neo, weav)
	if err != nil {
		t.Fatalf("SelfHeal: %v", err)
	}
	if res.Neo4jApplied != 0 || res.WeaviateApplied != 0 {
		t.Fatalf("expected zero applies, got %+v", res)
	}
	if len(neo.seen) != 0 || len(weav.seen) != 0 {
		t.Fatalf("backends touched: neo=%v weav=%v", neo.seen, weav.seen)
	}
	if len(store.neo4jWrites) != 0 || len(store.weavWrites) != 0 {
		t.Fatalf("watermarks touched: neo=%v weav=%v", store.neo4jWrites, store.weavWrites)
	}
}

// TestSelfHeal_FreshInstallAppliesAll exercises the never-written
// watermark path: an unwritten watermark is the empty string, so every
// datom is in the replay window.
func TestSelfHeal_FreshInstallAppliesAll(t *testing.T) {
	dir := t.TempDir()
	txs := sortedTxs(4)
	path := filepath.Join(dir, "01HREPLAY000000000000000000-pid1.jsonl")
	writeSegment(t, path, txs)

	store := &fakeStore{} // zero value: both watermarks never written
	neo := &fakeApplier{name: "neo4j"}
	weav := &fakeApplier{name: "weaviate"}

	res, err := SelfHeal(context.Background(), []string{path}, store, neo, weav)
	if err != nil {
		t.Fatalf("SelfHeal: %v", err)
	}
	if res.Neo4jApplied != 4 || res.WeaviateApplied != 4 {
		t.Fatalf("applied: got %+v want 4/4", res)
	}
	if len(neo.seen) != 4 || len(weav.seen) != 4 {
		t.Fatalf("backend counts: neo=%d weav=%d", len(neo.seen), len(weav.seen))
	}
	for i, tx := range txs {
		if neo.seen[i] != tx {
			t.Fatalf("neo order at %d: got %s want %s", i, neo.seen[i], tx)
		}
		if weav.seen[i] != tx {
			t.Fatalf("weav order at %d: got %s want %s", i, weav.seen[i], tx)
		}
	}
}

// TestSelfHeal_EmptyLogIsNoop verifies that a fresh install with no
// segments at all returns a zero Result and advances no watermark.
func TestSelfHeal_EmptyLogIsNoop(t *testing.T) {
	store := &fakeStore{}
	neo := &fakeApplier{name: "neo4j"}
	weav := &fakeApplier{name: "weaviate"}

	res, err := SelfHeal(context.Background(), nil, store, neo, weav)
	if err != nil {
		t.Fatalf("SelfHeal: %v", err)
	}
	if res.LogMaxTx != "" || res.Neo4jApplied != 0 || res.WeaviateApplied != 0 {
		t.Fatalf("expected zero Result, got %+v", res)
	}
	if len(store.neo4jWrites) != 0 || len(store.weavWrites) != 0 {
		t.Fatalf("watermarks touched on empty log: %v %v", store.neo4jWrites, store.weavWrites)
	}
}

// TestSelfHeal_BackendFailureIdentifiesTx covers AC4: on Apply error
// SelfHeal returns a *BackendError naming the backend and failing tx,
// and does NOT advance the watermark for that backend.
func TestSelfHeal_BackendFailureIdentifiesTx(t *testing.T) {
	dir := t.TempDir()
	txs := sortedTxs(3)
	path := filepath.Join(dir, "01HREPLAY000000000000000000-pid1.jsonl")
	writeSegment(t, path, txs)

	store := &fakeStore{} // both unwritten
	boom := errors.New("bolt: connection refused")
	neo := &fakeApplier{name: "neo4j", failOn: txs[1], failErr: boom}
	weav := &fakeApplier{name: "weaviate"}

	_, err := SelfHeal(context.Background(), []string{path}, store, neo, weav)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var be *BackendError
	if !errors.As(err, &be) {
		t.Fatalf("expected *BackendError, got %T: %v", err, err)
	}
	if be.Backend != "neo4j" {
		t.Fatalf("backend: got %q want neo4j", be.Backend)
	}
	if be.Tx != txs[1] {
		t.Fatalf("tx: got %q want %q", be.Tx, txs[1])
	}
	if !errors.Is(err, boom) {
		t.Fatalf("underlying error lost: %v", err)
	}
	// Watermark not advanced for the failing backend.
	if len(store.neo4jWrites) != 0 {
		t.Fatalf("neo4j watermark advanced despite failure: %v", store.neo4jWrites)
	}
	// Weaviate branch never ran because Neo4j errored first.
	if len(weav.seen) != 0 {
		t.Fatalf("weaviate ran despite earlier failure: %v", weav.seen)
	}
}

// TestSelfHeal_NilBackendSkipped covers the degraded-mode path where
// only one of the two backends is configured.
func TestSelfHeal_NilBackendSkipped(t *testing.T) {
	dir := t.TempDir()
	txs := sortedTxs(2)
	path := filepath.Join(dir, "01HREPLAY000000000000000000-pid1.jsonl")
	writeSegment(t, path, txs)

	store := &fakeStore{}
	neo := &fakeApplier{name: "neo4j"}

	res, err := SelfHeal(context.Background(), []string{path}, store, neo, nil)
	if err != nil {
		t.Fatalf("SelfHeal: %v", err)
	}
	if res.Neo4jApplied != 2 {
		t.Fatalf("neo applied: %d want 2", res.Neo4jApplied)
	}
	if res.WeaviateApplied != 0 {
		t.Fatalf("weav applied without client: %d", res.WeaviateApplied)
	}
	if len(store.weavWrites) != 0 {
		t.Fatalf("weaviate watermark touched with no client: %v", store.weavWrites)
	}
}
