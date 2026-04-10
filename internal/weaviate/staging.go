// internal/weaviate/staging.go adds the staging-class surface used by
// `cortex rebuild` so the Weaviate side of the rebuild pipeline can
// build a fresh collection off to the side and swap it in atomically
// (bulk copy + delete) instead of touching the live Entry / Frame
// classes mid-run.
//
// Design:
//
//   - EntryStaging / FrameStaging classes mirror the live Entry /
//     Frame classes (same property set, same vectorizer=none). The
//     staging classes live alongside the live ones in the same
//     Weaviate instance — Weaviate scopes objects by class, not by
//     database, so this is the natural way to partition.
//
//   - Rebuild writes into the staging classes as it replays the log.
//     When the rebuild apply pass finishes cleanly, Swap copies every
//     object from staging into live (deleting and recreating the live
//     classes first so stale live-only rows don't survive the swap)
//     and then drops the staging classes so no stale state is left
//     behind. On any failure before Swap, Cleanup drops the staging
//     classes without touching live.
//
//   - The swap is a "bulk copy + delete" — it is NOT a single-
//     transaction rename (Weaviate has no rename-class op). The
//     intermediate state between "deleted live" and "copied staging
//     into live" is a brief empty window. This is acceptable for
//     rebuild semantics: rebuild is an explicit operator action, the
//     operator knows the corpus is being re-materialized, and the
//     Neo4j side of the rebuild pipeline carries the same caveat.
//
// This closes cortex-s84 (MAJ-008 follow-up from grill-code round 2/3).
package weaviate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ClassEntryStaging / ClassFrameStaging are the Weaviate class names
// used by rebuild while it streams a rebuilt corpus off to the side.
// Weaviate class names must be valid GraphQL type identifiers — they
// start with an uppercase letter and cannot contain underscores in the
// leading position, so we use PascalCase "EntryStaging" rather than
// the "Entry_staging" hint in the original bead.
const (
	ClassEntryStaging = "EntryStaging"
	ClassFrameStaging = "FrameStaging"
)

// StagingClient is the interface cmd/cortex/staging_backends.go uses
// to talk to the Weaviate side of rebuild's staging namespace. It is
// satisfied by *HTTPClient; the interface exists so unit tests can
// inject a fake without standing up an httptest server for every
// assertion.
type StagingClient interface {
	// EnsureStagingSchema creates the EntryStaging and FrameStaging
	// classes if they do not already exist. Idempotent.
	EnsureStagingSchema(ctx context.Context) error

	// UpsertStaging writes one object into the staging class that
	// corresponds to the supplied live class (ClassEntry →
	// ClassEntryStaging, ClassFrame → ClassFrameStaging). Any other
	// live class is rejected as a programmer error. cortexID is a
	// prefixed ULID (e.g. "entry:01ARZ..."); the implementation
	// derives a stable Weaviate UUID from it using the same hash as
	// the live BackendApplier so staging and live rows round-trip
	// to the same object id after Swap.
	UpsertStaging(ctx context.Context, liveClass, cortexID string, vector []float32, properties map[string]any) error

	// SwapStagingToLive bulk-copies every object from the staging
	// classes into the live classes (deleting the live classes first
	// so stale live-only rows don't survive) and then drops the
	// staging classes so the staging namespace ends empty. A failure
	// at any step leaves the swap partial; callers are expected to
	// pair this with CleanupStaging on the error path.
	SwapStagingToLive(ctx context.Context) error

	// CleanupStaging drops the EntryStaging and FrameStaging classes
	// without touching live. Safe to call when the staging classes
	// may not exist — a missing class is swallowed so the failure
	// path of a half-run rebuild does not compound errors.
	CleanupStaging(ctx context.Context) error
}

// entryStagingClassDefinition returns the EntryStaging class schema.
// Property set mirrors EntryClassDefinition exactly so a Swap-time
// copy preserves every field rebuild wrote during the apply pass.
func entryStagingClassDefinition() classDefinition {
	def := EntryClassDefinition()
	def.Class = ClassEntryStaging
	def.Description = "Cortex episodic observation (rebuild staging namespace)."
	return def
}

// frameStagingClassDefinition returns the FrameStaging class schema.
func frameStagingClassDefinition() classDefinition {
	def := FrameClassDefinition()
	def.Class = ClassFrameStaging
	def.Description = "Cortex frame instance (rebuild staging namespace)."
	return def
}

