package merge

// Acceptance tests for cortex-4kq.45 (cortex subject merge with
// accretive aliases).
//
// The four bead acceptance criteria each map to one or more tests
// here:
//
//   AC1 "After merge, an alias lookup for psi_b returns the canonical
//        psi_a"  →  TestMerge_AliasDatomPointsToCanonical
//
//   AC2 "Neither psi_a nor psi_b datoms are mutated or deleted
//        (verified by pre/post byte compare)"  →
//        TestMerge_AppendOnlyPreservesPriorBytes (uses real log.Writer)
//
//   AC3 "Merging two subjects with contradictory facets keeps both
//        claims with provenance and emits contradiction edges"  →
//        TestMerge_ContradictionEdgesEmittedWithProvenance
//
//   AC4 "cortex subject merge records invoking identity from
//        CORTEX_AGENT or USER"  →  TestMerge_RecordsActorIdentity
//
// The remaining tests guard the validation surface, the
// reader-failure-aborts-before-write invariant, and the deterministic
// contradiction ordering that downstream consumers rely on.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
)

// fakeLog is an in-memory LogAppender capturing the datom group(s)
// the pipeline emits. failOn, when non-nil, replaces the success
// path so the test can prove the merge surfaces a log error.
type fakeLog struct {
	groups [][]datom.Datom
	failOn error
}

func (f *fakeLog) Append(group []datom.Datom) (string, error) {
	if f.failOn != nil {
		return "", f.failOn
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return group[0].Tx, nil
}

// fakeReader is a deterministic in-memory SubjectFacetReader. Every
// PSI looked up returns the matching entry from facets, or an empty
// map if absent. fail is consulted first so a single test can prove
// the reader-error path aborts cleanly.
type fakeReader struct {
	facets map[string]map[string]string
	fail   error
}

func (f *fakeReader) SubjectFacets(_ context.Context, p string) (map[string]string, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	if v, ok := f.facets[p]; ok {
		return v, nil
	}
	return map[string]string{}, nil
}

func newPipeline(lg LogAppender, rdr SubjectFacetReader) *Pipeline {
	return &Pipeline{
		Log:          lg,
		Reader:       rdr,
		Now:          func() time.Time { return time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC) },
		Actor:        "tester",
		InvocationID: ulid.Make().String(),
	}
}

// findDatom returns the first datom in the group whose entity and
// attribute match. Convenience for AC assertions.
func findDatom(t *testing.T, group []datom.Datom, entity, attr string) datom.Datom {
	t.Helper()
	for _, d := range group {
		if d.E == entity && d.A == attr {
			return d
		}
	}
	t.Fatalf("no datom for entity=%q attr=%q", entity, attr)
	return datom.Datom{}
}

func unmarshalString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	return s
}

