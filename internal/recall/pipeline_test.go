package recall

import (
	"context"
	"fmt"
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

// TestRecall_RequestLimitOverridesPipelineDefault covers cortex-voa:
// a positive Request.Limit takes effect over the pipeline default.
func TestRecall_RequestLimitOverridesPipelineDefault(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{}
	ppr := map[string]float64{}
	for i := 0; i < 15; i++ {
		id := "entry:" + string(rune('A'+i))
		entries[id] = makeEntry(id, 0.9, now, []float32{1, 0, 0})
		ppr[id] = float64(i) / 15.0
	}
	p := newTestPipeline(entries, ppr)
	resp, err := p.Recall(context.Background(), Request{Query: "q", Limit: 3})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("request limit not honored: got %d want 3", len(resp.Results))
	}
	// Reinforcement datoms must match the truncated result set, not the
	// pre-truncation candidate pool.
	if len(resp.ReinforcementDatoms) != 3*3 {
		t.Fatalf("reinforcement datoms: got %d want 9", len(resp.ReinforcementDatoms))
	}
}

// TestRecall_RequestLimitAboveCandidatesReturnsAll covers the capped
// case: --limit 50 against 15 candidates returns all 15.
func TestRecall_RequestLimitAboveCandidatesReturnsAll(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{}
	ppr := map[string]float64{}
	for i := 0; i < 15; i++ {
		id := "entry:" + string(rune('A'+i))
		entries[id] = makeEntry(id, 0.9, now, []float32{1, 0, 0})
		ppr[id] = float64(i) / 15.0
	}
	p := newTestPipeline(entries, ppr)
	resp, err := p.Recall(context.Background(), Request{Query: "q", Limit: 50})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 15 {
		t.Fatalf("got %d results, want 15", len(resp.Results))
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

// TestRecall_RelevanceFloorDropsIrrelevantCandidates reproduces
// cortex-7y4: without a relevance floor, a query that shares no
// semantic match still surfaces entries because the composite is
// propped up by w_base*B(e). The floor requires cosine similarity
// >= RelevanceFloor; PPR does not rescue zero-similarity entries
// (see the similarity-only rationale on Pipeline.RelevanceFloor).
func TestRecall_RelevanceFloorDropsIrrelevantCandidates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// Both entries have a full base_activation (fresh writes) and
	// embeddings orthogonal to the fake query vector {1,0,0}, so
	// similarity is 0. PPR is non-trivial to prove the gate does
	// not let PPR alone rescue a zero-similarity entry.
	entries := map[string]EntryState{
		"entry:IRR1": makeEntry("entry:IRR1", 1.0, now, []float32{0, 1, 0}),
		"entry:IRR2": makeEntry("entry:IRR2", 1.0, now, []float32{0, 0, 1}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:IRR1": 0.5,
		"entry:IRR2": 0.3,
	})
	p.RelevanceFloor = 0.10

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("relevance floor not honored: got %d results; first=%+v",
			len(resp.Results), resp.Results[0])
	}
	if len(resp.ReinforcementDatoms) != 0 {
		t.Fatalf("relevance floor did not suppress reinforcement: %d datoms",
			len(resp.ReinforcementDatoms))
	}
}

// TestRecall_RelevanceFloorGatesOnSimilarityOnly confirms the floor
// is a similarity-only gate. PPR is not a rescue signal — an entry
// with strong PPR but zero similarity is dropped, because PPR
// touches graph-connected neighbors regardless of semantic match
// and letting it override the semantic gate is the exact failure
// mode cortex-7y4's verification caught.
func TestRecall_RelevanceFloorGatesOnSimilarityOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		// Zero similarity, strong PPR → should be DROPPED despite
		// the high graph signal.
		"entry:PPR_ONLY": makeEntry("entry:PPR_ONLY", 1.0, now, []float32{0, 1, 0}),
		// Similarity above floor → surfaces regardless of PPR.
		"entry:SIM_OK": makeEntry("entry:SIM_OK", 1.0, now, []float32{1, 0, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:PPR_ONLY": 0.9,
		"entry:SIM_OK":   0.01,
	})
	p.RelevanceFloor = 0.10

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := make(map[string]bool, len(resp.Results))
	for _, r := range resp.Results {
		ids[r.EntryID] = true
	}
	if !ids["entry:SIM_OK"] {
		t.Errorf("sim-above-floor entry should survive: got %v", ids)
	}
	if ids["entry:PPR_ONLY"] {
		t.Errorf("sim-below-floor entry must not be rescued by PPR: got %v", ids)
	}
}

