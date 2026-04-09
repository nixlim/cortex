package watermark

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeNeo4j is a minimal in-memory Neo4jClient double. It stores the
// single :CortexMeta watermark node as a map and interprets the test's
// Cypher by substring match, which is sufficient for the MERGE / MATCH
// statements the watermark store emits.
type fakeNeo4j struct {
	written map[string]string // key "tx" / "updated_at"
	writeErr error
	readErr  error
}

func (f *fakeNeo4j) WriteEntries(_ context.Context, cypher string, params map[string]any) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if !strings.Contains(cypher, "MERGE") {
		return errors.New("fakeNeo4j: unexpected cypher: " + cypher)
	}
	if f.written == nil {
		f.written = map[string]string{}
	}
	if tx, ok := params["tx"].(string); ok {
		f.written["tx"] = tx
	}
	if u, ok := params["updated_at"].(string); ok {
		f.written["updated_at"] = u
	}
	return nil
}

func (f *fakeNeo4j) QueryGraph(_ context.Context, cypher string, _ map[string]any) ([]map[string]any, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if !strings.Contains(cypher, "MATCH") {
		return nil, errors.New("fakeNeo4j: unexpected read cypher: " + cypher)
	}
	if f.written == nil {
		return nil, nil
	}
	return []map[string]any{{"tx": f.written["tx"]}}, nil
}

// fakeWeaviate is a minimal in-memory WeaviateClient double.
type fakeWeaviate struct {
	obj       map[string]any
	ensureErr error
	writeErr  error
	readErr   error
}

func (f *fakeWeaviate) EnsureSchema(_ context.Context) error { return f.ensureErr }
func (f *fakeWeaviate) Upsert(_ context.Context, class, id string, vector []float32, properties map[string]any) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if class != weaviateClass || id != weaviateObjectID {
		return errors.New("fakeWeaviate: wrong class/id: " + class + "/" + id)
	}
	if len(vector) != 0 {
		return errors.New("fakeWeaviate: watermark must upsert with empty vector")
	}
	f.obj = properties
	return nil
}
func (f *fakeWeaviate) GetObject(_ context.Context, class, id string) (map[string]any, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if class != weaviateClass || id != weaviateObjectID {
		return nil, errors.New("fakeWeaviate: wrong class/id: " + class + "/" + id)
	}
	return f.obj, nil
}

func pinnedTime() time.Time {
	return time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
}

func newTestStore() (*Store, *fakeNeo4j, *fakeWeaviate) {
	fn := &fakeNeo4j{}
	fw := &fakeWeaviate{}
	s := NewStore(fn, fw)
	s.now = pinnedTime
	return s, fn, fw
}

// TestRead_FreshInstall covers the acceptance criterion "Reading a
// fresh cortex install returns the zero-valued Watermark with no
// error". Both backends are empty; the returned struct must have
// Neo4jWritten=false, WeaviateWritten=false, and no error.
func TestRead_FreshInstall(t *testing.T) {
	s, _, _ := newTestStore()
	wm, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read on fresh install: %v", err)
	}
	if wm.Neo4jWritten || wm.WeaviateWritten {
		t.Errorf("expected both backends unwritten, got %+v", wm)
	}
	if wm.Neo4jTx != "" || wm.WeaviateTx != "" {
		t.Errorf("expected zero-valued tx strings, got %+v", wm)
	}
}

// TestUpdateNeo4j_RoundTrip covers "Writing tx T5 via Update then
// calling Read returns T5 for the same backend".
func TestUpdateNeo4j_RoundTrip(t *testing.T) {
	s, _, _ := newTestStore()
	if err := s.UpdateNeo4j(context.Background(), "01HT5"); err != nil {
		t.Fatalf("UpdateNeo4j: %v", err)
	}
	wm, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if wm.Neo4jTx != "01HT5" {
		t.Errorf("Neo4jTx = %q, want 01HT5", wm.Neo4jTx)
	}
	if !wm.Neo4jWritten {
		t.Errorf("Neo4jWritten should be true after UpdateNeo4j")
	}
}

