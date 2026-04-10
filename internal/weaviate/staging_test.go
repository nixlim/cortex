package weaviate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stagingFake is a tiny in-memory Weaviate HTTP double focused on the
// surface that staging.go touches: schema create, schema delete,
// object POST, and /v1/graphql Get. It is deliberately separate from
// fakeWeaviate in client_test.go because the staging tests want to
// assert on class-level state (created, deleted, objects routed to
// the correct class) rather than on upsert wire bodies.
type stagingFake struct {
	server *httptest.Server

	mu      sync.Mutex
	classes map[string]bool                     // class → exists
	objects map[string]map[string]stagingObject // class → id → object
}

type stagingObject struct {
	ID         string
	Vector     []float32
	Properties map[string]any
}

func newStagingFake() *stagingFake {
	f := &stagingFake{
		classes: map[string]bool{},
		objects: map[string]map[string]stagingObject{},
	}
	mux := http.NewServeMux()

	// Schema endpoints. POST /v1/schema creates a class; DELETE
	// /v1/schema/{class} drops it.
	mux.HandleFunc("/v1/schema", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var def classDefinition
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &def); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.classes[def.Class] {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":[{"message":"class already exists"}]}`)
			return
		}
		f.classes[def.Class] = true
		f.objects[def.Class] = map[string]stagingObject{}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/v1/schema/", func(w http.ResponseWriter, r *http.Request) {
		class := strings.TrimPrefix(r.URL.Path, "/v1/schema/")
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.classes[class] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.classes, class)
		delete(f.objects, class)
		w.WriteHeader(http.StatusOK)
	})

	// POST /v1/objects creates an object.
	mux.HandleFunc("/v1/objects", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var in struct {
			Class      string         `json:"class"`
			ID         string         `json:"id"`
			Vector     []float32      `json:"vector"`
			Properties map[string]any `json:"properties"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.classes[in.Class] {
			http.Error(w, "class does not exist", http.StatusUnprocessableEntity)
			return
		}
		if _, exists := f.objects[in.Class][in.ID]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":[{"message":"id already exists"}]}`)
			return
		}
		f.objects[in.Class][in.ID] = stagingObject{
			ID: in.ID, Vector: in.Vector, Properties: in.Properties,
		}
		w.WriteHeader(http.StatusOK)
	})

	// PATCH /v1/objects/{class}/{id} merges.
	mux.HandleFunc("/v1/objects/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/objects/"), "/")
		if len(parts) != 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		class, id := parts[0], parts[1]
		var in struct {
			Class      string         `json:"class"`
			ID         string         `json:"id"`
			Vector     []float32      `json:"vector"`
			Properties map[string]any `json:"properties"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		obj := f.objects[class][id]
		obj.ID = id
		if len(in.Vector) > 0 {
			obj.Vector = in.Vector
		}
		if obj.Properties == nil {
			obj.Properties = map[string]any{}
		}
		for k, v := range in.Properties {
			obj.Properties[k] = v
		}
		f.objects[class][id] = obj
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /v1/graphql serves Get queries for listObjectsWithVectors.
	mux.HandleFunc("/v1/graphql", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		_ = json.Unmarshal(body, &req)

		// Determine which class the query is asking for.
		f.mu.Lock()
		defer f.mu.Unlock()
		class := ""
		for name := range f.classes {
			if strings.Contains(req.Query, name+"(") {
				class = name
				break
			}
		}
		rows := make([]map[string]any, 0, len(f.objects[class]))
		for _, obj := range f.objects[class] {
			row := map[string]any{}
			for k, v := range obj.Properties {
				row[k] = v
			}
			vecAny := make([]any, len(obj.Vector))
			for i, v := range obj.Vector {
				vecAny[i] = float64(v)
			}
			row["_additional"] = map[string]any{
				"id":     obj.ID,
				"vector": vecAny,
			}
			rows = append(rows, row)
		}
		env := map[string]any{
			"data": map[string]any{
				"Get": map[string]any{class: rows},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(env)
	})

	f.server = httptest.NewServer(mux)
	return f
}

func (f *stagingFake) Close() { f.server.Close() }

func (f *stagingFake) classExists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.classes[name]
}

func (f *stagingFake) objectCount(class string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects[class])
}

// TestEnsureStagingSchemaIsIdempotent covers the Create path of a
// rebuild: the first call creates the two staging classes, the second
// is a no-op, and neither touches the live classes.
func TestEnsureStagingSchemaIsIdempotent(t *testing.T) {
	f := newStagingFake()
	defer f.Close()
	c := NewHTTPClient(f.server.URL, 0)

	if err := c.EnsureStagingSchema(context.Background()); err != nil {
		t.Fatalf("EnsureStagingSchema first: %v", err)
	}
	if err := c.EnsureStagingSchema(context.Background()); err != nil {
		t.Fatalf("EnsureStagingSchema second: %v", err)
	}
	if !f.classExists(ClassEntryStaging) || !f.classExists(ClassFrameStaging) {
		t.Errorf("staging classes not created: %+v", f.classes)
	}
	if f.classExists(ClassEntry) || f.classExists(ClassFrame) {
		t.Errorf("live classes must not be touched by staging ensure")
	}
}

// TestUpsertStagingRoutesToStagingClass verifies UpsertStaging lands
// the object in the staging counterpart of the supplied live class
// and preserves the vector + cortex_id property.
func TestUpsertStagingRoutesToStagingClass(t *testing.T) {
	f := newStagingFake()
	defer f.Close()
	c := NewHTTPClient(f.server.URL, 0)

	if err := c.EnsureStagingSchema(context.Background()); err != nil {
		t.Fatalf("EnsureStagingSchema: %v", err)
	}
	err := c.UpsertStaging(context.Background(), ClassEntry, "entry:01ABCTEST",
		[]float32{0.1, 0.2, 0.3}, map[string]any{"project": "pay-gw"})
	if err != nil {
		t.Fatalf("UpsertStaging: %v", err)
	}
	if got := f.objectCount(ClassEntryStaging); got != 1 {
		t.Fatalf("staging objects = %d, want 1", got)
	}
	if got := f.objectCount(ClassEntry); got != 0 {
		t.Errorf("live Entry objects = %d, want 0", got)
	}
}

// TestSwapStagingToLivePromotesEverything exercises the full swap:
// EnsureStagingSchema, a couple of UpsertStaging calls, then
// SwapStagingToLive. Afterwards the live classes exist, the objects
// have been copied, and the staging classes are gone.
func TestSwapStagingToLivePromotesEverything(t *testing.T) {
	f := newStagingFake()
	defer f.Close()
	c := NewHTTPClient(f.server.URL, 0)

	ctx := context.Background()
	if err := c.EnsureStagingSchema(ctx); err != nil {
		t.Fatalf("EnsureStagingSchema: %v", err)
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("entry:01ABC%02d", i)
		err := c.UpsertStaging(ctx, ClassEntry, id,
			[]float32{float32(i), float32(i + 1)},
			map[string]any{"cortex_id": id, "project": "pay-gw", "content": "body"})
		if err != nil {
			t.Fatalf("UpsertStaging %d: %v", i, err)
		}
	}
	if err := c.SwapStagingToLive(ctx); err != nil {
		t.Fatalf("SwapStagingToLive: %v", err)
	}
	if got := f.objectCount(ClassEntry); got != 3 {
		t.Errorf("live Entry count = %d, want 3", got)
	}
	if f.classExists(ClassEntryStaging) || f.classExists(ClassFrameStaging) {
		t.Errorf("staging classes should be dropped after swap")
	}
	if !f.classExists(ClassEntry) || !f.classExists(ClassFrame) {
		t.Errorf("live classes should be recreated by swap")
	}
}

// TestCleanupStagingDropsStagingButNotLive ensures a failure-path
// cleanup leaves live alone.
func TestCleanupStagingDropsStagingButNotLive(t *testing.T) {
	f := newStagingFake()
	defer f.Close()
	c := NewHTTPClient(f.server.URL, 0)

	ctx := context.Background()
	// Pre-seed: live classes exist, staging classes exist.
	if err := c.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if err := c.EnsureStagingSchema(ctx); err != nil {
		t.Fatalf("EnsureStagingSchema: %v", err)
	}

	if err := c.CleanupStaging(ctx); err != nil {
		t.Fatalf("CleanupStaging: %v", err)
	}
	if f.classExists(ClassEntryStaging) || f.classExists(ClassFrameStaging) {
		t.Errorf("staging still present after Cleanup")
	}
	if !f.classExists(ClassEntry) || !f.classExists(ClassFrame) {
		t.Errorf("live class dropped by CleanupStaging — should be untouched")
	}
}

// TestCleanupStagingOnMissingClassIsNoop covers the idempotency
// contract: calling CleanupStaging when the staging classes have
// already been dropped must not return an error.
func TestCleanupStagingOnMissingClassIsNoop(t *testing.T) {
	f := newStagingFake()
	defer f.Close()
	c := NewHTTPClient(f.server.URL, 0)

	if err := c.CleanupStaging(context.Background()); err != nil {
		t.Fatalf("CleanupStaging on empty weaviate returned %v, want nil", err)
	}
}
