// cmd/cortex/communities_test.go covers the cortex communities detect
// subcommand's procedural core. The chicken-and-egg bug it fixes was
// that no command in the CLI ever called community.Detector.Detect +
// Persist, so analyze and reflect both queried an empty :Community
// layer and returned zero candidates regardless of how much content
// the log held. These tests pin the bootstrap behavior so a
// regression would surface immediately:
//
//   - TestDetectAndPersistCommunities_HappyPathLeiden: a fake bolt
//     client returns leiden stream rows; we assert that
//     ensureSemanticProjection runs first, leiden is invoked once per
//     resolution level, and the persist write fires for every
//     community produced. The fake records WriteEntries params so we
//     can assert per-level community counts without a live Neo4j.
//
//   - TestDetectAndPersistCommunities_LouvainFallback: leiden errors
//     out (simulating a GDS plugin without the leiden procedure);
//     we assert the louvain fallback is taken and the result reports
//     algorithm=louvain.
//
//   - TestDetectAndPersistCommunities_BothAlgorithmsFail: both
//     algorithms error; we assert that the call surfaces a
//     COMMUNITY_DETECT_FAILED operational error and that no Persist
//     was attempted.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/neo4j"
)

// fakeVectorFetcher is a community.VectorFetcher double. It returns
// the pre-seeded vectors map verbatim — the enrich pass skips ids
// missing from the map, so tests that don't care about cosine/mdl
// accuracy can supply an empty map (enrichment runs but avg_cosine
// stays 0 and mdl_ratio stays 1.0 — both non-null, which is all
// cortex-6ef's AC requires from the default fast path).
type fakeVectorFetcher struct {
	vectors map[string][]float32
}

func (f *fakeVectorFetcher) FetchVectorsByCortexIDs(_ context.Context, _ string, _ []string) (map[string][]float32, error) {
	if f.vectors == nil {
		return map[string][]float32{}, nil
	}
	return f.vectors, nil
}

// fakeCommunityClient is a neo4j.Client double scoped to the surfaces
// the detect command actually touches: RunGDS (for the projection
// fast-path probes and the leiden / louvain stream), WriteEntries
// (for community.Detector.Persist), and QueryGraph (for the live
// node + relationship count probe used by projectionMatchesLive).
// Unused methods panic so a test that accidentally exercises them
// fails fast rather than silently passing.
type fakeCommunityClient struct {
	// existsReturn controls whether projectionMatchesLive's
	// gds.graph.exists probe reports the projection as present.
	// false (default) takes the slow path: drop + project.
	existsReturn bool

	// leidenErr is returned when the leiden stream cypher is seen.
	// Setting it forces the louvain fallback path.
	leidenErr error
	// louvainErr is returned when the louvain stream cypher is seen.
	louvainErr error

	// leidenRows is the stream rows returned for leiden calls. Each
	// row must carry "nodeId" and "communityId" int64 entries.
	leidenRows []map[string]any
	// louvainRows is the equivalent for the louvain fallback path.
	louvainRows []map[string]any

	// gdsCalls records every RunGDS invocation in order so tests can
	// assert the precise sequence (exists → drop → project →
	// leiden.stream).
	gdsCalls []string
	// writeCalls records every WriteEntries invocation. The detect
	// command calls Persist once per community, so len(writeCalls)
	// must equal sum(communities per level) plus the enrich pass.
	writeCalls []map[string]any

	// enrichRows is the payload returned for the level-0 enrich
	// read-back query. Nil means "no level-0 communities have entry
	// members" — enrichment performs no writes in that case.
	enrichRows []map[string]any
}

func (f *fakeCommunityClient) Ping(context.Context) error { panic("unused") }
func (f *fakeCommunityClient) ProbeProcedures(context.Context) (neo4j.ProcedureAvailability, error) {
	panic("unused")
}
func (f *fakeCommunityClient) Close(context.Context) error { return nil }

