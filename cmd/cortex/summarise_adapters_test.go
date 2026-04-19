package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/claudecli"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/summarise"
)

// fakeSummNeo4j is a neo4j.Client double that returns a scripted set
// of rows per-query. Each adapter test seeds the rows it needs; the
// other methods panic so an accidental call fails loudly rather than
// silently corrupting test state.
type fakeSummNeo4j struct {
	priorRows     []map[string]any
	communityRows []map[string]any
	lastCypher    string
	err           error
}

func (f *fakeSummNeo4j) Ping(context.Context) error { panic("unused") }
func (f *fakeSummNeo4j) ProbeProcedures(context.Context) (neo4j.ProcedureAvailability, error) {
	panic("unused")
}
func (f *fakeSummNeo4j) WriteEntries(context.Context, string, map[string]any) error {
	panic("unused")
}
func (f *fakeSummNeo4j) RunGDS(context.Context, string, map[string]any) ([]map[string]any, error) {
	panic("unused")
}
func (f *fakeSummNeo4j) Close(context.Context) error { return nil }

func (f *fakeSummNeo4j) QueryGraph(_ context.Context, cypher string, _ map[string]any) ([]map[string]any, error) {
	f.lastCypher = cypher
	if f.err != nil {
		return nil, f.err
	}
	switch {
	case strings.Contains(cypher, "f.frame_type = 'CommunityBrief'"):
		return f.priorRows, nil
	case strings.Contains(cypher, ":Community {level: 0}"):
		return f.communityRows, nil
	}
	return nil, nil
}

// ---------- PriorBriefLoader ----------

func TestPriorBriefLoader_PicksLatestPerCommunity(t *testing.T) {
	// Two briefs for community c1 (newest tx wins), one for c2.
	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	rows := []map[string]any{
		{
			"frame_id": "frame:01NEWER",
			"slots": mustJSON(map[string]any{
				"community_id":    "c1",
				"membership_hash": "new",
			}),
			"tx": "01HY000000000000000000000Z",
		},
		{
			"frame_id": "frame:01OLDER",
			"slots": mustJSON(map[string]any{
				"community_id":    "c1",
				"membership_hash": "old",
			}),
			"tx": "01HX000000000000000000000Z",
		},
		{
			"frame_id": "frame:01C2",
			"slots": mustJSON(map[string]any{
				"community_id":    "c2",
				"membership_hash": "c2hash",
			}),
			"tx": "01HY000000000000000000000Z",
		},
	}
	loader := &neo4jPriorBriefLoader{client: &fakeSummNeo4j{priorRows: rows}}
	got, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got["c1"].MembershipHash != "new" {
		t.Errorf("c1 hash: got %q want %q", got["c1"].MembershipHash, "new")
	}
	if got["c1"].FrameID != "frame:01NEWER" {
		t.Errorf("c1 frame_id: got %q want frame:01NEWER", got["c1"].FrameID)
	}
	if got["c2"].MembershipHash != "c2hash" {
		t.Errorf("c2 hash: got %q", got["c2"].MembershipHash)
	}
}

func TestPriorBriefLoader_SkipsMalformedSlots(t *testing.T) {
	rows := []map[string]any{
		{"frame_id": "frame:bad", "slots": "not json", "tx": "01HZ"},
		{"frame_id": "frame:ok", "slots": `{"community_id":"c1","membership_hash":"h"}`, "tx": "01HY"},
	}
	loader := &neo4jPriorBriefLoader{client: &fakeSummNeo4j{priorRows: rows}}
	got, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 valid brief, got %d", len(got))
	}
	if got["c1"].MembershipHash != "h" {
		t.Errorf("c1 hash: got %q", got["c1"].MembershipHash)
	}
}

// ---------- CommunityMaterialiser ----------