// TestRecall_LayeredGate_RescuesBorderlinePositive covers cortex-y6g:
// a candidate with sim below the strict floor but above the hard
// floor should be KEPT when its PPR is strong enough to clear the
// smooth rescue clause. sim=0.50, ppr=0.40, hard=0.40, strict=0.55,
// alpha=0.15 → rescue budget 0.40 - 0.06 = 0.34; sim=0.50 >= 0.34,
// so KEEP.
func TestRecall_LayeredGate_RescuesBorderlinePositive(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// Build an embedding that yields sim=0.50 against query vec {1,0,0}.
	// cosine({1,0,0}, {0.5, s, 0}) = 0.5 / sqrt(0.25+s^2). Solve for
	// sim=0.50 → s^2 = 0.75 → s = sqrt(0.75).
	s := float32(0.8660254)
	entries := map[string]EntryState{
		"entry:BORDER": makeEntry("entry:BORDER", 1.0, now, []float32{0.5, s, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:BORDER": 0.40,
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:BORDER" {
		t.Fatalf("borderline positive should be rescued by PPR: got %+v", resp.Results)
	}
}

// TestRecall_LayeredGate_DropsBelowHardFloor covers the hard-floor
// clause: sim=0.30 is below hard=0.40 and MUST be dropped regardless
// of PPR strength.
func TestRecall_LayeredGate_DropsBelowHardFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// cosine({1,0,0}, {0.3, s, 0}) = 0.3 / sqrt(0.09+s^2). Solve for
	// sim=0.30 → s^2 = 0.91 → s ≈ 0.9539.
	s := float32(0.9539392)
	entries := map[string]EntryState{
		"entry:LOW": makeEntry("entry:LOW", 1.0, now, []float32{0.3, s, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:LOW": 0.99,
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("sub-hard-floor candidate must not be rescued by PPR: %+v", resp.Results)
	}
}

// TestRecall_LayeredGate_KeepsAboveStrict covers the fast-path strict
// clause: a candidate with sim >= SimFloorStrict survives regardless
// of PPR.
func TestRecall_LayeredGate_KeepsAboveStrict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// cosine({1,0,0}, {0.65, s, 0}) = 0.65 / sqrt(0.4225+s^2). For
	// sim=0.65 → s = 0, vector is {0.65,0,0} which is parallel to
	// {1,0,0}, giving sim=1.0. Instead, use {0.65, sqrt(1-0.4225),0}
	// normalized... simpler: use an embedding that produces sim=0.65
	// directly. {0.65, sqrt(0.5775), 0} has norm 1 and sim 0.65.
	s := float32(0.7599342)
	entries := map[string]EntryState{
		"entry:STRICT": makeEntry("entry:STRICT", 1.0, now, []float32{0.65, s, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:STRICT": 0.0,
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:STRICT" {
		t.Fatalf("above-strict candidate must survive regardless of PPR: got %+v", resp.Results)
	}
}

// TestRecall_LayeredGate_BackCompatAlias covers the legacy
// RelevanceFloor alias: setting only RelevanceFloor must reproduce
// the pre-cortex-9uc single-floor gate exactly — drop iff
// sim < RelevanceFloor, with no Stage 1+2a hard floor, no rescue,
// and no Stage 3 composite floor inherited as side-effects.
func TestRecall_LayeredGate_BackCompatAlias(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	sLow := float32(0.9539392) // sim=0.30
	sHi := float32(0.7141428)  // sim=0.70
	entries := map[string]EntryState{
		"entry:DROP": makeEntry("entry:DROP", 1.0, now, []float32{0.3, sLow, 0}),
		"entry:KEEP": makeEntry("entry:KEEP", 1.0, now, []float32{0.7, sHi, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:DROP": 0.30,
		"entry:KEEP": 0.30,
	})
	p.RelevanceFloor = 0.55

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	ids := make(map[string]bool, len(resp.Results))
	for _, r := range resp.Results {
		ids[r.EntryID] = true
	}
	if !ids["entry:KEEP"] {
		t.Errorf("above-floor candidate dropped under back-compat alias: %v", ids)
	}
	if ids["entry:DROP"] {
		t.Errorf("sub-floor candidate surfaced under back-compat alias: %v", ids)
	}
	// Legacy-only path must NOT inherit the new layered defaults.
	if p.SimFloorStrict != 0.55 {
		t.Errorf("back-compat alias: SimFloorStrict got %v want 0.55", p.SimFloorStrict)
	}
	if p.SimFloorHard != 0.55 {
		t.Errorf("back-compat alias: SimFloorHard got %v want 0.55 (= strict)", p.SimFloorHard)
	}
	if p.RescueAlpha != 0 {
		t.Errorf("back-compat alias: RescueAlpha got %v want 0", p.RescueAlpha)
	}
	if p.CompositeFloor != 0 {
		t.Errorf("back-compat alias: CompositeFloor got %v want 0", p.CompositeFloor)
	}
	if p.PPRBaselineMinN != 0 {
		t.Errorf("back-compat alias: PPRBaselineMinN got %v want 0", p.PPRBaselineMinN)
	}
}

// TestRecall_LegacyAlias_NoSurpriseLayering reproduces MAJ-001 from
// the cortex-9uc grill review: a caller that sets only the legacy
// RelevanceFloor field must NOT silently inherit Stage 1+2a hard
// floor, rescue, or Stage 3 composite floor defaults. A candidate
// that would survive the old single-floor gate must still survive.
func TestRecall_LegacyAlias_NoSurpriseLayering(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// sim=0.15 candidate: above legacy floor 0.10, but WOULD be
	// dropped by the new SimFloorHard=0.40 default and by the new
	// CompositeFloor=0.45 default (composite = 0.7*0.15 + 0.3*0.20
	// = 0.165). Under the fixed legacy-only path, it must survive.
	// cosine({1,0,0}, {0.15, s, 0}) = 0.15 / sqrt(0.0225+s^2) = 0.15
	// → s^2 = 0.9775 → s ≈ 0.9887.
	s := float32(0.9887365)
	entries := map[string]EntryState{
		"entry:BORDER": makeEntry("entry:BORDER", 1.0, now, []float32{0.15, s, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:BORDER": 0.20,
	})
	p.RelevanceFloor = 0.10

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:BORDER" {
		t.Fatalf("legacy-only caller saw surprise layered drop: %+v", resp.Results)
	}
	for stage, n := range resp.Diagnostics.DroppedByStage {
		if n != 0 {
			t.Errorf("legacy-only path fired layered stage %q (%d drops)", stage, n)
		}
	}
}

// TestRecall_LegacyAlias_OptsInWhenGateFieldSet confirms the
// opt-in contract: setting any RelevanceGate field alongside the
// legacy RelevanceFloor triggers the full layered defaults. Here
// SimFloorHard is set explicitly, so a sim=0.15 candidate must be
// dropped at the hard floor.
func TestRecall_LegacyAlias_OptsInWhenGateFieldSet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.9887365) // sim=0.15
	entries := map[string]EntryState{
		"entry:LOW": makeEntry("entry:LOW", 1.0, now, []float32{0.15, s, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:LOW": 0.20,
	})
	p.RelevanceFloor = 0.10
	p.SimFloorHard = 0.40 // opt into layered behavior

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("opt-in layered path failed to drop sub-hard-floor: %+v", resp.Results)
	}
	if resp.Diagnostics.DroppedByStage[StageHardSimFloor] != 1 {
		t.Errorf("expected 1 hard-floor drop, got %v", resp.Diagnostics.DroppedByStage)
	}
}

// --- Stage 3 composite floor (cortex-2sg) ----------------------------

// TestRecall_Stage3_DropsBelowCompositeFloor verifies that candidates
// which survive Stage 1+2a can still be rejected by the composite
// gate gate_sim*sim + gate_ppr*ppr < composite_floor.
func TestRecall_Stage3_DropsBelowCompositeFloor(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// sim=0.55 candidates: 0.835 on y axis → unit vector, cos=0.55.
	sStrict := float32(0.8351647)
	// sim=0.75 candidate: 0.661 on y axis.
	sHigh := float32(0.6614378)
	entries := map[string]EntryState{
		"entry:DROP_A": makeEntry("entry:DROP_A", 1.0, now, []float32{0.55, sStrict, 0}),
		"entry:DROP_B": makeEntry("entry:DROP_B", 1.0, now, []float32{0.55, sStrict, 0}),
		"entry:KEEP":   makeEntry("entry:KEEP", 1.0, now, []float32{0.75, sHigh, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:DROP_A": 0.10, // composite = 0.7*0.55 + 0.3*0.10 = 0.415 < 0.45
		"entry:DROP_B": 0.05, // composite = 0.7*0.55 + 0.3*0.05 = 0.400 < 0.45
		"entry:KEEP":   0.30, // composite = 0.7*0.75 + 0.3*0.30 = 0.615 > 0.45
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:KEEP" {
		t.Fatalf("composite floor should drop DROP_A/DROP_B and keep KEEP: got %+v",
			resp.Results)
	}
}

// TestRecall_Stage3_AdaptiveTruncation_FewerThanLimit verifies that
// the limit becomes an upper bound: when only 2 of 6 candidates clear
// the composite floor, the response carries 2 results, not default_limit.
func TestRecall_Stage3_AdaptiveTruncation_FewerThanLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	sStrict := float32(0.8351647) // sim=0.55
	sHigh := float32(0.6)         // sim=0.80 (0.8^2 + 0.6^2 = 1)
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	// 4 weak candidates: sim=0.55, ppr=0 → composite = 0.385 < 0.45 → DROP.
	for _, id := range []string{"entry:W1", "entry:W2", "entry:W3", "entry:W4"} {
		entries[id] = makeEntry(id, 1.0, now, []float32{0.55, sStrict, 0})
		pprMap[id] = 0
	}
	// 2 strong candidates: sim=0.80, ppr=0.30 → composite = 0.65 → KEEP.
	for _, id := range []string{"entry:S1", "entry:S2"} {
		entries[id] = makeEntry(id, 1.0, now, []float32{0.8, sHigh, 0})
		pprMap[id] = 0.30
	}
	p := newTestPipeline(entries, pprMap)
	p.Limit = 10
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("adaptive truncation: expected 2 results under limit=10, got %d: %+v",
			len(resp.Results), resp.Results)
	}
	for _, r := range resp.Results {
		if r.EntryID != "entry:S1" && r.EntryID != "entry:S2" {
			t.Errorf("unexpected survivor: %s", r.EntryID)
		}
	}
}

// TestRecall_Stage3_StrongPositivesUnchanged verifies that strong
// candidates (all sim=0.80 ppr=0.40 → composite 0.68) flow through
// the composite gate unmolested.
func TestRecall_Stage3_StrongPositivesUnchanged(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	sHigh := float32(0.6) // sim=0.80
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	for _, id := range []string{"entry:A", "entry:B", "entry:C", "entry:D", "entry:E"} {
		entries[id] = makeEntry(id, 1.0, now, []float32{0.8, sHigh, 0})
		pprMap[id] = 0.40
	}
	p := newTestPipeline(entries, pprMap)
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 5 {
		t.Fatalf("strong positives should all survive: got %d results", len(resp.Results))
	}
}

// TestRecall_Stage3_DisabledByDefault verifies that a bare pipeline
// (no gate fields set) leaves CompositeFloor at 0 after fillDefaults,
// so Stage 3 is fully off. Existing cortex-y6g tests that set only
// Stage 1+2a fields continue to work because the composite default
// 0.45 still admits their designed borderline candidates.
func TestRecall_Stage3_DisabledByDefault(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 1.0, now, []float32{1, 0, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{"entry:A": 0.1})

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if p.CompositeFloor != 0 {
		t.Errorf("CompositeFloor must stay 0 when SimFloorStrict is 0; got %v",
			p.CompositeFloor)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("bare pipeline should pass the candidate through: got %+v", resp.Results)
	}
}

// TestRecall_Stage3_AfterRescue verifies the ordering constraint:
// Stage 3 runs after Stage 1+2a, so a borderline candidate rescued by
// PPR at Stage 2a (sim=0.50 ppr=0.40 → passes rescue 0.40-0.06=0.34)
// still gets evaluated by the composite gate
// (composite = 0.35+0.12 = 0.47). KEPT when floor=0.45, DROPPED when
// floor=0.50.
func TestRecall_Stage3_AfterRescue(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.8660254) // sim=0.50
	build := func() *Pipeline {
		entries := map[string]EntryState{
			"entry:R": makeEntry("entry:R", 1.0, now, []float32{0.5, s, 0}),
		}
		p := newTestPipeline(entries, map[string]float64{"entry:R": 0.40})
		p.SimFloorHard = 0.40
		p.SimFloorStrict = 0.55
		p.RescueAlpha = 0.15
		p.GateSimWeight = 0.7
		p.GatePPRWeight = 0.3
		return p
	}

	p := build()
	p.CompositeFloor = 0.45
	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall (floor=0.45): %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:R" {
		t.Fatalf("rescued candidate should clear composite floor 0.45: got %+v",
			resp.Results)
	}

	p2 := build()
	p2.CompositeFloor = 0.50
	resp2, err := p2.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall (floor=0.50): %v", err)
	}
	if len(resp2.Results) != 0 {
		t.Fatalf("rescued candidate should be dropped by composite floor 0.50: got %+v",
			resp2.Results)
	}
}

