// Factory for the Generator interface. This file is the single place
// where Cortex consults config.LLM.Provider and decides which concrete
// HTTP client to construct. Every cmd/cortex generation call site
// should go through NewGenerator rather than building a provider
// client by hand, so switching providers in config.yaml takes effect
// everywhere without touching command wiring.
//
// Design notes:
//
//   - API keys are read from environment variables here, never from
//     the config file. The config file only carries the NAME of the
//     env var (api_key_env). This mirrors the Neo4j password contract
//     (FR / US-10.7) and keeps rotation cheap: operators rotate a
//     secret by exporting the new value, no YAML edit required.
//   - Unknown providers, missing API keys, and (for the remote
//     providers) empty models all produce a LLM_CONFIG_INVALID error
//     so that `cortex <cmd>` fails fast at startup rather than later
//     in the middle of a pipeline.
//   - Ollama stays the default to preserve the local-only profile
//     that Phase 1-3 of the multi-provider plan assume.
package llm

import (
	"fmt"
	"os"
	"time"

	"github.com/nixlim/cortex/internal/anthropic"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/openai"
	"github.com/nixlim/cortex/internal/openrouter"
)

// ProviderOllama, ProviderAnthropic, ProviderOpenAI are the legal
// values of config.LLM.Provider. They are exported so doctor/status
// code paths can reference them without stringly-typing.
const (
	ProviderOllama     = "ollama"
	ProviderAnthropic  = "anthropic"
	ProviderOpenAI     = "openai"
	ProviderOpenRouter = "openrouter"
)

// NewGenerator builds the LLM Generator the call sites in cmd/cortex
// use for every non-embedding generation call. The returned value
// satisfies the Generator interface regardless of which provider was
// selected. The caller is expected to have already called
// config.Load so cfg carries the merged user+default values.
//
// For the ollama provider, budget is used as the per-request
// generation budget (passed through to ollama.Config.LinkDerivationTimeout,
// which the adapter uses as its generation budget). For the remote
// providers, the same duration is used as the HTTP client timeout.
// Pass 0 to let each provider fall back to its own default.
func NewGenerator(cfg config.Config, budget time.Duration) (Generator, error) {
	switch cfg.LLM.Provider {
	case "", ProviderOllama:
		// Empty string means "defaults omitted the field" — treat as
		// ollama so a pristine config.yaml still works.
		return ollama.NewHTTPClient(ollama.Config{
			Endpoint:              cfg.Endpoints.Ollama,
			EmbeddingModel:        "",
			GenerationModel:       ollamaGenerationModel(cfg),
			EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
			LinkDerivationTimeout: budget,
			NumCtx:                cfg.Ollama.NumCtx,
		}), nil

	case ProviderAnthropic:
		key, err := readAPIKey(cfg.LLM.Anthropic.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		if cfg.LLM.Anthropic.Model == "" {
			return nil, fmt.Errorf("llm: anthropic provider requires llm.anthropic.model")
		}
		return anthropic.NewHTTPClient(anthropic.Config{
			APIKey:    key,
			Model:     cfg.LLM.Anthropic.Model,
			MaxTokens: cfg.LLM.Anthropic.MaxTokens,
			BaseURL:   cfg.LLM.Anthropic.BaseURL,
			Timeout:   budget,
		}), nil

	case ProviderOpenAI:
		key, err := readAPIKey(cfg.LLM.OpenAI.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		if cfg.LLM.OpenAI.Model == "" {
			return nil, fmt.Errorf("llm: openai provider requires llm.openai.model")
		}
		return openai.NewHTTPClient(openai.Config{
			APIKey:    key,
			Model:     cfg.LLM.OpenAI.Model,
			MaxTokens: cfg.LLM.OpenAI.MaxTokens,
			BaseURL:   cfg.LLM.OpenAI.BaseURL,
			Timeout:   budget,
		}), nil

	case ProviderOpenRouter:
		key, err := readAPIKey(cfg.LLM.OpenRouter.APIKeyEnv)
		if err != nil {
			return nil, err
		}
		if cfg.LLM.OpenRouter.Model == "" {
			return nil, fmt.Errorf("llm: openrouter provider requires llm.openrouter.model (e.g. anthropic/claude-sonnet-4.5)")
		}
		return openrouter.NewHTTPClient(openrouter.Config{
			APIKey:      key,
			Model:       cfg.LLM.OpenRouter.Model,
			MaxTokens:   cfg.LLM.OpenRouter.MaxTokens,
			BaseURL:     cfg.LLM.OpenRouter.BaseURL,
			Timeout:     budget,
			HTTPReferer: cfg.LLM.OpenRouter.HTTPReferer,
			XTitle:      cfg.LLM.OpenRouter.XTitle,
		}), nil

	default:
		return nil, fmt.Errorf("llm: unknown provider %q (want one of: ollama, anthropic, openai, openrouter)", cfg.LLM.Provider)
	}
}

// readAPIKey fetches the env-var-named secret and returns a clean
// error if the name is empty or the variable is unset/empty. The
// error message names the env var so operators know which shell
// export is missing without digging through the factory.
func readAPIKey(envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("llm: api_key_env is empty in config")
	}
	v := os.Getenv(envName)
	if v == "" {
		return "", fmt.Errorf("llm: env var %s is not set (required for selected provider)", envName)
	}
	return v, nil
}

// ollamaGenerationModel is kept as a private helper so that if the
// cmd/cortex `defaultGenerationModel` constant ever moves into config,
// there is exactly one place to update. For now it returns the
// hardcoded default used by newOllamaClient.
func ollamaGenerationModel(_ config.Config) string {
	// This tracks defaultGenerationModel in cmd/cortex/commands.go
	// (qwen3:4b-instruct-2507). When that constant moves into
	// cfg.Ollama, this helper should read it from there. Until then,
	// returning an empty string would break the ollama adapter, so
	// we duplicate the name here and add a TODO note.
	return "qwen3:4b-instruct-2507"
}
