package community

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeNeo4j is a minimal Neo4jClient double. It serves a
// pre-programmed per-resolution row set so tests can assert that
// Detect groups rows correctly, picks the right algorithm, and
// surfaces RunGDS errors without a live Bolt connection.
type fakeNeo4j struct {
	// rowsByResolution[res] → rows for the RunGDS call whose
	// params["resolution"] == res. A single fixed list under -1
	// is used by the Louvain path where resolution is not in
	// the GDS params.
	rowsByResolution map[float64][]map[string]any
	runErr           error
	lastCypher       string
	runCalls         int

	written       []map[string]any
	writtenCypher []string
	writeErr      error
}

func (f *fakeNeo4j) RunGDS(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.runCalls++
	f.lastCypher = cypher
	if f.runErr != nil {
		return nil, f.runErr
	}
	res, _ := params["resolution"].(float64)
	rows, ok := f.rowsByResolution[res]
	if !ok {
		// Fallback for Louvain / tests that don't key by resolution.
		rows = f.rowsByResolution[-1]
	}
	return rows, nil
}

func (f *fakeNeo4j) WriteEntries(_ context.Context, cypher string, params map[string]any) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, params)
	f.writtenCypher = append(f.writtenCypher, cypher)
	return nil
}

// persistBatchCalls returns the Persist WriteEntries calls that
// are community-batch writes, skipping the leading wipe call
// introduced by cortex-udo. Tests that assert on bulk batching
// want the batch shape, not the wipe prelude.
func (f *fakeNeo4j) persistBatchCalls() []map[string]any {
	batches := make([]map[string]any, 0, len(f.written))
	for _, p := range f.written {
		if _, ok := p["communities"]; ok {
			batches = append(batches, p)
		}
	}
	return batches
}

func stubLeidenQuery(graphName string) string {
	return "FAKE_LEIDEN " + graphName
}

func stubLouvainQuery(graphName string) string {
	return "FAKE_LOUVAIN " + graphName
}

func newFixtureDetector(fn *fakeNeo4j) *Detector {
	return &Detector{
		Neo4j:        fn,
		LeidenQuery:  stubLeidenQuery,
		LouvainQuery: stubLouvainQuery,
		TopNodeCount: 2,
	}
}

func threeLevelConfig() Config {
	return Config{
		GraphName:     "cortex.semantic",
		Resolutions:   []float64{1.0, 0.5, 0.1},
		Levels:        3,
		MaxIterations: 10,
		Tolerance:     0.0001,
	}
}

// TestDetect_EnforcesResolutionsEqualLevels covers the constraint
// "len(resolutions) must equal levels; enforced at startup". A
// mismatch must fail fast with ErrResolutionLevelsMismatch rather
// than silently producing a degraded hierarchy.
func TestDetect_EnforcesResolutionsEqualLevels(t *testing.T) {
	d := newFixtureDetector(&fakeNeo4j{})
	cfg := threeLevelConfig()
	cfg.Levels = 4 // deliberately wrong

	_, err := d.Detect(context.Background(), AlgorithmLeiden, cfg)
	if err == nil {
		t.Fatal("expected ErrResolutionLevelsMismatch, got nil")
	}
	if !errors.Is(err, ErrResolutionLevelsMismatch) {
		t.Errorf("err = %v, want wrapping ErrResolutionLevelsMismatch", err)
	}
}

func TestDetect_EmptyGraphName(t *testing.T) {
	d := newFixtureDetector(&fakeNeo4j{})
	cfg := threeLevelConfig()
	cfg.GraphName = ""
	_, err := d.Detect(context.Background(), AlgorithmLeiden, cfg)
	if !errors.Is(err, ErrEmptyGraphName) {
		t.Errorf("err = %v, want ErrEmptyGraphName", err)
	}
}