func TestCommunityMaterialiser_SortsEntryIDsAndPopulatesEntries(t *testing.T) {
	rows := []map[string]any{
		{
			"community_id": "42",
			"members": []any{
				map[string]any{
					"entry_id":    "entry:Z",
					"kind":        "Observation",
					"body":        "body-of-Z",
					"encoding_at": "2026-04-18T12:00:00Z",
				},
				map[string]any{
					"entry_id":    "entry:A",
					"kind":        "Observation",
					"body":        "body-of-A",
					"encoding_at": "2026-04-18T11:00:00Z",
				},
			},
		},
	}
	m := &neo4jCommunityMaterialiser{client: &fakeSummNeo4j{communityRows: rows}}
	got, err := m.Materialise(context.Background())
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len: got %d want 1", len(got))
	}
	c := got[0]
	if c.ID != "42" {
		t.Errorf("community id: got %q", c.ID)
	}
	if len(c.EntryIDs) != 2 || c.EntryIDs[0] != "entry:A" || c.EntryIDs[1] != "entry:Z" {
		t.Errorf("EntryIDs not sorted: %v", c.EntryIDs)
	}
	if c.Entries[0].Body != "body-of-A" {
		t.Errorf("Entries[0].Body: got %q", c.Entries[0].Body)
	}
	if c.Entries[0].TS.IsZero() {
		t.Errorf("Entries[0].TS should be parsed from RFC3339")
	}
}

func TestCommunityMaterialiser_SkipsEmptyCommunities(t *testing.T) {
	rows := []map[string]any{
		{"community_id": "7", "members": []any{}},
	}
	m := &neo4jCommunityMaterialiser{client: &fakeSummNeo4j{communityRows: rows}}
	got, err := m.Materialise(context.Background())
	if err != nil {
		t.Fatalf("Materialise: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty community should be skipped, got %d", len(got))
	}
}

// ---------- FrameWriter ----------

type fakeAppendLog struct {
	groups [][]datom.Datom
	err    error
}

func (f *fakeAppendLog) Append(group []datom.Datom) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return group[0].Tx, nil
}

func TestFrameWriter_EmitsExpectedDatoms(t *testing.T) {
	log := &fakeAppendLog{}
	fixed := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	w := &logSummariseFrameWriter{
		log:          log,
		actor:        "tester",
		invocationID: "inv-1",
		now:          func() time.Time { return fixed },
	}
	frames := []summarise.Frame{
		{
			Type:          "CommunityBrief",
			Slots:         map[string]any{"community_id": "c1", "membership_hash": "h1"},
			Exemplars:     []string{"entry:E1", "entry:E2"},
			SchemaVersion: "v1",
		},
	}
	if err := w.Write(frames); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(log.groups) != 1 {
		t.Fatalf("groups: got %d want 1", len(log.groups))
	}
	g := log.groups[0]
	attrs := map[string]bool{}
	derivedCount := 0
	for _, d := range g {
		attrs[d.A] = true
		if d.A == "derived_from" {
			derivedCount++
		}
		if d.Src != "summarise" {
			t.Errorf("datom src: got %q want summarise", d.Src)
		}
		if d.InvocationID != "inv-1" {
			t.Errorf("invocation_id: got %q", d.InvocationID)
		}
		if d.Checksum == "" {
			t.Errorf("datom not sealed: %+v", d)
		}
	}
	for _, want := range []string{"frame.type", "frame.slots", "frame.schema_version"} {
		if !attrs[want] {
			t.Errorf("missing attr %q", want)
		}
	}
	if derivedCount != 2 {
		t.Errorf("derived_from datoms: got %d want 2", derivedCount)
	}
}

func TestFrameWriter_SkipsBareCommunityIDsInDerivedFrom(t *testing.T) {
	// ProjectBrief exemplars are plain community IDs ("42", "7") —
	// they must NOT become DERIVED_FROM datoms pointing at synthetic
	// CortexEntity nodes.
	log := &fakeAppendLog{}
	w := &logSummariseFrameWriter{
		log:          log,
		actor:        "tester",
		invocationID: "inv-1",
		now:          func() time.Time { return time.Now().UTC() },
	}
	frames := []summarise.Frame{
		{
			Type:      "ProjectBrief",
			Slots:     map[string]any{"project": "all"},
			Exemplars: []string{"42", "7"},
		},
	}
	if err := w.Write(frames); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, d := range log.groups[0] {
		if d.A == "derived_from" {
			t.Errorf("bare community id became a DERIVED_FROM datom: %+v", d)
		}
	}
}