// --- Stage 2b quantile-baseline rescue (cortex-5mp) -----------------

// TestRecall_Stage2b_QuantileRescue_LargeBimodal verifies that with
// >= PPRBaselineMinN candidates, the rescue clause becomes a strict
// upper-quartile PPR test: only candidates whose PPR exceeds the p75
// of the query's PPR distribution are rescued through the
// [hard, strict) sim band. Bimodal setup: 25 lows at ppr=0.10 and 5
// highs at ppr=0.50. p75 = values[int(0.75*30)] = values[22] = 0.10.
// Lows (0.10 > 0.10 = false) dropped; highs (0.50 > 0.10 = true) kept.
func TestRecall_Stage2b_QuantileRescue_LargeBimodal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.8660254) // sim=0.50 against query {1,0,0}
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("entry:L%02d", i)
		entries[id] = makeEntry(id, 1.0, now, []float32{0.5, s, 0})
		pprMap[id] = 0.10
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("entry:H%d", i)
		entries[id] = makeEntry(id, 1.0, now, []float32{0.5, s, 0})
		pprMap[id] = 0.50
	}
	p := newTestPipeline(entries, pprMap)
	p.Limit = 30
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.PPRBaselineMinN = 25
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 5 {
		t.Fatalf("expected 5 high-PPR rescues, got %d: %+v",
			len(resp.Results), resp.Results)
	}
	for _, r := range resp.Results {
		if !strings.HasPrefix(r.EntryID, "entry:H") {
			t.Errorf("unexpected survivor (low PPR should have been dropped): %s", r.EntryID)
		}
	}
}

