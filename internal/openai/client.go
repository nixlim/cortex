// Package openai is the Cortex adapter for OpenAI's chat completions
// API. It implements internal/llm.Generator against
// POST /v1/chat/completions and exposes a GET /v1/models probe for
// Ping.
//
// Design notes:
//
//   - Stdlib net/http only. The OpenAI SDK is deliberately avoided so
//     the surface area stays the three methods llm.Generator
//     requires and the dependency graph stays lean.
//   - Structured output is done via response_format={type:"json_schema",
//     strict:true}. OpenAI's strict mode imposes a handful of schema
//     rules that Cortex's prompt schemas do not always satisfy (they
//     were written against Ollama's looser format= contract), so
//     normalizeSchema rewrites the incoming schema to meet them: every
//     object gets additionalProperties:false and a required array
//     listing all of its properties, null types are rejected. This
//     keeps the Generator seam contract unchanged for callers — they
//     keep passing the same schema they would pass to Ollama.
//   - response_format is omitted entirely from plain Generate calls.
//     Under strict mode OpenAI returns the structured payload as a
//     JSON string in choices[0].message.content; we return it verbatim
//     so the caller can json.Unmarshal into its own type, matching
//     the Ollama adapter's contract.
//   - Ping hits GET /v1/models rather than a throwaway chat completion
//     because /v1/models is free, fast, and still exercises the
//     Authorization header — enough for a liveness/credential probe.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout is the fallback per-request budget used when the
// caller's context has no deadline. Call sites are expected to set
// their own deadline via context.WithTimeout; this is a safety net.
const DefaultTimeout = 60 * time.Second

// DefaultBaseURL is the production OpenAI API root. Tests inject a
// httptest.Server URL via Config.BaseURL.
const DefaultBaseURL = "https://api.openai.com"

// Config is the subset of Cortex config fields this adapter needs.
// BaseURL exists purely as a test seam; production callers leave it
// empty and get DefaultBaseURL.
type Config struct {
	APIKey    string
	Model     string
	MaxTokens int
	BaseURL   string
	Timeout   time.Duration
}

// HTTPClient is the live implementation of llm.Generator against
// OpenAI's REST API.
type HTTPClient struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	timeout   time.Duration
	http      *http.Client
}

// NewHTTPClient builds an adapter against the given OpenAI
// configuration. Empty BaseURL falls back to DefaultBaseURL; zero
// Timeout falls back to DefaultTimeout.
func NewHTTPClient(cfg Config) *HTTPClient {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &HTTPClient{
		baseURL:   base,
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		maxTokens: cfg.MaxTokens,
		timeout:   timeout,
		http:      &http.Client{},
	}
}

func ctxWithDefault(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// chatMessage / chatRequest / chatResponse are the minimal shapes the
// adapter encodes and decodes. ResponseFormat uses json.RawMessage so
// the "omit entirely" behaviour falls out of omitempty — a nil
// RawMessage serializes to nothing, which is what unstructured
// callers want.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Index        int         `json:"index"`
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
}

// errorEnvelope matches OpenAI's standard error body so we can surface
// the human-readable message rather than a raw JSON blob.
type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// Generate posts an unstructured chat completion and returns the
// assistant message content verbatim.
func (c *HTTPClient) Generate(ctx context.Context, prompt string) (string, error) {
	return c.doGenerate(ctx, prompt, nil)
}

// GenerateStructured posts a chat completion with response_format set
// to json_schema + strict:true. The schema is normalized to satisfy
// OpenAI's strict-mode rules before being embedded in the request.
func (c *HTTPClient) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	normalized, err := normalizeSchema(schema)
	if err != nil {
		return "", err
	}
	// Wrap the normalized schema in the json_schema response_format
	// envelope. strict:true is what enforces the adherence guarantee —
	// without it OpenAI treats json_schema as a hint and may emit
	// extra fields.
	rf := map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "structured_output",
			"strict": true,
			"schema": json.RawMessage(normalized),
		},
	}
	raw, err := json.Marshal(rf)
	if err != nil {
		return "", fmt.Errorf("openai: marshal response_format: %w", err)
	}
	return c.doGenerate(ctx, prompt, raw)
}

// doGenerate is the shared POST /v1/chat/completions path. A nil
// responseFormat omits the field; a non-nil value is embedded as-is.
func (c *HTTPClient) doGenerate(ctx context.Context, prompt string, responseFormat json.RawMessage) (string, error) {
	ctx, cancel := ctxWithDefault(ctx, c.timeout)
	defer cancel()

	reqBody := chatRequest{
		Model:          c.model,
		Messages:       []chatMessage{{Role: "user", Content: prompt}},
		ResponseFormat: responseFormat,
		MaxTokens:      c.maxTokens,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("openai: marshal chat body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openai: build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("openai: chat: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", chatError(resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("openai: decode chat response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("openai: chat returned no choices")
	}
	// Empty content under HTTP 200 is a silent failure mode (safety
	// filter, output budget hit, refusal). Surface it explicitly so the
	// caller's json.Unmarshal does not mint "unexpected end of JSON
	// input" several layers up.
	content := cr.Choices[0].Message.Content
	finish := cr.Choices[0].FinishReason
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("openai: model returned empty content (finish_reason=%q)", finish)
	}
	// finish_reason=="length" means the model hit max_tokens mid-response
	// and the content is a truncated prefix.
	if finish == "length" {
		return "", fmt.Errorf("openai: response truncated at max_tokens (finish_reason=\"length\"); raise llm.openai.max_tokens in ~/.cortex/config.yaml or cap list sizes in the prompt")
	}
	return content, nil
}

// chatError folds an OpenAI error body into a single error value.
// The provider's error envelope carries a human-readable message we
// want to surface; if decoding fails we fall back to a snippet of the
// raw body so the user still sees something actionable.
func chatError(status int, raw []byte) error {
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("openai: chat returned HTTP %d: %s", status, env.Error.Message)
	}
	snippet := strings.TrimSpace(string(raw))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	return fmt.Errorf("openai: chat returned HTTP %d: %s", status, snippet)
}

// Ping hits GET /v1/models. It validates reachability and credentials
// in one round trip without spending chat tokens. 200 means reachable
// and authorized; anything else is an error with the status code.
func (c *HTTPClient) Ping(ctx context.Context) error {
	ctx, cancel := ctxWithDefault(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("openai: build models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openai: models: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openai: models returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