// ---------- Runner composition ----------

type stubPrior struct {
	out map[summarise.CommunityID]summarise.PriorBrief
	err error
}

func (s *stubPrior) Load(context.Context) (map[summarise.CommunityID]summarise.PriorBrief, error) {
	return s.out, s.err
}

type stubComm struct {
	out []summarise.Community
	err error
}

func (s *stubComm) Materialise(context.Context) ([]summarise.Community, error) {
	return s.out, s.err
}

type stubFrameWriter struct {
	frames []summarise.Frame
	err    error
}

func (s *stubFrameWriter) Write(frames []summarise.Frame) error {
	if s.err != nil {
		return s.err
	}
	s.frames = append(s.frames, frames...)
	return nil
}

func TestSummariserRunner_IdempotentWhenHashesMatch(t *testing.T) {
	// Acceptance criterion (1): zero per-community LLM calls on a
	// re-run where every community's membership hash is unchanged.
	entryIDs := []string{"entry:A", "entry:B"}
	hash := summarise.MembershipHash(entryIDs)
	communities := []summarise.Community{{
		ID:       "c1",
		EntryIDs: entryIDs,
		Entries: []summarise.Entry{
			{ID: "entry:A", Body: "a"},
			{ID: "entry:B", Body: "b"},
		},
	}}
	prior := map[summarise.CommunityID]summarise.PriorBrief{
		"c1": {CommunityID: "c1", MembershipHash: hash},
	}

	var llmCalls int32
	fake := &fakeRunner{respond: func(req claudecli.Request) (claudecli.Response, error) {
		atomic.AddInt32(&llmCalls, 1)
		// Treat anything reaching the runner as the stitch prompt.
		return claudecli.Response{StructuredOutput: json.RawMessage(`{
			"project": "all",
			"generated_at": "2026-04-18T12:00:00Z",
			"community_ids": ["c1"],
			"stitched_narrative": "Short narrative that satisfies the schema minLength requirement."
		}`)}, nil
	}}
	stage, err := summarise.New(summarise.Config{Runner: fake})
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	writer := &stubFrameWriter{}
	runner := &summariserRunner{
		project: "all",
		stage:   stage,
		prior:   &stubPrior{out: prior},
		comm:    &stubComm{out: communities},
		writer:  writer,
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the stitch call should have happened — per-community were all skipped.
	if got := atomic.LoadInt32(&llmCalls); got != 1 {
		t.Errorf("llm calls: got %d want 1 (stitch only)", got)
	}
	if len(writer.frames) != 1 || writer.frames[0].Type != "ProjectBrief" {
		t.Errorf("expected exactly 1 ProjectBrief frame, got %+v", writer.frames)
	}
}

func TestSummariserRunner_PropagatesMaterialiseError(t *testing.T) {
	boom := errors.New("neo4j down")
	runner := &summariserRunner{
		project: "all",
		comm:    &stubComm{err: boom},
	}
	err := runner.Run(context.Background())
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("Run: got %v want wrapping %v", err, boom)
	}
}

func TestSummariserRunner_NoCommunitiesIsNotAnError(t *testing.T) {
	runner := &summariserRunner{
		project: "all",
		comm:    &stubComm{out: nil},
	}
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// fakeRunner mirrors internal/summarise/stage_test.go's helper so the
// adapter tests can drive the stage without spawning subprocesses.
type fakeRunner struct {
	respond func(req claudecli.Request) (claudecli.Response, error)
}

func (f *fakeRunner) Run(_ context.Context, req claudecli.Request) (claudecli.Response, error) {
	return f.respond(req)
}