// TestMerge_AliasDatomPointsToCanonical — AC1.
func TestMerge_AliasDatomPointsToCanonical(t *testing.T) {
	lg := &fakeLog{}
	p := newPipeline(lg, nil)
	res, err := p.Merge(context.Background(), Request{
		PsiA: "lib/foo",
		PsiB: "lib/bar",
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Canonical != "lib/foo" || res.Alias != "lib/bar" {
		t.Errorf("result canonical/alias = %q/%q, want lib/foo/lib/bar", res.Canonical, res.Alias)
	}
	if res.ContradictionCount != 0 {
		t.Errorf("ContradictionCount = %d, want 0 (nil reader)", res.ContradictionCount)
	}
	if len(lg.groups) != 1 {
		t.Fatalf("got %d groups, want 1", len(lg.groups))
	}
	group := lg.groups[0]

	// The alias_of datom must live on psi-b and point to psi-a.
	d := findDatom(t, group, "lib/bar", AttrAliasOf)
	if got := unmarshalString(t, d.V); got != "lib/foo" {
		t.Errorf("alias_of value = %q, want lib/foo", got)
	}
	// And the canonical pointer redundantly.
	d2 := findDatom(t, group, "lib/bar", AttrCanonical)
	if got := unmarshalString(t, d2.V); got != "lib/foo" {
		t.Errorf("canonical value = %q, want lib/foo", got)
	}
	// Source tag must be "merge" so history can filter the event.
	if d.Src != SrcMerge {
		t.Errorf("Src = %q, want %q", d.Src, SrcMerge)
	}
	// And every datom must be sealed (the Append path requires it,
	// but assert directly so a regression in buildMergeGroup surfaces
	// here rather than as a fakeLog passthrough).
	for i, dd := range group {
		if err := dd.Verify(); err != nil {
			t.Errorf("group[%d] verify: %v", i, err)
		}
	}
}

// TestMerge_RecordsActorIdentity — AC4.
func TestMerge_RecordsActorIdentity(t *testing.T) {
	lg := &fakeLog{}
	p := newPipeline(lg, nil)
	p.Actor = "alice@example"
	if _, err := p.Merge(context.Background(), Request{PsiA: "lib/a", PsiB: "lib/b"}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	d := findDatom(t, lg.groups[0], "lib/b", AttrMergeActor)
	if got := unmarshalString(t, d.V); got != "alice@example" {
		t.Errorf("merge.actor value = %q, want alice@example", got)
	}
	// And every datom must carry actor in its top-level Actor field.
	for i, dd := range lg.groups[0] {
		if dd.Actor != "alice@example" {
			t.Errorf("group[%d].Actor = %q, want alice@example", i, dd.Actor)
		}
	}
}

// TestMerge_ContradictionEdgesEmittedWithProvenance — AC3.
//
// Two subjects assert different values for facet "project". The merge
// must keep both claims (it must NOT mutate or delete either subject's
// existing datoms — that's AC2, exercised separately) and emit a
// contradiction edge entity carrying both values and both source PSIs.
func TestMerge_ContradictionEdgesEmittedWithProvenance(t *testing.T) {
	rdr := &fakeReader{facets: map[string]map[string]string{
		"lib/foo": {"project": "alpha", "owner": "team-a"},
		"lib/bar": {"project": "beta", "owner": "team-a"}, // disagrees on project, agrees on owner
	}}
	lg := &fakeLog{}
	p := newPipeline(lg, rdr)
	res, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.ContradictionCount != 1 {
		t.Fatalf("ContradictionCount = %d, want 1", res.ContradictionCount)
	}

	// Locate the contradiction-edge entity in the emitted group.
	group := lg.groups[0]
	var edgeID string
	for _, d := range group {
		if strings.HasPrefix(d.E, ContradictionPrefix) {
			edgeID = d.E
			break
		}
	}
	if edgeID == "" {
		t.Fatal("no contradiction edge entity emitted")
	}

	// All five attributes must be present on the edge entity.
	got := map[string]string{}
	for _, d := range group {
		if d.E != edgeID {
			continue
		}
		got[d.A] = unmarshalString(t, d.V)
	}
	want := map[string]string{
		AttrContradictionKey:     "project",
		AttrContradictionA:       "alpha",
		AttrContradictionB:       "beta",
		AttrContradictionSourceA: "lib/foo",
		AttrContradictionSourceB: "lib/bar",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("contradiction[%s] = %q, want %q", k, got[k], v)
		}
	}

	// The "owner" facet matches on both sides — no contradiction
	// edge should be emitted for it. We verify by counting distinct
	// contradiction-edge entities in the group.
	edges := map[string]struct{}{}
	for _, d := range group {
		if strings.HasPrefix(d.E, ContradictionPrefix) {
			edges[d.E] = struct{}{}
		}
	}
	if len(edges) != 1 {
		t.Errorf("distinct contradiction edges = %d, want 1", len(edges))
	}
}

// TestMerge_MultipleContradictionsAreSorted asserts the deterministic
// emission order — facet keys with disagreements emit edges in lex
// order so golden tests and human diffs stay stable.
func TestMerge_MultipleContradictionsAreSorted(t *testing.T) {
	rdr := &fakeReader{facets: map[string]map[string]string{
		"lib/foo": {"zeta": "1", "alpha": "1", "mike": "1"},
		"lib/bar": {"zeta": "2", "alpha": "2", "mike": "2"},
	}}
	lg := &fakeLog{}
	p := newPipeline(lg, rdr)
	if _, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"}); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Walk the group, collecting the contradiction.key values in the
	// order they appear. The first occurrence of each contradiction
	// edge entity defines its rank.
	seen := map[string]int{}
	keysInOrder := make([]string, 0, 3)
	for _, d := range lg.groups[0] {
		if !strings.HasPrefix(d.E, ContradictionPrefix) {
			continue
		}
		if _, ok := seen[d.E]; !ok {
			seen[d.E] = len(seen)
		}
		if d.A == AttrContradictionKey {
			keysInOrder = append(keysInOrder, unmarshalString(t, d.V))
		}
	}
	want := []string{"alpha", "mike", "zeta"}
	if len(keysInOrder) != len(want) {
		t.Fatalf("contradiction keys: got %v want %v", keysInOrder, want)
	}
	for i := range want {
		if keysInOrder[i] != want[i] {
			t.Errorf("contradiction key %d: got %q want %q", i, keysInOrder[i], want[i])
		}
	}
	// And the facet sort is stable: also assert via sort comparison.
	sortedCheck := append([]string(nil), keysInOrder...)
	sort.Strings(sortedCheck)
	for i := range sortedCheck {
		if sortedCheck[i] != keysInOrder[i] {
			t.Errorf("contradictions not in sorted order: %v", keysInOrder)
			break
		}
	}
}

// TestMerge_NoContradictionsWhenFacetsAgree — symmetric guard for the
// AC3 path: identical facets must NOT produce a contradiction edge.
func TestMerge_NoContradictionsWhenFacetsAgree(t *testing.T) {
	rdr := &fakeReader{facets: map[string]map[string]string{
		"lib/foo": {"project": "alpha"},
		"lib/bar": {"project": "alpha"},
	}}
	lg := &fakeLog{}
	p := newPipeline(lg, rdr)
	res, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.ContradictionCount != 0 {
		t.Errorf("ContradictionCount = %d, want 0", res.ContradictionCount)
	}
	for _, d := range lg.groups[0] {
		if strings.HasPrefix(d.E, ContradictionPrefix) {
			t.Errorf("unexpected contradiction edge datom: %+v", d)
		}
	}
}

// TestMerge_AsymmetricFacetsDoNotContradict — keys present in only
// one side are unification, not contradiction.
func TestMerge_AsymmetricFacetsDoNotContradict(t *testing.T) {
	rdr := &fakeReader{facets: map[string]map[string]string{
		"lib/foo": {"project": "alpha"},
		"lib/bar": {"owner": "team-a"},
	}}
	lg := &fakeLog{}
	p := newPipeline(lg, rdr)
	res, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.ContradictionCount != 0 {
		t.Errorf("ContradictionCount = %d, want 0 (asymmetric facets are not contradictions)", res.ContradictionCount)
	}
}

// TestMerge_ReaderErrorAbortsBeforeAnyWrite — invariant guard: the
// pipeline must consult the reader BEFORE touching the log so a
// transient reader failure cannot leave a half-merged subject pair.
func TestMerge_ReaderErrorAbortsBeforeAnyWrite(t *testing.T) {
	boom := errors.New("backend exploded")
	rdr := &fakeReader{fail: boom}
	lg := &fakeLog{}
	p := newPipeline(lg, rdr)
	_, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if err == nil {
		t.Fatal("expected reader error, got nil")
	}
	if !strings.Contains(err.Error(), "backend exploded") {
		t.Errorf("error %v does not wrap reader cause", err)
	}
	if len(lg.groups) != 0 {
		t.Errorf("log received %d groups despite reader failure; expected 0", len(lg.groups))
	}
}

// TestMerge_LogAppendErrorIsPropagated — surface log failures to the
// caller verbatim.
func TestMerge_LogAppendErrorIsPropagated(t *testing.T) {
	boom := errors.New("disk full")
	lg := &fakeLog{failOn: boom}
	p := newPipeline(lg, nil)
	_, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if err == nil {
		t.Fatal("expected log error, got nil")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error %v does not wrap log cause", err)
	}
}

// TestMerge_EmptyPSIRejected covers both sides via subtests so the
// validation messages stay symmetric.
func TestMerge_EmptyPSIRejected(t *testing.T) {
	cases := []Request{
		{PsiA: "", PsiB: "lib/bar"},
		{PsiA: "lib/foo", PsiB: ""},
		{PsiA: "  ", PsiB: "lib/bar"},
	}
	for _, c := range cases {
		_, err := newPipeline(&fakeLog{}, nil).Merge(context.Background(), c)
		if !errors.Is(err, ErrEmptyPSI) {
			t.Errorf("Merge(%+v) err = %v, want ErrEmptyPSI", c, err)
		}
	}
}

func TestMerge_SamePSIRejected(t *testing.T) {
	_, err := newPipeline(&fakeLog{}, nil).Merge(context.Background(), Request{
		PsiA: "lib/foo", PsiB: "lib/foo",
	})
	if !errors.Is(err, ErrSamePSI) {
		t.Errorf("err = %v, want ErrSamePSI", err)
	}
}

func TestMerge_NoLogRejected(t *testing.T) {
	p := &Pipeline{Actor: "x", InvocationID: ulid.Make().String()}
	_, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if !errors.Is(err, ErrNoLog) {
		t.Errorf("err = %v, want ErrNoLog", err)
	}
}

func TestMerge_EmptyActorRejected(t *testing.T) {
	p := &Pipeline{Log: &fakeLog{}, InvocationID: ulid.Make().String()}
	_, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"})
	if !errors.Is(err, ErrEmptyActor) {
		t.Errorf("err = %v, want ErrEmptyActor", err)
	}
}

// TestMerge_InvalidPSIRejected — both sides go through psi.Validate.
// We use an obviously bad namespace to provoke ErrUnknownNamespace
// inside psi.Validate; the merge must wrap it as ErrInvalidPSI.
func TestMerge_InvalidPSIRejected(t *testing.T) {
	_, err := newPipeline(&fakeLog{}, nil).Merge(context.Background(), Request{
		PsiA: "totally-not-a-namespace/x",
		PsiB: "lib/bar",
	})
	if !errors.Is(err, ErrInvalidPSI) {
		t.Errorf("err = %v, want ErrInvalidPSI", err)
	}
	_, err = newPipeline(&fakeLog{}, nil).Merge(context.Background(), Request{
		PsiA: "lib/foo",
		PsiB: "totally-not-a-namespace/y",
	})
	if !errors.Is(err, ErrInvalidPSI) {
		t.Errorf("err (psi-b) = %v, want ErrInvalidPSI", err)
	}
}

// writerAdapter bridges *log.Writer to LogAppender. The log package's
// Writer.Append already returns (string, error) so the adapter is
// trivial; declared here so the AC2 byte-compare test can use a real
// writer without polluting the production package.
type writerAdapter struct{ w *log.Writer }

func (a *writerAdapter) Append(group []datom.Datom) (string, error) {
	return a.w.Append(group)
}

// TestMerge_AppendOnlyPreservesPriorBytes — AC2.
//
// Pre-populate a real on-disk segment with some unrelated datoms,
// snapshot the segment file bytes, run a merge, then assert that
// every byte that existed before the merge is still present in the
// segment in the same position. The merge's new datoms may extend
// the segment (or land in a fresh segment if rollover fired), but
// no prior byte may be mutated or removed — that is the literal
// definition of "the log is append-only".
func TestMerge_AppendOnlyPreservesPriorBytes(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Seed with two unrelated datom groups so the segment file has
	// real prior content.
	for i := 0; i < 2; i++ {
		_, group := makeUnrelatedGroup(t)
		if _, err := w.Append(group); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}

	before := snapshotSegments(t, dir)
	if len(before) == 0 {
		t.Fatal("no segment files after seed")
	}

	p := &Pipeline{
		Log:          &writerAdapter{w: w},
		Now:          func() time.Time { return time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC) },
		Actor:        "tester",
		InvocationID: ulid.Make().String(),
	}
	if _, err := p.Merge(context.Background(), Request{PsiA: "lib/foo", PsiB: "lib/bar"}); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Every byte that existed before the merge must still be present
	// in the segment in the same position. New bytes may have been
	// appended after.
	for path, prior := range before {
		current, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("post-merge read %s: %v", path, err)
			continue
		}
		if len(current) < len(prior) {
			t.Errorf("segment %s shrank: was %d, now %d", path, len(prior), len(current))
			continue
		}
		if string(current[:len(prior)]) != string(prior) {
			t.Errorf("segment %s prior bytes mutated by merge", path)
		}
	}
}

// snapshotSegments reads every segment file in dir into a map keyed
// by absolute path, capturing the byte contents at call time.
func snapshotSegments(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", path, err)
		}
		out[path] = b
	}
	return out
}

// makeUnrelatedGroup builds a single 2-datom transaction group with
// content unrelated to subject merge, suitable for seeding a segment
// file in the AC2 test.
func makeUnrelatedGroup(t *testing.T) (string, []datom.Datom) {
	t.Helper()
	tx := ulid.Make().String()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	mk := func(attr, value string) datom.Datom {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        "seed",
			Op:           datom.OpAdd,
			E:            "entry:" + ulid.Make().String(),
			A:            attr,
			V:            raw,
			Src:          "test",
			InvocationID: tx,
		}
		if err := d.Seal(); err != nil {
			t.Fatalf("seal: %v", err)
		}
		return d
	}
	return tx, []datom.Datom{mk("body", "hello"), mk("kind", "Observation")}
}
