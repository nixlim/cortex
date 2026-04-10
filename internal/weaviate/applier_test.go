package weaviate

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nixlim/cortex/internal/datom"
)

// fakeUpserter records every Upsert call so tests can assert the
// applier's translation without standing up an httptest server.
type fakeUpserter struct {
	calls []upsertCall
	err   error
}

type upsertCall struct {
	class      string
	id         string
	vector     []float32
	properties map[string]any
}

func (f *fakeUpserter) Upsert(_ context.Context, class, id string, vector []float32, properties map[string]any) error {
	// Copy the property map so later mutations on the applier's bag
	// don't bleed into recorded calls.
	pcopy := make(map[string]any, len(properties))
	for k, v := range properties {
		pcopy[k] = v
	}
	f.calls = append(f.calls, upsertCall{class: class, id: id, vector: vector, properties: pcopy})
	return f.err
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return raw
}

func TestBackendApplier_Name(t *testing.T) {
	a := newBackendApplierFor(&fakeUpserter{})
	if a.Name() != "weaviate" {
		t.Fatalf("Name() = %q, want %q", a.Name(), "weaviate")
	}
}

func TestApply_EmptyEntityIsNoOp(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	if err := a.Apply(context.Background(), datom.Datom{Tx: "T1", A: "body", V: mustRaw(t, "x")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("expected 0 upserts for empty entity, got %d", len(f.calls))
	}
}

func TestApply_NonEntryNonFrameSkipped(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "subject:alice", A: "canonical", V: mustRaw(t, "alice")}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("expected subject to skip Weaviate, got %d calls", len(f.calls))
	}
}

func TestApply_EntryRoutesToClassEntry(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "body", V: mustRaw(t, "hello world")}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(f.calls))
	}
	c := f.calls[0]
	if c.class != ClassEntry {
		t.Errorf("class = %q, want %q", c.class, ClassEntry)
	}
	if c.properties["body"] != "hello world" {
		t.Errorf("body = %v", c.properties["body"])
	}
	if c.properties["cortex_id"] != "entry:01ARZ" {
		t.Errorf("cortex_id = %v", c.properties["cortex_id"])
	}
}

func TestApply_FrameRoutesToClassFrame(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "frame:01XYZ", A: "frame_type", V: mustRaw(t, "BugPattern")}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.calls[0].class != ClassFrame {
		t.Errorf("class = %q, want %q", f.calls[0].class, ClassFrame)
	}
}

func TestApply_AccumulatesAcrossCalls(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	calls := []datom.Datom{
		{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "body", V: mustRaw(t, "hello")},
		{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "kind", V: mustRaw(t, "note")},
		{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "facet.domain", V: mustRaw(t, "auth")},
	}
	for _, d := range calls {
		if err := a.Apply(context.Background(), d); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if len(f.calls) != 3 {
		t.Fatalf("expected 3 upserts (one per datom), got %d", len(f.calls))
	}
	final := f.calls[2].properties
	if final["body"] != "hello" {
		t.Errorf("final body = %v", final["body"])
	}
	if final["kind"] != "note" {
		t.Errorf("final kind = %v", final["kind"])
	}
	if final["facet_domain"] != "auth" {
		t.Errorf("final facet_domain = %v", final["facet_domain"])
	}
	if final["cortex_id"] != "entry:01ARZ" {
		t.Errorf("final cortex_id = %v", final["cortex_id"])
	}
}

func TestApply_RetractSetsRetractedFlag(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T2", Op: datom.OpRetract, E: "entry:01ARZ", A: "exists", V: mustRaw(t, false)}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if f.calls[0].properties["retracted"] != true {
		t.Errorf("expected retracted=true, got %v", f.calls[0].properties["retracted"])
	}
}

func TestApply_NumericValuesPreserveType(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "importance", V: mustRaw(t, 0.85)}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if v, ok := f.calls[0].properties["importance"].(float64); !ok || v != 0.85 {
		t.Errorf("expected float64 0.85, got %T %v", f.calls[0].properties["importance"], f.calls[0].properties["importance"])
	}

	d2 := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "retrieval_count", V: mustRaw(t, 7)}
	if err := a.Apply(context.Background(), d2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := f.calls[len(f.calls)-1].properties
	if v, ok := last["retrieval_count"].(int64); !ok || v != 7 {
		t.Errorf("expected int64 7, got %T %v", last["retrieval_count"], last["retrieval_count"])
	}
}

func TestApplyWithVector_PassesVectorThrough(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	vec := []float32{0.1, 0.2, 0.3}
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "body", V: mustRaw(t, "hello")}
	if err := a.ApplyWithVector(context.Background(), d, vec); err != nil {
		t.Fatalf("ApplyWithVector: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(f.calls))
	}
	if len(f.calls[0].vector) != 3 || f.calls[0].vector[0] != 0.1 {
		t.Errorf("vector not passed through: %v", f.calls[0].vector)
	}
}

func TestPropertyName_RewritesNonAlnum(t *testing.T) {
	cases := map[string]string{
		"body":         "body",
		"facet.domain": "facet_domain",
		"":             "value",
		"7count":       "p_7count",
	}
	for in, want := range cases {
		if got := propertyName(in); got != want {
			t.Errorf("propertyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeriveUUID_StableAndWellFormed(t *testing.T) {
	id := "entry:01ARZ"
	a := deriveUUID(id)
	b := deriveUUID(id)
	if a != b {
		t.Errorf("deriveUUID not stable: %q vs %q", a, b)
	}
	if len(a) != 36 {
		t.Errorf("deriveUUID length = %d, want 36", len(a))
	}
	// Version 5 nibble lives at byte index 14 (the first nibble of the
	// third hyphen-delimited group: xxxxxxxx-xxxx-Vxxx-xxxx-xxxxxxxxxxxx).
	if a[14] != '5' {
		t.Errorf("expected version 5 at index 14, got %c (%q)", a[14], a)
	}
	// RFC 4122 variant: byte at index 19 must be one of 8, 9, a, b.
	switch a[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("expected RFC 4122 variant at index 19, got %c (%q)", a[19], a)
	}
}

func TestForget_DropsScratchState(t *testing.T) {
	f := &fakeUpserter{}
	a := newBackendApplierFor(f)
	d := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "body", V: mustRaw(t, "hello")}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	a.Forget("entry:01ARZ")
	// Re-apply a different attribute; the resulting bag should NOT
	// contain the previous body since the scratch state was dropped.
	d2 := datom.Datom{Tx: "T1", Op: datom.OpAdd, E: "entry:01ARZ", A: "kind", V: mustRaw(t, "note")}
	if err := a.Apply(context.Background(), d2); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := f.calls[len(f.calls)-1].properties
	if _, ok := last["body"]; ok {
		t.Errorf("Forget did not drop scratch: body still present in %v", last)
	}
	if last["kind"] != "note" {
		t.Errorf("expected kind=note after re-apply, got %v", last["kind"])
	}
}
