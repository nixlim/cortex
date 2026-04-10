// cmd/cortex/recall_adapters_test.go covers the PPR adapter's
// projection-freshness contract.
//
// GDS projections are in-memory snapshots of the database at
// projection time, so a stale projection masks any writes that
// happened after the first call. cortex-6vi reproduced the failure
// in production: observe + recall succeeds once, then a subsequent
// observe + recall fails with PPR_FAILED because the newly-written
// seeds are not in the cached projection.
//
// The original fix (cortex-6vi) rebuilt the projection on every
// Run. That made recall correct but blew the bench envelope at
// n=200 (cortex-3kz, p95=11s). The current implementation checks
// whether the existing projection already matches the live graph
// and only rebuilds when it does not. Two tests guard both sides
// of the behavior:
//
//   - TestPPRRunner_RefreshesStaleProjection: when the projection
//     is absent (or the staleness check cannot complete), the
//     slow-path drop+project+pageRank sequence runs so cortex-6vi
//     cannot regress.
//
//   - TestPPRRunner_ReusesFreshProjection: when the projection
//     exists and its nodeCount+relationshipCount match the live
//     graph, the fast path skips drop+project so cortex-3kz stays
//     fixed.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/neo4j"
)

// fakeGDSClient is a minimal neo4j.Client stub that records RunGDS
// and QueryGraph invocations so tests can assert the exact sequence
// of GDS procedures the PPR runner emits. Unused methods panic: the
// PPR adapter should only touch the GDS + QueryGraph surfaces.
type fakeGDSClient struct {
	gdsCalls []struct {
		cypher string
		params map[string]any
	}
	queryCalls []struct {
		cypher string
		params map[string]any
	}
	// pprRows is returned when a pageRank.stream query is seen.
	pprRows []map[string]any
	// existsRows is returned when gds.graph.exists is called.
	// nil → len 0 → projectionMatchesLive returns false.
	existsRows []map[string]any
	// listRows is returned when gds.graph.list is called.
	listRows []map[string]any
	// liveRows is returned from QueryGraph for the live-count probe.
	liveRows []map[string]any
}

func (f *fakeGDSClient) Ping(context.Context) error { panic("unused") }
func (f *fakeGDSClient) ProbeProcedures(context.Context) (neo4j.ProcedureAvailability, error) {
	panic("unused")
}
func (f *fakeGDSClient) WriteEntries(context.Context, string, map[string]any) error {
	panic("unused")
}
func (f *fakeGDSClient) QueryGraph(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.queryCalls = append(f.queryCalls, struct {
		cypher string
		params map[string]any
	}{cypher: cypher, params: params})
	return f.liveRows, nil
}
func (f *fakeGDSClient) Close(context.Context) error { return nil }

func (f *fakeGDSClient) RunGDS(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.gdsCalls = append(f.gdsCalls, struct {
		cypher string
		params map[string]any
	}{cypher: cypher, params: params})
	switch {
	case strings.Contains(cypher, "gds.pageRank.stream"):
		return f.pprRows, nil
	case strings.Contains(cypher, "gds.graph.exists"):
		return f.existsRows, nil
	case strings.Contains(cypher, "gds.graph.list"):
		return f.listRows, nil
	}
	return nil, nil
}

// TestPPRRunner_RefreshesStaleProjection is the cortex-6vi regression
// guard. The fake reports that no projection exists, so every Run
// must fall through to the slow path (drop + project + pageRank).
// Two back-to-back runs must each emit the full sequence.
func TestPPRRunner_RefreshesStaleProjection(t *testing.T) {
	fake := &fakeGDSClient{
		pprRows: []map[string]any{
			{"id": "entry:01HXTESTENTRY", "score": 0.5},
		},
		// existsRows nil → projection is absent → slow path.
	}
	runner := &neo4jPPRRunner{client: fake, graphName: "cortex.semantic"}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := runner.Run(ctx, []string{"entry:01HXTESTENTRY"}, 0.85, 20); err != nil {
			t.Fatalf("Run #%d: %v", i+1, err)
		}
	}

	// Each Run: exists-check, drop, project, pageRank → 4 GDS calls × 2.
	if got, want := len(fake.gdsCalls), 8; got != want {
		for i, c := range fake.gdsCalls {
			t.Logf("call %d: %s", i, firstLine(c.cypher))
		}
		t.Fatalf("gds call count: got %d, want %d (exists+drop+project+pageRank x2)", got, want)
	}

	wantSeq := []string{
		"gds.graph.exists",
		"gds.graph.drop",
		"gds.graph.project",
		"gds.pageRank.stream",
		"gds.graph.exists",
		"gds.graph.drop",
		"gds.graph.project",
		"gds.pageRank.stream",
	}
	for i, want := range wantSeq {
		if !strings.Contains(fake.gdsCalls[i].cypher, want) {
			t.Errorf("call[%d]: expected %s, got %s", i, want, firstLine(fake.gdsCalls[i].cypher))
		}
	}

	// Drop must use failIfMissing=false so the very first Run in a
	// fresh database (no projection yet) is not an error.
	for i, call := range fake.gdsCalls {
		if !strings.Contains(call.cypher, "gds.graph.drop") {
			continue
		}
		if !strings.Contains(call.cypher, "false") {
			t.Errorf("call[%d] drop missing failIfMissing=false: %s", i, call.cypher)
		}
	}
}

// TestPPRRunner_ReusesFreshProjection is the cortex-3kz regression
// guard. When the GDS projection exists and its node+relationship
// counts match the live graph, Run must skip drop+project and go
// straight to the pageRank query.
func TestPPRRunner_ReusesFreshProjection(t *testing.T) {
	fake := &fakeGDSClient{
		pprRows: []map[string]any{
			{"id": "entry:01HXTESTENTRY", "score": 0.5},
		},
		existsRows: []map[string]any{{"exists": true}},
		listRows:   []map[string]any{{"nodeCount": int64(42), "relationshipCount": int64(7)}},
		liveRows:   []map[string]any{{"nodes": int64(42), "rels": int64(7)}},
	}
	runner := &neo4jPPRRunner{client: fake, graphName: "cortex.semantic"}
	ctx := context.Background()

	if _, err := runner.Run(ctx, []string{"entry:01HXTESTENTRY"}, 0.85, 20); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Fast path: exists, list, pageRank → 3 GDS calls. No drop or
	// project calls should have run.
	if got, want := len(fake.gdsCalls), 3; got != want {
		for i, c := range fake.gdsCalls {
			t.Logf("call %d: %s", i, firstLine(c.cypher))
		}
		t.Fatalf("gds call count: got %d, want %d (exists+list+pageRank)", got, want)
	}
	for _, c := range fake.gdsCalls {
		if strings.Contains(c.cypher, "gds.graph.drop") || strings.Contains(c.cypher, "gds.graph.project") {
			t.Errorf("fast path must not drop or project; got: %s", firstLine(c.cypher))
		}
	}

	// The live-count probe runs through QueryGraph exactly once per
	// freshness check.
	if got := len(fake.queryCalls); got != 1 {
		t.Errorf("QueryGraph call count: got %d, want 1 (live count probe)", got)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
