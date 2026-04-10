package neo4j

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/datom"
)

// fakeGraphWriter records every WriteEntries call so tests can assert
// on the Cypher and parameters the applier emits without standing up
// a live Neo4j.
type fakeGraphWriter struct {
	calls []writeCall
	err   error
}

type writeCall struct {
	cypher string
	params map[string]any
}

func (f *fakeGraphWriter) WriteEntries(_ context.Context, cypher string, params map[string]any) error {
	f.calls = append(f.calls, writeCall{cypher: cypher, params: params})
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
	a := newBackendApplierFor(&fakeGraphWriter{})
	if a.Name() != "neo4j" {
		t.Fatalf("Name() = %q, want %q", a.Name(), "neo4j")
	}
}

func TestApply_EmptyEntityIsNoOp(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	if err := a.Apply(context.Background(), datom.Datom{Tx: "T1", A: "body", V: mustRaw(t, "x")}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(g.calls) != 0 {
		t.Fatalf("expected 0 writes for empty entity, got %d", len(g.calls))
	}
}

func TestApply_EntryBodyMergesNodeWithProperty(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "entry:01ARZ", A: "body", V: mustRaw(t, "hello world"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(g.calls) != 1 {
		t.Fatalf("expected 1 write, got %d", len(g.calls))
	}
	c := g.calls[0]
	if !strings.Contains(c.cypher, "MERGE (n:Entry {id: $id})") {
		t.Errorf("expected Entry MERGE, got %q", c.cypher)
	}
	if !strings.Contains(c.cypher, "SET n.body = $value") {
		t.Errorf("expected body SET, got %q", c.cypher)
	}
	if c.params["id"] != "entry:01ARZ" {
		t.Errorf("id param = %v", c.params["id"])
	}
	if c.params["value"] != "hello world" {
		t.Errorf("value param = %v", c.params["value"])
	}
	if c.params["tx"] != "T1" {
		t.Errorf("tx param = %v", c.params["tx"])
	}
}

func TestApply_FrameKindMapsToFrameLabel(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "frame:01XYZ", A: "frame_type", V: mustRaw(t, "BugPattern"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "MERGE (n:Frame") {
		t.Errorf("expected Frame label, got %q", g.calls[0].cypher)
	}
}

func TestApply_FacetDotIsRewrittenToUnderscore(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "entry:01ARZ", A: "facet.domain", V: mustRaw(t, "auth"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "SET n.facet_domain = $value") {
		t.Errorf("expected facet_domain property, got %q", g.calls[0].cypher)
	}
}

func TestApply_TrailEdgeMaterializesRelationship(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "entry:01ARZ", A: "trail", V: mustRaw(t, "trail:T9"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(g.calls) != 2 {
		t.Fatalf("expected 2 writes (edge + property), got %d", len(g.calls))
	}
	edgeCypher := g.calls[0].cypher
	if !strings.Contains(edgeCypher, "MERGE (s:Entry {id: $src})") ||
		!strings.Contains(edgeCypher, "MERGE (t:Trail {id: $tgt})") ||
		!strings.Contains(edgeCypher, "[r:IN_TRAIL]") {
		t.Errorf("expected IN_TRAIL edge, got %q", edgeCypher)
	}
	if g.calls[0].params["src"] != "entry:01ARZ" || g.calls[0].params["tgt"] != "trail:T9" {
		t.Errorf("edge params = %v", g.calls[0].params)
	}
}

func TestApply_SubjectEdgeUsesAboutRelationship(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "entry:01ARZ", A: "subject", V: mustRaw(t, "subject:alice"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "[r:ABOUT]") {
		t.Errorf("expected ABOUT relationship, got %q", g.calls[0].cypher)
	}
	if !strings.Contains(g.calls[0].cypher, "MERGE (t:Subject {id: $tgt})") {
		t.Errorf("expected Subject target, got %q", g.calls[0].cypher)
	}
}

func TestApply_RetractExistsSetsRetractedFlag(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T2", Op: datom.OpRetract,
		E: "entry:01ARZ", A: "exists", V: mustRaw(t, false),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "SET n.retracted = true") {
		t.Errorf("expected retracted flag, got %q", g.calls[0].cypher)
	}
}

func TestApply_RetractNonExistsRemovesProperty(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T2", Op: datom.OpRetract,
		E: "entry:01ARZ", A: "body", V: mustRaw(t, ""),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "REMOVE n.body") {
		t.Errorf("expected REMOVE n.body, got %q", g.calls[0].cypher)
	}
}

func TestApply_NumericValuesPreserveType(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "entry:01ARZ", A: "importance", V: mustRaw(t, 0.85),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if v, ok := g.calls[0].params["value"].(float64); !ok || v != 0.85 {
		t.Errorf("expected float64 0.85, got %T %v", g.calls[0].params["value"], g.calls[0].params["value"])
	}

	g.calls = nil
	d.A = "retrieval_count"
	d.V = mustRaw(t, 7)
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if v, ok := g.calls[0].params["value"].(int64); !ok || v != 7 {
		t.Errorf("expected int64 7, got %T %v", g.calls[0].params["value"], g.calls[0].params["value"])
	}
}

func TestApply_UnknownPrefixFallsBackToCortexEntity(t *testing.T) {
	g := &fakeGraphWriter{}
	a := newBackendApplierFor(g)
	d := datom.Datom{
		Tx: "T1", Op: datom.OpAdd,
		E: "weird:thing", A: "body", V: mustRaw(t, "x"),
	}
	if err := a.Apply(context.Background(), d); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(g.calls[0].cypher, "MERGE (n:CortexEntity") {
		t.Errorf("expected CortexEntity fallback, got %q", g.calls[0].cypher)
	}
}
