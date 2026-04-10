// Package weaviate is the Cortex adapter for the managed Weaviate service.
//
// The adapter is deliberately thin: it hides Weaviate's REST wire format
// behind a small Go interface (Client) so call sites in the write and
// recall pipelines don't need to know that collections are called
// "classes", that vector search goes through GraphQL, or that the
// idempotent schema install path requires reading a 422 response body
// for an "already exists" marker.
//
// The package uses stdlib net/http rather than the official
// weaviate-go-client. Phase 1 does not need the client's batch helpers
// or generated GraphQL types, and avoiding a large transitive dep tree
// matches the project constraint "no third-party dependencies beyond
// stdlib plus selected CLI/database drivers".
//
// Spec references:
//   docs/spec/cortex-spec.md §"Weaviate"
//   docs/spec/cortex-spec.md §"cortex up Readiness Contract" (Weaviate probe)
//   docs/spec/cortex-spec.md §"Configuration Defaults" (endpoints, timeouts)
package weaviate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ClassEntry is the Weaviate collection name for episodic observations.
// Stored objects carry the entry's canonical ULID in the `cortex_id`
// property and a caller-supplied embedding vector.
const ClassEntry = "Entry"

// ClassFrame is the Weaviate collection name for frame instances.
const ClassFrame = "Frame"

// DefaultTimeout is the fallback request timeout used when the caller
// passes a zero duration to NewClient. Vector operations honor
// timeouts.embedding_seconds from config; non-vector probes use this.
const DefaultTimeout = 10 * time.Second

// Client is the interface exposed to callers. Phase 1 uses a single
// concrete implementation (*HTTPClient) but the interface exists so
// higher layers can swap in a fake for unit tests without depending on
// this package's HTTP machinery.
type Client interface {
	// Health reports whether Weaviate is reachable and ready to serve.
	// Implementations MUST hit /v1/.well-known/ready and return nil on
	// HTTP 200, a non-nil error otherwise.
	Health(ctx context.Context) error

	// Ready is an alias for Health retained for symmetry with the
	// interface listed in cortex-4kq.13's task spec. Callers that want
	// to distinguish "live but not ready" may probe /v1/.well-known/live
	// separately; Phase 1 treats them as equivalent.
	Ready(ctx context.Context) error

	// EnsureSchema creates the Entry and Frame collections if they do
	// not already exist. It MUST be idempotent: calling it twice in a
	// row must not return an error.
	EnsureSchema(ctx context.Context) error

	// Upsert stores or replaces an object in the given class using the
	// object's canonical id as the Weaviate object UUID derivation seed.
	// The vector's length is not validated here — the caller (the write
	// pipeline) is responsible for matching the configured embedding
	// model dimension.
	Upsert(ctx context.Context, class, id string, vector []float32, properties map[string]any) error

	// NearestNeighbors returns up to k objects from the class whose
	// cosine similarity to the query vector is >= cosineFloor. A
	// cosineFloor of 0 disables the floor. Results are ordered by
	// descending similarity (i.e., ascending Weaviate distance).
	NearestNeighbors(ctx context.Context, class string, vector []float32, k int, cosineFloor float64) ([]SearchResult, error)

	// FetchVectorsByCortexIDs returns the stored embedding vector for
	// every object in the class whose cortex_id property is in the
	// supplied set. Missing ids are absent from the returned map (no
	// error). The recall pipeline uses this to populate
	// EntryState.Embedding so the cosine rerank scores against real
	// vectors instead of the all-zero fallback.
	FetchVectorsByCortexIDs(ctx context.Context, class string, cortexIDs []string) (map[string][]float32, error)
}

// SearchResult is a single hit returned by NearestNeighbors.
type SearchResult struct {
	// ID is the Weaviate object UUID (not the Cortex entry ULID — those
	// are carried in Properties["cortex_id"]).
	ID string
	// CosineSimilarity is the cosine similarity in [-1, 1] recovered
	// from Weaviate's reported distance via cos = 1 - distance.
	CosineSimilarity float64
	// Properties carries the object payload returned by Weaviate.
	Properties map[string]any
}

// HTTPClient is the direct-HTTP implementation of Client.
type HTTPClient struct {
	baseURL string
	http    *http.Client

	// timeout is applied when the context passed to a method has no
	// deadline of its own. Callers that want per-operation budgets (per
	// spec, timeouts.embedding_seconds for vector ops) should pass a
	// context with a deadline and this field will be ignored.
	timeout time.Duration
}

