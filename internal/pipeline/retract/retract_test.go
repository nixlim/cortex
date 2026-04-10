package retract

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
)

// fakeLog is an in-memory LogAppender that records every group it
// receives, in order. Tests inspect groups directly to assert on the
// emitted datoms without going through a real segment file.
type fakeLog struct {
	groups [][]datom.Datom
	failOn int // 1-based index of the call that should fail; 0 = never
	calls  int
}

func (f *fakeLog) Append(group []datom.Datom) (string, error) {
	f.calls++
	if f.failOn > 0 && f.calls == f.failOn {
		return "", errors.New("synthetic log failure")
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return group[0].Tx, nil
}

// newPipeline returns a Pipeline wired with a fake log and a pinned
// clock so test datoms are deterministic.
func newPipeline(t *testing.T) (*Pipeline, *fakeLog) {
	t.Helper()
	fl := &fakeLog{}
	return &Pipeline{
		Log:          fl,
		Now:          func() time.Time { return time.Unix(0, 0).UTC() },
		Actor:        "alice",
		InvocationID: "01HPTESTINVOCATION0000000000",
	}, fl
}

// TestRetract_TargetWritesSealedGroup is the AC1 case: a basic
// retraction of a single entity emits one group containing the
// tombstone, reason, and actor audit datoms — all sealed and sharing
// one tx.
func TestRetract_TargetWritesSealedGroup(t *testing.T) {
	p, fl := newPipeline(t)
	res, err := p.Retract(context.Background(), Request{
		EntityID: "entry:abc",
		Reason:   "superseded by ADR-12",
	})
	if err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if len(res.EntityIDs) != 1 || res.EntityIDs[0] != "entry:abc" {
		t.Fatalf("Result.EntityIDs: %v", res.EntityIDs)
	}
	if len(fl.groups) != 1 {
		t.Fatalf("groups: got %d want 1", len(fl.groups))
	}
	g := fl.groups[0]
	// Tombstone + reason + actor = 3 datoms (no cascade source on the
	// operator-named target).
	if len(g) != 3 {
		t.Fatalf("group size: got %d want 3", len(g))
	}
	for i, d := range g {
		if d.Tx != res.TxIDs[0] {
			t.Errorf("datom %d tx: got %s want %s", i, d.Tx, res.TxIDs[0])
		}
		if d.E != "entry:abc" {
			t.Errorf("datom %d entity: got %s", i, d.E)
		}
		if d.Actor != "alice" {
			t.Errorf("datom %d actor: got %s", i, d.Actor)
		}
		if d.Src != SrcRetract {
			t.Errorf("datom %d src: got %s want %s", i, d.Src, SrcRetract)
		}
		if d.Checksum == "" {
			t.Errorf("datom %d not sealed", i)
		}
		if err := d.Verify(); err != nil {
			t.Errorf("datom %d verify: %v", i, err)
		}
	}

	// First datom is the OpRetract tombstone against AttrExists.
	if g[0].Op != datom.OpRetract {
		t.Errorf("group[0].Op = %s want retract", g[0].Op)
	}
	if g[0].A != AttrExists {
		t.Errorf("group[0].A = %s want %s", g[0].A, AttrExists)
	}

	// Second is the reason audit.
	if g[1].Op != datom.OpAdd || g[1].A != AttrRetractReason {
		t.Errorf("group[1] = (%s,%s) want (add,%s)", g[1].Op, g[1].A, AttrRetractReason)
	}
	var reason string
	if err := json.Unmarshal(g[1].V, &reason); err != nil || reason != "superseded by ADR-12" {
		t.Errorf("reason value: %s err=%v", reason, err)
	}

	// Third is the actor audit.
	if g[2].Op != datom.OpAdd || g[2].A != AttrRetractActor {
		t.Errorf("group[2] = (%s,%s) want (add,%s)", g[2].Op, g[2].A, AttrRetractActor)
	}
	var actor string
	if err := json.Unmarshal(g[2].V, &actor); err != nil || actor != "alice" {
		t.Errorf("actor value: %s err=%v", actor, err)
	}
}

// TestRetract_CascadeWritesChildren is the AC2 case: --cascade plus a
// resolver returning two children produces three groups in target-
// first order, with the children carrying retract.cascade_source
// pointing back at the target.
func TestRetract_CascadeWritesChildren(t *testing.T) {
	p, fl := newPipeline(t)
	p.Resolver = ChildResolverFunc(func(_ context.Context, parent string) ([]string, error) {
		if parent != "frame:f1" {
			t.Errorf("resolver called with unexpected parent %q", parent)
		}
		return []string{"frame:c1", "frame:c2"}, nil
	})

	res, err := p.Retract(context.Background(), Request{
		EntityID: "frame:f1",
		Reason:   "wrong assumption",
		Cascade:  true,
	})
	if err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if want := []string{"frame:f1", "frame:c1", "frame:c2"}; !equalStrings(res.EntityIDs, want) {
		t.Errorf("EntityIDs: got %v want %v", res.EntityIDs, want)
	}
	if len(fl.groups) != 3 {
		t.Fatalf("groups: got %d want 3", len(fl.groups))
	}

	// Target group has no cascade_source datom.
	for _, d := range fl.groups[0] {
		if d.A == AttrRetractCascadeSource {
			t.Errorf("target group should not carry cascade_source: %+v", d)
		}
	}

	// Each child group must carry exactly one cascade_source datom
	// pointing at the operator-named target.
	for i := 1; i < 3; i++ {
		g := fl.groups[i]
		if len(g) != 4 {
			t.Errorf("child group %d size: got %d want 4", i, len(g))
		}
		var srcDatoms int
		for _, d := range g {
			if d.A == AttrRetractCascadeSource {
				srcDatoms++
				var v string
				if err := json.Unmarshal(d.V, &v); err != nil || v != "frame:f1" {
					t.Errorf("child %d cascade_source value: %s err=%v", i, v, err)
				}
			}
		}
		if srcDatoms != 1 {
			t.Errorf("child group %d cascade_source datoms: got %d want 1", i, srcDatoms)
		}
	}

	// Every group's tx is unique — children get their own tx ULIDs.
	txs := map[string]struct{}{}
	for _, g := range fl.groups {
		txs[g[0].Tx] = struct{}{}
	}
	if len(txs) != 3 {
		t.Errorf("expected 3 distinct tx ULIDs, got %d", len(txs))
	}
}

// TestRetract_CascadeWithoutResolverErrors covers the safety guard:
// --cascade with a nil resolver must fail loudly rather than
// silently retracting only the target.
func TestRetract_CascadeWithoutResolverErrors(t *testing.T) {
	p, fl := newPipeline(t)
	p.Resolver = nil
	_, err := p.Retract(context.Background(), Request{
		EntityID: "frame:f1",
		Cascade:  true,
	})
	if !errors.Is(err, ErrNoResolver) {
		t.Errorf("err: got %v want ErrNoResolver", err)
	}
	if len(fl.groups) != 0 {
		t.Errorf("log touched on cascade-without-resolver: %d groups", len(fl.groups))
	}
}

// TestRetract_ResolverErrorAbortsBeforeAnyWrite is the partial-cascade
// guard: if the resolver fails, the pipeline must NOT have already
// written the target's retraction. The bead's "no half-cascaded log"
// invariant.
func TestRetract_ResolverErrorAbortsBeforeAnyWrite(t *testing.T) {
	p, fl := newPipeline(t)
	p.Resolver = ChildResolverFunc(func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("neo4j unavailable")
	})
	_, err := p.Retract(context.Background(), Request{
		EntityID: "frame:f1",
		Cascade:  true,
	})
	if err == nil {
		t.Fatal("expected error from resolver")
	}
	if !strings.Contains(err.Error(), "resolve children") {
		t.Errorf("err message: %v", err)
	}
	if len(fl.groups) != 0 {
		t.Errorf("log touched after resolver failure: %d groups", len(fl.groups))
	}
}

