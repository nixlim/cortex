package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureObserver records every ObserveRequest the migrator sends,
// assigning a fresh entry id per call so Report.Created increments
// correctly. Tests inspect the captured list to verify facet shape,
// kind mapping, and the one-to-one drawer→Observation /
// diary→SessionReflection invariant.
type captureObserver struct {
	mu        sync.Mutex
	requests  []ObserveRequest
	failEvery int // 0 = never fail; N>0 → fail every Nth record
	callCount int
}

func (c *captureObserver) Observe(_ context.Context, req ObserveRequest) (*ObserveResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callCount++
	if c.failEvery > 0 && c.callCount%c.failEvery == 0 {
		return nil, fmt.Errorf("synthetic observe failure on call %d", c.callCount)
	}
	c.requests = append(c.requests, req)
	return &ObserveResult{
		EntryID: fmt.Sprintf("entry:%d", c.callCount),
		Tx:      fmt.Sprintf("tx:%d", c.callCount),
	}, nil
}

// buildFixture constructs a 10-drawer + 5-diary MemPalace JSONL
// export, matching the exact shape used by the bead's acceptance
// criterion. Returning an io.Reader keeps the fixture in-memory so
// no tempfile is required.
func buildFixture() *bytes.Buffer {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := 0; i < 10; i++ {
		_ = enc.Encode(Record{
			Kind:    KindDrawer,
			ID:      fmt.Sprintf("d%d", i),
			Body:    fmt.Sprintf("drawer body %d", i),
			Domain:  "engineering",
			Project: "cortex",
		})
	}
	for i := 0; i < 5; i++ {
		_ = enc.Encode(Record{
			Kind:    KindDiary,
			ID:      fmt.Sprintf("diary%d", i),
			Body:    fmt.Sprintf("diary entry %d", i),
			Domain:  "engineering",
			Project: "cortex",
			Tags:    map[string]string{"mood": "focused"},
		})
	}
	return &buf
}

