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
)

// classDefinition is the subset of Weaviate's schema class shape we need
// for Phase 1. We declare vectorizer=none because Cortex produces
// embeddings externally via the Ollama adapter; Weaviate stores the
// vector we give it and does not call out to any vectorization service.
type classDefinition struct {
	Class       string              `json:"class"`
	Description string              `json:"description,omitempty"`
	Vectorizer  string              `json:"vectorizer"`
	VectorIndex string              `json:"vectorIndexType,omitempty"`
	Properties  []classPropertyDef  `json:"properties"`
}

type classPropertyDef struct {
	Name        string   `json:"name"`
	DataType    []string `json:"dataType"`
	Description string   `json:"description,omitempty"`
}

// EntryClassDefinition returns the Entry class schema. The property set
// is deliberately minimal — enough for the write pipeline to persist
// the canonical cortex_id, project, timestamps, and the full content
// payload. Weaviate HNSW indexes the caller-supplied vector; the
// property layer is only used for filter-style retrieval and for
// returning payloads in recall responses.
func EntryClassDefinition() classDefinition {
	return classDefinition{
		Class:       ClassEntry,
		Description: "Cortex episodic observation (one datom transaction group).",
		Vectorizer:  "none",
		VectorIndex: "hnsw",
		Properties: []classPropertyDef{
			{Name: "cortex_id", DataType: []string{"text"}, Description: "Canonical Cortex ULID for the entry."},
			{Name: "project", DataType: []string{"text"}, Description: "Project namespace the entry belongs to."},
			{Name: "subject", DataType: []string{"text"}, Description: "Subject (user) PSI id, if bound."},
			{Name: "content", DataType: []string{"text"}, Description: "Primary content body of the observation."},
			{Name: "importance", DataType: []string{"number"}, Description: "Base importance score in [0,1]."},
			{Name: "created_at", DataType: []string{"date"}, Description: "UTC wall-clock creation timestamp."},
		},
	}
}

// FrameClassDefinition returns the Frame class schema. Frame instances
// are the structured surface Cortex extracts from episodic entries via
// `cortex reflect` and related commands; they carry a frame_type
// discriminator pointing at an entry in the built-in frame registry.
func FrameClassDefinition() classDefinition {
	return classDefinition{
		Class:       ClassFrame,
		Description: "Cortex frame instance (structured projection over entries).",
		Vectorizer:  "none",
		VectorIndex: "hnsw",
		Properties: []classPropertyDef{
			{Name: "cortex_id", DataType: []string{"text"}, Description: "Canonical Cortex ULID for the frame instance."},
			{Name: "frame_type", DataType: []string{"text"}, Description: "Frame type name from the built-in registry."},
			{Name: "project", DataType: []string{"text"}, Description: "Project namespace the frame belongs to."},
			{Name: "content", DataType: []string{"text"}, Description: "Serialized frame slot payload."},
			{Name: "source_entries", DataType: []string{"text[]"}, Description: "Cortex IDs of entries the frame was derived from."},
			{Name: "created_at", DataType: []string{"date"}, Description: "UTC wall-clock creation timestamp."},
		},
	}
}

// EnsureSchema creates the Entry and Frame classes if they do not
// already exist. The operation is idempotent: a second call is a
// no-op. The Weaviate REST schema endpoint returns HTTP 422 with a
// body containing "already exists" when a class is re-created, which
// we translate into ErrAlreadyExists and swallow at this layer.
func (c *HTTPClient) EnsureSchema(ctx context.Context) error {
	ctx, cancel := c.ctxWithDefault(ctx)
	defer cancel()

	for _, def := range []classDefinition{EntryClassDefinition(), FrameClassDefinition()} {
		if err := c.createClass(ctx, def); err != nil {
			if errors.Is(err, ErrAlreadyExists) {
				continue
			}
			return err
		}
	}
	return nil
}

// createClass POSTs a single class definition to /v1/schema. It
// returns ErrAlreadyExists when Weaviate reports the class is already
// present, distinguishing that case from other 4xx/5xx responses so
// EnsureSchema can remain idempotent without hiding genuine failures.
func (c *HTTPClient) createClass(ctx context.Context, def classDefinition) error {
	body, err := json.Marshal(def)
	if err != nil {
		return fmt.Errorf("weaviate: marshal class %q: %w", def.Class, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/schema", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("weaviate: build schema request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("weaviate: schema POST: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusUnprocessableEntity:
		// Weaviate uses 422 for validation failures *and* for
		// "class name already in use". The latter is recognizable by
		// the error body containing "already exists" (case-insensitive
		// substring match against the upstream error message).
		if isAlreadyExistsBody(raw) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("weaviate: schema 422 for class %q: %s", def.Class, strings.TrimSpace(string(raw)))
	default:
		return fmt.Errorf("weaviate: schema HTTP %d for class %q: %s", resp.StatusCode, def.Class, strings.TrimSpace(string(raw)))
	}
}

// isAlreadyExistsBody inspects a 422 response body for the
// "already exists" marker Weaviate uses when a class name collides
// with an existing class. We use a case-insensitive substring match
// rather than a structured decode because the upstream error shape
// has changed across minor versions and a substring match is stable
// across those revisions.
func isAlreadyExistsBody(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "already exists")
}