// TestRetract_EmptyEntityIDIsRejected covers the input validation
// guard.
func TestRetract_EmptyEntityIDIsRejected(t *testing.T) {
	p, fl := newPipeline(t)
	_, err := p.Retract(context.Background(), Request{EntityID: "  "})
	if !errors.Is(err, ErrEmptyEntityID) {
		t.Errorf("err: got %v want ErrEmptyEntityID", err)
	}
	if len(fl.groups) != 0 {
		t.Errorf("log touched on empty entity id")
	}
}

// TestRetract_EmptyActorIsRejected — audit datoms must always carry
// an identity, so a misconfigured pipeline fails immediately.
func TestRetract_EmptyActorIsRejected(t *testing.T) {
	p, _ := newPipeline(t)
	p.Actor = ""
	_, err := p.Retract(context.Background(), Request{EntityID: "entry:x"})
	if !errors.Is(err, ErrEmptyActor) {
		t.Errorf("err: got %v want ErrEmptyActor", err)
	}
}

// TestRetract_NoLogIsRejected — guard against the zero-value Pipeline.
func TestRetract_NoLogIsRejected(t *testing.T) {
	p := &Pipeline{Actor: "alice"}
	_, err := p.Retract(context.Background(), Request{EntityID: "entry:x"})
	if !errors.Is(err, ErrNoLog) {
		t.Errorf("err: got %v want ErrNoLog", err)
	}
}

