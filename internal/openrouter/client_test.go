package openrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *HTTPClient) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewHTTPClient(Config{
		APIKey:      "test-key",
		Model:       "anthropic/claude-sonnet-4.5",
		MaxTokens:   256,
		BaseURL:     srv.URL,
		HTTPReferer: "https://cortex.local",
		XTitle:      "cortex",
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
		if got := r.Header.Get("HTTP-Referer"); got != "https://cortex.local" {
			t.Errorf("HTTP-Referer header = %q", got)
		}
		if got := r.Header.Get("X-Title"); got != "cortex" {
			t.Errorf("X-Title header = %q", got)
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

func TestGenerateOmitsOptionalHeadersWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, present := r.Header["Http-Referer"]; present {
			t.Error("HTTP-Referer must be omitted when empty")
		}
		if _, present := r.Header["X-Title"]; present {
			t.Error("X-Title must be omitted when empty")
		}
		writeChatCompletion(w, "ok")
	}))
	defer srv.Close()
	c := NewHTTPClient(Config{APIKey: "k", Model: "m", BaseURL: srv.URL})
	if _, err := c.Generate(context.Background(), "hi"); err != nil {
		t.Fatalf("Generate: %v", err)
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
		if body["model"] != "anthropic/claude-sonnet-4.5" {
			t.Errorf("model = %v, want slug-prefixed id", body["model"])
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

func TestChatPathTargetsOpenRouterAPIV1(t *testing.T) {
	_, c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("chat path = %q, want /v1/chat/completions", r.URL.Path)
		}
		writeChatCompletion(w, "ok")
	})
	if _, err := c.Generate(context.Background(), "p"); err != nil {
		t.Fatal(err)
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

func TestChatCompletionsNon200ReturnsErrorMessage(t *testing.T) {
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

	props := m["properties"].(map[string]any)
	meta := props["meta"].(map[string]any)
	if meta["additionalProperties"] != false {
		t.Error("meta.additionalProperties not set")
	}
	if _, ok := meta["required"].([]any); !ok {
		t.Error("meta.required not set")
	}

	tags := props["tags"].(map[string]any)
	items := tags["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Error("items.additionalProperties not set")
	}
	req, ok := items["required"].([]any)
	if !ok || len(req) != 2 {
		t.Errorf("items.required = %#v, want two entries", items["required"])
	}
}

func writeChatCompletion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	body := map[string]any{
		"id":      "gen-test",
		"object":  "chat.completion",
		"created": 0,
		"model":   "anthropic/claude-sonnet-4.5",
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
