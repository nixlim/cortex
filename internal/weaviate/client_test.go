package weaviate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWeaviate bundles an httptest.Server and the handler state the
// individual tests want to assert against (request counts, recorded
// bodies, programmed responses). Each test constructs its own so
// tests stay independent and parallelizable.
type fakeWeaviate struct {
	server *httptest.Server

	readyStatus  int32 // atomic, so tests can flip it mid-flight
	schemaCalls  int32
	schemaErrors int // how many of the first N calls return 422/already-exists

	upsertBodies [][]byte

	// graphqlResults is the fixed hit list returned from every
	// /v1/graphql POST. Tests set this before issuing the query.
	graphqlResults []map[string]any
}

func newFakeWeaviate() *fakeWeaviate {
	f := &fakeWeaviate{readyStatus: http.StatusOK}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/.well-known/ready", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&f.readyStatus)))
	})

	mux.HandleFunc("/v1/schema", func(w http.ResponseWriter, r *http.Request) {
		n := int(atomic.AddInt32(&f.schemaCalls, 1))
		if n <= f.schemaErrors {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":[{"message":"class name Entry already exists"}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/v1/objects/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.upsertBodies = append(f.upsertBodies, body)
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/v1/graphql", func(w http.ResponseWriter, r *http.Request) {
		// Extract the class name from the query so the response
		// envelope nests under the right key. The tests only use
		// ClassEntry / ClassFrame, which are both valid identifiers.
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		_ = json.Unmarshal(body, &req)
		class := ClassEntry
		if strings.Contains(req.Query, ClassFrame) {
			class = ClassFrame
		}
		env := map[string]any{
			"data": map[string]any{
				"Get": map[string]any{
					class: f.graphqlResults,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
	})

	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeWeaviate) Close() { f.server.Close() }

// newTestClient builds an HTTPClient targeting a fake server with a
// tight default timeout so a hung test fails loudly instead of hanging
// the `go test` process.
func newTestClient(f *fakeWeaviate) *HTTPClient {
	return NewHTTPClient(f.server.URL, 2*time.Second)
}

func TestHealth_OK(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()

	c := newTestClient(f)
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health returned error against ready server: %v", err)
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready returned error against ready server: %v", err)
	}
}

func TestHealth_StoppedService(t *testing.T) {
	f := newFakeWeaviate()
	// Shut down immediately so the TCP connect fails. This is the
	// closest unit-level analog to "Weaviate is stopped" — the
	// acceptance criterion asks for a non-nil error in that case.
	addr := f.server.URL
	f.Close()

	c := NewHTTPClient(addr, 500*time.Millisecond)
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("Health returned nil against stopped server, expected error")
	}
}

func TestHealth_Non200(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()
	atomic.StoreInt32(&f.readyStatus, http.StatusServiceUnavailable)

	c := newTestClient(f)
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("Health returned nil on 503, expected error")
	}
}

// TestEnsureSchema_Idempotent covers the acceptance criterion
// "Creating the Entry and Frame collections is idempotent (calling
// twice does not error)". The fake server is configured to return
// "already exists" on the first two class creations so the second
// EnsureSchema call — which hits both classes again — would fail if
// the adapter did not translate 422/"already exists" into a swallowed
// ErrAlreadyExists.
func TestEnsureSchema_Idempotent(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()
	// First two POSTs succeed (creating Entry and Frame). Next two
	// POSTs return 422 already-exists. A total of 4 POSTs is made by
	// two EnsureSchema calls (2 classes each).
	f.schemaErrors = 0 // first pair succeeds

	c := newTestClient(f)
	if err := c.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema first call: %v", err)
	}
	// Now program the fake to return already-exists for every
	// subsequent call. The handler's counter is monotonically
	// increasing; we reset schemaErrors to a large sentinel.
	f.schemaErrors = 1000
	if err := c.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema second call (should swallow already-exists): %v", err)
	}
	if got := atomic.LoadInt32(&f.schemaCalls); got != 4 {
		t.Fatalf("expected 4 schema POSTs across two EnsureSchema calls, got %d", got)
	}
}

