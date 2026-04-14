// Package anthropic is the Cortex adapter for Anthropic's Messages
// API. It implements llm.Generator (Generate, GenerateStructured,
// Ping) against POST /v1/messages using stdlib net/http only.
//
// Design notes:
//
//   - Stdlib only. No anthropic-sdk-go dependency. The surface area we
//     need is small (three methods, one endpoint) and taking a
//     third-party SDK would bring in a transitive tree we don't need
//     for a Phase 2 adapter.
//   - Structured output is implemented via Anthropic's tool-use
//     mechanism. We declare a single tool "emit_structured_output"
//     whose input_schema is the caller's schema, then set tool_choice
//     to force that tool. The model's response carries a tool_use
//     block whose input field is an already-parsed JSON object; we
//     return it verbatim as a json.RawMessage so the caller sees the
//     same bytes Anthropic emitted (no re-parse round trip).
//   - Prompt caching: the tool definition is stable across every
//     ingest call (only the user prompt changes), so we attach
//     cache_control: {"type": "ephemeral"} to the tool block. This
//     turns the tool schema into a cache breakpoint and lets the
//     ingest pipeline amortise the schema over a run. We do NOT put
//     the cache breakpoint on the system prompt because Cortex's
//     current ingest path doesn't use a system prompt, and caching an
//     empty string is a no-op.
//   - Ping issues a trivial messages call (max_tokens=1, single "ping"
//     user message). 200 OK proves both reachability and that the API
//     key is valid; any other status returns an error with the status
//     code and a snippet of the response body.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Multi-provider LLM support" (Phase 2a)
package anthropic

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

// DefaultBaseURL is the production Anthropic API root. Exposed as a
// constant so the test harness and the Config default share a single
// source of truth.
const DefaultBaseURL = "https://api.anthropic.com"

// DefaultTimeout is the fallback per-request budget when the caller's
// context carries no deadline. Mirrors the ollama adapter so the two
// providers behave consistently on the non-context path.
const DefaultTimeout = 60 * time.Second

// anthropicVersion is the API version header value. Anthropic pins
// breaking changes behind this header; 2023-06-01 is the stable
// version the Messages API ships under.
const anthropicVersion = "2023-06-01"

// structuredToolName is the name of the synthetic tool we declare on
// every GenerateStructured call. It is referenced in two places (the
// tools array and tool_choice) so it lives in a constant.
const structuredToolName = "emit_structured_output"

// Config is the subset of Cortex config fields this adapter needs.
// BaseURL is configurable so httptest.Server can inject a local URL;
// leave it zero in production to get DefaultBaseURL.
type Config struct {
	APIKey       string        // x-api-key header value
	Model        string        // e.g. "claude-3-5-sonnet-20241022"
	MaxTokens    int           // required by Anthropic on every call
	BaseURL      string        // optional override; defaults to DefaultBaseURL
	Timeout      time.Duration // optional; defaults to DefaultTimeout
	SystemPrompt string        // optional; omitted from the wire when empty
}

// HTTPClient is the live Generator implementation.
type HTTPClient struct {
	apiKey       string
	model        string
	maxTokens    int
	baseURL      string
	systemPrompt string
	timeout      time.Duration
	http         *http.Client
}

// NewHTTPClient builds an adapter from a Config. Zero-valued BaseURL
// and Timeout are replaced with sensible defaults; APIKey, Model, and
// MaxTokens are the caller's responsibility (we do not default them
// because their correct values depend on the operator's account).
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
		apiKey:       cfg.APIKey,
		model:        cfg.Model,
		maxTokens:    cfg.MaxTokens,
		baseURL:      base,
		systemPrompt: cfg.SystemPrompt,
		timeout:      timeout,
		http:         &http.Client{},
	}
}