// NewHTTPClient constructs a client targeting a Weaviate instance at
// httpEndpoint. Accepted forms:
//
//	localhost:9397
//	http://localhost:9397
//	https://weaviate.internal:9397
//
// A scheme-less endpoint defaults to http:// (Phase 1 services bind
// loopback-only, so TLS is not required).
//
// timeout is the default request budget used when the caller's context
// has no deadline. Pass 0 for DefaultTimeout.
func NewHTTPClient(httpEndpoint string, timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	base := normalizeBaseURL(httpEndpoint)
	return &HTTPClient{
		baseURL: base,
		http: &http.Client{
			// We deliberately don't set an http.Client Timeout here: it
			// would override per-request deadlines and defeat the
			// caller's context-budget control. Use context deadlines
			// instead.
		},
		timeout: timeout,
	}
}

// normalizeBaseURL trims trailing slashes and prepends http:// if the
// caller passed a bare host:port.
func normalizeBaseURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}
	return endpoint
}

// ctxWithDefault attaches c.timeout as a deadline if ctx has none.
// Returns the possibly-new context and a cancel func that is always
// safe to call, even if no new timer was started.
func (c *HTTPClient) ctxWithDefault(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.timeout)
}

// Health probes /v1/.well-known/ready and returns nil on HTTP 200.
func (c *HTTPClient) Health(ctx context.Context) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/.well-known/ready", nil)
	if err != nil {
		return fmt.Errorf("weaviate: build ready request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: ready: %w", err)
	}
	defer resp.Body.Close()
	// Drain to let keep-alive reuse the connection.
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("weaviate: ready returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// Ready is an alias for Health; see the Client interface docs.
func (c *HTTPClient) Ready(ctx context.Context) error { return c.Health(ctx) }

// GetObject fetches a single object by class and id. It is the
// mirror of Upsert and is used by the watermark store (and by the
// self-healing replay path) to read per-object state from Weaviate
// without going through the GraphQL query path.
//
// Returns (nil, nil) when the object does not exist — callers use
// that shape to distinguish "never written" from "written with the
// zero value", which the watermark package relies on.
func (c *HTTPClient) GetObject(ctx context.Context, class, id string) (map[string]any, error) {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	url := fmt.Sprintf("%s/v1/objects/%s/%s", c.baseURL, class, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("weaviate: build get request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weaviate: get %s/%s: %w", class, id, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		var outer struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &outer); err != nil {
			return nil, fmt.Errorf("weaviate: decode get response: %w", err)
		}
		return outer.Properties, nil
	case http.StatusNotFound:
		// Never-written sentinel — callers use this to distinguish a
		// fresh install from an explicit zero value.
		return nil, nil
	default:
		return nil, fmt.Errorf("weaviate: get %s/%s returned HTTP %d: %s", class, id, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// Upsert stores or merges an object. Weaviate's REST surface splits
// the lifecycle across three endpoints with subtly different semantics:
//
//   POST  /v1/objects              — create (422s if the id exists)
//   PUT   /v1/objects/{class}/{id} — full REPLACE (wipes any field
//                                     not present in the body, including
//                                     the vector — see bead cortex-jw6)
//   PATCH /v1/objects/{class}/{id} — MERGE (only listed fields change;
//                                     unmentioned fields are preserved)
//
// Cortex's applier model requires merge semantics: after the initial
// write, self-heal replays individual datoms as tiny bags that do not
// carry the vector, so a PUT-based fallback would strip the vector on
// every subsequent backend apply. PATCH preserves the vector and the
// other properties the earlier write laid down, which is what the
// applier's scratch-bag design intends.
//
// Upsert therefore:
//  1. POST /v1/objects  — create.
//  2. On 422 (already exists), PATCH /v1/objects/{class}/{id} — merge.
//
// The id MUST be a UUIDv5 or v4 string; callers that have ULIDs
// convert first via DeriveUUID in internal/datom.
//
// Bead cortex-b8a documents the original PUT-only implementation,
// which failed every first write because PUT cannot create.
// Bead cortex-jw6 documents the PUT-replace vector-stripping failure
// that motivated the switch to PATCH.
func (c *HTTPClient) Upsert(ctx context.Context, class, id string, vector []float32, properties map[string]any) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	body := map[string]any{
		"class":      class,
		"id":         id,
		"properties": properties,
	}
	// Vector is omitted entirely when the caller passes a zero-length
	// slice. Under PATCH semantics, omitting the vector preserves the
	// existing stored vector — which is exactly what self-heal wants
	// when replaying link datoms that carry no embedding.
	if len(vector) > 0 {
		body["vector"] = vector
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("weaviate: marshal upsert body: %w", err)
	}

	// First attempt: POST /v1/objects (create).
	createURL := c.baseURL + "/v1/objects"
	createResp, createRaw, err := c.doUpsertRequest(ctx, http.MethodPost, createURL, buf)
	if err != nil {
		return fmt.Errorf("weaviate: upsert post: %w", err)
	}
	switch createResp {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusUnprocessableEntity:
		// "already exists" — fall through to the PATCH merge path.
	default:
		return fmt.Errorf("weaviate: upsert post %s/%s returned HTTP %d: %s", class, id, createResp, strings.TrimSpace(string(createRaw)))
	}

	// Second attempt: PATCH /v1/objects/{class}/{id} (merge).
	mergeURL := fmt.Sprintf("%s/v1/objects/%s/%s", c.baseURL, class, id)
	mergeResp, mergeRaw, err := c.doUpsertRequest(ctx, http.MethodPatch, mergeURL, buf)
	if err != nil {
		return fmt.Errorf("weaviate: upsert patch: %w", err)
	}
	switch mergeResp {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("weaviate: upsert patch %s/%s returned HTTP %d: %s", class, id, mergeResp, strings.TrimSpace(string(mergeRaw)))
	}
}

// doUpsertRequest is the shared round-trip helper for Upsert's POST
// and PUT phases. It returns the status code and response body so the
// caller can switch on status without duplicating the request plumbing.
func (c *HTTPClient) doUpsertRequest(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, nil
}

// graphqlRequest is the shape of a POST body to /v1/graphql.
type graphqlRequest struct {
	Query string `json:"query"`
}

// graphqlResponse is the envelope Weaviate returns from /v1/graphql.
// The Data field is intentionally left as raw JSON so we can decode
// class-specific response shapes without generating types for every
// collection.
type graphqlResponse struct {
	Data   json.RawMessage  `json:"data"`
	Errors []graphqlErrItem `json:"errors"`
}

type graphqlErrItem struct {
	Message string `json:"message"`
}

// NearestNeighbors runs a GraphQL nearVector query against the class
// and filters results whose cosine similarity falls below cosineFloor.
//
// Weaviate reports `_additional.distance`. For the cosine metric the
// relationship is:
//
//	cosine_similarity = 1 - distance
//
// We over-fetch slightly (2*k) when a floor is active so that the
// post-filter doesn't silently return fewer than k results just
// because Weaviate sorted some below-floor neighbors ahead of
// above-floor ones in tail positions. This mirrors the
// "fetch-then-filter" pattern used by the recall pipeline.
func (c *HTTPClient) NearestNeighbors(ctx context.Context, class string, vector []float32, k int, cosineFloor float64) ([]SearchResult, error) {
	if k <= 0 {
		return nil, nil
	}
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	fetchLimit := k
	if cosineFloor > 0 {
		fetchLimit = k * 2
	}

	vecJSON, err := json.Marshal(vector)
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal vector: %w", err)
	}

	// GraphQL is textual; the only untrusted input here is `class`,
	// which is provided by our own code (ClassEntry / ClassFrame). We
	// still reject any suspicious characters as defense-in-depth.
	if !isSafeClassName(class) {
		return nil, fmt.Errorf("weaviate: unsafe class name %q", class)
	}
	query := fmt.Sprintf(
		`{ Get { %s(nearVector: {vector: %s}, limit: %d) { _additional { id distance } } } }`,
		class, string(vecJSON), fetchLimit,
	)

	buf, err := json.Marshal(graphqlRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal graphql: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/graphql", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("weaviate: build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weaviate: graphql: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("weaviate: read graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weaviate: graphql returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env graphqlResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("weaviate: decode graphql envelope: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("weaviate: graphql error: %s", env.Errors[0].Message)
	}

	// Decode { "Get": { <class>: [ {_additional: {...}, ...}, ... ] } }
	var outer struct {
		Get map[string][]map[string]any `json:"Get"`
	}
	if err := json.Unmarshal(env.Data, &outer); err != nil {
		return nil, fmt.Errorf("weaviate: decode graphql data: %w", err)
	}

	rows := outer.Get[class]
	out := make([]SearchResult, 0, len(rows))
	for _, row := range rows {
		add, _ := row["_additional"].(map[string]any)
		id, _ := add["id"].(string)
		distance, _ := toFloat64(add["distance"])
		cosSim := 1.0 - distance
		if cosineFloor > 0 && cosSim < cosineFloor {
			continue
		}

		props := make(map[string]any, len(row)-1)
		for k2, v := range row {
			if k2 == "_additional" {
				continue
			}
			props[k2] = v
		}
		out = append(out, SearchResult{
			ID:               id,
			CosineSimilarity: cosSim,
			Properties:       props,
		})
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

// FetchVectorsByCortexIDs runs a single GraphQL Get query that pulls
// the cortex_id property and the _additional.vector for every object
// in `class` whose cortex_id is in the supplied set. The result is a
// map from cortex_id to the dense embedding vector. cortex_ids absent
// from Weaviate (cold cache, never written, evicted) are absent from
// the returned map; the caller treats absence as "no vector"
// rather than as an error.
//
// Implementation note: Weaviate's GraphQL `where` filter on a text
// property uses ContainsAny / Equal operators. We build an Or chain of
// Equal terms — small id sets (typically the recall candidate window)
// keep this query well under the GraphQL parser limits.
func (c *HTTPClient) FetchVectorsByCortexIDs(ctx context.Context, class string, cortexIDs []string) (map[string][]float32, error) {
	if len(cortexIDs) == 0 {
		return map[string][]float32{}, nil
	}
	if !isSafeClassName(class) {
		return nil, fmt.Errorf("weaviate: unsafe class name %q", class)
	}
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	// Build the filter: { operator: Or, operands: [ {Equal cortex_id=<id>} ... ] }.
	// Each Equal term carries a JSON-encoded valueText so a cortex_id
	// containing a quote (it should not, but defense-in-depth) cannot
	// break out of the query string.
	var ops []string
	for _, id := range cortexIDs {
		quoted, err := json.Marshal(id)
		if err != nil {
			return nil, fmt.Errorf("weaviate: marshal cortex_id: %w", err)
		}
		ops = append(ops,
			fmt.Sprintf(`{path:["cortex_id"], operator:Equal, valueText:%s}`, string(quoted)))
	}
	where := fmt.Sprintf(`{operator:Or, operands:[%s]}`, strings.Join(ops, ","))

	// limit must accommodate the full id set; oversize is harmless.
	query := fmt.Sprintf(
		`{ Get { %s(where: %s, limit: %d) { cortex_id _additional { vector } } } }`,
		class, where, len(cortexIDs),
	)

	buf, err := json.Marshal(graphqlRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal graphql: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/graphql", bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("weaviate: build graphql request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weaviate: graphql: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("weaviate: read graphql response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("weaviate: graphql returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env graphqlResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("weaviate: decode graphql envelope: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("weaviate: graphql error: %s", env.Errors[0].Message)
	}

	var outer struct {
		Get map[string][]map[string]any `json:"Get"`
	}
	if err := json.Unmarshal(env.Data, &outer); err != nil {
		return nil, fmt.Errorf("weaviate: decode graphql data: %w", err)
	}

	out := make(map[string][]float32, len(cortexIDs))
	for _, row := range outer.Get[class] {
		cortexID, _ := row["cortex_id"].(string)
		if cortexID == "" {
			continue
		}
		add, _ := row["_additional"].(map[string]any)
		rawVec, _ := add["vector"].([]any)
		if len(rawVec) == 0 {
			continue
		}
		vec := make([]float32, 0, len(rawVec))
		for _, x := range rawVec {
			f, ok := toFloat64(x)
			if !ok {
				continue
			}
			vec = append(vec, float32(f))
		}
		if len(vec) > 0 {
			out[cortexID] = vec
		}
	}
	return out, nil
}

// toFloat64 converts a JSON-number-ish value into float64. GraphQL
// responses over JSON unmarshal numbers as float64 by default but we
// guard against json.Number and int for safety.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// isSafeClassName rejects any class name that is not plain alphanumerics.
// Weaviate class names are GraphQL type identifiers and must start with
// an uppercase letter; this check is defensive against accidental
// injection via caller misuse.
func isSafeClassName(class string) bool {
	if class == "" {
		return false
	}
	for i, r := range class {
		switch {
		case i == 0 && r >= 'A' && r <= 'Z':
			// First rune must be uppercase ASCII — Weaviate class
			// names are GraphQL type identifiers and GraphQL
			// conventionally starts types with uppercase.
		case i > 0 && r >= 'A' && r <= 'Z':
		case i > 0 && r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		case i > 0 && r == '_':
		default:
			return false
		}
	}
	return true
}

// ErrAlreadyExists is returned by low-level schema helpers when a
// class already exists in Weaviate. EnsureSchema swallows it to make
// the overall operation idempotent; callers that need the distinction
// can use errors.Is.
var ErrAlreadyExists = errors.New("weaviate: class already exists")