// TestDetect_ProducesThreeLevelsFrom100NodeFixture covers the
// acceptance criterion "Running community detection on a 100-node
// fixture graph produces 3 levels". We fake out the GDS stream
// with 100 nodes split into coarser and coarser communities at
// each resolution.
func TestDetect_ProducesThreeLevelsFrom100NodeFixture(t *testing.T) {
	// Resolution 1.0: 10 communities of 10 nodes each (fine).
	// Resolution 0.5: 5 communities of 20 nodes each (medium).
	// Resolution 0.1: 2 communities of 50 nodes each (coarse).
	rows10 := buildFixtureRows(100, 10)
	rows5 := buildFixtureRows(100, 5)
	rows2 := buildFixtureRows(100, 2)

	fn := &fakeNeo4j{
		rowsByResolution: map[float64][]map[string]any{
			1.0: rows10,
			0.5: rows5,
			0.1: rows2,
		},
	}
	d := newFixtureDetector(fn)
	hierarchy, err := d.Detect(context.Background(), AlgorithmLeiden, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(hierarchy) != 3 {
		t.Fatalf("hierarchy has %d levels, want 3", len(hierarchy))
	}
	if got := len(hierarchy[0]); got != 10 {
		t.Errorf("level 0 has %d communities, want 10", got)
	}
	if got := len(hierarchy[1]); got != 5 {
		t.Errorf("level 1 has %d communities, want 5", got)
	}
	if got := len(hierarchy[2]); got != 2 {
		t.Errorf("level 2 has %d communities, want 2", got)
	}
	// Every community must be tagged with its own level index.
	for level := range hierarchy {
		for _, c := range hierarchy[level] {
			if c.Level != level {
				t.Errorf("community level=%d found in slot %d", c.Level, level)
			}
			// TopNodeCount was 2, so each community's TopNodes
			// must be capped at 2 even though members are longer.
			if len(c.TopNodes) > 2 {
				t.Errorf("TopNodes length %d exceeds cap 2", len(c.TopNodes))
			}
		}
	}
}

// TestDetect_LeidenPathBuildsLeidenQuery and its Louvain sibling
// cover the acceptance criterion "When gds.leiden.stream is
// unavailable, the system falls back to gds.louvain.stream". We
// can't log a WARN from inside this package without dragging a
// logger in, so the caller is responsible for emitting the WARN
// after noticing ProcedureAvailability.LeidenUnavailable. What we
// verify here is that the two algorithms route to the correct
// query builder — the rest of the fallback decision lives in the
// call site that combines neo4j.ProbeProcedures with this package.
func TestDetect_LeidenPathBuildsLeidenQuery(t *testing.T) {
	fn := &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: nil}}
	d := newFixtureDetector(fn)
	_, err := d.Detect(context.Background(), AlgorithmLeiden, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !strings.HasPrefix(fn.lastCypher, "FAKE_LEIDEN ") {
		t.Errorf("Detect used cypher %q, want FAKE_LEIDEN prefix", fn.lastCypher)
	}
}

func TestDetect_LouvainFallbackPath(t *testing.T) {
	// Louvain is invoked ONCE (not per level) because GDS Louvain does
	// not honor a resolution parameter — the hierarchy is instead
	// decoded from intermediateCommunityIds. See cortex-i3w.
	fn := &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: {
		{"nodeId": int64(1), "communityId": int64(100), "intermediateCommunityIds": []int64{10, 50, 100}},
		{"nodeId": int64(2), "communityId": int64(100), "intermediateCommunityIds": []int64{11, 50, 100}},
		{"nodeId": int64(3), "communityId": int64(200), "intermediateCommunityIds": []int64{20, 60, 200}},
	}}}
	d := newFixtureDetector(fn)
	_, err := d.Detect(context.Background(), AlgorithmLouvain, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !strings.HasPrefix(fn.lastCypher, "FAKE_LOUVAIN ") {
		t.Errorf("Detect used cypher %q, want FAKE_LOUVAIN prefix", fn.lastCypher)
	}
	if fn.runCalls != 1 {
		t.Errorf("RunGDS called %d times, want 1 (single Louvain run)", fn.runCalls)
	}
}

// TestDetect_LouvainHierarchyMonotonic covers cortex-i3w's core
// acceptance: the multi-level Louvain hierarchy must be monotonic
// (coarser levels have fewer-or-equal communities). We stage a
// three-node, three-level dendrogram where the three nodes start in
// three distinct communities, collapse to two, then to one.
func TestDetect_LouvainHierarchyMonotonic(t *testing.T) {
	fn := &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: {
		{"nodeId": int64(1), "communityId": int64(900), "intermediateCommunityIds": []int64{10, 50, 900}},
		{"nodeId": int64(2), "communityId": int64(900), "intermediateCommunityIds": []int64{11, 50, 900}},
		{"nodeId": int64(3), "communityId": int64(900), "intermediateCommunityIds": []int64{12, 51, 900}},
	}}}
	d := newFixtureDetector(fn)
	h, err := d.Detect(context.Background(), AlgorithmLouvain, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(h) != 3 {
		t.Fatalf("levels: got %d want 3", len(h))
	}
	if len(h[0]) != 3 || len(h[1]) != 2 || len(h[2]) != 1 {
		t.Fatalf("community counts not monotonic: l0=%d l1=%d l2=%d",
			len(h[0]), len(h[1]), len(h[2]))
	}
	for i := 1; i < len(h); i++ {
		if len(h[i]) > len(h[i-1]) {
			t.Errorf("level %d (%d) has more communities than level %d (%d)",
				i, len(h[i]), i-1, len(h[i-1]))
		}
	}
}