// TestRetract_EmptyReasonIsAllowed — the bead says reason is optional.
// The audit datom is still emitted (with an empty value) so the audit
// shape is stable.
func TestRetract_EmptyReasonIsAllowed(t *testing.T) {
	p, fl := newPipeline(t)
	_, err := p.Retract(context.Background(), Request{EntityID: "entry:x"})
	if err != nil {
		t.Fatal(err)
	}
	g := fl.groups[0]
	if len(g) != 3 {
		t.Fatalf("group size: %d", len(g))
	}
	var reason string
	if err := json.Unmarshal(g[1].V, &reason); err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("reason: got %q want empty", reason)
	}
}

// TestRetract_LogAppendErrorIsPropagated — if the log refuses an
// append (disk full, lock timeout) the error reaches the caller and
// the result records what was committed before the failure.
func TestRetract_LogAppendErrorIsPropagated(t *testing.T) {
	p, fl := newPipeline(t)
	p.Resolver = ChildResolverFunc(func(_ context.Context, _ string) ([]string, error) {
		return []string{"frame:c1"}, nil
	})
	fl.failOn = 2 // succeed on target, fail on first child

	res, err := p.Retract(context.Background(), Request{
		EntityID: "frame:f1",
		Cascade:  true,
	})
	if err == nil {
		t.Fatal("expected log error")
	}
	if !strings.Contains(err.Error(), "synthetic log failure") {
		t.Errorf("err: %v", err)
	}
	// Result records the one entity that committed before the failure.
	if len(res.EntityIDs) != 1 || res.EntityIDs[0] != "frame:f1" {
		t.Errorf("partial result: %v", res.EntityIDs)
	}
}