func (f *fakeCommunityClient) WriteEntries(_ context.Context, _ string, params map[string]any) error {
	f.writeCalls = append(f.writeCalls, params)
	return nil
}

func (f *fakeCommunityClient) QueryGraph(_ context.Context, _ string, _ map[string]any) ([]map[string]any, error) {
	// Live count probe used by projectionMatchesLive. Returning a
	// shape that does not match the projection list call below
	// guarantees the slow path runs, which is what every test in
	// this file wants — we want to assert the full
	// drop+project sequence.
	return []map[string]any{{"nodes": int64(0), "rels": int64(0)}}, nil
}

func (f *fakeCommunityClient) RunGDS(_ context.Context, cypher string, _ map[string]any) ([]map[string]any, error) {
	f.gdsCalls = append(f.gdsCalls, cypher)
	switch {
	case strings.Contains(cypher, "gds.graph.exists"):
		return []map[string]any{{"exists": f.existsReturn}}, nil
	case strings.Contains(cypher, "gds.graph.list"):
		// Returning a count that won't match the live probe forces
		// the slow path. The fast path is exercised in
		// recall_adapters_test.go; the detect tests focus on the
		// slow path because that's the bootstrap scenario.
		return []map[string]any{{"nodeCount": int64(99), "relationshipCount": int64(99)}}, nil
	case strings.Contains(cypher, "gds.graph.drop"),
		strings.Contains(cypher, "gds.graph.project"):
		return nil, nil
	case strings.Contains(cypher, "gds.leiden.stream"):
		return f.leidenRows, f.leidenErr
	case strings.Contains(cypher, "gds.louvain.stream"):
		return f.louvainRows, f.louvainErr
	case strings.Contains(cypher, "MATCH (c:Community {level: 0})<-[:IN_COMMUNITY]"):
		// Enrich pass read-back: return whatever the test seeded, or
		// an empty row set. The happy-path tests don't care about
		// enrichment content beyond "runs without error"; the
		// cosine/MDL computation itself is covered in enrich_test.go.
		return f.enrichRows, nil
	}
	return nil, nil
}

// fixtureCommunityRows builds a stream-row set with `nodes` total
// nodes split evenly across `communities` community ids. Each row is
// the {nodeId, communityId, intermediateCommunityIds} shape that
// gds.leiden.stream / gds.louvain.stream emits and that
// community.Detector.Detect collapses via groupByCommunity.
func fixtureCommunityRows(nodes, communities int) []map[string]any {
	rows := make([]map[string]any, 0, nodes)
	for i := 0; i < nodes; i++ {
		rows = append(rows, map[string]any{
			"nodeId":      int64(i),
			"communityId": int64(i % communities),
		})
	}
	return rows
}

