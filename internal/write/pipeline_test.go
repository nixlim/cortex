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
	// body, kind, facet.artifact, facet.domain, facet.project,
	// encoding_at, base_activation = 7 datoms (no embedder wired).
	if len(g) != 7 {
		t.Fatalf("group size: got %d want 7", len(g))
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
	// Deterministic attribute order: body, kind, facet.* (sorted),
	// encoding_at, base_activation.
	wantAttrs := []string{"body", "kind", "facet.artifact", "facet.domain", "facet.project", "encoding_at", "base_activation"}
	for i, want := range wantAttrs {
		if g[i].A != want {
			t.Fatalf("attr %d: got %s want %s", i, g[i].A, want)
		}
	}
	// FR-031: base_activation is seeded to 1.0 on every observe.
	if string(g[6].V) != "1" {
		t.Fatalf("base_activation value: got %s want 1", string(g[6].V))
	}
}

// fakeEmbedder is a deterministic Embedder double that captures Embed
// invocations and returns canned model metadata. It exists so the
// FR-051 digest-capture path can be exercised without an Ollama
// process. The recorded callCount lets tests assert Embed is called at
// most once per observe.
type fakeEmbedder struct {
	vec       []float32
	model     string
	digest    string
	embedErr  error
	digestErr error
	embeds    int
	digests   int
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	f.embeds++
	if f.embedErr != nil {
		return nil, f.embedErr
	}
	return f.vec, nil
}

func (f *fakeEmbedder) ModelDigest(_ context.Context) (string, string, error) {
	f.digests++
	if f.digestErr != nil {
		return "", "", f.digestErr
	}
	return f.model, f.digest, nil
}

// TestObserve_EmbedderEmitsModelNameAndDigestDatoms covers FR-051 /
// SC-002: when an embedder is wired, every observe entry records the
// embedding model name and digest as their own attribute datoms. The
// rebuild pipeline relies on this to detect MODEL_DIGEST_RACE.
func TestObserve_EmbedderEmitsModelNameAndDigestDatoms(t *testing.T) {
	p, log := newTestPipeline(t)
	p.Embedder = &fakeEmbedder{
		vec:    []float32{0.1, 0.2, 0.3},
		model:  "nomic-embed-text",
		digest: "sha256:deadbeef",
	}

	_, err := p.Observe(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(log.groups) != 1 {
		t.Fatalf("groups: %d", len(log.groups))
	}
	g := log.groups[0]
	// 5 base attrs + encoding_at + base_activation + embedding_model_name + embedding_model_digest = 9.
	if len(g) != 9 {
		t.Fatalf("group size: got %d want 9", len(g))
	}
	var name, digest string
	for _, d := range g {
		switch d.A {
		case "embedding_model_name":
			name = string(d.V)
		case "embedding_model_digest":
			digest = string(d.V)
		}
	}
	if name != `"nomic-embed-text"` {
		t.Errorf("embedding_model_name: got %s", name)
	}
	if digest != `"sha256:deadbeef"` {
		t.Errorf("embedding_model_digest: got %s", digest)
	}
}

// TestObserve_EmbedderMissingDigestIsOperationalError ensures the
// pipeline refuses to commit if the embedder cannot produce a digest.
// This is the FR-051 invariant: a vector without a digest is forbidden
// because it would silently bypass rebuild's pinned-model check.
func TestObserve_EmbedderMissingDigestIsOperationalError(t *testing.T) {
	p, log := newTestPipeline(t)
	p.Embedder = &fakeEmbedder{
		vec:    []float32{1, 2, 3},
		model:  "nomic-embed-text",
		digest: "", // simulates a buggy adapter
	}
	_, err := p.Observe(context.Background(), validRequest())
	var e *errs.Error
	if !errors.As(err, &e) || e.Code != "EMBEDDING_MODEL_UNAVAILABLE" {
		t.Fatalf("want EMBEDDING_MODEL_UNAVAILABLE, got %v", err)
	}
	if e.Kind != errs.KindOperational {
		t.Fatalf("kind: got %v want Operational", e.Kind)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on missing-digest reject: %d", len(log.groups))
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
	if !errors.As(err, &e) || e.Code != "BACKEND_APPLY_PARTIAL" {
		t.Fatalf("want BACKEND_APPLY_PARTIAL, got %v", err)
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
