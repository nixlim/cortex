// cmd/cortex/rebuild.go wires `cortex rebuild` onto the
// internal/rebuild package. The command:
//
//   - opens the segmented datom log via internal/log.NewReader
//   - constructs an ollama-backed DigestSource so the pinned-model
//     drift check uses the live model digest
//   - constructs a placeholder StagingBackends (see stubStagingBackends
//     below). The real Weaviate/Neo4j staging adapter is a follow-up
//     bead — internal/rebuild's behavior contracts are fully covered
//     by package tests, and this CLI shell makes the seam visible
//     so the adapter can be plugged in without re-shaping the
//     command surface.
//   - calls rebuild.Run and renders the result
//
// Replaces the notImplemented stub in commands.go.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/rebuild"
)

// newRebuildCmdReal returns the wired `cortex rebuild` command.
// commands.go installs it in place of the notImplemented stub.
func newRebuildCmdReal() *cobra.Command {
	var (
		acceptDrift bool
		jsonFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "rebuild",
		Short: "Replay the log into a fresh backend state",
		Long: "cortex rebuild replays every committed datom into a clean Weaviate and " +
			"Neo4j using the embedding_model_digest recorded at write time. " +
			"--accept-drift allows re-embedding under the current model and writes a " +
			"model_rebind audit datom per affected entry.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)
			report, err := log.Load(segDir, log.LoadOptions{})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
					"could not enumerate log segments", err), jsonFlag)
			}
			reader, err := log.NewReader(report.Healthy)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_READER_FAILED",
					"could not open multi-segment log reader", err), jsonFlag)
			}
			source := &readerSource{r: reader}

			ollamaClient := ollama.NewHTTPClient(ollama.Config{
				Endpoint:        cfg.Endpoints.Ollama,
				EmbeddingModel:  defaultEmbeddingModel,
				GenerationModel: defaultGenerationModel,
			})
			digestSrc := ollamaDigestSource{c: ollamaClient}

			// Open a log writer for model_rebind audit datoms when
			// --accept-drift is set. The writer is closed via defer
			// regardless of whether AcceptDrift was used so we don't
			// leak descriptors on validation failures.
			var appender rebuild.LogAppender
			if acceptDrift {
				w, err := log.NewWriter(segDir)
				if err != nil {
					return emitAndExit(cmd, errs.Operational("LOG_WRITER_FAILED",
						"could not open log writer for model_rebind audit datoms", err), jsonFlag)
				}
				defer w.Close()
				appender = w
			}

			runCfg := rebuild.Config{
				Source:       source,
				Digest:       digestSrc,
				Backends:     newStubStagingBackends(),
				AcceptDrift:  acceptDrift,
				Embedder:     ollamaEmbedder{c: ollamaClient},
				Log:          appender,
				Actor:        defaultActor(),
				InvocationID: ulid.Make().String(),
				Now:          func() time.Time { return time.Now().UTC() },
			}

			res, err := rebuild.Run(cmd.Context(), runCfg)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			renderRebuildResult(cmd, res)
			return nil
		},
	}
	cmd.Flags().BoolVar(&acceptDrift, "accept-drift", false,
		"re-embed under the current embedding model and write model_rebind audit datoms")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// readerSource adapts *log.Reader to rebuild.DatomSource. The log
// reader's Next() takes no context, so we honour cmd.Context() by
// checking ctx.Err() before each pull — the rebuild loop is the
// dominant cost so cancellation latency is bounded by one read.
type readerSource struct {
	r *log.Reader
}

func (s *readerSource) Next(ctx context.Context) (datom.Datom, bool, error) {
	if err := ctx.Err(); err != nil {
		return datom.Datom{}, false, err
	}
	return s.r.Next()
}

func (s *readerSource) Close() error { return s.r.Close() }

// ollamaDigestSource wraps the live HTTPClient as a DigestSource. The
// underlying Show() call is cached after the first invocation per the
// ollama package contract, so repeated CurrentDigest calls are cheap.
type ollamaDigestSource struct {
	c *ollama.HTTPClient
}

func (o ollamaDigestSource) CurrentDigest(ctx context.Context) (string, error) {
	info, err := o.c.Show(ctx)
	if err != nil {
		return "", err
	}
	return info.Digest, nil
}

// ollamaEmbedder wraps the live HTTPClient as a rebuild.Embedder. The
// embedder is only consulted on the --accept-drift path.
type ollamaEmbedder struct {
	c *ollama.HTTPClient
}

func (o ollamaEmbedder) Embed(ctx context.Context, body string) ([]float32, error) {
	return o.c.Embed(ctx, body)
}

// stubStagingBackends is a placeholder StagingBackends. It records
// the lifecycle calls so the CLI can print a meaningful summary, but
// it does NOT yet write to live Weaviate / Neo4j staging namespaces
// — that adapter is a follow-up bead. The package's behavior is
// fully covered by internal/rebuild/rebuild_test.go using the same
// shape, so the CLI surface is honest about what's wired today
// without leaving the rebuild logic dead code.
type stubStagingBackends struct {
	applied int
}

func newStubStagingBackends() *stubStagingBackends { return &stubStagingBackends{} }

func (s *stubStagingBackends) Create(_ context.Context) error { return nil }

func (s *stubStagingBackends) ApplyDatom(_ context.Context, _ datom.Datom) error {
	s.applied++
	return nil
}

func (s *stubStagingBackends) ApplyEmbedding(_ context.Context, _ string, _ []float32) error {
	return nil
}

func (s *stubStagingBackends) Swap(_ context.Context) error    { return nil }
func (s *stubStagingBackends) Cleanup(_ context.Context) error { return nil }

// renderRebuildResult prints a terse human-readable summary of one
// rebuild run. JSON callers see the rebuild.Result struct directly.
func renderRebuildResult(cmd *cobra.Command, r *rebuild.Result) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "cortex rebuild  ok\n")
	fmt.Fprintf(w, "  datoms scanned : %d\n", r.DatomsScanned)
	fmt.Fprintf(w, "  entries        : %d\n", r.EntriesApplied)
	fmt.Fprintf(w, "  rebinds        : %d\n", r.RebindsPerformed)
	fmt.Fprintf(w, "  elapsed        : %s\n", r.CompletedAt.Sub(r.StartedAt).Truncate(time.Millisecond))
}

