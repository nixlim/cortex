// cmd/cortex/recall_adapters_test.go covers the PPR adapter's
// projection-refresh contract. GDS projections are in-memory snapshots
// of the database at projection time, so a stale projection masks any
// writes that happened after the first call. cortex-6vi reproduced
// the failure in production: observe + recall succeeds once, then a
// subsequent observe + recall fails with PPR_FAILED because the
// newly-written seeds are not in the cached projection.
//
// The test asserts that every Run() invocation issues a drop+project
// pair so the projection is rebuilt each time. A second assertion
// proves the drop call uses failIfMissing=false so the first Run in
// a fresh database does not error.
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/neo4j"
)

// fakeGDSClient is a minimal neo4j.Client stub that records RunGDS
// invocations so the test can assert the exact sequence of GDS
// procedures the PPR runner emits. Every non-RunGDS method panics:
// the PPR adapter should only touch the GDS surface.
type fakeGDSClient struct {
	gdsCalls []struct {
		cypher string
		params map[string]any
	}
	// pprRows is returned verbatim when a pageRank.stream query is seen.
	pprRows []map[string]any
}

func (f *fakeGDSClient) Ping(context.Context) error { panic("unused") }
func (f *fakeGDSClient) ProbeProcedures(context.Context) (neo4j.ProcedureAvailability, error) {
	panic("unused")
}
func (f *fakeGDSClient) WriteEntries(context.Context, string, map[string]any) error {
	panic("unused")
}
func (f *fakeGDSClient) QueryGraph(context.Context, string, map[string]any) ([]map[string]any, error) {
	panic("unused")
}
func (f *fakeGDSClient) Close(context.Context) error { return nil }

func (f *fakeGDSClient) RunGDS(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.gdsCalls = append(f.gdsCalls, struct {
		cypher string
		params map[string]any
	}{cypher: cypher, params: params})
	if strings.Contains(cypher, "gds.pageRank.stream") {
		return f.pprRows, nil
	}
	return nil, nil
}

// TestPPRRunner_RefreshesProjectionOnEveryRun is the cortex-6vi
// regression test. Two back-to-back Run() calls with non-empty seeds
// must each emit a drop+project pair before the pageRank query, so a
// projection built before the second observe cannot leave the second
// recall stranded on stale snapshot data.
func TestPPRRunner_RefreshesProjectionOnEveryRun(t *testing.T) {
	fake := &fakeGDSClient{
		pprRows: []map[string]any{
			{"id": "entry:01HXTESTENTRY", "score": 0.5},
		},
	}
	runner := &neo4jPPRRunner{client: fake, graphName: "cortex.semantic"}
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := runner.Run(ctx, []string{"entry:01HXTESTENTRY"}, 0.85, 20); err != nil {
			t.Fatalf("Run #%d: %v", i+1, err)
		}
	}

	// Expect exactly three GDS calls per Run: drop, project, pageRank.
	if got, want := len(fake.gdsCalls), 6; got != want {
		for i, c := range fake.gdsCalls {
			t.Logf("call %d: %s", i, firstLine(c.cypher))
		}
		t.Fatalf("gds call count: got %d, want %d (drop+project+pageRank x2)", got, want)
	}

	wantSeq := []string{
		"gds.graph.drop",
		"gds.graph.project",
		"gds.pageRank.stream",
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

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "\n"); idx >= 0 {
		return s[:idx]
	}
	return s
}
