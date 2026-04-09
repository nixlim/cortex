package write

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/security/secrets"
)

// fakeLog is an in-memory LogAppender that records the datom groups
// it received. Tests assert on the recorded history to prove the
// validation paths never append anything.
type fakeLog struct {
	groups  [][]datom.Datom
	failErr error
}

func (f *fakeLog) Append(group []datom.Datom) (string, error) {
	if f.failErr != nil {
		return "", f.failErr
	}
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	return group[0].Tx, nil
}

// fakeBackend records every datom applied. It never fails so success
// paths can count the apply fan-out.
type fakeBackend struct {
	name string
	seen []datom.Datom
}

func (f *fakeBackend) Name() string { return f.name }
func (f *fakeBackend) Apply(_ context.Context, d datom.Datom) error {
	f.seen = append(f.seen, d)
	return nil
}

// newTestPipeline builds a Pipeline wired with a fake log and a real
// built-in secret detector. Tests that need a Registry, Embedder, or
// backend applier construct them locally.
func newTestPipeline(t *testing.T) (*Pipeline, *fakeLog) {
	t.Helper()
	det, err := secrets.LoadBuiltin(0)
	if err != nil {
		t.Fatalf("LoadBuiltin: %v", err)
	}
	log := &fakeLog{}
	p := &Pipeline{
		Detector:     det,
		Log:          log,
		Actor:        "test",
		InvocationID: "01HPTESTINVOCATION0000000000",
		Now:          func() time.Time { return time.Unix(0, 0).UTC() },
	}
	return p, log
}

// validRequest builds a minimal valid ObserveRequest used as the base
// of every happy-path test.
func validRequest() ObserveRequest {
	return ObserveRequest{
		Body: "API has TOCTOU race",
		Kind: "ObservedRace",
		Facets: map[string]string{
			"domain":   "Security",
			"artifact": "Service",
			"project":  "pay-gw",
		},
	}
}

// TestObserve_HappyPathWritesSealedGroup covers AC1: a valid invocation
// succeeds, returns a populated EntryID, and the log saw one sealed
// datom group carrying every required facet.
func TestObserve_HappyPathWritesSealedGroup(t *testing.T) {
	p, log := newTestPipeline(t)
	res, err := p.Observe(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if res == nil || res.EntryID == "" {
		t.Fatalf("missing EntryID: %+v", res)
	}
	if !strings.HasPrefix(res.EntryID, "entry:") {
		t.Fatalf("EntryID prefix: got %s want entry:<ulid>", res.EntryID)
	}
	if res.Tx == "" {
		t.Fatalf("missing Tx")
	}
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
	g := log.groups[0]
	// body, kind, facet.artifact, facet.domain, facet.project = 5 datoms.
	if len(g) != 5 {
		t.Fatalf("group size: got %d want 5", len(g))
	}
	// Every datom is sealed and shares the same tx.
	for i, d := range g {
		if d.Tx != res.Tx {
			t.Fatalf("datom %d tx: got %s want %s", i, d.Tx, res.Tx)
		}
		if d.Checksum == "" {
			t.Fatalf("datom %d not sealed", i)
		}
		if d.E != res.EntryID {
			t.Fatalf("datom %d entry: got %s want %s", i, d.E, res.EntryID)
		}
		if d.Src != "observe" {
			t.Fatalf("datom %d src: got %s", i, d.Src)
		}
		if err := d.Verify(); err != nil {
			t.Fatalf("datom %d verify: %v", i, err)
		}
	}
	// Deterministic attribute order: body, kind, facet.* (sorted).
	wantAttrs := []string{"body", "kind", "facet.artifact", "facet.domain", "facet.project"}
	for i, want := range wantAttrs {
		if g[i].A != want {
			t.Fatalf("attr %d: got %s want %s", i, g[i].A, want)
		}
	}
}

// TestObserve_RejectsReflectionOnlyKind covers AC2: a BugPattern kind
// is reflection-only and must be rejected with the exact error code,
// and nothing may be written to the log.
func TestObserve_RejectsReflectionOnlyKind(t *testing.T) {
	p, log := newTestPipeline(t)
	req := validRequest()
	req.Body = "body"
	req.Kind = "BugPattern"

	_, err := p.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *errs.Error, got %T: %v", err, err)
	}
	if e.Code != "REFLECTION_ONLY_KIND" {
		t.Fatalf("code: got %s want REFLECTION_ONLY_KIND", e.Code)
	}
	if e.Kind != errs.KindValidation {
		t.Fatalf("kind: got %v want Validation", e.Kind)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on rejected kind: %d groups", len(log.groups))
	}
}

// TestObserve_MissingKindRejectedWithoutWrite covers AC3: an empty
// --kind produces MISSING_KIND and writes zero datoms.
func TestObserve_MissingKindRejectedWithoutWrite(t *testing.T) {
	p, log := newTestPipeline(t)
	req := validRequest()
	req.Kind = ""

	_, err := p.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "MISSING_KIND" {
		t.Fatalf("want MISSING_KIND, got %v", err)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on missing kind: %d groups", len(log.groups))
	}
}

