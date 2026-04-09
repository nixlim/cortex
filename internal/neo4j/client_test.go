package neo4j

import (
	"context"
	"errors"
	"testing"
)

// fakeRunner is a cypherRunner test double. It records every query
// it receives and returns a programmable result for each one, keyed
// by a substring match against the Cypher text. Tests use the
// substring match rather than exact-string equality so trivial
// whitespace or parameter-style edits don't break them.
type fakeRunner struct {
	responses []fakeResponse
	seen      []fakeCall
}

type fakeResponse struct {
	matches string // substring that must appear in the query
	rows    []map[string]any
	err     error
}

type fakeCall struct {
	cypher string
	params map[string]any
}

func (f *fakeRunner) Run(ctx context.Context, cypher string, params map[string]any) ([]map[string]any, error) {
	f.seen = append(f.seen, fakeCall{cypher: cypher, params: params})
	for _, r := range f.responses {
		if r.matches == "" || containsSubstr(cypher, r.matches) {
			return r.rows, r.err
		}
	}
	return nil, errors.New("fakeRunner: no matching response")
}

func containsSubstr(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
outer:
	for i := 0; i+len(n) <= len(h); i++ {
		for j := 0; j < len(n); j++ {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}

// TestProbeProcedures_AllPresent covers the acceptance criterion
// "ProbeProcedures reports gds.pageRank.stream as available when
// connected to the custom cortex/neo4j-gds image". With the fake
// returning all three procedures, the struct's three availability
// flags should all be true and LeidenUnavailable should be false.
func TestProbeProcedures_AllPresent(t *testing.T) {
	r := &fakeRunner{
		responses: []fakeResponse{{
			matches: "SHOW PROCEDURES",
			rows: []map[string]any{
				{"name": ProcPageRankStream},
				{"name": ProcLeidenStream},
				{"name": ProcLouvainStream},
			},
		}},
	}
	avail, err := probeProcedures(context.Background(), r)
	if err != nil {
		t.Fatalf("probeProcedures: %v", err)
	}
	if !avail.PageRankStream || !avail.LeidenStream || !avail.LouvainStream {
		t.Fatalf("expected all three procs available, got %+v", avail)
	}
	if avail.LeidenUnavailable {
		t.Fatal("LeidenUnavailable should be false when Leiden is available")
	}
}

// TestProbeProcedures_LeidenMissingLouvainPresent covers the
// acceptance criterion "When gds.leiden.stream is missing the probe
// returns a LeidenUnavailable signal and gds.louvain.stream is
// reported available".
func TestProbeProcedures_LeidenMissingLouvainPresent(t *testing.T) {
	r := &fakeRunner{
		responses: []fakeResponse{{
			matches: "SHOW PROCEDURES",
			rows: []map[string]any{
				{"name": ProcPageRankStream},
				{"name": ProcLouvainStream},
				// no Leiden
			},
		}},
	}
	avail, err := probeProcedures(context.Background(), r)
	if err != nil {
		t.Fatalf("probeProcedures: %v", err)
	}
	if avail.LeidenStream {
		t.Fatal("LeidenStream should be false")
	}
	if !avail.LouvainStream {
		t.Fatal("LouvainStream should be true")
	}
	if !avail.LeidenUnavailable {
		t.Fatal("LeidenUnavailable should be true when Leiden missing and Louvain present")
	}
}

func TestProbeProcedures_BothMissing(t *testing.T) {
	// Neither Leiden nor Louvain present — LeidenUnavailable is
	// deliberately false because there is no usable fallback, and
	// the caller must fail the whole community-detection step
	// rather than silently switching to Louvain.
	r := &fakeRunner{
		responses: []fakeResponse{{
			matches: "SHOW PROCEDURES",
			rows: []map[string]any{
				{"name": ProcPageRankStream},
			},
		}},
	}
	avail, err := probeProcedures(context.Background(), r)
	if err != nil {
		t.Fatalf("probeProcedures: %v", err)
	}
	if avail.LeidenStream || avail.LouvainStream {
		t.Fatalf("expected both Leiden and Louvain unavailable, got %+v", avail)
	}
	if avail.LeidenUnavailable {
		t.Fatal("LeidenUnavailable should be false when Louvain is also missing (no fallback)")
	}
}

func TestProbeProcedures_SurfacesRunError(t *testing.T) {
	// If SHOW PROCEDURES itself fails (older Neo4j, permission
	// denied, etc.), the probe must surface the error rather than
	// returning a zero-value ProcedureAvailability.
	sentinel := errors.New("boom")
	r := &fakeRunner{
		responses: []fakeResponse{{matches: "", err: sentinel}},
	}
	_, err := probeProcedures(context.Background(), r)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel, got %v", err)
	}
}

func TestNormalizeBoltURL(t *testing.T) {
	cases := map[string]string{
		"localhost:7687":            "bolt://localhost:7687",
		"bolt://localhost:7687":     "bolt://localhost:7687",
		"neo4j://cluster.local:7687": "neo4j://cluster.local:7687",
		"bolt+s://secure:7687":      "bolt+s://secure:7687",
		"localhost:7687/":           "bolt://localhost:7687",
	}
	for in, want := range cases {
		if got := normalizeBoltURL(in); got != want {
			t.Errorf("normalizeBoltURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEscapeGraphName_RejectsUnsafe(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unsafe graph name")
		}
	}()
	escapeGraphName("bad'name")
}

func TestEscapeGraphName_AcceptsSafe(t *testing.T) {
	got := escapeGraphName("cortex.semantic")
	if got != "cortex.semantic" {
		t.Errorf("got %q", got)
	}
}

func TestGDSQueries_ContainExpectedProcedureNames(t *testing.T) {
	if !containsSubstr(PersonalizedPageRankQuery("g"), "gds.pageRank.stream") {
		t.Error("PersonalizedPageRankQuery missing gds.pageRank.stream")
	}
	if !containsSubstr(LeidenStreamQuery("g"), "gds.leiden.stream") {
		t.Error("LeidenStreamQuery missing gds.leiden.stream")
	}
	if !containsSubstr(LouvainStreamQuery("g"), "gds.louvain.stream") {
		t.Error("LouvainStreamQuery missing gds.louvain.stream")
	}
}