// TestRetract_ResolverDedupesParentInChildList — defensive check: if
// a buggy graph query returns the parent itself in its own child
// list, the pipeline must not retract the target twice.
func TestRetract_ResolverDedupesParentInChildList(t *testing.T) {
	p, fl := newPipeline(t)
	p.Resolver = ChildResolverFunc(func(_ context.Context, parent string) ([]string, error) {
		// Pathological: include parent in child list.
		return []string{"frame:c1", parent, "frame:c2", ""}, nil
	})
	res, err := p.Retract(context.Background(), Request{
		EntityID: "frame:f1",
		Cascade:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Expect target + 2 unique children = 3 groups, no duplicates.
	if want := []string{"frame:f1", "frame:c1", "frame:c2"}; !equalStrings(res.EntityIDs, want) {
		t.Errorf("EntityIDs: got %v want %v", res.EntityIDs, want)
	}
	if len(fl.groups) != 3 {
		t.Errorf("groups: got %d want 3", len(fl.groups))
	}
}

// TestRetract_AppendOnlyPreservesPriorBytes is the core AC4 test:
// "No prior datom is mutated or deleted (verified by byte-comparing
// segment files before and after)". We use a real internal/log.Writer
// over a temp dir, snapshot every segment file's bytes after some
// initial appends, run a retraction, and verify the original prefix
// of the segment is byte-identical to the snapshot.
func TestRetract_AppendOnlyPreservesPriorBytes(t *testing.T) {
	dir := t.TempDir()
	w, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Pre-populate the segment with some unrelated datoms so the
	// retract path has prior content to preserve.
	for i := 0; i < 3; i++ {
		preGroup := []datom.Datom{{
			Tx:           ulidString(t),
			Ts:           "2026-04-10T00:00:00Z",
			Actor:        "system",
			Op:           datom.OpAdd,
			E:            "entry:pre",
			A:            "body",
			V:            json.RawMessage(`"hello"`),
			Src:          "test",
			InvocationID: "01HPTESTINVOCATION0000000000",
		}}
		if err := preGroup[0].Seal(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Append(preGroup); err != nil {
			t.Fatalf("pre-append %d: %v", i, err)
		}
	}

	// Snapshot every segment file's bytes BEFORE the retraction.
	segs := listSegmentFiles(t, dir)
	if len(segs) == 0 {
		t.Fatal("no segments after pre-append")
	}
	before := snapshotFiles(t, segs)

	// Drive the real Writer through the retract pipeline.
	p := &Pipeline{
		Log:          writerAdapter{w},
		Now:          func() time.Time { return time.Unix(0, 0).UTC() },
		Actor:        "alice",
		InvocationID: "01HPTESTINVOCATION0000000000",
	}
	if _, err := p.Retract(context.Background(), Request{
		EntityID: "entry:pre",
		Reason:   "test",
	}); err != nil {
		t.Fatalf("Retract: %v", err)
	}

	// AC4 invariant: every byte that was in the segment before the
	// retraction is still there, in the same position, after the
	// retraction. The retraction may APPEND new bytes (and probably
	// will), but the prefix must be byte-identical to the snapshot.
	for path, prior := range before {
		current := readFile(t, path)
		if len(current) < len(prior) {
			t.Errorf("segment %s shrank: was %d, now %d", path, len(prior), len(current))
			continue
		}
		if string(current[:len(prior)]) != string(prior) {
			t.Errorf("segment %s prior bytes mutated", path)
		}
	}

	// And the new tail must contain a retract datom for entry:pre.
	all, err := log.ReadAll(listSegmentFiles(t, dir))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	var sawRetract bool
	for _, d := range all {
		if d.Op == datom.OpRetract && d.E == "entry:pre" && d.A == AttrExists {
			sawRetract = true
		}
	}
	if !sawRetract {
		t.Errorf("no retract datom for entry:pre in final log")
	}
}

// writerAdapter bridges *log.Writer's Append signature to the package's
// LogAppender interface (the interface uses a named return for the tx
// string, but the underlying types match).
type writerAdapter struct{ w *log.Writer }

func (a writerAdapter) Append(group []datom.Datom) (string, error) {
	return a.w.Append(group)
}

// listSegmentFiles returns absolute paths to every *.jsonl file in dir.
func listSegmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func snapshotFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, p := range paths {
		out[p] = readFile(t, p)
	}
	return out
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// equalStrings compares two []string for deep equality.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ulidString returns a fresh ULID for test datom construction.
func ulidString(t *testing.T) string {
	t.Helper()
	return ulid.Make().String()
}
