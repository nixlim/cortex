package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient wires an HTTPClient at the given test server so every
// test shares one construction path.
func newTestClient(t *testing.T, srv *httptest.Server) *HTTPClient {
	t.Helper()
	return NewHTTPClient(Config{
		APIKey:    "test-key",
		Model:     "claude-test",
		MaxTokens: 128,
		BaseURL:   srv.URL,
	})
}

// decodeRequest is a small helper that pulls the JSON body off the
// handler's request. Tests that need to inspect the wire shape use
// this so the path stays consistent.
func decodeRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode body: %v (raw=%s)", err, string(raw))
	}
	return m
}

func TestGenerate_SendsHeadersAndParsesText(t *testing.T) {
	var gotAPIKey, gotVersion, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotContentType = r.Header.Get("content-type")

		// Generate (unstructured) should NOT include tools or tool_choice.
		body := decodeRequest(t, r)
		if _, ok := body["tools"]; ok {
			t.Errorf("generate: tools should be absent, got %v", body["tools"])
		}
		if _, ok := body["tool_choice"]; ok {
			t.Errorf("generate: tool_choice should be absent, got %v", body["tool_choice"])
		}

		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"hello world"}],"model":"claude-test","stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	got, err := c.Generate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "hello world" {
		t.Errorf("Generate text: got %q want %q", got, "hello world")
	}
	if gotAPIKey != "test-key" {
		t.Errorf("x-api-key: got %q want %q", gotAPIKey, "test-key")
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version: got %q want %q", gotVersion, "2023-06-01")
	}
	if !strings.Contains(gotContentType, "application/json") {
		t.Errorf("content-type: got %q", gotContentType)
	}
}

func TestGenerateStructured_ForcesToolAndReturnsInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := decodeRequest(t, r)

		// tool_choice must force emit_structured_output.
		tc, ok := body["tool_choice"].(map[string]any)
		if !ok {
			t.Fatalf("tool_choice missing or wrong type: %v", body["tool_choice"])
		}
		if tc["type"] != "tool" || tc["name"] != "emit_structured_output" {
			t.Errorf("tool_choice: got %v", tc)
		}

		// tools array must carry our single tool with the schema and
		// the cache_control breakpoint.
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("tools: got %v", body["tools"])
		}
		tool0, ok := tools[0].(map[string]any)
		if !ok {
			t.Fatalf("tool[0] wrong type: %v", tools[0])
		}
		if tool0["name"] != "emit_structured_output" {
			t.Errorf("tool[0] name: got %v", tool0["name"])
		}
		if _, ok := tool0["input_schema"]; !ok {
			t.Errorf("tool[0] input_schema missing")
		}
		cc, ok := tool0["cache_control"].(map[string]any)
		if !ok || cc["type"] != "ephemeral" {
			t.Errorf("tool[0] cache_control: got %v", tool0["cache_control"])
		}

		// Respond with a tool_use block carrying a structured input.
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"emit_structured_output","input":{"summary":"ok","count":3}}],"model":"claude-test","stop_reason":"tool_use"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	schema := json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"},"count":{"type":"integer"}},"required":["summary","count"]}`)
	got, err := c.GenerateStructured(context.Background(), "produce json", schema)
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	// The returned string must be valid JSON matching the tool_use.input
	// that the server emitted, preserving field values verbatim.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("returned json invalid: %v (got=%s)", err, got)
	}
	if parsed["summary"] != "ok" {
		t.Errorf("summary: got %v", parsed["summary"])
	}
	if parsed["count"].(float64) != 3 {
		t.Errorf("count: got %v", parsed["count"])
	}
}

func TestPing_OKReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ping must send max_tokens=1 so we don't burn tokens on a
		// liveness probe.
		body := decodeRequest(t, r)
		if mt, _ := body["max_tokens"].(float64); mt != 1 {
			t.Errorf("ping max_tokens: got %v want 1", body["max_tokens"])
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"model":"claude-test","stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestPing_401ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatalf("Ping: expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error should mention status: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("error should surface provider message: %v", err)
	}
}

func TestPing_500ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream exploded`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatalf("Ping: expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Errorf("error should include body snippet: %v", err)
	}
}

func TestGenerate_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is required"}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.Generate(context.Background(), "hi")
	if err == nil {
		t.Fatalf("Generate: expected error on 400")
	}
	if !strings.Contains(err.Error(), "max_tokens is required") {
		t.Errorf("error should surface provider message: %v", err)
	}
}
