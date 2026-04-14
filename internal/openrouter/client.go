// Package openrouter is the Cortex adapter for OpenRouter's OpenAI-
// compatible chat completions API. It implements internal/llm.Generator
// against POST /api/v1/chat/completions and exposes GET /api/v1/models
// for Ping.
//
// Design notes:
//
//   - OpenRouter proxies hundreds of upstream models behind a single
//     endpoint and a single API key. Model identifiers carry the
//     upstream-provider prefix (e.g. "anthropic/claude-sonnet-4.5",
//     "openai/gpt-4o-mini", "google/gemini-2.0-flash-exp"). The adapter
//     passes the model string through verbatim; operators are expected
//     to name a slug that OpenRouter resolves.
//   - The wire protocol is OpenAI-compatible, so the request and
//     response shapes mirror internal/openai. We deliberately LIFT the
//     code rather than embed *openai.HTTPClient because OpenRouter's
//     header conventions (HTTP-Referer, X-Title) and error envelopes
//     will drift from OpenAI's over time; keeping the two providers
//     decoupled protects each from the other's churn.
//   - Structured output: strict json_schema mode is used when the
//     caller invokes GenerateStructured. OpenRouter forwards the
//     response_format block to the upstream provider as-is. If the
//     operator picks a model that does not support strict mode (e.g.
//     some open-source Llama variants), the request will fail at the
//     upstream boundary. Document the model-choice contract in
//     config.yaml comments; per-provider fallback to non-strict is a
//     follow-on.
//   - Prompt caching: OpenRouter supports provider-specific caching
//     hints in the payload. Out of scope for the initial landing.
//   - Ping hits GET /api/v1/models rather than a throwaway chat
//     completion: free, fast, still exercises the Authorization header.
package openrouter

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

// DefaultTimeout is the fallback per-request budget when the caller's
// context has no deadline. Call sites are expected to set their own
// deadline via context.WithTimeout; this is a safety net.
const DefaultTimeout = 60 * time.Second

// DefaultBaseURL is the production OpenRouter API root. The chat and
// models paths are appended as "/v1/chat/completions" and
// "/v1/models" respectively, so the full URLs resolve to
// https://openrouter.ai/api/v1/chat/completions and
// https://openrouter.ai/api/v1/models. Tests inject a httptest.Server
// URL via Config.BaseURL.
const DefaultBaseURL = "https://openrouter.ai/api"

// Config is the subset of Cortex config fields this adapter needs.
// HTTPReferer and XTitle are OpenRouter-specific optional headers that
// attribute traffic to a site or app in the OpenRouter dashboard;
// leaving them empty is fine.
type Config struct {
	APIKey      string
	Model       string
	MaxTokens   int
	BaseURL     string
	Timeout     time.Duration
	HTTPReferer string
	XTitle      string
}

// HTTPClient is the live implementation of llm.Generator against
// OpenRouter's REST API.
type HTTPClient struct {
	baseURL     string
	apiKey      string
	model       string
	maxTokens   int
	timeout     time.Duration
	httpReferer string
	xTitle      string
	http        *http.Client
}

// NewHTTPClient builds an adapter against the given OpenRouter config.
// Empty BaseURL falls back to DefaultBaseURL; zero Timeout falls back
// to DefaultTimeout.
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
		baseURL:     base,
		apiKey:      cfg.APIKey,
		model:       cfg.Model,
		maxTokens:   cfg.MaxTokens,
		timeout:     timeout,
		httpReferer: cfg.HTTPReferer,
		xTitle:      cfg.XTitle,
		http:        &http.Client{},
	}
}

func ctxWithDefault(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

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

// errorEnvelope matches OpenRouter's standard error body. The shape
// mirrors OpenAI's but OpenRouter is free to diverge; we decode
// defensively and fall back to a raw snippet if the shape drifts.
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
// strict-mode rules before being embedded in the request.
func (c *HTTPClient) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	normalized, err := normalizeSchema(schema)
	if err != nil {
		return "", err
	}
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
		return "", fmt.Errorf("openrouter: marshal response_format: %w", err)
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
		return "", fmt.Errorf("openrouter: marshal chat body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openrouter: build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if c.httpReferer != "" {
		req.Header.Set("HTTP-Referer", c.httpReferer)
	}
	if c.xTitle != "" {
		req.Header.Set("X-Title", c.xTitle)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter: chat: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", chatError(resp.StatusCode, raw)
	}

	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("openrouter: decode chat response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("openrouter: chat returned no choices")
	}
	// Empty content under HTTP 200 is a silent failure mode: it typically
	// means the upstream model rejected strict json_schema, tripped a
	// safety filter, or exhausted its output budget before producing any
	// JSON. Surface it here as an explicit error rather than returning
	// "" and letting the caller's json.Unmarshal bleed "unexpected end of
	// JSON input" several layers up.
	content := cr.Choices[0].Message.Content
	finish := cr.Choices[0].FinishReason
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("openrouter: model returned empty content (finish_reason=%q; likely the upstream model does not honor strict json_schema, hit its output budget, or was filtered — try anthropic/claude-sonnet-4.5 or openai/gpt-4o-mini)", finish)
	}
	// finish_reason=="length" means the model hit max_tokens mid-response
	// and the content is a truncated prefix — passing it through would
	// fail json.Unmarshal two layers up with an opaque "unexpected end
	// of JSON input". Surface it explicitly and name the config knob
	// the operator should raise.
	if finish == "length" {
		return "", fmt.Errorf("openrouter: response truncated at max_tokens (finish_reason=\"length\"); raise llm.openrouter.max_tokens in ~/.cortex/config.yaml or cap list sizes in the prompt")
	}
	return content, nil
}

// chatError folds an OpenRouter error body into a single error value.
func chatError(status int, raw []byte) error {
	var env errorEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Message != "" {
		return fmt.Errorf("openrouter: chat returned HTTP %d: %s", status, env.Error.Message)
	}
	snippet := strings.TrimSpace(string(raw))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	return fmt.Errorf("openrouter: chat returned HTTP %d: %s", status, snippet)
}

// Ping hits GET /v1/models. Validates reachability and credentials in
// one round trip without spending chat tokens.
func (c *HTTPClient) Ping(ctx context.Context) error {
	ctx, cancel := ctxWithDefault(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return fmt.Errorf("openrouter: build models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openrouter: models: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openrouter: models returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}