// ctxWithDefault attaches a fallback deadline when the caller's
// context has none. Mirrors the pattern in internal/ollama so the two
// adapters behave identically from the caller's perspective.
func ctxWithDefault(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// message is the minimal /v1/messages request message shape. Cortex
// only sends single-turn user messages; multi-turn and assistant
// prefill are out of scope for Phase 2.
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// tool is the request-side tool declaration. input_schema is the
// caller's JSON schema passed through verbatim (json.RawMessage so we
// do not re-parse/re-marshal). cache_control is Anthropic's prompt-
// caching breakpoint knob; we attach it to the tool because the tool
// definition is the stable prefix across ingest calls.
type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// toolChoice forces the model to emit a specific tool. We use the
// "tool" variant (with a name) rather than "any" so there is no
// ambiguity in response parsing — the response is guaranteed to
// contain a tool_use block whose name matches structuredToolName.
type toolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// messagesRequest is the /v1/messages request body. All tool-related
// fields are omitempty so Generate (which passes none) produces a
// clean body with only model/max_tokens/messages.
type messagesRequest struct {
	Model      string      `json:"model"`
	MaxTokens  int         `json:"max_tokens"`
	System     string      `json:"system,omitempty"`
	Messages   []message   `json:"messages"`
	Tools      []tool      `json:"tools,omitempty"`
	ToolChoice *toolChoice `json:"tool_choice,omitempty"`
}

// contentBlock is a single element of the response content array.
// Anthropic returns a heterogeneous list (text blocks, tool_use
// blocks, etc.); we decode the union via Type and the optional
// Text/Name/Input fields. Input is json.RawMessage so a tool_use
// block's structured payload is preserved byte-for-byte.
type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// messagesResponse is the minimal shape of a successful /v1/messages
// response that Cortex consumes. The real response carries more
// fields (id, usage, stop_reason) but we do not surface them.
type messagesResponse struct {
	Content []contentBlock `json:"content"`
}

// errorResponse is Anthropic's non-200 envelope. We decode it to
// surface the provider-supplied message in our wrapped error so the
// operator sees a useful reason in cortex status output.
type errorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Generate posts an unstructured prompt and returns the first text
// block from the response. No tools, no tool_choice. The system
// prompt from Config is attached if non-empty.
func (c *HTTPClient) Generate(ctx context.Context, prompt string) (string, error) {
	req := messagesRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    c.systemPrompt,
		Messages:  []message{{Role: "user", Content: prompt}},
	}
	resp, err := c.doMessages(ctx, req)
	if err != nil {
		return "", err
	}
	for _, blk := range resp.Content {
		if blk.Type == "text" {
			return blk.Text, nil
		}
	}
	return "", fmt.Errorf("anthropic: generate response had no text block")
}

// GenerateStructured posts a prompt plus a forced tool call whose
// input_schema is the caller's schema. The returned string is the
// raw JSON bytes of the tool_use block's input field — i.e. the
// structured output Anthropic committed to under the schema.
//
// Why a synthetic tool: Anthropic's Messages API doesn't have a
// response_format=json_schema knob (unlike OpenAI). The canonical way
// to constrain output to a schema is to declare a tool whose
// input_schema is the schema and force the model to call it. The
// returned tool_use.input is guaranteed (by the provider) to
// conform.
func (c *HTTPClient) GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error) {
	req := messagesRequest{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    c.systemPrompt,
		Messages:  []message{{Role: "user", Content: prompt}},
		Tools: []tool{{
			Name:         structuredToolName,
			Description:  "Emit the requested structured output conforming to the provided schema",
			InputSchema:  schema,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}},
		ToolChoice: &toolChoice{Type: "tool", Name: structuredToolName},
	}
	resp, err := c.doMessages(ctx, req)
	if err != nil {
		return "", err
	}
	for _, blk := range resp.Content {
		if blk.Type == "tool_use" && blk.Name == structuredToolName {
			if len(blk.Input) == 0 {
				return "", fmt.Errorf("anthropic: tool_use block had empty input")
			}
			return string(blk.Input), nil
		}
	}
	return "", fmt.Errorf("anthropic: response had no tool_use block for %q", structuredToolName)
}

// Ping issues the smallest possible messages call. A 200 response
// proves the network path AND the API key (Anthropic rejects missing
// or invalid keys with 401 before dispatching the model), which is
// exactly the contract llm.Generator.Ping requires.
func (c *HTTPClient) Ping(ctx context.Context) error {
	req := messagesRequest{
		Model:     c.model,
		MaxTokens: 1,
		Messages:  []message{{Role: "user", Content: "ping"}},
	}
	_, err := c.doMessages(ctx, req)
	return err
}

// doMessages is the shared request/response helper used by Generate,
// GenerateStructured, and Ping. It owns header wiring, context
// deadline fallback, non-200 decoding, and body close.
func (c *HTTPClient) doMessages(ctx context.Context, req messagesRequest) (*messagesResponse, error) {
	ctx, cancel := ctxWithDefault(ctx, c.timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal messages body: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build messages request: %w", err)
	}
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: messages: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Try to decode the structured error envelope; if that fails,
		// fall back to the raw body (truncated) so the operator still
		// gets something actionable.
		var er errorResponse
		if jerr := json.Unmarshal(raw, &er); jerr == nil && er.Error.Message != "" {
			return nil, fmt.Errorf("anthropic: messages returned HTTP %d: %s", resp.StatusCode, er.Error.Message)
		}
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		return nil, fmt.Errorf("anthropic: messages returned HTTP %d: %s", resp.StatusCode, snippet)
	}

	var mr messagesResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		return nil, fmt.Errorf("anthropic: decode messages response: %w", err)
	}
	return &mr, nil
}