func TestDetectAndPersistCommunities_HappyPathLeiden(t *testing.T) {
	// 12 nodes divided into 4 communities. The same fixture is
	// returned for every resolution level — community.Detector.Detect
	// runs the stream once per resolution and groups by communityId,
	// so we expect 4 communities at every level.
	rows := fixtureCommunityRows(12, 4)
	fake := &fakeCommunityClient{
		leidenRows: rows,
	}

	res, err := detectAndPersistCommunities(context.Background(), fake, &fakeVectorFetcher{}, "Entry", "cortex.semantic", nil)
	if err != nil {
		t.Fatalf("detectAndPersistCommunities: %v", err)
	}

	if res.Algorithm != "leiden" {
		t.Errorf("algorithm: got %q, want leiden", res.Algorithm)
	}
	if res.Levels != 3 {
		t.Errorf("levels: got %d, want 3", res.Levels)
	}
	for i, n := range res.CommunitiesByLvl {
		if n != 4 {
			t.Errorf("level %d communities: got %d, want 4", i, n)
		}
	}
	for i, n := range res.MembersByLvl {
		if n != 12 {
			t.Errorf("level %d members: got %d, want 12", i, n)
		}
	}

	// ensureCommunityProjection always drops and rebuilds (the
	// derived projection's counts never match the live graph, so a
	// fast-path probe would always fall through anyway — see
	// cortex-rjz). The sequence is therefore drop → project → three
	// leiden streams (one per resolution) → the enrich read-back.
	// Total: 6 GDS calls.
	wantSeq := []string{
		"gds.graph.drop",
		"gds.graph.project",
		"gds.leiden.stream",
		"gds.leiden.stream",
		"gds.leiden.stream",
		"MATCH (c:Community {level: 0})",
	}
	if got, want := len(fake.gdsCalls), len(wantSeq); got != want {
		for i, c := range fake.gdsCalls {
			t.Logf("call %d: %s", i, firstLine(c))
		}
		t.Fatalf("gds call count: got %d, want %d", got, want)
	}
	for i, want := range wantSeq {
		if !strings.Contains(fake.gdsCalls[i], want) {
			t.Errorf("call[%d]: expected %s, got %s", i, want, firstLine(fake.gdsCalls[i]))
		}
	}

	// Persist is bulk-batched: one WriteEntries call per chunk per
	// level, preceded by the atomic-replace wipe (cortex-udo). The
	// fixture has 4 communities per level, well under PersistBatchSize,
	// so each level ships in a single call. 1 wipe + 3 levels × 1 chunk
	// = 4 calls. See cortex-xek, cortex-udo.
	if got, want := len(fake.writeCalls), 4; got != want {
		t.Errorf("WriteEntries call count: got %d, want %d", got, want)
	}
	// Spot-check the bulk parameter shape on the first batch write
	// (index 1 — index 0 is the DETACH DELETE wipe). The UNWIND-based
	// Cypher takes a top-level $level plus a $communities list; each
	// entry in that list carries the per-community fields
	// (community_id, member_count, members, summary).
	first := fake.writeCalls[1]
	if _, ok := first["level"]; !ok {
		t.Errorf("persist params missing top-level key %q: %v", "level", first)
	}
	batch, ok := first["communities"].([]map[string]any)
	if !ok {
		t.Fatalf("persist params[%q] shape = %T, want []map[string]any", "communities", first["communities"])
	}
	if len(batch) == 0 {
		t.Fatalf("persist 'communities' batch is empty")
	}
	for _, key := range []string{"level", "community_id", "member_count", "members"} {
		if _, ok := batch[0][key]; !ok {
			t.Errorf("persist communities[0] missing key %q: %v", key, batch[0])
		}
	}
}

func TestDetectAndPersistCommunities_LouvainFallback(t *testing.T) {
	rows := fixtureCommunityRows(8, 2)
	fake := &fakeCommunityClient{
		leidenErr:   errors.New("there is no procedure with the name `gds.leiden.stream`"),
		louvainRows: rows,
	}

	res, err := detectAndPersistCommunities(context.Background(), fake, &fakeVectorFetcher{}, "Entry", "cortex.semantic", nil)
	if err != nil {
		t.Fatalf("detectAndPersistCommunities: %v", err)
	}

	if res.Algorithm != "louvain" {
		t.Errorf("algorithm: got %q, want louvain", res.Algorithm)
	}
	if res.Levels != 3 {
		t.Errorf("levels: got %d, want 3", res.Levels)
	}
	for i, n := range res.CommunitiesByLvl {
		if n != 2 {
			t.Errorf("level %d communities: got %d, want 2", i, n)
		}
	}

	// Leiden is attempted once (errors immediately), then Louvain is
	// attempted ONCE — cortex-i3w fixed Louvain to run a single time
	// and decode intermediateCommunityIds for the level hierarchy,
	// rather than rerunning per resolution. Plus the community
	// projection setup (drop, project — no fast-path probe) and the
	// enrich read-back. Total: 2 setup + 1 leiden + 1 louvain + 1
	// enrich = 5.
	if got, want := len(fake.gdsCalls), 5; got != want {
		for i, c := range fake.gdsCalls {
			t.Logf("call %d: %s", i, firstLine(c))
		}
		t.Fatalf("gds call count: got %d, want %d", got, want)
	}
	if !strings.Contains(fake.gdsCalls[2], "gds.leiden.stream") {
		t.Errorf("call[2]: expected leiden first attempt, got %s", firstLine(fake.gdsCalls[2]))
	}
	if !strings.Contains(fake.gdsCalls[3], "gds.louvain.stream") {
		t.Errorf("call[3]: expected louvain fallback, got %s", firstLine(fake.gdsCalls[3]))
	}

	// Louvain ran once; persist is bulk-batched so each of the three
	// dendrogram levels ships in one WriteEntries call (both levels'
	// community counts are well under PersistBatchSize), preceded by
	// the atomic-replace wipe (cortex-udo). 1 wipe + 3 levels × 1 chunk
	// = 4 calls. See cortex-xek, cortex-udo.
	if got, want := len(fake.writeCalls), 4; got != want {
		t.Errorf("WriteEntries call count: got %d, want %d", got, want)
	}
}

