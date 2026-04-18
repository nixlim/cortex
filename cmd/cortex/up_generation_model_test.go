package main

import (
	"testing"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/llm"
)

// Regression test for cortex-se3: when llm.provider is a remote
// provider (anthropic / openai / openrouter), cortex up / doctor must
// not treat the absence of the local Ollama generation model as a
// failure — generation is routed off-host and the local model is
// never invoked. The decision is centralised in
// generationModelForProvider so the two wire-ups stay aligned.
func TestGenerationModelForProvider(t *testing.T) {
	cases := []struct {
		provider string
		want     string
	}{
		{"", defaultGenerationModel},
		{llm.ProviderOllama, defaultGenerationModel},
		{llm.ProviderAnthropic, ""},
		{llm.ProviderOpenAI, ""},
		{llm.ProviderOpenRouter, ""},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			cfg := config.Config{}
			cfg.LLM.Provider = tc.provider
			got := generationModelForProvider(cfg)
			if got != tc.want {
				t.Fatalf("provider=%q: got %q, want %q", tc.provider, got, tc.want)
			}
		})
	}
}
