// Package llm defines the narrow, provider-agnostic interface the
// Cortex generation call sites (ingest summarizer, link proposer,
// concept extractor, reflector, analyzer, community summarizer, trail
// summarizer) depend on.
//
// Why a separate package from internal/ollama:
//
//   - FR-051 pins embedding_model_name and embedding_model_digest on
//     every Entry/Frame datom. Embedding therefore stays anchored to
//     the Ollama adapter because that is where the single-Show digest
//     capture lives. Generation has no digest invariant in the spec,
//     so the seam can be drawn cleanly between the two.
//   - The generation surface is small (Generate, GenerateStructured,
//     Ping) and does not need to know anything about Ollama's
//     /api/show, /api/tags, or model-name semantics. Keeping it free
//     of those concerns lets a remote provider (Anthropic, OpenAI)
//     implement the interface with a 200-line HTTP client.
//
// Phase 1 of the multi-provider LLM effort introduces this interface
// and migrates the adapter structs in cmd/cortex to depend on it.
// Construction points still build *ollama.HTTPClient in Phase 1 (which
// already satisfies llm.Generator). Phase 3 replaces those call sites
// with a config-driven factory.
package llm

import (
	"context"
	"encoding/json"
)

// Generator is the generation surface every LLM provider must
// implement. It is deliberately narrow: one unstructured completion
// method, one schema-constrained completion method, and a health
// probe. Streaming, multi-turn, and tool-calling beyond structured
// output are out of scope.
type Generator interface {
	// Generate posts a prompt and returns the raw completion text.
	// Callers are responsible for prompt templating (internal/prompts)
	// and response parsing.
	Generate(ctx context.Context, prompt string) (string, error)

	// GenerateStructured posts a prompt with a JSON schema and returns
	// a JSON string guaranteed (by the provider) to conform to the
	// schema. The schema is passed through to the provider verbatim;
	// each provider may apply a compatibility shim before sending it
	// on the wire (e.g. OpenAI strict-mode normalization).
	GenerateStructured(ctx context.Context, prompt string, schema json.RawMessage) (string, error)

	// Ping is a lightweight health probe used by doctor and status.
	// It MUST return nil when the provider is reachable and the
	// configured credentials are valid, non-nil otherwise. Ping must
	// not have side effects beyond the network round trip.
	Ping(ctx context.Context) error
}
