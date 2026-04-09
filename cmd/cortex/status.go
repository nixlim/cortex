// Command cortex — `cortex status` wire-up.
//
// This file binds the `cortex status` cobra command to the orchestration
// in internal/infra. It constructs shallow Weaviate / Neo4j / Ollama
// probes (Ping + Version) and hands them to infra.Check, then renders
// the resulting Report as either a human table or a JSON object on
// stdout per the --json flag.
//
// Spec references:
//   docs/spec/cortex-spec.md US-10 scenarios "Status reports running
//   and degraded services" / BDD rows 53-54
//   docs/spec/cortex-spec.md line 208 (per-component payload shape)
package main

import (
	"context"
	"encoding/json"
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

// runStatus is the RunE for `cortex status`. The jsonOut flag is
// captured in commands.go newStatusCmd() and forwarded here so this
// file stays free of cobra-flag bookkeeping.
func runStatus(cmd *cobra.Command, _ []string, jsonOut bool) error {
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

	weaviateClient := weaviate.NewHTTPClient(cfg.Endpoints.WeaviateHTTP, 1*time.Second)

	// Status does not need Neo4j credentials for a shallow Bolt ping to
	// succeed — the driver authenticates before running queries — but
	// EnsureNeo4jPassword is idempotent so we read the existing value
	// without regenerating it.
	password, _, _ := infra.EnsureNeo4jPassword(configPath)
	neoClient, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      1 * time.Second,
		MaxPoolSize:  2,
	})
	if err != nil {
		// Even a client-construction failure should produce a report
		// that marks Neo4j as down rather than aborting the whole
		// command; status is a reporting tool, not a gate.
		neoClient = nil
	}
	if neoClient != nil {
		defer neoClient.Close(context.Background())
	}

	ollamaClient := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      1 * time.Second,
		LinkDerivationTimeout: 1 * time.Second,
	})

	opts := infra.StatusOptions{
		Weaviate:   weaviateStatusAdapter{c: weaviateClient, endpoint: cfg.Endpoints.WeaviateHTTP},
		Neo4j:      neo4jStatusAdapter{c: neoClient},
		Ollama:     ollamaStatusAdapter{c: ollamaClient, endpoint: cfg.Endpoints.Ollama},
		CortexHome: filepath.Join(home, ".cortex"),
		Timeout:    infra.DefaultStatusTimeout,
	}

	report := infra.Check(cmd.Context(), opts)

	if jsonOut {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderHumanStatus(cmd, report)
	return nil
}

// renderHumanStatus writes a terse, column-aligned summary suitable for
// operators reading a terminal. The JSON shape is the source of truth;
// this is only a friendlier projection of the same data.
func renderHumanStatus(cmd *cobra.Command, r infra.Report) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "cortex status  (%d ms)\n", r.ElapsedMS)
	fmt.Fprintf(w, "  weaviate : %-9s %s\n", r.Weaviate.Status, r.Weaviate.Version)
	if r.Weaviate.Error != "" {
		fmt.Fprintf(w, "             %s\n", r.Weaviate.Error)
	}
	fmt.Fprintf(w, "  neo4j    : %-9s %s\n", r.Neo4j.Status, r.Neo4j.Version)
	if r.Neo4j.Error != "" {
		fmt.Fprintf(w, "             %s\n", r.Neo4j.Error)
	}
	fmt.Fprintf(w, "  ollama   : %-9s %s\n", r.Ollama.Status, r.Ollama.Version)
	if r.Ollama.Error != "" {
		fmt.Fprintf(w, "             %s\n", r.Ollama.Error)
	}
	fmt.Fprintf(w, "  disk     : %d bytes under ~/.cortex\n", r.DiskUsageBytes)
}

// ---------------------------------------------------------------------------
// Probe adapters — bridge live clients to the narrow infra interfaces.
// ---------------------------------------------------------------------------

type weaviateStatusAdapter struct {
	c        *weaviate.HTTPClient
	endpoint string
}

func (a weaviateStatusAdapter) Ready(ctx context.Context) error {
	return a.c.Ready(ctx)
}

func (a weaviateStatusAdapter) Version(ctx context.Context) (string, error) {
	return infra.FetchWeaviateVersion(ctx, a.endpoint)
}

type neo4jStatusAdapter struct {
	c *neo4j.BoltClient
}

func (a neo4jStatusAdapter) Ping(ctx context.Context) error {
	if a.c == nil {
		return fmt.Errorf("neo4j: client not initialised")
	}
	return a.c.Ping(ctx)
}

func (a neo4jStatusAdapter) Version(ctx context.Context) (string, error) {
	if a.c == nil {
		return "", fmt.Errorf("neo4j: client not initialised")
	}
	rows, err := a.c.QueryGraph(ctx,
		`CALL dbms.components() YIELD name, versions, edition
         RETURN name + "-" + edition AS product, versions[0] AS version
         LIMIT 1`, nil)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("neo4j: dbms.components() returned no rows")
	}
	if v, ok := rows[0]["version"].(string); ok {
		return v, nil
	}
	return "", fmt.Errorf("neo4j: version column missing")
}

type ollamaStatusAdapter struct {
	c        *ollama.HTTPClient
	endpoint string
}

func (a ollamaStatusAdapter) Ping(ctx context.Context) error {
	return a.c.Ping(ctx)
}

func (a ollamaStatusAdapter) Version(ctx context.Context) (string, error) {
	return infra.FetchOllamaVersion(ctx, a.endpoint)
}