// TestDetect_LouvainFallsBackToCommunityID verifies the robustness
// path: when intermediateCommunityIds is absent, every level uses
// the final communityId (single-level fallback).
func TestDetect_LouvainFallsBackToCommunityID(t *testing.T) {
	fn := &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: {
		{"nodeId": int64(1), "communityId": int64(7)},
		{"nodeId": int64(2), "communityId": int64(7)},
		{"nodeId": int64(3), "communityId": int64(8)},
	}}}
	d := newFixtureDetector(fn)
	h, err := d.Detect(context.Background(), AlgorithmLouvain, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	for level, comms := range h {
		if len(comms) != 2 {
			t.Errorf("level %d: got %d communities want 2", level, len(comms))
		}
	}
}

// TestDetect_LouvainShorterDendrogramTailCollapses verifies that
// when Louvain returns fewer dendrogram levels than Cortex asks for,
// the tail levels reuse the coarsest entry rather than going
// out-of-bounds (monotonicity preserved via ties).
func TestDetect_LouvainShorterDendrogramTailCollapses(t *testing.T) {
	fn := &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: {
		{"nodeId": int64(1), "communityId": int64(2), "intermediateCommunityIds": []int64{1, 2}},
		{"nodeId": int64(2), "communityId": int64(2), "intermediateCommunityIds": []int64{2, 2}},
	}}}
	d := newFixtureDetector(fn)
	h, err := d.Detect(context.Background(), AlgorithmLouvain, threeLevelConfig())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// Level 0 uses idx 0 (two distinct ids → 2 communities).
	// Level 1 uses idx 1 (both ids = 2 → 1 community).
	// Level 2 clamps to idx 1 → same as level 1.
	if len(h[0]) != 2 || len(h[1]) != 1 || len(h[2]) != 1 {
		t.Fatalf("tail collapse: l0=%d l1=%d l2=%d",
			len(h[0]), len(h[1]), len(h[2]))
	}
}

func TestDetect_SurfacesRunGDSError(t *testing.T) {
	fn := &fakeNeo4j{runErr: errors.New("gds blew up")}
	d := newFixtureDetector(fn)
	_, err := d.Detect(context.Background(), AlgorithmLeiden, threeLevelConfig())
	if err == nil || !strings.Contains(err.Error(), "gds blew up") {
		t.Errorf("err = %v, want wrap of gds blew up", err)
	}
}

func TestDetect_MissingQueryBuilder(t *testing.T) {
	d := &Detector{
		Neo4j:       &fakeNeo4j{rowsByResolution: map[float64][]map[string]any{-1: nil}},
		LeidenQuery: nil, // forgotten
	}
	_, err := d.Detect(context.Background(), AlgorithmLeiden, threeLevelConfig())
	if err == nil {
		t.Fatal("expected error for missing query builder")
	}
}

// TestPersist_BulkBatchesCommunities covers cortex-xek: Persist must
// ship communities to Neo4j in bulk so the wall-clock cost of a
// detect run is O(levels × chunks), not O(total communities). The
// test fixture has 1 community at level 0 and 2 at level 1; a
// correctly bulk-batched Persist sends one WriteEntries call per
// level (2 total) with the communities packed into a $communities
// list parameter. The pre-fix implementation sent one call per
// community (3 total).
func TestPersist_BulkBatchesCommunities(t *testing.T) {
	fn := &fakeNeo4j{}
	d := newFixtureDetector(fn)
	hierarchy := [][]Community{
		{{ID: 10, Level: 0, Members: []int64{1, 2, 3}}},
		{{ID: 20, Level: 1, Members: []int64{1, 2, 3, 4}}, {ID: 21, Level: 1, Members: []int64{5}}},
	}
	if err := d.Persist(context.Background(), hierarchy); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	batches := fn.persistBatchCalls()
	if len(batches) != 2 {
		t.Fatalf("wrote %d bulk calls, want 2 (one per level)", len(batches))
	}
	// Level 0: one community in the batch.
	level0 := batches[0]
	if got := level0["level"]; got != 0 {
		t.Errorf("level-0 batch level param = %v, want 0", got)
	}
	batch0, ok := level0["communities"].([]map[string]any)
	if !ok {
		t.Fatalf("level-0 batch 'communities' param shape = %T, want []map[string]any", level0["communities"])
	}
	if len(batch0) != 1 {
		t.Fatalf("level-0 batch size = %d, want 1", len(batch0))
	}
	if batch0[0]["community_id"] != int64(10) || batch0[0]["member_count"] != 3 {
		t.Errorf("level-0 batch entry = %+v", batch0[0])
	}
	// Level 1: two communities in the batch.
	level1 := batches[1]
	batch1, _ := level1["communities"].([]map[string]any)
	if len(batch1) != 2{
		t.Fatalf("level-1 batch size = %d, want 2", len(batch1))
	}
}