// EnsureStagingSchema is the staging counterpart to EnsureSchema.
func (c *HTTPClient) EnsureStagingSchema(ctx context.Context) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	for _, def := range []classDefinition{entryStagingClassDefinition(), frameStagingClassDefinition()} {
		if err := c.createClass(ctx, def); err != nil {
			if err == ErrAlreadyExists {
				continue
			}
			return fmt.Errorf("weaviate: ensure staging class %q: %w", def.Class, err)
		}
	}
	return nil
}

// stagingCounterpart returns the staging class name for a live class.
// Any unrecognized class is rejected as a programmer error: the write
// pipeline only lands datoms in ClassEntry and ClassFrame, so rebuild
// has no business asking us to stage anything else.
func stagingCounterpart(liveClass string) (string, error) {
	switch liveClass {
	case ClassEntry:
		return ClassEntryStaging, nil
	case ClassFrame:
		return ClassFrameStaging, nil
	default:
		return "", fmt.Errorf("weaviate: no staging counterpart for class %q", liveClass)
	}
}

// UpsertStaging maps the live class to its staging counterpart and
// delegates to Upsert after deriving the Weaviate object UUID from
// the prefixed ULID the caller supplied. Vector and properties flow
// through unchanged so the staging row is indistinguishable from what
// a Swap copy would later land in the live class.
func (c *HTTPClient) UpsertStaging(ctx context.Context, liveClass, cortexID string, vector []float32, properties map[string]any) error {
	stagingClass, err := stagingCounterpart(liveClass)
	if err != nil {
		return err
	}
	if properties == nil {
		properties = map[string]any{}
	}
	// Guarantee the cortex_id property is present on staging rows so
	// Swap's GraphQL copy can round-trip the identifier back to the
	// live class in the same shape the live write pipeline uses.
	if _, ok := properties["cortex_id"]; !ok {
		properties["cortex_id"] = cortexID
	}
	return c.Upsert(ctx, stagingClass, deriveUUID(cortexID), vector, properties)
}

// SwapStagingToLive promotes the staging classes to live:
//
//  1. Drop the live Entry and Frame classes (DELETE /v1/schema/{class}).
//  2. Recreate them from EntryClassDefinition / FrameClassDefinition so
//     Weaviate has a clean, empty collection to copy into.
//  3. For each (staging, live) pair, list every object in staging and
//     POST it into live with the exact same id + vector + properties.
//  4. Drop the staging classes so no stale state is left behind.
//
// The implementation is intentionally sequential (no parallel fan-out):
// rebuild is an explicit operator action and the correctness of the
// swap matters far more than its latency.
func (c *HTTPClient) SwapStagingToLive(ctx context.Context) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	pairs := []struct {
		staging string
		live    string
		def     classDefinition
	}{
		{ClassEntryStaging, ClassEntry, EntryClassDefinition()},
		{ClassFrameStaging, ClassFrame, FrameClassDefinition()},
	}

	// Phase 1: drop + recreate each live class so the subsequent copy
	// lands in a pristine collection. A missing live class is fine —
	// deleteClass swallows 404.
	for _, p := range pairs {
		if err := c.deleteClass(ctx, p.live); err != nil {
			return fmt.Errorf("weaviate swap: drop live %q: %w", p.live, err)
		}
		if err := c.createClass(ctx, p.def); err != nil {
			// A race where the class was recreated between the delete
			// and the create is fine; treat ErrAlreadyExists as a no-op.
			if err != ErrAlreadyExists {
				return fmt.Errorf("weaviate swap: recreate live %q: %w", p.live, err)
			}
		}
	}

	// Phase 2: bulk copy each staging class into its live counterpart.
	for _, p := range pairs {
		objs, err := c.listObjectsWithVectors(ctx, p.staging)
		if err != nil {
			return fmt.Errorf("weaviate swap: list %q: %w", p.staging, err)
		}
		for _, obj := range objs {
			if err := c.Upsert(ctx, p.live, obj.ID, obj.Vector, obj.Properties); err != nil {
				return fmt.Errorf("weaviate swap: copy %s/%s → %s: %w", p.staging, obj.ID, p.live, err)
			}
		}
	}

	// Phase 3: drop the staging classes so a subsequent rebuild starts
	// from an empty staging namespace.
	if err := c.CleanupStaging(ctx); err != nil {
		return fmt.Errorf("weaviate swap: drop staging: %w", err)
	}
	return nil
}

