package recall

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/actr"
)

// --- fake dependencies -----------------------------------------------

type fakeConcepts struct{ out []string }

func (f *fakeConcepts) Extract(_ context.Context, _ string) ([]string, error) {
	return f.out, nil
}

type fakeSeeds struct{ out []string }

func (f *fakeSeeds) Resolve(_ context.Context, _ []string, _ int) ([]string, error) {
	return f.out, nil
}

type fakePPR struct{ scores map[string]float64 }

func (f *fakePPR) Run(_ context.Context, _ []string, _ float64, _ int) (map[string]float64, error) {
	return f.scores, nil
}

type fakeLoader struct{ entries map[string]EntryState }

func (f *fakeLoader) Load(_ context.Context, ids []string) (map[string]EntryState, error) {
	out := make(map[string]EntryState, len(ids))
	for _, id := range ids {
		if e, ok := f.entries[id]; ok {
			out[id] = e
		}
	}
	return out, nil
}

type fakeEmbedder struct{ vec []float32 }

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return f.vec, nil
}

type fakeContext struct{}

func (fakeContext) Trail(_ context.Context, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	return "trail-context:" + id, nil
}
func (fakeContext) Community(_ context.Context, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	return "community-context:" + id, nil
}

// --- helpers ---------------------------------------------------------

// makeEntry builds an EntryState with a supplied base_activation
// stored directly on the activation state so the visibility threshold
// checks can be exercised without running a decay window.
func makeEntry(id string, base float64, encoded time.Time, embedding []float32) EntryState {
	st := activation.Seed(encoded)
	st.BaseActivation = base
	return EntryState{
		EntryID:     id,
		Body:        "body of " + id,
		Embedding:   embedding,
		Activation:  st,
		TrailID:     "trail-" + id,
		CommunityID: "community-" + id,
	}
}

// newTestPipeline builds a Pipeline pre-wired with a fixed clock,
// single-concept extractor, single-seed resolver, and deterministic
// PPR scores. Callers customize the loader and PPR map.
func newTestPipeline(entries map[string]EntryState, ppr map[string]float64) *Pipeline {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &Pipeline{
		Concepts:     &fakeConcepts{out: []string{"c"}},
		Seeds:        &fakeSeeds{out: []string{"seed"}},
		PPR:          &fakePPR{scores: ppr},
		Loader:       &fakeLoader{entries: entries},
		Embedder:     &fakeEmbedder{vec: []float32{1, 0, 0}},
		Context:      fakeContext{},
		Now:          func() time.Time { return now },
		Actor:        "test",
		InvocationID: "01HTESTINVOCATION0000000000",
	}
}

// --- tests -----------------------------------------------------------

// TestRecall_HappyPathReturnsAnnotatedResults covers AC1: a populated
// graph yields ≤10 results, each with trail_context, community_context,
// and why_surfaced fields.
func TestRecall_HappyPathReturnsAnnotatedResults(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 0.9, now, []float32{1, 0, 0}),
		"entry:B": makeEntry("entry:B", 0.8, now, []float32{0, 1, 0}),
		"entry:C": makeEntry("entry:C", 0.7, now, []float32{1, 1, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:A": 0.6,
		"entry:B": 0.4,
		"entry:C": 0.2,
	})

	resp, err := p.Recall(context.Background(), Request{Query: "my query"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) > DefaultLimit {
		t.Fatalf("results exceed default limit: %d", len(resp.Results))
	}
	if len(resp.Results) == 0 {
		t.Fatalf("no results")
	}
	for _, r := range resp.Results {
		if r.TrailContext == "" {
			t.Errorf("%s: missing trail context", r.EntryID)
		}
		if r.CommunityContext == "" {
			t.Errorf("%s: missing community context", r.EntryID)
		}
		if len(r.WhySurfaced) != 4 {
			t.Errorf("%s: why_surfaced should have 4 lines, got %d", r.EntryID, len(r.WhySurfaced))
		}
	}
}

// TestRecall_BelowThresholdEntryExcluded covers AC3: results with
// base_activation < 0.05 are absent from default recall output.
func TestRecall_BelowThresholdEntryExcluded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:HI":  makeEntry("entry:HI", 0.9, now, []float32{1, 0, 0}),
		"entry:LOW": makeEntry("entry:LOW", 0.03, now, []float32{1, 0, 0}), // strictly below 0.05
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:HI":  0.5,
		"entry:LOW": 0.9,
	})
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range resp.Results {
		if r.EntryID == "entry:LOW" {
			t.Fatalf("below-threshold entry surfaced: %+v", r)
		}
	}
	// And the visible one must be present.
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:HI" {
		t.Fatalf("expected single entry:HI result, got %+v", resp.Results)
	}
}

// TestRecall_ExactThresholdInclusive covers AC4: a result with
// base_activation == 0.05 appears in default recall (inclusive
// threshold).
func TestRecall_ExactThresholdInclusive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:EDGE": makeEntry("entry:EDGE", 0.05, now, []float32{1, 0, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{"entry:EDGE": 0.1})
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:EDGE" {
		t.Fatalf("inclusive threshold not honored: %+v", resp.Results)
	}
}

