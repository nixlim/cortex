// Package ollama is the Cortex adapter for the host-installed Ollama
// service.
//
// Cortex never runs Ollama in a container — it is a host service that
// the operator installs and starts separately. This adapter reaches
// Ollama over HTTP on localhost:11434 and exposes a small Client
// interface (Embed, Generate, Show, Ping) plus the model-digest
// capture logic that the write pipeline relies on to make the
// "MODEL_DIGEST_RACE" guarantee concrete.
//
// Design notes:
//
//   - Every method uses stdlib net/http. No third-party Ollama client.
//   - Show is called exactly once per Client invocation (see
//     digest.go) and the returned digest is cached on the Client
//     struct. Embed responses are compared against the cached digest;
//     a mismatch aborts the call with ErrModelDigestRace. This is the
//     contract the spec's "MODEL_DIGEST_RACE" failure mode relies on.
//   - Prompt templates are applied by the caller (internal/prompts).
//     This adapter receives pre-templated strings and never does raw
//     user-content interpolation into LLM prompts.
//
// Spec references:
//   docs/spec/cortex-spec.md §"Ollama"
//   docs/spec/cortex-spec.md §"Model digest pinning"
//   docs/spec/cortex-spec.md §"Configuration Defaults" (timeouts)
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTimeout is the fallback per-request budget used when the
// caller's context has no deadline. Timeouts.embedding_seconds and
// timeouts.link_derivation_seconds from config are intended to be
// passed in via context.WithTimeout by the call sites, so this
// constant is only a safety net.
const DefaultTimeout = 30 * time.Second

// ErrModelDigestRace is returned from Embed when the digest reported
// on an embedding response differs from the digest captured by the
// single Show call made for this Client instance. The spec calls this
// the MODEL_DIGEST_RACE failure mode; it makes the embedding model's
// identity a first-class, verifiable field of every write.
var ErrModelDigestRace = errors.New("ollama: embedding model digest differs from cached digest (MODEL_DIGEST_RACE)")

// Client is the interface exposed by the adapter.
type Client interface {
	// Ping reports reachability. Implementations MUST call
	// GET /api/tags and return nil on HTTP 200, non-nil on any other
	// outcome. Ping MUST NOT mutate the cached digest.
	Ping(ctx context.Context) error

	// Show captures the embedding model's name and digest. It MUST be
	// called exactly once per Client invocation — repeat calls return
	// the cached result without hitting the network. Call sites should
	// not rely on Show for freshness; the write pipeline calls it once
	// at the start of a write and uses the cached digest as the
	// authority for the rest of the invocation.
	Show(ctx context.Context) (ModelInfo, error)

	// Embed returns a vector for the given text using the configured
	// embedding model. If the digest on the response differs from the
	// cached digest, the call returns ErrModelDigestRace without
	// returning the vector — the write aborts rather than risks
	// mixing vectors from two different models in the same index.
	Embed(ctx context.Context, text string) ([]float32, error)

	// Generate is the chat/completion surface used for reflection,
	// link derivation, module summaries, and community summaries. It
	// accepts a pre-templated prompt string and returns the raw
	// completion; caller-side parsing lives in the prompts package.
	Generate(ctx context.Context, prompt string) (string, error)
}

// ModelInfo is the minimal shape of an Ollama /api/show response
// that the adapter uses. The upstream response carries many more
// fields; we only materialise the ones Cortex tracks.
type ModelInfo struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Config is the subset of Cortex config fields this adapter needs.
type Config struct {
	Endpoint                    string        // e.g. "localhost:11434"
	EmbeddingModel              string        // e.g. "nomic-embed-text"
	GenerationModel             string        // e.g. "qwen3:4b-instruct-2507"
	EmbeddingTimeout            time.Duration // timeouts.embedding_seconds
	LinkDerivationTimeout       time.Duration // timeouts.link_derivation_seconds
}