func TestEnsureSchema_RealErrorSurfaces(t *testing.T) {
	// A 422 whose body does NOT contain "already exists" must be
	// surfaced as a real error, not swallowed. Otherwise a genuinely
	// malformed schema definition would be silently discarded.
	f := newFakeWeaviate()
	defer f.Close()

	// Swap the schema handler for one that returns a non-idempotent
	// 422 with a different error message.
	f.server.Config.Handler.(*http.ServeMux).HandleFunc("/v1/schema/force422", func(w http.ResponseWriter, r *http.Request) {})
	// We'll instead test via createClass directly against a bespoke
	// httptest server.

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"error":[{"message":"invalid property datatype 'wat'"}]}`)
	}))
	defer badSrv.Close()
	c := NewHTTPClient(badSrv.URL, time.Second)
	err := c.EnsureSchema(context.Background())
	if err == nil {
		t.Fatal("expected non-idempotent 422 to be surfaced as an error")
	}
	if strings.Contains(err.Error(), "already exists") {
		t.Fatalf("error should not be tagged already-exists: %v", err)
	}
}

func TestUpsert_SendsVectorAndProperties(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()

	c := newTestClient(f)
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i) / 384.0
	}
	props := map[string]any{
		"cortex_id": "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"project":   "demo",
		"content":   "hello world",
	}
	if err := c.Upsert(context.Background(), ClassEntry, "b9a2c3d4-5e6f-7890-abcd-ef0123456789", vec, props); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(f.upsertBodies) != 1 {
		t.Fatalf("expected 1 upsert body recorded, got %d", len(f.upsertBodies))
	}
	var decoded struct {
		Class      string         `json:"class"`
		ID         string         `json:"id"`
		Vector     []float32      `json:"vector"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(f.upsertBodies[0], &decoded); err != nil {
		t.Fatalf("decode recorded body: %v", err)
	}
	if decoded.Class != ClassEntry {
		t.Errorf("class = %q, want %q", decoded.Class, ClassEntry)
	}
	if len(decoded.Vector) != 384 {
		t.Errorf("vector length = %d, want 384", len(decoded.Vector))
	}
	if decoded.Properties["project"] != "demo" {
		t.Errorf("properties.project = %v, want demo", decoded.Properties["project"])
	}
}

// TestNearestNeighbors_HonorsCosineFloor covers the acceptance
// criterion "NearestNeighbors honors a cosine floor by excluding
// results below the floor". The fake server returns two hits, one
// with distance 0.10 (cos sim 0.90) and one with distance 0.50
// (cos sim 0.50). A floor of 0.75 must drop the second.
func TestNearestNeighbors_HonorsCosineFloor(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()
	f.graphqlResults = []map[string]any{
		{
			"cortex_id": "near",
			"_additional": map[string]any{
				"id":       "00000000-0000-0000-0000-000000000001",
				"distance": 0.10,
			},
		},
		{
			"cortex_id": "far",
			"_additional": map[string]any{
				"id":       "00000000-0000-0000-0000-000000000002",
				"distance": 0.50,
			},
		},
	}

	c := newTestClient(f)
	vec := []float32{0.1, 0.2, 0.3}
	hits, err := c.NearestNeighbors(context.Background(), ClassEntry, vec, 5, 0.75)
	if err != nil {
		t.Fatalf("NearestNeighbors: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit above cosine floor 0.75, got %d", len(hits))
	}
	if hits[0].Properties["cortex_id"] != "near" {
		t.Errorf("top hit cortex_id = %v, want near", hits[0].Properties["cortex_id"])
	}
	if hits[0].CosineSimilarity < 0.89 || hits[0].CosineSimilarity > 0.91 {
		t.Errorf("cosine similarity = %v, want ~0.90", hits[0].CosineSimilarity)
	}
}

// TestNearestNeighbors_TopHitOnIdenticalVector is the unit-level
// analog to the integration acceptance criterion "Upsert with a
// 384-dim vector succeeds and a subsequent NearestNeighbors(k=1) on
// the same vector returns that object as the top result". The fake
// server returns the upserted object's id as the single top hit. The
// real end-to-end check against Weaviate 1.36.9 lives in the
// integration-tagged test.
func TestNearestNeighbors_TopHitOnIdenticalVector(t *testing.T) {
	f := newFakeWeaviate()
	defer f.Close()
	f.graphqlResults = []map[string]any{
		{
			"cortex_id": "self",
			"_additional": map[string]any{
				"id":       "00000000-0000-0000-0000-00000000aaaa",
				"distance": 0.0,
			},
		},
	}

	c := newTestClient(f)
	vec := make([]float32, 384)
	hits, err := c.NearestNeighbors(context.Background(), ClassEntry, vec, 1, 0)
	if err != nil {
		t.Fatalf("NearestNeighbors: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	if hits[0].ID != "00000000-0000-0000-0000-00000000aaaa" {
		t.Errorf("top hit ID = %q", hits[0].ID)
	}
	if hits[0].CosineSimilarity != 1.0 {
		t.Errorf("cosine similarity = %v, want 1.0", hits[0].CosineSimilarity)
	}
}

func TestIsSafeClassName(t *testing.T) {
	cases := map[string]bool{
		"Entry":       true,
		"Frame":       true,
		"My_Class1":   true,
		"":            false,
		"1Entry":      false,
		"Entry Frame": false,
		"Entry;DROP":  false,
		"entry":       false, // must start uppercase per GraphQL type rules
	}
	for name, want := range cases {
		if got := isSafeClassName(name); got != want {
			t.Errorf("isSafeClassName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := map[string]string{
		"localhost:9397":          "http://localhost:9397",
		"http://localhost:9397":   "http://localhost:9397",
		"http://localhost:9397/":  "http://localhost:9397",
		"https://foo.bar:9397":    "https://foo.bar:9397",
		"http://foo.bar:9397/v1/": "http://foo.bar:9397/v1",
	}
	for in, want := range cases {
		if got := normalizeBaseURL(in); got != want {
			t.Errorf("normalizeBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}