func TestUpdateWeaviate_RoundTrip(t *testing.T) {
	s, _, _ := newTestStore()
	if err := s.UpdateWeaviate(context.Background(), "01HW5"); err != nil {
		t.Fatalf("UpdateWeaviate: %v", err)
	}
	wm, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if wm.WeaviateTx != "01HW5" {
		t.Errorf("WeaviateTx = %q, want 01HW5", wm.WeaviateTx)
	}
	if !wm.WeaviateWritten {
		t.Errorf("WeaviateWritten should be true after UpdateWeaviate")
	}
}

// TestZeroULID_DistinguishedFromNeverWritten covers "The returned
// struct distinguishes a never-written watermark from one explicitly
// set to the zero ULID". The zero-ULID string is 26 zero characters
// (00000000000000000000000000); we pick a stand-in sentinel here.
func TestZeroULID_DistinguishedFromNeverWritten(t *testing.T) {
	const zeroULID = "00000000000000000000000000"

	// Case 1: explicit zero write.
	s1, _, _ := newTestStore()
	if err := s1.Update(context.Background(), zeroULID); err != nil {
		t.Fatalf("Update: %v", err)
	}
	wm1, _ := s1.Read(context.Background())
	if !wm1.Neo4jWritten || !wm1.WeaviateWritten {
		t.Fatalf("expected both Written flags true after explicit zero write, got %+v", wm1)
	}
	if wm1.Neo4jTx != zeroULID || wm1.WeaviateTx != zeroULID {
		t.Errorf("tx values = %+v, want both = zeroULID", wm1)
	}

	// Case 2: never-written — Written flags must be false even
	// though the tx strings are the same empty string.
	s2, _, _ := newTestStore()
	wm2, _ := s2.Read(context.Background())
	if wm2.Neo4jWritten || wm2.WeaviateWritten {
		t.Errorf("expected Written flags false on fresh install, got %+v", wm2)
	}

	// The critical distinguishing property: Written flag differs
	// between the two cases, even if a naive caller couldn't tell
	// them apart from the tx string alone.
	if wm1.Neo4jWritten == wm2.Neo4jWritten {
		t.Error("Neo4jWritten flag must distinguish fresh install from explicit zero write")
	}
}

// TestUpdate_AtomicPerBackend covers "Update is atomic per backend
// (interrupting between Neo4j and Weaviate updates leaves each
// backend in a valid prior state)". We simulate the interruption by
// injecting a Weaviate write error and asserting that the Neo4j
// write is still visible in a subsequent Read.
func TestUpdate_AtomicPerBackend(t *testing.T) {
	s, _, fw := newTestStore()
	fw.writeErr = errors.New("simulated weaviate failure mid-update")

	// Update propagates the Weaviate failure.
	err := s.Update(context.Background(), "01HT9")
	if err == nil {
		t.Fatal("expected Update to surface weaviate failure")
	}

	// Clear the Weaviate error so Read can succeed — the point of
	// the test is to assert that the Neo4j side is intact, not to
	// exercise Read's error path.
	fw.writeErr = nil

	wm, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("Read after partial update: %v", err)
	}
	if wm.Neo4jTx != "01HT9" {
		t.Errorf("Neo4j side should reflect the completed write, got %+v", wm)
	}
	if wm.WeaviateWritten {
		t.Errorf("Weaviate side should remain unwritten after failed upsert, got %+v", wm)
	}
}

func TestUpdateNeo4j_NoClient(t *testing.T) {
	s := NewStore(nil, &fakeWeaviate{})
	if err := s.UpdateNeo4j(context.Background(), "tx"); err == nil {
		t.Fatal("expected error when no neo4j client configured")
	}
}

func TestUpdateWeaviate_NoClient(t *testing.T) {
	s := NewStore(&fakeNeo4j{}, nil)
	if err := s.UpdateWeaviate(context.Background(), "tx"); err == nil {
		t.Fatal("expected error when no weaviate client configured")
	}
}
