package recall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/activation"
)

// --- fakes for mode tests --------------------------------------------

type fakeSimilar struct {
	hits    []Result
	lastK   int
	callErr error
	calls   int
}

func (f *fakeSimilar) Similar(_ context.Context, _ []float32, k int) ([]Result, error) {
	f.calls++
	f.lastK = k
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.hits, nil
}

// panicPPR fails loudly if invoked — used to prove --mode=similar
// does not reach the default PPR path (AC1).
type panicPPR struct{}

func (panicPPR) Run(context.Context, []string, float64, int) (map[string]float64, error) {
	panic("PPR invoked from --mode=similar")
}

type fakeGraph struct {
	traverse TraverseResult
	path     PathResult
	community []string
	err      error
}

func (f *fakeGraph) Traverse(_ context.Context, _ string, _ int) (TraverseResult, error) {
	return f.traverse, f.err
}
func (f *fakeGraph) ShortestPath(_ context.Context, _, _ string) (PathResult, error) {
	return f.path, f.err
}
func (f *fakeGraph) CommunityMembers(_ context.Context, _ string) ([]string, error) {
	return f.community, f.err
}

func newTestModes(sim *fakeSimilar, g *fakeGraph, loader EntryLoader) *Modes {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &Modes{
		Similar:  sim,
		Graph:    g,
		Loader:   loader,
		Embedder: &fakeEmbedder{vec: []float32{1, 0, 0}},
		Context:  fakeContext{},
		Now:      func() time.Time { return now },
	}
}

