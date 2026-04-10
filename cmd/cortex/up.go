// Command cortex — `cortex up` wire-up.
//
// This file binds the `cortex up` cobra command (constructed in
// commands.go) to the real orchestration in internal/infra. It owns
// the glue that turns a config.Config into concrete adapter clients
// and passes them to infra.Run. The readiness contract itself lives
// in internal/infra/up.go; everything in this file is environment
// assembly.
//
// Spec references:
//   docs/spec/cortex-spec.md §"cortex up Readiness Contract"
//   docs/spec/cortex-spec.md US-10 "Start managed infrastructure"
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/spf13/cobra"
)

// defaultEmbeddingModel / defaultGenerationModel are the Phase 1
// pinned model names from cortex-spec.md §"Pinned Service Versions".
// They are used when the operator has not overridden them in config.
const (
	defaultEmbeddingModel = "nomic-embed-text"
	// qwen3:4b-instruct is Alibaba's Qwen3 4B pure-instruct variant,
	// distinct from the default `qwen3:4b` tag which is the hybrid
	// thinking model (the two have different digests on the Ollama
	// registry; verified via direct manifest probe). The pure-instruct
	// variant avoids <think> tags in responses, keeping the JSON
	// parser in ollamaLinkProposer stable. Replaces the phantom
	// "llama3.1:8b-instruct" tag that never existed on the Ollama
	// registry and blocked fresh-machine `cortex up` runs (bead
	// cortex-3z1).
	defaultGenerationModel = "qwen3:4b-instruct"
)

// runUp is the RunE for `cortex up`. It is wired in commands.go
// newUpCmd() via a handoff so the stub and the real implementation
// can live in separate files without re-registering the cobra node.
func runUp(cmd *cobra.Command, _ []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return errs.Operational("HOME_NOT_FOUND",
			"cannot locate user home directory", err)
	}
	configPath := filepath.Join(home, ".cortex", "config.yaml")

	cfg, err := config.Load(configPath)
	if err != nil {
		return errs.Operational("CONFIG_LOAD_FAILED",
			"failed to load cortex configuration", err)
	}

	// Ensure the Neo4j bootstrap password exists up front so the Bolt
	// client can authenticate. infra.Run calls EnsureNeo4jPassword
	// again, which is idempotent and returns the same value on the
	// second call.
	password, _, err := infra.EnsureNeo4jPassword(configPath)
	if err != nil {
		return errs.Operational(infra.CodeCredentialWriteFailed,
			"failed to persist neo4j bootstrap credential", err)
	}

	weaviateClient := weaviate.NewHTTPClient(cfg.Endpoints.WeaviateHTTP, 5*time.Second)

	neoClient, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})
	if err != nil {
		return errs.Operational("NEO4J_CLIENT_INIT",
			"failed to initialise neo4j client", err)
	}
	defer neoClient.Close(context.Background())

	ollamaClient := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
		LinkDerivationTimeout: time.Duration(cfg.Timeouts.LinkDerivationSeconds) * time.Second,
		NumCtx:                cfg.Ollama.NumCtx,
	})

	composeFile := filepath.Join("docker", "docker-compose.yaml")

	err = infra.Run(cmd.Context(), infra.UpOptions{
		ComposeFile:     composeFile,
		ConfigPath:      configPath,
		StartupBudget:   infra.DefaultStartupBudget,
		EmbeddingModel:  defaultEmbeddingModel,
		GenerationModel: defaultGenerationModel,
		Docker:          infra.ExecDocker{},
		Weaviate:        weaviateClient,
		Neo4j:           neo4jAdapter{c: neoClient},
		Ollama:          ollamaAdapter{c: ollamaClient, endpoint: cfg.Endpoints.Ollama},
		OnPasswordGenerated: func(path string) {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"cortex: generated new Neo4j bootstrap password (stored in %s mode 0600)\n",
				path)
		},
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "cortex: managed stack is ready")
	return nil
}

// neo4jAdapter bridges the live neo4j.BoltClient (whose ProbeProcedures
// returns a rich ProcedureAvailability struct) to the narrow
// infra.Neo4jReady interface cortex up needs. GDS is considered
// "available" if any of the GDS procedures Cortex probes for returned
// true — the community-detection pipeline will downgrade to Louvain
// internally when Leiden is missing (FR-028).
type neo4jAdapter struct {
	c *neo4j.BoltClient
}

func (a neo4jAdapter) Ping(ctx context.Context) error {
	return a.c.Ping(ctx)
}

func (a neo4jAdapter) GDSAvailable(ctx context.Context) (bool, error) {
	avail, err := a.c.ProbeProcedures(ctx)
	if err != nil {
		return false, err
	}
	// PageRank is the canonical signal: it is required by the recall
	// pipeline and is baked into every supported GDS version. If
	// PageRank is absent, GDS itself is not loaded.
	return avail.PageRankStream || avail.LeidenStream || avail.LouvainStream, nil
}

// ollamaAdapter bridges ollama.HTTPClient (Ping only) to the
// infra.OllamaReady interface, which also needs ListModels. We do not
// extend the ollama.Client interface because tag enumeration is a
// lifecycle-only concern; the adapter delegates to the helper in
// internal/infra/ollama_probe.go.
type ollamaAdapter struct {
	c        *ollama.HTTPClient
	endpoint string
}

func (a ollamaAdapter) Ping(ctx context.Context) error {
	return a.c.Ping(ctx)
}

func (a ollamaAdapter) ListModels(ctx context.Context) ([]string, error) {
	return infra.ListOllamaModels(ctx, a.endpoint)
}
