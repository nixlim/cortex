package community

import (
	"context"
	"math"
	"testing"
)

// enrichFakeNeo4j is a Neo4jClient double for EnrichLevel0. It feeds
// a pre-programmed row set to the level-0 read-back query and captures
// every WriteEntries call so assertions can verify the computed
// avg_cosine / mdl_ratio properties land on the right community.
type enrichFakeNeo4j struct {
	readRows []map[string]any
	writes   []map[string]any
	readErr  error
}

func (f *enrichFakeNeo4j) RunGDS(_ context.Context, _ string, _ map[string]any) ([]map[string]any, error) {
	return f.readRows, f.readErr
}
func (f *enrichFakeNeo4j) WriteEntries(_ context.Context, _ string, params map[string]any) error {
	f.writes = append(f.writes, params)
	return nil
}

type enrichFakeFetcher struct {
	vectors map[string][]float32
}

func (f *enrichFakeFetcher) FetchVectorsByCortexIDs(_ context.Context, _ string, _ []string) (map[string][]float32, error) {
	return f.vectors, nil
}

// TestEnrichLevel0_ComputesCosineAndMDL covers cortex-6ef's main AC:
// after enrichment, level-0 :Community nodes carry non-null
// avg_cosine and mdl_ratio and the cosine is in [0,1].
func TestEnrichLevel0_ComputesCosineAndMDL(t *testing.T) {
	fn := &enrichFakeNeo4j{readRows: []map[string]any{
		{
			"community_id": int64(7),
			"entry_ids":    []string{"entry:A", "entry:B"},
		},
	}}
	d := &Detector{Neo4j: fn}
	fetcher := &enrichFakeFetcher{vectors: map[string][]float32{
		// Two identical vectors → cosine = 1.0.
		"entry:A": {1, 0, 0},
		"entry:B": {1, 0, 0},
	}}

	summary, err := d.EnrichLevel0(context.Background(), fetcher, "Entry")
	if err != nil {
		t.Fatalf("EnrichLevel0: %v", err)
	}
	if summary.CommunitiesEnriched != 1 {
		t.Errorf("communities enriched: got %d want 1", summary.CommunitiesEnriched)
	}
	if summary.VectorsFetched != 2 {
		t.Errorf("vectors fetched: got %d want 2", summary.VectorsFetched)
	}

	if len(fn.writes) != 1 {
		t.Fatalf("writes: got %d want 1", len(fn.writes))
	}
	w := fn.writes[0]
	avg, _ := w["avg_cosine"].(float64)
	mdl, _ := w["mdl_ratio"].(float64)
	if math.Abs(avg-1.0) > 1e-9 {
		t.Errorf("avg_cosine: got %v want 1.0", avg)
	}
	if avg < 0 || avg > 1 {
		t.Errorf("avg_cosine out of [0,1]: %v", avg)
	}
	// mdl = 1 + 1.0 * ln(1+2) = 1 + ln(3) ≈ 2.0986
	wantMDL := 1 + math.Log(3)
	if math.Abs(mdl-wantMDL) > 1e-9 {
		t.Errorf("mdl_ratio: got %v want %v", mdl, wantMDL)
	}
	if mdl <= 0 {
		t.Errorf("mdl_ratio must be positive: got %v", mdl)
	}
}

// TestEnrichLevel0_OrthogonalVectorsGiveZeroCosine confirms the gate:
// two orthogonal unit vectors yield avg_cosine = 0, mdl_ratio = 1.0.
func TestEnrichLevel0_OrthogonalVectorsGiveZeroCosine(t *testing.T) {
	fn := &enrichFakeNeo4j{readRows: []map[string]any{
		{
			"community_id": int64(1),
			"entry_ids":    []string{"a", "b"},
		},
	}}
	d := &Detector{Neo4j: fn}
	fetcher := &enrichFakeFetcher{vectors: map[string][]float32{
		"a": {1, 0, 0},
		"b": {0, 1, 0},
	}}

	_, err := d.EnrichLevel0(context.Background(), fetcher, "Entry")
	if err != nil {
		t.Fatalf("EnrichLevel0: %v", err)
	}
	if len(fn.writes) != 1 {
		t.Fatalf("writes: got %d want 1", len(fn.writes))
	}
	avg, _ := fn.writes[0]["avg_cosine"].(float64)
	mdl, _ := fn.writes[0]["mdl_ratio"].(float64)
	if avg != 0 {
		t.Errorf("avg_cosine orthogonal: got %v want 0", avg)
	}
	if mdl != 1.0 {
		t.Errorf("mdl_ratio orthogonal: got %v want 1.0", mdl)
	}
}

// TestEnrichLevel0_SingletonCommunityProducesNonNullProperties proves
// a community with <2 resolvable vectors still gets non-null
// properties (so reflect's IS NOT NULL predicate still matches).
func TestEnrichLevel0_SingletonCommunityProducesNonNullProperties(t *testing.T) {
	fn := &enrichFakeNeo4j{readRows: []map[string]any{
		{"community_id": int64(1), "entry_ids": []string{"only"}},
	}}
	d := &Detector{Neo4j: fn}
	fetcher := &enrichFakeFetcher{vectors: map[string][]float32{
		"only": {1, 0, 0},
	}}
	_, err := d.EnrichLevel0(context.Background(), fetcher, "Entry")
	if err != nil {
		t.Fatalf("EnrichLevel0: %v", err)
	}
	if len(fn.writes) != 1 {
		t.Fatalf("writes: got %d want 1", len(fn.writes))
	}
	w := fn.writes[0]
	if _, ok := w["avg_cosine"]; !ok {
		t.Error("avg_cosine missing from write")
	}
	if _, ok := w["mdl_ratio"]; !ok {
		t.Error("mdl_ratio missing from write")
	}
	mdl, _ := w["mdl_ratio"].(float64)
	if mdl <= 0 {
		t.Errorf("mdl_ratio must be positive: got %v", mdl)
	}
}

// TestEnrichLevel0_MissingVectorSkipsButStillWrites checks that an
// entry without a vector is skipped from the cosine computation but
// doesn't prevent the enrichment write from happening.
func TestEnrichLevel0_MissingVectorSkipsButStillWrites(t *testing.T) {
	fn := &enrichFakeNeo4j{readRows: []map[string]any{
		{"community_id": int64(1), "entry_ids": []string{"a", "b", "missing"}},
	}}
	d := &Detector{Neo4j: fn}
	fetcher := &enrichFakeFetcher{vectors: map[string][]float32{
		"a": {1, 0, 0},
		"b": {1, 0, 0},
		// "missing" deliberately absent.
	}}
	summary, err := d.EnrichLevel0(context.Background(), fetcher, "Entry")
	if err != nil {
		t.Fatalf("EnrichLevel0: %v", err)
	}
	if summary.VectorsFetched != 2 {
		t.Errorf("vectors fetched: got %d want 2", summary.VectorsFetched)
	}
	if len(fn.writes) != 1 {
		t.Fatalf("writes: got %d want 1", len(fn.writes))
	}
}