// TestPersist_ChunksAtBatchSize reproduces cortex-xek on a large
// synthetic hierarchy: 2000 level-0 communities must be persisted in
// ⌈2000 / PersistBatchSize⌉ calls, not 2000. This is the scale case
// that motivates the fix — the Apr 15 cortex + myagentsgigs graph hit
// ~40k communities at level 0 and Persist was taking ~12 minutes at
// one autocommit round trip each.
func TestPersist_ChunksAtBatchSize(t *testing.T) {
	fn := &fakeNeo4j{}
	d := newFixtureDetector(fn)
	const N = 2000
	level0 := make([]Community, N)
	for i := 0; i < N; i++ {
		level0[i] = Community{ID: int64(i), Level: 0, Members: []int64{int64(i)}}
	}
	if err := d.Persist(context.Background(), [][]Community{level0}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	expectedCalls := (N + PersistBatchSize - 1) / PersistBatchSize
	batches := fn.persistBatchCalls()
	if len(batches) != expectedCalls {
		t.Fatalf("wrote %d calls for %d communities at batch size %d; want %d",
			len(batches), N, PersistBatchSize, expectedCalls)
	}
	// Sanity: every batch is ≤ PersistBatchSize.
	var seen int
	for i, w := range batches {
		batch, ok := w["communities"].([]map[string]any)
		if !ok {
			t.Fatalf("call %d: 'communities' param shape = %T, want []map[string]any", i, w["communities"])
		}
		if len(batch) > PersistBatchSize {
			t.Errorf("call %d: batch size %d > PersistBatchSize %d", i, len(batch), PersistBatchSize)
		}
		seen += len(batch)
	}
	if seen != N {
		t.Errorf("total communities across batches = %d, want %d", seen, N)
	}
}

// TestPersist_WipesStaleCommunitiesBeforeBatches covers cortex-udo:
// Persist must DETACH DELETE every :Community (and the
// :IN_COMMUNITY edges attached to them) before writing the new
// hierarchy, so each detect cycle is an atomic replace rather than
// a MERGE-accumulate. Without the wipe, prior-run communities whose
// ids no longer appear in the current detection linger forever as
// orphans (observed 2026-04-15 on cortex + myagentsgigs: 20,510
// zero-entry :Community nodes after the wildcard-projection fix).
func TestPersist_WipesStaleCommunitiesBeforeBatches(t *testing.T) {
	fn := &fakeNeo4j{}
	d := newFixtureDetector(fn)
	hierarchy := [][]Community{
		{{ID: 1, Level: 0, Members: []int64{10, 11}}},
	}
	if err := d.Persist(context.Background(), hierarchy); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if len(fn.writtenCypher) < 2 {
		t.Fatalf("wrote %d calls; want wipe + at least one batch", len(fn.writtenCypher))
	}
	if !strings.Contains(fn.writtenCypher[0], "DETACH DELETE") ||
		!strings.Contains(fn.writtenCypher[0], ":Community") {
		t.Errorf("first call = %q; want a :Community DETACH DELETE", fn.writtenCypher[0])
	}
	// Wipe must precede the first batch write.
	if _, ok := fn.written[0]["communities"]; ok {
		t.Error("first Persist call was a batch write, not the wipe")
	}
}

// TestPersist_SkipsWipeWhenHierarchyEmpty guards the degenerate
// case: a detect run that produced nothing (tiny graph, partial GDS
// failure) must not clobber the last known good state. Persist with
// an empty hierarchy must be a no-op — no wipe, no batch writes.
func TestPersist_SkipsWipeWhenHierarchyEmpty(t *testing.T) {
	fn := &fakeNeo4j{}
	d := newFixtureDetector(fn)
	if err := d.Persist(context.Background(), [][]Community{}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if len(fn.written) != 0 {
		t.Fatalf("empty hierarchy produced %d writes; want 0", len(fn.written))
	}
	// Also cover the "levels present but all empty" shape, which is
	// what a caller that pre-allocated hierarchy[] but never filled
	// any level would pass in.
	if err := d.Persist(context.Background(), [][]Community{{}, {}}); err != nil {
		t.Fatalf("Persist empty levels: %v", err)
	}
	if len(fn.written) != 0 {
		t.Fatalf("hierarchy of empty levels produced %d writes; want 0", len(fn.written))
	}
}

func TestPersist_NoClient(t *testing.T) {
	d := &Detector{}
	if err := d.Persist(context.Background(), nil); err == nil {
		t.Fatal("expected error with no neo4j client")
	}
}

// --- Refresh tests ---

type stubSummarizer struct {
	calls int
	err   error
}

func (s *stubSummarizer) Summarize(_ context.Context, c Community) (string, error) {
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	return fmt.Sprintf("summary-L%d-C%d-N%d", c.Level, c.ID, len(c.Members)), nil
}

// TestRefresh_OnlyChangedCommunities covers the acceptance criterion
// "Refresh after reflection regenerates summaries only for
// communities whose membership changed".
func TestRefresh_OnlyChangedCommunities(t *testing.T) {
	prior := [][]Community{
		{
			{ID: 1, Level: 0, Members: []int64{1, 2, 3}, Summary: "cached-A"},
			{ID: 2, Level: 0, Members: []int64{4, 5, 6}, Summary: "cached-B"},
		},
	}
	next := [][]Community{
		{
			{ID: 1, Level: 0, Members: []int64{3, 2, 1}}, // same set, different order
			{ID: 2, Level: 0, Members: []int64{4, 5, 99}}, // one member swapped
		},
	}
	s := &stubSummarizer{}
	r := &Refresher{Summarizer: s}

	out, regen, err := r.Refresh(context.Background(), prior, next)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if s.calls != 1 {
		t.Errorf("Summarizer called %d times, want 1 (only community #2 changed)", s.calls)
	}
	if len(regen) != 1 || regen[0] != (Key{Level: 0, ID: 2}) {
		t.Errorf("regenerated = %+v, want [{0 2}]", regen)
	}
	// Unchanged community carries forward the cached summary verbatim.
	if out[0][0].Summary != "cached-A" {
		t.Errorf("unchanged community summary = %q, want cached-A", out[0][0].Summary)
	}
	// Changed community gets a fresh summary.
	if !strings.HasPrefix(out[0][1].Summary, "summary-L0-C2") {
		t.Errorf("changed community summary = %q", out[0][1].Summary)
	}
}

// TestRefresh_NewCommunityAlwaysRegenerated — a community absent from
// the prior hierarchy is treated as changed (membership went from
// empty to non-empty).
func TestRefresh_NewCommunityAlwaysRegenerated(t *testing.T) {
	prior := [][]Community{{}}
	next := [][]Community{
		{{ID: 7, Level: 0, Members: []int64{1, 2}}},
	}
	s := &stubSummarizer{}
	r := &Refresher{Summarizer: s}
	_, regen, err := r.Refresh(context.Background(), prior, next)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if s.calls != 1 || len(regen) != 1 {
		t.Errorf("calls=%d regen=%v, want exactly one regeneration", s.calls, regen)
	}
}

func TestRefresh_SummarizerError(t *testing.T) {
	prior := [][]Community{{}}
	next := [][]Community{{{ID: 1, Level: 0, Members: []int64{1}}}}
	r := &Refresher{Summarizer: &stubSummarizer{err: errors.New("boom")}}
	_, _, err := r.Refresh(context.Background(), prior, next)
	if err == nil {
		t.Fatal("expected summarizer error to propagate")
	}
}

func TestRefresh_NoSummarizer(t *testing.T) {
	r := &Refresher{}
	_, _, err := r.Refresh(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error with no summarizer")
	}
}

func TestSameMembership(t *testing.T) {
	cases := []struct {
		name string
		a, b []int64
		want bool
	}{
		{"equal same order", []int64{1, 2, 3}, []int64{1, 2, 3}, true},
		{"equal different order", []int64{1, 2, 3}, []int64{3, 1, 2}, true},
		{"different length", []int64{1, 2}, []int64{1, 2, 3}, false},
		{"same length different ids", []int64{1, 2, 3}, []int64{1, 2, 4}, false},
		{"both empty", nil, nil, true},
	}
	for _, c := range cases {
		if got := sameMembership(c.a, c.b); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// buildFixtureRows synthesises nodeCount rows spread evenly across
// groupCount communities. Node i is assigned to community (i % groupCount).
func buildFixtureRows(nodeCount, groupCount int) []map[string]any {
	rows := make([]map[string]any, nodeCount)
	for i := 0; i < nodeCount; i++ {
		rows[i] = map[string]any{
			"nodeId":      int64(i),
			"communityId": int64(i % groupCount),
		}
	}
	return rows
}