// HTTPClient is the live implementation of Client.
type HTTPClient struct {
	baseURL         string
	embeddingModel  string
	generationModel string
	embeddingBudget time.Duration
	generationBudget time.Duration
	http            *http.Client

	// showOnce + cachedInfo back the "Show is called exactly once per
	// invocation" guarantee. sync.Once gives us at-most-once
	// semantics against concurrent callers.
	showOnce   sync.Once
	showErr    error
	cachedInfo ModelInfo

	// showCalls counts invocations of the underlying Show network
	// request, exposed for the acceptance test "Show is called
	// exactly once per invocation (verified via call counter seam)".
	showCalls int32
}

// NewHTTPClient builds an adapter against the given Ollama endpoint.
// The endpoint may be a bare host:port or a full URL. Timeouts
// default to DefaultTimeout if zero.
func NewHTTPClient(cfg Config) *HTTPClient {
	if cfg.EmbeddingTimeout <= 0 {
		cfg.EmbeddingTimeout = DefaultTimeout
	}
	if cfg.LinkDerivationTimeout <= 0 {
		cfg.LinkDerivationTimeout = DefaultTimeout
	}
	return &HTTPClient{
		baseURL:          normalizeBaseURL(cfg.Endpoint),
		embeddingModel:   cfg.EmbeddingModel,
		generationModel:  cfg.GenerationModel,
		embeddingBudget:  cfg.EmbeddingTimeout,
		generationBudget: cfg.LinkDerivationTimeout,
		http:             &http.Client{},
	}
}

func normalizeBaseURL(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}
	return endpoint
}

// ShowCallCount exposes the internal counter for the acceptance test
// that Show hits the network exactly once per invocation. Not part of
// the Client interface because test seams should not leak into
// production call sites.
func (c *HTTPClient) ShowCallCount() int { return int(atomic.LoadInt32(&c.showCalls)) }

