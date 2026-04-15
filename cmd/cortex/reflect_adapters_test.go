// cmd/cortex/reflect_adapters_test.go guards the Cypher that
// neo4jReflectClusterSource.Candidates ships to Neo4j. The Cypher
// must read Entry properties using the same names the Neo4j
// applier writes — otherwise the cluster source silently drops
// every candidate through coalesce + predicate fallthrough.
//
// cortex-h9y reproduced the production failure: reflect returned
// zero candidates against a freshly-enriched 20k-community graph
// because the Cypher was reading e.tx (never populated) instead
// of e.last_tx (the canonical applier property). The tests below
// lock that property name in place at the query-string level so
// a future drift between applier and reader fails loudly at
// `go test` time rather than quietly at runtime.

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/neo4j"
)

// captureQueryClient is a minimal neo4j.Client stub that records
// every QueryGraph call's Cypher string and returns a pre-programmed
// result. It exposes only the two methods neo4jReflectClusterSource
// touches; everything else panics so a regression that widens the
// adapter's surface is caught immediately.
type captureQueryClient struct {
	cypher string
	params map[string]any
	rows   []map[string]any
}

func (c *captureQueryClient) QueryGraph(_ context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	c.cypher = cypher
	c.params = params
	return c.rows, nil
}

// The rest of neo4j.Client is unused by Candidates. Panicking on
// access keeps the test honest.
func (c *captureQueryClient) Ping(context.Context) error { panic("unused") }
func (c *captureQueryClient) RunGDS(context.Context, string, map[string]any) ([]map[string]any, error) {
	panic("unused")
}
func (c *captureQueryClient) WriteEntries(context.Context, string, map[string]any) error {
	panic("unused")
}
func (c *captureQueryClient) ProbeProcedures(context.Context) (neo4j.ProcedureAvailability, error) {
	panic("unused")
}
func (c *captureQueryClient) Close(context.Context) error { return nil }

// TestReflectClusterSource_ComputesDistinctTimestamps is the
// cortex-5aq regression guard. Pre-fix the cluster source read
// DistinctTimestamps from a non-existent Community property
// (c.distinct_timestamps), which always returned 0 and pushed
// every candidate into the INSUFFICIENT_TIMESTAMPS reject bucket.
// Post-fix the value must be computed in Go from the already-
// parsed exemplar timestamps, grouping by UTC day.
func TestReflectClusterSource_ComputesDistinctTimestamps(t *testing.T) {
	// Three exemplars spanning three distinct UTC days. The fake
	// deliberately leaves "distinct_ts" unset on the row so the
	// Go-side computation is the only thing that can produce a
	// non-zero count.
	fake := &captureQueryClient{
		rows: []map[string]any{
			{
				"id": "C42",
				"members": []any{
					map[string]any{
						"entry_id": "entry:A",
						"ts":       "2026-04-13T10:00:00Z",
						"tx":       "tx-a",
					},
					map[string]any{
						"entry_id": "entry:B",
						"ts":       "2026-04-14T11:00:00Z",
						"tx":       "tx-b",
					},
					map[string]any{
						"entry_id": "entry:C",
						"ts":       "2026-04-15T12:00:00Z",
						"tx":       "tx-c",
					},
				},
				"avg_cosine": 0.9,
				"mdl_ratio":  1.5,
				// No "distinct_ts" key — forces the Go code to compute.
			},
		},
	}
	src := &neo4jReflectClusterSource{client: fake}

	candidates, err := src.Candidates(context.Background(), "")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates: got %d want 1", len(candidates))
	}
	if got := candidates[0].DistinctTimestamps; got != 3 {
		t.Errorf("DistinctTimestamps: got %d want 3 (exemplars span 3 distinct UTC days)", got)
	}
}

// TestReflectClusterSource_CollapsesSameDayTimestamps locks in the
// UTC-day granularity: two exemplars on the same calendar day at
// different times must count as one distinct day, not two. Without
// this behaviour the MinDistinctTimestamps threshold would accept
// intraday chatter as cross-day evidence.
func TestReflectClusterSource_CollapsesSameDayTimestamps(t *testing.T) {
	fake := &captureQueryClient{
		rows: []map[string]any{
			{
				"id": "C1",
				"members": []any{
					map[string]any{"entry_id": "e:1", "ts": "2026-04-15T00:00:00Z", "tx": "t1"},
					map[string]any{"entry_id": "e:2", "ts": "2026-04-15T23:59:59Z", "tx": "t2"},
					map[string]any{"entry_id": "e:3", "ts": "2026-04-16T01:00:00Z", "tx": "t3"},
				},
			},
		},
	}
	src := &neo4jReflectClusterSource{client: fake}

	candidates, err := src.Candidates(context.Background(), "")
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if got := candidates[0].DistinctTimestamps; got != 2 {
		t.Errorf("DistinctTimestamps: got %d want 2 (two same-day + one next-day exemplars)", got)
	}
}

// TestReflectClusterSource_CypherReadsLastTx is the cortex-h9y
// regression guard. It proves the Cypher query references
// e.last_tx, not e.tx, so the applier-writer invariant
// (applier writes last_tx, reader reads last_tx) stays aligned.
// Pre-fix the Cypher contained "e.tx" and this test would fail.
func TestReflectClusterSource_CypherReadsLastTx(t *testing.T) {
	fake := &captureQueryClient{}
	src := &neo4jReflectClusterSource{client: fake}

	if _, err := src.Candidates(context.Background(), ""); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if fake.cypher == "" {
		t.Fatal("Candidates did not call QueryGraph")
	}
	if !strings.Contains(fake.cypher, "e.last_tx") {
		t.Errorf("Cypher must read e.last_tx (the applier's canonical property); got:\n%s", fake.cypher)
	}
	// Guard against the original bug regressing: the Cypher must
	// NOT reference a bare e.tx anywhere, which would be invisible
	// to the applier's property-write path.
	if strings.Contains(fake.cypher, "e.tx") && !strings.Contains(fake.cypher, "e.last_tx") {
		t.Errorf("Cypher references e.tx but not e.last_tx; applier writes last_tx so this row would always return tx='':\n%s", fake.cypher)
	}
}