// TestObserve_SecretInBodyRejectedWithoutWrite covers AC4: a body
// containing the canonical AWS fixture key must be rejected with
// SECRET_DETECTED and no datoms may be written. The detector returns
// only the rule name so the envelope carries no secret payload.
func TestObserve_SecretInBodyRejectedWithoutWrite(t *testing.T) {
	p, log := newTestPipeline(t)
	req := validRequest()
	req.Body = "please do not leak this: AKIAIOSFODNN7EXAMPLE"

	_, err := p.Observe(context.Background(), req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "SECRET_DETECTED" {
		t.Fatalf("want SECRET_DETECTED, got %v", err)
	}
	// Details carries the rule name, not the matched substring.
	if rule, _ := e.Details["rule"].(string); rule == "" {
		t.Fatalf("expected rule name in details, got %v", e.Details)
	}
	// And the envelope message does not contain the fixture key —
	// the matched substring must never be echoed.
	if strings.Contains(e.Message, "AKIA") {
		t.Fatalf("secret substring leaked into error message: %q", e.Message)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on secret-rejected write: %d groups", len(log.groups))
	}
}

// TestObserve_MissingFacetRejected asserts that both required facets
// (domain, project) are enforced.
func TestObserve_MissingFacetRejected(t *testing.T) {
	for _, missing := range []string{"domain", "project"} {
		t.Run(missing, func(t *testing.T) {
			p, log := newTestPipeline(t)
			req := validRequest()
			delete(req.Facets, missing)
			_, err := p.Observe(context.Background(), req)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var e *errs.Error
			if !errors.As(err, &e) || e.Code != "MISSING_FACET" {
				t.Fatalf("want MISSING_FACET, got %v", err)
			}
			if got, _ := e.Details["facet"].(string); got != missing {
				t.Fatalf("facet detail: got %v want %s", e.Details["facet"], missing)
			}
			if len(log.groups) != 0 {
				t.Fatalf("log touched: %d", len(log.groups))
			}
		})
	}
}

// TestObserve_EmptyBodyRejected covers EMPTY_BODY: whitespace-only
// bodies are not sufficient.
func TestObserve_EmptyBodyRejected(t *testing.T) {
	p, log := newTestPipeline(t)
	req := validRequest()
	req.Body = "   "
	_, err := p.Observe(context.Background(), req)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "EMPTY_BODY" {
		t.Fatalf("want EMPTY_BODY, got %v", err)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on empty body")
	}
}

// TestObserve_UnknownKindRejected covers UNKNOWN_KIND: a kind that is
// neither agent-writable nor reflection-only.
func TestObserve_UnknownKindRejected(t *testing.T) {
	p, _ := newTestPipeline(t)
	req := validRequest()
	req.Kind = "Gobbledygook"
	_, err := p.Observe(context.Background(), req)
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "UNKNOWN_KIND" {
		t.Fatalf("want UNKNOWN_KIND, got %v", err)
	}
}

// TestObserve_SubjectAttachesCanonicalDatom exercises the subject
// resolution path: a well-formed PSI gets canonicalized and ends up
// as a "subject" attribute inside the datom group.
func TestObserve_SubjectAttachesCanonicalDatom(t *testing.T) {
	p, log := newTestPipeline(t)
	req := validRequest()
	req.Subject = "lib/go-redis"
	_, err := p.Observe(context.Background(), req)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(log.groups) != 1 {
		t.Fatalf("groups: %d", len(log.groups))
	}
	var found bool
	for _, d := range log.groups[0] {
		if d.A == "subject" {
			found = true
			if !strings.Contains(string(d.V), "lib/go-redis") {
				t.Fatalf("subject datom value: %s", string(d.V))
			}
		}
	}
	if !found {
		t.Fatalf("no subject datom in group")
	}
}

// TestObserve_BackendsReceiveEveryDatom verifies that when Neo4j and
// Weaviate appliers are wired, each one sees every datom in the
// committed group exactly once and in emission order.
func TestObserve_BackendsReceiveEveryDatom(t *testing.T) {
	p, log := newTestPipeline(t)
	neo := &fakeBackend{name: "neo4j"}
	weav := &fakeBackend{name: "weaviate"}
	p.Neo4j = neo
	p.Weaviate = weav

	_, err := p.Observe(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	g := log.groups[0]
	if len(neo.seen) != len(g) {
		t.Fatalf("neo seen: got %d want %d", len(neo.seen), len(g))
	}
	if len(weav.seen) != len(g) {
		t.Fatalf("weav seen: got %d want %d", len(weav.seen), len(g))
	}
	for i := range g {
		if neo.seen[i].A != g[i].A {
			t.Fatalf("neo order mismatch at %d", i)
		}
		if weav.seen[i].A != g[i].A {
			t.Fatalf("weav order mismatch at %d", i)
		}
	}
}

// TestObserve_BackendFailureDoesNotRollbackLog covers the spec
// constraint "Backend apply failures do not roll back the committed
// log": the log Append has already succeeded, and the pipeline must
// return an operational error but still surface the committed
// EntryID/Tx so the caller can log it.
func TestObserve_BackendFailureDoesNotRollbackLog(t *testing.T) {
	p, log := newTestPipeline(t)
	boom := errors.New("bolt: connection refused")
	p.Neo4j = &failingBackend{name: "neo4j", err: boom}

	res, err := p.Observe(context.Background(), validRequest())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "NEO4J_APPLY_FAILED" {
		t.Fatalf("want NEO4J_APPLY_FAILED, got %v", err)
	}
	if e.Kind != errs.KindOperational {
		t.Fatalf("kind: got %v want Operational", e.Kind)
	}
	if res == nil || res.EntryID == "" {
		t.Fatalf("result missing despite committed log: %+v", res)
	}
	// Log still recorded the group — no rollback.
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
}

type failingBackend struct {
	name string
	err  error
}

func (f *failingBackend) Name() string                           { return f.name }
func (f *failingBackend) Apply(context.Context, datom.Datom) error { return f.err }