// TestRecall_Stage2b_FallbackBelowMinN verifies that when the
// candidate set is below PPRBaselineMinN, the gate falls back to the
// Stage 2a Option-1 formula sim >= SimFloorHard - RescueAlpha*ppr.
// 10 candidates (< 25), sim=0.50 ppr=0.40 → Option-1 rescue budget
// 0.40 - 0.06 = 0.34; sim=0.50 clears it. Composite gate
// 0.7*0.50 + 0.3*0.40 = 0.47 > 0.45 → all 10 survive.
func TestRecall_Stage2b_FallbackBelowMinN(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.8660254)
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("entry:B%02d", i)
		entries[id] = makeEntry(id, 1.0, now, []float32{0.5, s, 0})
		pprMap[id] = 0.40
	}
	p := newTestPipeline(entries, pprMap)
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.PPRBaselineMinN = 25
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 10 {
		t.Fatalf("expected 10 survivors via Option-1 fallback, got %d",
			len(resp.Results))
	}
}

// TestRecall_Stage2b_HardFloorStillWins verifies that the hard floor
// check runs BEFORE the quantile rescue. 30 candidates with sim=0.30
// (below hard=0.40) and ppr=0.50 must all be dropped even though
// their PPR is above any p75 of this distribution.
func TestRecall_Stage2b_HardFloorStillWins(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.9539392) // sim=0.30
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("entry:Z%02d", i)
		entries[id] = makeEntry(id, 1.0, now, []float32{0.3, s, 0})
		pprMap[id] = 0.50
	}
	p := newTestPipeline(entries, pprMap)
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.PPRBaselineMinN = 25

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 0 {
		t.Fatalf("sub-hard-floor candidates must not reach Stage 2b: %+v",
			resp.Results)
	}
}