// TestRun_FixtureProducesCorrectCounts covers the primary acceptance
// criterion: a 10+5 export produces 10 episodic entries and 5
// SessionReflection entries with migrated=true / source_system tags.
func TestRun_FixtureProducesCorrectCounts(t *testing.T) {
	obs := &captureObserver{}
	report, err := Run(context.Background(), buildFixture(), obs.Observe, RunOptions{
		SynthesizedTrailID: "trail:migration-01",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Created != 15 {
		t.Errorf("Created = %d, want 15", report.Created)
	}
	if report.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0; reasons: %v", report.Skipped, report.SkippedReasons)
	}
	if report.TrailID != "trail:migration-01" {
		t.Errorf("Report.TrailID = %q, want trail:migration-01", report.TrailID)
	}

	var drawers, diaries int
	for _, req := range obs.requests {
		switch req.Kind {
		case "Observation":
			drawers++
		case "SessionReflection":
			diaries++
			if req.TrailID != "trail:migration-01" {
				t.Errorf("diary missing synthesized trail: %+v", req)
			}
		default:
			t.Errorf("unexpected kind %q", req.Kind)
		}
		if req.Facets["migrated"] != "true" {
			t.Errorf("request missing migrated=true facet: %+v", req.Facets)
		}
		if req.Facets["source_system"] != SourceSystem {
			t.Errorf("request missing source_system facet: %+v", req.Facets)
		}
		if req.Facets["domain"] == "" || req.Facets["project"] == "" {
			t.Errorf("request missing required facets: %+v", req.Facets)
		}
	}
	if drawers != 10 {
		t.Errorf("drawers → Observation = %d, want 10", drawers)
	}
	if diaries != 5 {
		t.Errorf("diaries → SessionReflection = %d, want 5", diaries)
	}
}

// TestRun_DefaultFacetsApplied — a record that omits domain/project
// must still produce a valid ObserveRequest because the write
// pipeline rejects observe calls without those facets. The migrator
// falls back to RunOptions.DefaultDomain / DefaultProject, and then
// to hard-coded "migrated" / "mempalace".
func TestRun_DefaultFacetsApplied(t *testing.T) {
	input := mustEncode(t, Record{Kind: KindDrawer, Body: "no facets"})
	obs := &captureObserver{}
	_, err := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{
		DefaultDomain:  "personal",
		DefaultProject: "scratch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(obs.requests))
	}
	f := obs.requests[0].Facets
	if f["domain"] != "personal" || f["project"] != "scratch" {
		t.Errorf("defaults not applied: %+v", f)
	}
}

// TestRun_HardCodedFallbackFacets — with no defaults and no record
// fields, the very last fallback is applied so the request is still
// valid.
func TestRun_HardCodedFallbackFacets(t *testing.T) {
	input := mustEncode(t, Record{Kind: KindDrawer, Body: "no facets at all"})
	obs := &captureObserver{}
	_, err := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := obs.requests[0].Facets
	if f["domain"] != "migrated" || f["project"] != "mempalace" {
		t.Errorf("hard-coded fallback facets wrong: %+v", f)
	}
}

// TestRun_RecordTagsNeverOverrideReservedFacets — a mischievous
// export that tries to set migrated=false via a record tag must not
// override the spec-mandated facet.
func TestRun_RecordTagsNeverOverrideReservedFacets(t *testing.T) {
	input := mustEncode(t, Record{
		Kind:    KindDrawer,
		Body:    "sneaky",
		Domain:  "d",
		Project: "p",
		Tags: map[string]string{
			"migrated":      "false",
			"source_system": "forged",
			"mood":          "calm",
		},
	})
	obs := &captureObserver{}
	_, err := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f := obs.requests[0].Facets
	if f["migrated"] != "true" {
		t.Errorf("migrated tag override succeeded: %+v", f)
	}
	if f["source_system"] != SourceSystem {
		t.Errorf("source_system tag override succeeded: %+v", f)
	}
	if f["mood"] != "calm" {
		t.Errorf("benign tag lost: %+v", f)
	}
}

// TestRun_MalformedLinesAreSkippedNotFatal — one garbage line in
// the middle of a valid export must produce a skip count of 1 and
// leave the surrounding records unaffected.
func TestRun_MalformedLinesAreSkippedNotFatal(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(mustEncode(t, Record{Kind: KindDrawer, Body: "first", Domain: "d", Project: "p"}))
	buf.WriteString("{garbage not json}\n")
	buf.Write(mustEncode(t, Record{Kind: KindDrawer, Body: "third", Domain: "d", Project: "p"}))

	obs := &captureObserver{}
	report, err := Run(context.Background(), &buf, obs.Observe, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != 2 {
		t.Errorf("Created = %d, want 2", report.Created)
	}
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
	if len(report.SkippedReasons) != 1 || !strings.Contains(report.SkippedReasons[0], "line 2") {
		t.Errorf("unexpected skip reasons: %v", report.SkippedReasons)
	}
}

// TestRun_UnknownKindIsSkipped — "note" is not a MemPalace kind
// the migrator knows; it must skip rather than crash.
func TestRun_UnknownKindIsSkipped(t *testing.T) {
	input := mustEncode(t, Record{Kind: "note", Body: "???"})
	obs := &captureObserver{}
	report, err := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 1 || report.Created != 0 {
		t.Errorf("report = %+v, want Skipped=1 Created=0", report)
	}
}

// TestRun_EmptyBodyIsSkipped
func TestRun_EmptyBodyIsSkipped(t *testing.T) {
	input := mustEncode(t, Record{Kind: KindDrawer, Body: "   "})
	obs := &captureObserver{}
	report, _ := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{})
	if report.Skipped != 1 {
		t.Errorf("empty body not skipped: %+v", report)
	}
}

// TestRun_ObserveErrorIsSkipped — a per-record observe failure is
// recorded as a skip, not a run-level abort. This lets an operator
// re-run the migration after fixing whatever caused the failure
// (e.g., ollama not reachable on first try).
func TestRun_ObserveErrorIsSkipped(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 4; i++ {
		buf.Write(mustEncode(t, Record{Kind: KindDrawer, Body: fmt.Sprintf("r%d", i), Domain: "d", Project: "p"}))
	}
	obs := &captureObserver{failEvery: 2} // fail call 2 and 4
	report, err := Run(context.Background(), &buf, obs.Observe, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Created != 2 || report.Skipped != 2 {
		t.Errorf("report = %+v, want Created=2 Skipped=2", report)
	}
}

// TestRun_NilObserveFuncIsError — a nil callback is a programmer
// bug, not a runtime condition, and must surface immediately.
func TestRun_NilObserveFuncIsError(t *testing.T) {
	_, err := Run(context.Background(), bytes.NewReader(nil), nil, RunOptions{})
	if err == nil {
		t.Fatal("expected error on nil ObserveFunc")
	}
}

// TestCanonicalizePath_RejectsEmpty
func TestCanonicalizePath_RejectsEmpty(t *testing.T) {
	if _, err := CanonicalizePath(""); err == nil {
		t.Fatal("expected error on empty path")
	}
	if _, err := CanonicalizePath("   "); err == nil {
		t.Fatal("expected error on whitespace path")
	}
}

// TestCanonicalizePath_Absolutizes — a relative path becomes absolute
// and cleaned.
func TestCanonicalizePath_Absolutizes(t *testing.T) {
	got, err := CanonicalizePath("./export/../export/mempalace.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("CanonicalizePath returned relative %q", got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("CanonicalizePath left .. in %q", got)
	}
}

// TestRun_OversizedLineIsSkipped — a line longer than MaxRecordBytes
// is rejected by the scanner. We use a tiny cap and feed a 1KB blob
// to keep the test cheap.
func TestRun_OversizedLineIsSkipped(t *testing.T) {
	big := strings.Repeat("x", 1024)
	input := []byte(fmt.Sprintf(`{"kind":"drawer","body":%q,"domain":"d","project":"p"}`+"\n", big))
	obs := &captureObserver{}
	report, err := Run(context.Background(), bytes.NewReader(input), obs.Observe, RunOptions{MaxRecordBytes: 256})
	if err == nil {
		t.Fatal("expected scanner error for oversized line")
	}
	if !errors.Is(err, err) || !strings.Contains(err.Error(), "scan") {
		t.Errorf("err = %v, want scanner-level error", err)
	}
	if report.Created != 0 {
		t.Errorf("Created = %d, want 0 when line is over cap", report.Created)
	}
}

// TestReport_SkipReasonsCapBounded — a pathological export with
// thousands of bad lines must not grow SkippedReasons unboundedly.
func TestReport_SkipReasonsCapBounded(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 50; i++ {
		buf.WriteString("garbage\n")
	}
	obs := &captureObserver{}
	report, _ := Run(context.Background(), &buf, obs.Observe, RunOptions{SkipReasonsCap: 10})
	if report.Skipped != 50 {
		t.Errorf("Skipped = %d, want 50", report.Skipped)
	}
	if len(report.SkippedReasons) != 10 {
		t.Errorf("SkippedReasons len = %d, want 10 (cap)", len(report.SkippedReasons))
	}
}

// mustEncode encodes rec as a single JSONL line (body + '\n').
func mustEncode(t *testing.T, rec Record) []byte {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return append(b, '\n')
}