// TestDetectAndPersistCommunities_RespectsResolutionsOverride covers
// cortex-4i2: a non-nil resolutions slice passed through from the
// --resolutions CLI flag must drive the hierarchy depth and the
// per-level gds.leiden.stream calls, not the package default. Four
// resolutions → four leiden calls and four levels in the result.
func TestDetectAndPersistCommunities_RespectsResolutionsOverride(t *testing.T) {
	rows := fixtureCommunityRows(6, 2)
	fake := &fakeCommunityClient{leidenRows: rows}

	res, err := detectAndPersistCommunities(
		context.Background(), fake, &fakeVectorFetcher{},
		"Entry", "cortex.semantic",
		[]float64{4.0, 2.0, 1.0, 0.5},
	)
	if err != nil {
		t.Fatalf("detectAndPersistCommunities: %v", err)
	}
	if res.Levels != 4 {
		t.Errorf("levels: got %d, want 4", res.Levels)
	}
	var leidenCalls int
	for _, c := range fake.gdsCalls {
		if strings.Contains(c, "gds.leiden.stream") {
			leidenCalls++
		}
	}
	if leidenCalls != 4 {
		t.Errorf("leiden stream calls: got %d, want 4 (one per resolution)", leidenCalls)
	}
}

func TestParseResolutionsFlag(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    []float64
		wantErr bool
	}{
		{"empty falls back to default", "", nil, false},
		{"whitespace falls back to default", "   ", nil, false},
		{"single value", "2.5", []float64{2.5}, false},
		{"three values", "3.0,1.5,0.7", []float64{3.0, 1.5, 0.7}, false},
		{"tolerates inner whitespace", " 3.0 , 1.5 , 0.7 ", []float64{3.0, 1.5, 0.7}, false},
		{"rejects non-numeric token", "3.0,oops,0.7", nil, true},
		{"rejects zero", "3.0,0,0.7", nil, true},
		{"rejects negative", "3.0,-1,0.7", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseResolutionsFlag(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseResolutionsFlag(%q) err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseResolutionsFlag(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseResolutionsFlag(%q)[%d] = %v, want %v", tc.input, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDetectAndPersistCommunities_BothAlgorithmsFail(t *testing.T) {
	fake := &fakeCommunityClient{
		leidenErr:  errors.New("leiden boom"),
		louvainErr: errors.New("louvain boom"),
	}

	res, err := detectAndPersistCommunities(context.Background(), fake, &fakeVectorFetcher{}, "Entry", "cortex.semantic", nil)
	if err == nil {
		t.Fatalf("expected error, got result %+v", res)
	}
	if res != nil {
		t.Errorf("result on failure: got %+v, want nil", res)
	}
	// Persist must not run when detection fails — there is no
	// hierarchy to write.
	if got := len(fake.writeCalls); got != 0 {
		t.Errorf("WriteEntries call count on failure: got %d, want 0", got)
	}
}