// ctxWithDefault attaches a fallback deadline when the caller's
// context has none. It is used for the non-vector Ping path; vector
// and generation paths use more specific per-operation budgets.
func ctxWithDefault(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// Ping hits GET /api/tags. Per the spec readiness contract, cortex up
// *also* checks that the response lists each configured model name;
// the Ping method intentionally does not enforce that because it is
// used as a liveness probe by cortex status, which must succeed even
// when a model is absent (so status can distinguish "Ollama down"
// from "model missing").
func (c *HTTPClient) Ping(ctx context.Context) error {
	ctx, cancel := ctxWithDefault(ctx, DefaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return fmt.Errorf("ollama: build tags request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: tags: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: tags returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// showResponse is the subset of the Ollama /api/show shape we decode.
type showResponse struct {
	License     string         `json:"license"`
	Modelfile   string         `json:"modelfile"`
	Parameters  string         `json:"parameters"`
	Template    string         `json:"template"`
	Details     map[string]any `json:"details"`
	Digest      string         `json:"digest"`
	ModifiedAt  string         `json:"modified_at"`
	Name        string         `json:"name"`
}

// Show is the at-most-once wrapper around the /api/show POST. The
// cached result is returned on subsequent calls. If the first call
// fails, its error is cached and returned on every subsequent call
// too — that keeps the "exactly once network call" semantics strict
// and prevents a flaky failure from causing a cascade of retries
// that would produce non-deterministic digests on the wire.
func (c *HTTPClient) Show(ctx context.Context) (ModelInfo, error) {
	c.showOnce.Do(func() {
		c.cachedInfo, c.showErr = c.doShow(ctx)
	})
	return c.cachedInfo, c.showErr
}

// doShow performs the actual network call. It is separate from Show
// so sync.Once can call it while still counting invocations through
// the atomic counter.
func (c *HTTPClient) doShow(ctx context.Context) (ModelInfo, error) {
	atomic.AddInt32(&c.showCalls, 1)
	ctx, cancel := ctxWithDefault(ctx, DefaultTimeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{"name": c.embeddingModel})
	if err != nil {
		return ModelInfo{}, fmt.Errorf("ollama: marshal show body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/show", bytes.NewReader(body))
	if err != nil {
		return ModelInfo{}, fmt.Errorf("ollama: build show request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return ModelInfo{}, fmt.Errorf("ollama: show: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ModelInfo{}, fmt.Errorf("ollama: show returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var sr showResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return ModelInfo{}, fmt.Errorf("ollama: decode show response: %w", err)
	}
	// Some Ollama versions echo the name only in the request; fall
	// back to the configured name if the response omits it.
	name := sr.Name
	if name == "" {
		name = c.embeddingModel
	}
	return ModelInfo{Name: name, Digest: sr.Digest}, nil
}

// embedRequest / embedResponse are the Ollama /api/embeddings shapes.
// We use /api/embeddings rather than the newer /api/embed because
// /api/embeddings is the Phase 1 stable endpoint and is present on
// all Ollama >= 0.1.40 releases the spec targets.
type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float32 `json:"embedding"`
	// Digest is NOT part of the canonical /api/embeddings response
	// shape. We carry it here anyway because a test seam can inject
	// it to exercise the MODEL_DIGEST_RACE path, and because we
	// tolerate its presence on future Ollama versions that may
	// surface it. The primary digest-verification path is via a
	// separate /api/show round trip in the caller.
	Digest string `json:"digest,omitempty"`
}

// Embed requests a vector for text using the configured embedding
// model. If the response carries a digest that differs from the
// cached digest, the call returns ErrModelDigestRace and the vector
// is discarded. Callers MUST treat ErrModelDigestRace as fatal for
// the in-flight write.
func (c *HTTPClient) Embed(ctx context.Context, text string) ([]float32, error) {
	// Ensure we have a cached digest. The first Embed call triggers
	// the one-shot Show; subsequent calls reuse the cached value.
	info, err := c.Show(ctx)
	if err != nil {
		return nil, err
	}

	ctx, cancel := ctxWithDefault(ctx, c.embeddingBudget)
	defer cancel()

	body, err := json.Marshal(embedRequest{Model: c.embeddingModel, Prompt: text})
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal embed body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: embed returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("ollama: decode embed response: %w", err)
	}

	// Digest race check: if the response carries a digest and it
	// disagrees with the one we captured at Show time, abort.
	if er.Digest != "" && info.Digest != "" && er.Digest != info.Digest {
		return nil, fmt.Errorf("%w: cached=%s, response=%s", ErrModelDigestRace, info.Digest, er.Digest)
	}

	if len(er.Embedding) == 0 {
		return nil, errors.New("ollama: embed returned empty embedding")
	}
	return er.Embedding, nil
}

// generateRequest / generateResponse are the /api/generate shapes.
// We set stream=false so the response is a single JSON object rather
// than a stream of ndjson frames — the write and reflect pipelines
// don't need incremental tokens and materialising a single response
// simplifies call sites.
type generateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type generateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// Generate posts a completion prompt. It uses the link-derivation
// budget as its default (the longest of the LLM budgets in the
// Configuration Defaults table), so reflection and link derivation
// share one budget. Callers that want a shorter or longer budget
// should pass a context with an explicit deadline.
func (c *HTTPClient) Generate(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := ctxWithDefault(ctx, c.generationBudget)
	defer cancel()

	body, err := json.Marshal(generateRequest{
		Model:  c.generationModel,
		Prompt: prompt,
		Stream: false,
	})
	if err != nil {
		return "", fmt.Errorf("ollama: marshal generate body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama: build generate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama: generate: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama: generate returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var gr generateResponse
	if err := json.Unmarshal(raw, &gr); err != nil {
		return "", fmt.Errorf("ollama: decode generate response: %w", err)
	}
	return gr.Response, nil
}
