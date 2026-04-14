package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer wires an httptest.Server whose handler inspects the
// request (headers, body) and returns a canned response. The handler
// receives the decoded chat request body (or nil for non-chat paths)
// so individual tests can assert on it without re-decoding.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *HTTPClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewHTTPClient(Config{
		APIKey:    "test-key",
		Model:     "gpt-4o-mini",
		MaxTokens: 256,
		BaseURL:   srv.URL,
	})
	return srv, c
}

func TestGenerateSendsAuthorizationHeader(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want %q", got, "Bearer test-key")
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type header = %q, want application/json", got)
		}
		writeChatCompletion(w, "hello")
	})
	got, err := c.Generate(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "hello" {
		t.Errorf("Generate = %q, want %q", got, "hello")
	}
}

func TestGenerateParsesPlainTextContent(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if _, present := body["response_format"]; present {
			t.Error("Generate must not send response_format field")
		}
		writeChatCompletion(w, "the quick brown fox")
	})
	got, err := c.Generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "the quick brown fox" {
		t.Errorf("Generate = %q", got)
	}
}

func TestGenerateStructuredSendsStrictJSONSchema(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		rf, ok := body["response_format"].(map[string]any)
		if !ok {
			t.Fatalf("response_format missing or wrong type: %#v", body["response_format"])
		}
		if rf["type"] != "json_schema" {
			t.Errorf("response_format.type = %v, want json_schema", rf["type"])
		}
		js, ok := rf["json_schema"].(map[string]any)
		if !ok {
			t.Fatalf("json_schema missing")
		}
		if js["strict"] != true {
			t.Errorf("json_schema.strict = %v, want true", js["strict"])
		}
		// The structured response is a JSON STRING under strict mode.
		writeChatCompletion(w, `{"name":"alice","age":30}`)
	})

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"}
		}
	}`)
	got, err := c.GenerateStructured(context.Background(), "prompt", schema)
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	// The adapter must return the content verbatim so callers can
	// unmarshal into their own type.
	var parsed struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("unmarshal returned content: %v", err)
	}
	if parsed.Name != "alice" || parsed.Age != 30 {
		t.Errorf("parsed = %+v", parsed)
	}
}

func TestPingOK(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("Ping path = %q, want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("Ping method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"data":[]}`)
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestPingUnauthorized(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	})
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping: want error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("Ping err = %v, want status code", err)
	}
}

func TestChatCompletionsNon200ReturnsBodySnippet(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"invalid model","type":"invalid_request_error"}}`)
	})
	_, err := c.Generate(context.Background(), "prompt")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid model") {
		t.Errorf("err = %v, want status and message", err)
	}
}

func TestNormalizeSchemaAddsAdditionalPropertiesFalse(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		}
	}`)
	out, err := normalizeSchema(in)
	if err != nil {
		t.Fatalf("normalizeSchema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", m["additionalProperties"])
	}
}

func TestNormalizeSchemaFillsRequired(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"},
			"email": {"type": "string"}
		}
	}`)
	out, err := normalizeSchema(in)
	if err != nil {
		t.Fatalf("normalizeSchema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	req, ok := m["required"].([]any)
	if !ok {
		t.Fatalf("required not array: %#v", m["required"])
	}
	if len(req) != 3 {
		t.Errorf("required has %d entries, want 3", len(req))
	}
	seen := map[string]bool{}
	for _, v := range req {
		if s, ok := v.(string); ok {
			seen[s] = true
		}
	}
	for _, want := range []string{"name", "age", "email"} {
		if !seen[want] {
			t.Errorf("required missing %q", want)
		}
	}
}

func TestNormalizeSchemaRejectsNullType(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"scalar null", `{"type":"object","properties":{"x":{"type":"null"}}}`},
		{"nullable union", `{"type":"object","properties":{"x":{"type":["string","null"]}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := normalizeSchema(json.RawMessage(tc.in))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), "null types") {
				t.Errorf("err = %v, want null types message", err)
			}
		})
	}
}

func TestNormalizeSchemaRecursesIntoPropertiesAndItems(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"tags": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string"},
						"value": {"type": "string"}
					}
				}
			},
			"meta": {
				"type": "object",
				"properties": {
					"created": {"type": "string"}
				}
			}
		}
	}`)
	out, err := normalizeSchema(in)
	if err != nil {
		t.Fatalf("normalizeSchema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Nested object under properties.meta should be normalized.
	props := m["properties"].(map[string]any)
	meta := props["meta"].(map[string]any)
	if meta["additionalProperties"] != false {
		t.Error("meta.additionalProperties not set")
	}
	if _, ok := meta["required"].([]any); !ok {
		t.Error("meta.required not set")
	}

	// Nested object inside array items should be normalized.
	tags := props["tags"].(map[string]any)
	items := tags["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Error("items.additionalProperties not set")
	}
	req, ok := items["required"].([]any)
	if !ok || len(req) != 2 {
		t.Errorf("items.required = %#v, want two entries", items["required"])
	}

	// Root required should also list top-level properties.
	rootReq, ok := m["required"].([]any)
	if !ok || len(rootReq) != 2 {
		t.Errorf("root required = %#v, want two entries", m["required"])
	}
}

// writeChatCompletion writes a minimal chat.completion response body
// with the given content string as the assistant message.
func writeChatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 0,
		"model":   "gpt-4o-mini",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}