// TestRecall_Stage2b_DisabledByPPRBaselineMinN_Zero verifies the
// Option-1 fallback is used whenever useQuantile is false. Setting
// PPRBaselineMinN above the candidate count guarantees fallback even
// with a distribution (all ppr=0.40) that the quantile path would
// otherwise drop (p75=0.40, ppr > p75 false → drop). Option-1 rescue
// 0.50 >= 0.40 - 0.06 = 0.34 → all 30 kept.
// (Note: fillDefaults promotes a literal PPRBaselineMinN=0 to 25 when
// the gate is enabled, so we use a large value as the disabling
// sentinel for this test.)
func TestRecall_Stage2b_DisabledByPPRBaselineMinN_Zero(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := float32(0.8660254)
	entries := map[string]EntryState{}
	pprMap := map[string]float64{}
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("entry:F%02d", i)
		entries[id] = makeEntry(id, 1.0, now, []float32{0.5, s, 0})
		pprMap[id] = 0.40
	}
	p := newTestPipeline(entries, pprMap)
	p.Limit = 30
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.PPRBaselineMinN = 100
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 30 {
		t.Fatalf("Option-1 fallback should rescue all 30; got %d",
			len(resp.Results))
	}
}

// TestRecall_Diagnostics_PerStageDropCounts verifies cortex-9ti:
// each of the three gate stages increments its own counter in
// Response.Diagnostics.DroppedByStage. Uses quantile-rescue mode
// (PPRBaselineMinN=1) so the rescue path can actually reject
// candidates — the Option-1 fallback cannot (rescue floor equals
// hard floor when ppr >= 0).
func TestRecall_Diagnostics_PerStageDropCounts(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	// Unit-norm embeddings, query vec is {1,0,0}.
	s30 := float32(0.9539392) // sim=0.30
	s50 := float32(0.8660254) // sim=0.50
	s60 := float32(0.8)       // sim=0.60 (unit vector {0.6, 0.8, 0})
	s80 := float32(0.6)       // sim=0.80 (unit vector {0.8, 0.6, 0})
	entries := map[string]EntryState{
		"entry:HARD": makeEntry("entry:HARD", 1.0, now, []float32{0.30, s30, 0}),
		"entry:R1":   makeEntry("entry:R1", 1.0, now, []float32{0.50, s50, 0}),
		"entry:R2":   makeEntry("entry:R2", 1.0, now, []float32{0.50, s50, 0}),
		"entry:COMP": makeEntry("entry:COMP", 1.0, now, []float32{0.60, s60, 0}),
		"entry:KEEP": makeEntry("entry:KEEP", 1.0, now, []float32{0.80, s80, 0}),
	}
	// PPR distribution → sorted [0.1, 0.1, 0.2, 0.5, 0.9], idx=int(0.75*5)=3,
	// so p75 = 0.5. R1/R2 with ppr <= 0.2 are not strict upper-quartile
	// outliers → rescue fails.
	p := newTestPipeline(entries, map[string]float64{
		"entry:HARD": 0.9,
		"entry:R1":   0.1,
		"entry:R2":   0.2,
		"entry:COMP": 0.05, // composite = 0.7*0.60 + 0.3*0.05 = 0.435 < 0.45
		"entry:KEEP": 0.5, // composite = 0.7*0.80 + 0.3*0.5 = 0.710 > 0.45
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3
	p.PPRBaselineMinN = 1 // enable quantile-rescue mode

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].EntryID != "entry:KEEP" {
		t.Fatalf("expected single survivor entry:KEEP, got %+v", resp.Results)
	}
	got := resp.Diagnostics.DroppedByStage
	want := map[string]int{
		StageHardSimFloor:   1,
		StageRescuePath:     2,
		StageCompositeFloor: 1,
	}
	if len(got) != len(want) {
		t.Fatalf("DroppedByStage len: got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("DroppedByStage[%s]: got %d want %d (full=%v)", k, got[k], v, got)
		}
	}
}

// TestRecall_Diagnostics_NoDropsEmpty verifies that when no candidate
// is dropped by any gate stage, the DroppedByStage map is empty.
func TestRecall_Diagnostics_NoDropsEmpty(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s80 := float32(0.6) // sim=0.80
	entries := map[string]EntryState{
		"entry:A": makeEntry("entry:A", 1.0, now, []float32{0.8, s80, 0}),
		"entry:B": makeEntry("entry:B", 1.0, now, []float32{0.8, s80, 0}),
	}
	p := newTestPipeline(entries, map[string]float64{
		"entry:A": 0.5,
		"entry:B": 0.6,
	})
	p.SimFloorHard = 0.40
	p.SimFloorStrict = 0.55
	p.RescueAlpha = 0.15
	p.CompositeFloor = 0.45
	p.GateSimWeight = 0.7
	p.GatePPRWeight = 0.3

	resp, err := p.Recall(context.Background(), Request{Query: "q"})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 survivors, got %d", len(resp.Results))
	}
	if len(resp.Diagnostics.DroppedByStage) != 0 {
		t.Errorf("DroppedByStage should be empty when nothing dropped: %v",
			resp.Diagnostics.DroppedByStage)
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