// CleanupStaging drops the EntryStaging and FrameStaging classes. A
// missing class is swallowed (404) so this is safe on a failure path
// where Create may not have run yet.
func (c *HTTPClient) CleanupStaging(ctx context.Context) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	for _, cls := range []string{ClassEntryStaging, ClassFrameStaging} {
		if err := c.deleteClass(ctx, cls); err != nil {
			return fmt.Errorf("weaviate: cleanup staging %q: %w", cls, err)
		}
	}
	return nil
}

// deleteClass removes a Weaviate class (and all of its objects) via
// DELETE /v1/schema/{class}. A 404 is treated as success because the
// two callers (Swap, CleanupStaging) both want "make sure this is
// gone" semantics, not "fail if it was already gone".
func (c *HTTPClient) deleteClass(ctx context.Context, class string) error {
	if !isSafeClassName(class) {
		return fmt.Errorf("weaviate: unsafe class name %q", class)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/v1/schema/"+class, nil)
	if err != nil {
		return fmt.Errorf("weaviate: build delete class request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: delete class %q: %w", class, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("weaviate: delete class %q returned HTTP %d: %s",
			class, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// stagedObject is the intermediate shape used by listObjectsWithVectors.
// It bundles the id, vector, and property bag in a form ready to hand
// straight to Upsert during a swap.
type stagedObject struct {
	ID         string
	Vector     []float32
	Properties map[string]any
}

// listObjectsWithVectors returns every object in a class along with
// its stored embedding vector. The call uses GraphQL Get with an
// explicit vector-include rather than REST /v1/objects because the
// REST surface does not consistently return vectors on all Weaviate
// versions, while GraphQL does.
//
// The caller passes a class name that has already been validated as
// a safe GraphQL identifier. A generous limit is used (10_000) so a
// typical Phase 1 corpus fits in a single request; a TODO-scale corpus
// would need paginated iteration, tracked as a follow-up.
func (c *HTTPClient) listObjectsWithVectors(ctx context.Context, class string) ([]stagedObject, error) {
	if !isSafeClassName(class) {
		return nil, fmt.Errorf("weaviate: unsafe class name %q", class)
	}

	// Build a GraphQL query that returns every known property on the
	// class plus the _additional vector. We enumerate properties from
	// the class definition rather than using `... on <class>` because
	// Weaviate's GraphQL does not return unlisted properties.
	var def classDefinition
	switch class {
	case ClassEntryStaging:
		def = entryStagingClassDefinition()
	case ClassFrameStaging:
		def = frameStagingClassDefinition()
	case ClassEntry:
		def = EntryClassDefinition()
	case ClassFrame:
		def = FrameClassDefinition()
	default:
		return nil, fmt.Errorf("weaviate: listObjectsWithVectors does not know class %q", class)
	}
	fields := make([]string, 0, len(def.Properties)+1)
	for _, p := range def.Properties {
		fields = append(fields, p.Name)
	}
	fields = append(fields, "_additional { id vector }")
	query := fmt.Sprintf(
		`{ Get { %s(limit: %d) { %s } } }`,
		class, 10000, strings.Join(fields, " "),
	)

	buf, err := json.Marshal(graphqlRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("weaviate: marshal graphql: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/graphql", strings.NewReader(string(buf)))
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

	rows := outer.Get[class]
	out := make([]stagedObject, 0, len(rows))
	for _, row := range rows {
		add, _ := row["_additional"].(map[string]any)
		id, _ := add["id"].(string)
		if id == "" {
			continue
		}
		rawVec, _ := add["vector"].([]any)
		var vec []float32
		if len(rawVec) > 0 {
			vec = make([]float32, 0, len(rawVec))
			for _, x := range rawVec {
				f, ok := toFloat64(x)
				if !ok {
					continue
				}
				vec = append(vec, float32(f))
			}
		}
		props := make(map[string]any, len(row)-1)
		for k, v := range row {
			if k == "_additional" {
				continue
			}
			props[k] = v
		}
		out = append(out, stagedObject{ID: id, Vector: vec, Properties: props})
	}
	return out, nil
}
