package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ListOllamaModels hits GET <endpoint>/api/tags and returns every model
// name reported by the host Ollama daemon. endpoint may be a bare
// host:port pair or a full URL; scheme defaults to http.
//
// This helper lives in the infra package rather than internal/ollama
// because the Ollama adapter's Client interface exposes only the
// invocation surface (Ping/Show/Embed/Generate) — tag enumeration is a
// lifecycle-only concern (cortex up/status/doctor) and does not need to
// pollute the vector-write interface.
//
// Spec reference:
//   docs/spec/cortex-spec.md §"cortex up Readiness Contract" — the
//   Ollama probe "returns 200 and the response contains each configured
//   model name".
func ListOllamaModels(ctx context.Context, endpoint string) ([]string, error) {
	base := strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("ollama tags: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama tags: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama tags: read body: %w", err)
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("ollama tags: decode: %w", err)
	}
	names := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		names = append(names, m.Name)
	}
	return names, nil
}