// TestRecall_ThreeReinforcementDatomsPerResult covers AC2: every
// returned result produces three reinforcement datoms for
// base_activation, retrieval_count, and last_retrieved_at.
func TestRecall_ThreeReinforcementDatomsPerResult(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 0.9, now, []float32{1, 0, 0}),
		"entry:B": makeEntry("entry:B", 0.8, now, []float32{1, 0, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:A": 0.5,
		"entry:B": 0.3,
	})
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	wantCount := 3 * len(resp.Results)
	if len(resp.ReinforcementDatoms) != wantCount {
		t.Fatalf("reinforcement datoms: got %d want %d", len(resp.ReinforcementDatoms), wantCount)
	}
	// Check that every result has exactly one datom for each of the
	// three LWW attributes.
	byEntry := make(map[string]map[string]int)
	for _, d := range resp.ReinforcementDatoms {
		if byEntry[d.E] == nil {
			byEntry[d.E] = map[string]int{}
		}
		byEntry[d.E][d.A]++
	}
	for _, r := range resp.Results {
		attrs := byEntry[r.EntryID]
		for _, want := range []string{"base_activation", "retrieval_count", "last_retrieved_at"} {
			if attrs[want] != 1 {
				t.Errorf("entry %s attr %s count: got %d want 1", r.EntryID, want, attrs[want])
			}
		}
	}
	// All reinforcement datoms must carry src=recall.
	for _, d := range resp.ReinforcementDatoms {
		if d.Src != "recall" {
			t.Errorf("datom src: got %s want recall", d.Src)
		}
	}
}

// TestRecall_LimitTruncatesTo10 proves the default limit cap: given
// 15 visible candidates, only 10 survive.
func TestRecall_LimitTruncatesTo10(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{}
	ppr := map[string]float64{}
	for i := 0; i < 15; i++ {
		id := "entry:" + string(rune('A'+i))
		entries[id] = makeEntry(id, 0.9, now, []float32{1, 0, 0})
		ppr[id] = float64(i) / 15.0
	}
	p := newTestPipeline(entries, ppr)
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != DefaultLimit {
		t.Fatalf("limit not enforced: got %d want %d", len(resp.Results), DefaultLimit)
	}
}

// TestRecall_OrderedByCompositeScore verifies the composite score
// (w_base*B + w_ppr*PPR + w_sim*sim + w_imp*I) drives the final
// ranking. Entry C has the highest PPR but a lower similarity — we
// verify the expected ordering falls out correctly.
func TestRecall_OrderedByCompositeScore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 0.5, now, []float32{1, 0, 0}),
		"entry:B": makeEntry("entry:B", 0.5, now, []float32{0, 1, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:A": 0.1,
		"entry:B": 0.9,
	})
	// Query embedding aligns perfectly with entry:A; entry:B is
	// orthogonal. Similarity contribution: A=1.0, B=0.0.
	// Composite: A = 0.3*0.5 + 0.3*0.1 + 0.3*1 + 0.1*0 = 0.48
	//           B = 0.3*0.5 + 0.3*0.9 + 0.3*0 + 0.1*0 = 0.42
	// So A should win.
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results: %d want 2", len(resp.Results))
	}
	if resp.Results[0].EntryID != "entry:A" {
		t.Fatalf("ranking: got first=%s want entry:A; scores=%v",
			resp.Results[0].EntryID, []float64{resp.Results[0].Score, resp.Results[1].Score})
	}
}

// TestRecall_EvictedEntryAbsent verifies that evicted entries do not
// appear in default recall even if PPR ranks them highly.
func TestRecall_EvictedEntryAbsent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	evicted := makeEntry("entry:EVICT", 0.9, now, []float32{1, 0, 0})
	evicted.Activation = evicted.Activation.Evict()
	entries := map[string]EntryState{
		"entry:GOOD":  makeEntry("entry:GOOD", 0.9, now, []float32{1, 0, 0}),
		"entry:EVICT": evicted,
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:GOOD":  0.1,
		"entry:EVICT": 0.99,
	})
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	for _, r := range resp.Results {
		if r.EntryID == "entry:EVICT" {
			t.Fatalf("evicted entry surfaced")
		}
	}
}

// TestRecall_EmptyQueryRejected covers the precondition.
func TestRecall_EmptyQueryRejected(t *testing.T) {
	p := newTestPipeline(nil, nil)
	_, err := p.Recall(context.Background(), Request{Query: ""})
	if err == nil || !strings.Contains(err.Error(), "EMPTY_QUERY") {
		t.Fatalf("want EMPTY_QUERY, got %v", err)
	}
}

// TestRecall_WhySurfacedMentionsAllFourTerms confirms the why-surfaced
// trace records every composite term so "explain the ranking"
// introspection is meaningful.
func TestRecall_WhySurfacedMentionsAllFourTerms(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 0.9, now, []float32{1, 0, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{"entry:A": 0.5})
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	trace := strings.Join(resp.Results[0].WhySurfaced, "\n")
	for _, token := range []string{"base=", "ppr=", "sim=", "imp="} {
		if !strings.Contains(trace, token) {
			t.Errorf("why_surfaced missing %q; got:\n%s", token, trace)
		}
	}
}

// TestRecall_DefaultsApplied ensures the zero values get replaced
// with the spec defaults.
func TestRecall_DefaultsApplied(t *testing.T) {
	p := newTestPipeline(
		map[string]EntryState{"entry:A": makeEntry("entry:A", 0.9, time.Unix(1_700_000_000, 0).UTC(), []float32{1, 0, 0})},
		map[string]float64{"entry:A": 0.5},
	)
	_, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if p.Limit != DefaultLimit {
		t.Errorf("Limit default not applied: %d", p.Limit)
	}
	if p.Damping != DefaultPPRDamping {
		t.Errorf("Damping default not applied: %v", p.Damping)
	}
	if p.VisibilityThreshold != activation.VisibilityThreshold {
		t.Errorf("VisibilityThreshold default not applied: %v", p.VisibilityThreshold)
	}
	if p.DecayExponent != activation.DefaultDecayExponent {
		t.Errorf("DecayExponent default not applied: %v", p.DecayExponent)
	}
	if p.Weights != actr.DefaultWeights() {
		t.Errorf("Weights default not applied: %+v", p.Weights)
	}
}