// TestMode_SimilarBypassesPPR covers AC1: --mode=similar is pure
// Weaviate nearest-neighbor without Neo4j PPR. Evidence: (a) the
// fakeSimilar is invoked with the configured k, (b) no PPR runner
// is even wired in the Modes struct (the type has no PPR field).
func TestMode_SimilarBypassesPPR(t *testing.T) {
	sim := &fakeSimilar{hits: []Result{
		{EntryID: "entry:A", Score: 0.95},
		{EntryID: "entry:B", Score: 0.80},
	}}
	m := newTestModes(sim, nil, nil)

	res, err := m.RecallSimilar(context.Background(), ModeRequest{Query: "q", Limit: 5})
	if err != nil {
		t.Fatalf("RecallSimilar: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("results: got %d want 2", len(res))
	}
	if res[0].EntryID != "entry:A" {
		t.Fatalf("order: got %s want entry:A first", res[0].EntryID)
	}
	if sim.lastK != 5 {
		t.Fatalf("k passthrough: got %d want 5", sim.lastK)
	}
	if sim.calls != 1 {
		t.Fatalf("Similar call count: %d", sim.calls)
	}
}

// TestMode_SimilarEmptyQueryRejected asserts precondition validation.
func TestMode_SimilarEmptyQueryRejected(t *testing.T) {
	m := newTestModes(&fakeSimilar{}, nil, nil)
	_, err := m.RecallSimilar(context.Background(), ModeRequest{Query: ""})
	if err == nil || !strings.Contains(err.Error(), "EMPTY_QUERY") {
		t.Fatalf("want EMPTY_QUERY, got %v", err)
	}
}

// TestMode_TraverseReturnsReachableSubgraph covers AC2: --mode=traverse
// --from=<id> --depth=2 returns entities within 2 hops with typed
// edge labels.
func TestMode_TraverseReturnsReachableSubgraph(t *testing.T) {
	graph := &fakeGraph{traverse: TraverseResult{
		SeedID: "entry:SEED",
		Nodes:  []string{"entry:SEED", "entry:A", "entry:B"},
		Edges: []TraverseEdge{
			{SourceID: "entry:SEED", TargetID: "entry:A", EdgeType: "SIMILAR_TO"},
			{SourceID: "entry:A", TargetID: "entry:B", EdgeType: "CAUSES"},
		},
	}}
	m := newTestModes(nil, graph, nil)
	res, err := m.RecallTraverse(context.Background(), ModeRequest{From: "entry:SEED", Depth: 2})
	if err != nil {
		t.Fatalf("RecallTraverse: %v", err)
	}
	if res.SeedID != "entry:SEED" {
		t.Fatalf("seed: %s", res.SeedID)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("nodes: %d want 3", len(res.Nodes))
	}
	if len(res.Edges) != 2 {
		t.Fatalf("edges: %d want 2", len(res.Edges))
	}
	for _, e := range res.Edges {
		if e.EdgeType == "" {
			t.Errorf("edge missing type label: %+v", e)
		}
	}
}

func TestMode_TraverseRequiresFromAndDepth(t *testing.T) {
	m := newTestModes(nil, &fakeGraph{}, nil)
	_, err := m.RecallTraverse(context.Background(), ModeRequest{Depth: 2})
	if err == nil || !strings.Contains(err.Error(), "MISSING_FROM") {
		t.Fatalf("want MISSING_FROM, got %v", err)
	}
	_, err = m.RecallTraverse(context.Background(), ModeRequest{From: "x", Depth: 0})
	if err == nil || !strings.Contains(err.Error(), "INVALID_DEPTH") {
		t.Fatalf("want INVALID_DEPTH, got %v", err)
	}
}

// TestMode_PathReturnsShortestPath covers the AC3 happy path.
func TestMode_PathReturnsShortestPath(t *testing.T) {
	graph := &fakeGraph{path: PathResult{
		FromID: "entry:A",
		ToID:   "entry:B",
		Nodes:  []string{"entry:A", "entry:M", "entry:B"},
		Edges: []TraverseEdge{
			{SourceID: "entry:A", TargetID: "entry:M", EdgeType: "REFINES"},
			{SourceID: "entry:M", TargetID: "entry:B", EdgeType: "CAUSES"},
		},
	}}
	m := newTestModes(nil, graph, nil)
	res, err := m.RecallPath(context.Background(), ModeRequest{From: "entry:A", To: "entry:B"})
	if err != nil {
		t.Fatalf("RecallPath: %v", err)
	}
	if len(res.Nodes) != 3 {
		t.Fatalf("path length: got %d want 3", len(res.Nodes))
	}
}

// TestMode_PathEmptyWhenNoRoute covers AC3: no path exists → empty
// result (not an error).
func TestMode_PathEmptyWhenNoRoute(t *testing.T) {
	graph := &fakeGraph{path: PathResult{
		FromID: "entry:A",
		ToID:   "entry:Z",
		Nodes:  nil,
	}}
	m := newTestModes(nil, graph, nil)
	res, err := m.RecallPath(context.Background(), ModeRequest{From: "entry:A", To: "entry:Z"})
	if err != nil {
		t.Fatalf("RecallPath: %v", err)
	}
	if len(res.Nodes) != 0 {
		t.Fatalf("expected empty path, got %d nodes", len(res.Nodes))
	}
}

func TestMode_PathMissingEndpoints(t *testing.T) {
	m := newTestModes(nil, &fakeGraph{}, nil)
	_, err := m.RecallPath(context.Background(), ModeRequest{From: "x"})
	if err == nil || !strings.Contains(err.Error(), "MISSING_ENDPOINTS") {
		t.Fatalf("want MISSING_ENDPOINTS, got %v", err)
	}
}

// TestMode_CommunityListsMembers covers the community mode: every
// entry in the named community is returned.
func TestMode_CommunityListsMembers(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	loader := &fakeLoader{entries: map[string]EntryState{
		"entry:A": makeEntry("entry:A", 0.9, now, nil),
		"entry:B": makeEntry("entry:B", 0.9, now, nil),
	}}
	graph := &fakeGraph{community: []string{"entry:A", "entry:B"}}
	m := newTestModes(nil, graph, loader)
	res, err := m.RecallCommunity(context.Background(), ModeRequest{CommunityID: "community-1"})
	if err != nil {
		t.Fatalf("RecallCommunity: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("members: got %d want 2", len(res))
	}
}

// TestMode_SurpriseIncludesBelowThreshold covers AC4: surprise may
// surface items below the default visibility threshold.
func TestMode_SurpriseIncludesBelowThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// Old entry below visibility threshold.
	old := EntryState{
		EntryID: "entry:OLD",
		Body:    "ancient observation",
		Activation: activation.State{
			EncodingAt:     now.Add(-1_000_000 * time.Second),
			BaseActivation: 0.001, // way below 0.05
		},
	}
	// Fresh entry above threshold, low surprise score.
	fresh := EntryState{
		EntryID: "entry:FRESH",
		Body:    "new observation",
		Activation: activation.State{
			EncodingAt:     now.Add(-1 * time.Second),
			BaseActivation: 0.9,
		},
	}
	loader := &fakeLoader{entries: map[string]EntryState{
		"entry:OLD":   old,
		"entry:FRESH": fresh,
	}}
	sim := &fakeSimilar{hits: []Result{
		{EntryID: "entry:OLD"},
		{EntryID: "entry:FRESH"},
	}}
	m := newTestModes(sim, nil, loader)
	res, err := m.RecallSurprise(context.Background(), ModeRequest{Query: "q"})
	if err != nil {
		t.Fatalf("RecallSurprise: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("surprise results: got %d want 2", len(res))
	}
	// The old entry should win — it's the most "surprising" (1 - recency).
	if res[0].EntryID != "entry:OLD" {
		t.Fatalf("surprise ordering: got %s first want entry:OLD", res[0].EntryID)
	}
	// AC4: below-threshold entry surfaced at all — the default-mode
	// filter would have removed it.
	found := false
	for _, r := range res {
		if r.EntryID == "entry:OLD" {
			found = true
		}
	}
	if !found {
		t.Fatalf("below-threshold entry not surfaced by surprise")
	}
}

// TestMode_SurpriseExcludesEvicted confirms the one exclusion that
// surprise still honors: evicted entries.
func TestMode_SurpriseExcludesEvicted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evicted := EntryState{
		EntryID: "entry:EVICT",
		Activation: activation.State{
			EncodingAt: now.Add(-1_000_000 * time.Second),
			Evicted:    true,
		},
	}
	loader := &fakeLoader{entries: map[string]EntryState{"entry:EVICT": evicted}}
	sim := &fakeSimilar{hits: []Result{{EntryID: "entry:EVICT"}}}
	m := newTestModes(sim, nil, loader)
	res, err := m.RecallSurprise(context.Background(), ModeRequest{Query: "q"})
	if err != nil {
		t.Fatalf("RecallSurprise: %v", err)
	}
	for _, r := range res {
		if r.EntryID == "entry:EVICT" {
			t.Fatalf("evicted entry surfaced by surprise")
		}
	}
}

// TestMode_SimilarErrorPropagates proves adapter errors become
// operational envelopes, not panics.
func TestMode_SimilarErrorPropagates(t *testing.T) {
	sim := &fakeSimilar{callErr: errors.New("weaviate: connection refused")}
	m := newTestModes(sim, nil, nil)
	_, err := m.RecallSimilar(context.Background(), ModeRequest{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "SIMILAR_SEARCH_FAILED") {
		t.Fatalf("want SIMILAR_SEARCH_FAILED, got %v", err)
	}
}

// Use of panicPPR is only a sanity guard — Modes doesn't even hold a
// PPR field, so nothing should ever call it. The test is here to
// document that the PPR interface is not reachable from any alternate
// mode.
var _ PPRRunner = panicPPR{}
