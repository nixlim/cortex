// cmd/cortex/ingest.go wires `cortex ingest`, `cortex ingest status`,
// and `cortex ingest resume` onto the internal/ingest pipeline.
//
// The file is kept as a thin wiring layer: ingest.Pipeline owns the
// orchestration (walk, group, bounded summarize, stable write, trail
// synthesis, per-project state persistence); this file only builds
// the minimum production adapter set it needs:
//
//   - ollamaSummarizer: prompts.Render(NameModuleSummary) + Ollama Generate
//   - writePipelineEntryWriter: write.Pipeline.Observe kind=Observation
//   - ingestTrailAppender: direct log.Writer append of begin/end datoms
//     using the spec-defined "trail:ingest:<project>:<rfc3339>" id
//   - jsonFileStateStore: per-project JSON under ~/.cortex/state/ingest
//   - walkerWalk: walker.Walk → languages.File adapter
//
// Post-ingest reflection is intentionally left nil in Phase 1 — the
// spec allows a "graceful skip" until reflect is wired.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-2, FR-022/023/024/049
//	bead cortex-4kq.51
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/ingest"
	"github.com/nixlim/cortex/internal/languages"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/llm"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/opslog"
	"github.com/nixlim/cortex/internal/prompts"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/trail"
	"github.com/nixlim/cortex/internal/walker"
	"github.com/nixlim/cortex/internal/weaviate"
	"github.com/nixlim/cortex/internal/write"
)

// newIngestCmdReal returns the wired `cortex ingest` command tree.
// It replaces the notImplemented stub in commands.go.
func newIngestCmdReal() *cobra.Command {
	var (
		project  string
		commit   string
		dryRun   bool
		resume   bool
		jsonFlag bool
		quiet    bool
	)
	cmd := &cobra.Command{
		Use:   "ingest <path>",
		Short: "Walk a repository and ingest module summaries",
		Long: "cortex ingest walks the project root, groups files by language " +
			"strategy, summarizes each module with Ollama, writes one episodic " +
			"entry per module, and synthesizes an ingest trail. Re-running the " +
			"command is idempotent: modules already ingested are skipped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				return emitAndExit(cmd, errs.Validation("MISSING_PROJECT_NAME",
					"cortex ingest requires --project", nil), jsonFlag)
			}
			if err := ensureCortexIgnore(args[0]); err != nil {
				return emitAndExit(cmd, errs.Operational("CORTEXIGNORE_INIT_FAILED",
					"could not create .cortexignore at project root", err), jsonFlag)
			}
			cfg, segDir, err := loadIngestConfig()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			pipeline, cleanup, err := buildIngestPipeline(cfg, segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			pipeline.Progress = newIngestProgressReporter(cmd.ErrOrStderr(), quiet)

			res, err := pipeline.Ingest(cmd.Context(), ingest.Request{
				ProjectRoot: args[0],
				ProjectName: project,
				CommitSHA:   commit,
				DryRun:      dryRun,
				Resume:      resume,
			})
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			return renderIngestResult(cmd, res, jsonFlag)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "required project name for scoping")
	cmd.Flags().StringVar(&commit, "commit", "", "optional commit SHA to record in ingest state")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "walk + summarize without writing entries")
	cmd.Flags().BoolVar(&resume, "resume", false, "resume an earlier interrupted run (same as a plain re-run)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress per-module stderr progress lines")

	cmd.AddCommand(newIngestStatusCmd())
	cmd.AddCommand(newIngestResumeCmd())
	return cmd
}

// newIngestStatusCmd wires `cortex ingest status` onto Pipeline.Status.
// Reads per-project state without side effects.
func newIngestStatusCmd() *cobra.Command {
	var project string
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report last-ingested commit and counts for a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				return emitAndExit(cmd, errs.Validation("MISSING_PROJECT_NAME",
					"cortex ingest status requires --project", nil), jsonFlag)
			}
			cfg, segDir, err := loadIngestConfig()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			pipeline, cleanup, err := buildIngestPipeline(cfg, segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			state, ok, err := pipeline.Status(cmd.Context(), project)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			inProgress := state.RunInProgress()
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"project":               project,
					"found":                 ok,
					"in_progress":           inProgress,
					"run_started_at":        state.RunStartedAt,
					"last_commit_sha":       state.LastCommitSHA,
					"last_ingested_at":      state.LastIngestedAt,
					"last_trail_id":         state.LastTrailID,
					"total_entries_written": state.TotalEntriesWritten,
					"completed_modules":     len(state.CompletedModuleIDs),
				})
			}
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "no ingest state for project %q\n", project)
				return nil
			}
			if inProgress {
				fmt.Fprintf(cmd.OutOrStdout(),
					"project=%s RUN IN PROGRESS started=%s completed=%d\n",
					project,
					state.RunStartedAt.Format(time.RFC3339),
					len(state.CompletedModuleIDs))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"project=%s entries=%d modules=%d last_commit=%s last_trail=%s last_at=%s\n",
				project, state.TotalEntriesWritten, len(state.CompletedModuleIDs),
				state.LastCommitSHA, state.LastTrailID,
				state.LastIngestedAt.Format(time.RFC3339))
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "required project name")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// newIngestResumeCmd wires `cortex ingest resume`. Semantically this is
// identical to a plain re-run because Pipeline.Ingest is idempotent on
// previously completed modules, but keeping a dedicated verb matches
// the documented CLI surface and lets operators express intent.
func newIngestResumeCmd() *cobra.Command {
	var (
		project  string
		jsonFlag bool
		quiet    bool
	)
	cmd := &cobra.Command{
		Use:   "resume <path>",
		Short: "Process only missing modules for a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				return emitAndExit(cmd, errs.Validation("MISSING_PROJECT_NAME",
					"cortex ingest resume requires --project", nil), jsonFlag)
			}
			if err := ensureCortexIgnore(args[0]); err != nil {
				return emitAndExit(cmd, errs.Operational("CORTEXIGNORE_INIT_FAILED",
					"could not create .cortexignore at project root", err), jsonFlag)
			}
			cfg, segDir, err := loadIngestConfig()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			pipeline, cleanup, err := buildIngestPipeline(cfg, segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			pipeline.Progress = newIngestProgressReporter(cmd.ErrOrStderr(), quiet)

			res, err := pipeline.Ingest(cmd.Context(), ingest.Request{
				ProjectRoot: args[0],
				ProjectName: project,
				Resume:      true,
			})
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			return renderIngestResult(cmd, res, jsonFlag)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "required project name")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress per-module stderr progress lines")
	return cmd
}

// newIngestProgressReporter builds an ingest.Pipeline Progress callback
// that prints a single stderr line per module completion and, when an
// ops.log writer is available, also writes a structured INGEST_MODULE_DONE
// record through internal/opslog. When quiet is true the stderr side is
// silenced but the opslog side still fires — the assumption is that
// operators who set --quiet still want durable structured telemetry.
// See cortex-so5.
func newIngestProgressReporter(stderr io.Writer, quiet bool) func(ingest.ProgressEvent) {
	opsWriter := tryOpenOpsLogWriter()
	return func(ev ingest.ProgressEvent) {
		if !quiet {
			tag := "ok  "
			if ev.Err != nil {
				tag = "fail"
			}
			fmt.Fprintf(stderr,
				"[%3d/%-3d] %s  %-8s  %s  (%4.1fs, %dB)\n",
				ev.DoneCount, ev.TotalCount, tag,
				ev.Language, ev.ModuleID,
				ev.Elapsed.Seconds(), ev.ByteCount)
			if ev.Err != nil {
				fmt.Fprintf(stderr, "           error: %v\n", ev.Err)
			}
		}
		if opsWriter != nil {
			level := opslog.LevelInfo
			errStr := ""
			if ev.Err != nil {
				level = opslog.LevelError
				errStr = ev.Err.Error()
			}
			_ = opsWriter.Write(opslog.Event{
				Level:     level,
				Component: "ingest",
				Message: fmt.Sprintf("INGEST_MODULE_DONE module=%s lang=%s done=%d total=%d elapsed_ms=%d bytes=%d",
					ev.ModuleID, ev.Language, ev.DoneCount, ev.TotalCount, ev.Elapsed.Milliseconds(), ev.ByteCount),
				Error: errStr,
			})
		}
	}
}

// tryOpenOpsLogWriter opens ~/.cortex/ops.log through the internal/opslog
// writer and returns nil on any failure (no home dir, no permission, bad
// config). The ingest CLI treats opslog as best-effort telemetry: if the
// file cannot be opened, per-module progress still prints to stderr and
// the run proceeds.
func tryOpenOpsLogWriter() *opslog.Writer {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	w, err := opslog.New(opslog.Options{
		Path:         filepath.Join(home, ".cortex", "ops.log"),
		InvocationID: ulid.Make().String(),
	})
	if err != nil {
		return nil
	}
	return w
}

// loadIngestConfig centralizes the config load + segment dir expansion
// all three ingest subcommands share.
func loadIngestConfig() (config.Config, string, error) {
	cfg, err := config.Load(defaultConfigPath())
	if err != nil {
		return config.Config{}, "", errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err)
	}
	return cfg, expandHome(cfg.Log.SegmentDir), nil
}

// buildIngestPipeline constructs an ingest.Pipeline with the production
// adapters. The cleanup closure closes the log writer and the Neo4j
// bolt client.
func buildIngestPipeline(cfg config.Config, segDir string) (*ingest.Pipeline, func(), error) {
	writer, err := log.NewWriter(segDir)
	if err != nil {
		return nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}

	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		_ = writer.Close()
		return nil, func() {}, errs.Operational("SECRETS_INIT_FAILED",
			"could not initialize secret detector", err)
	}

	// The ingest summarizer performs a 32K-context structured-output
	// call per module which on a local Ollama can take several minutes
	// per request — so we use cfg.Timeouts.IngestSummarySeconds as the
	// per-generation budget. Phase 3 split this into two clients:
	// a generator built via the provider factory, and an embedder
	// pinned to Ollama (FR-051 embedding digest invariant).
	generator, err := newGenerator(cfg, time.Duration(cfg.Timeouts.IngestSummarySeconds)*time.Second)
	if err != nil {
		_ = writer.Close()
		return nil, func() {}, errs.Operational("LLM_CONFIG_INVALID",
			"could not construct LLM generator", err)
	}
	ollamaClient := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
		LinkDerivationTimeout: time.Duration(cfg.Timeouts.IngestSummarySeconds) * time.Second,
		NumCtx:                cfg.Ollama.NumCtx,
	})

	weaviateClient := newWeaviateClient(cfg)

	// Open a Bolt client for the Neo4j BackendApplier so the ingest
	// write pipeline mirrors observe: concept extraction (cortex-v4g),
	// link derivation, and entry-node materialization all require
	// Neo4j. Failure here degrades to "no neo4j applier" and the
	// log commit remains authoritative — self-heal will replay the
	// missing rows on the next command (FR-004).
	cfgPath := defaultConfigPath()
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, boltErr := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})

	cleanup := func() {
		_ = writer.Close()
		if bolt != nil {
			_ = bolt.Close(context.Background())
		}
	}

	var neoApplier write.BackendApplier
	if boltErr == nil {
		neoApplier = neo4j.NewBackendApplier(bolt)
	}
	weaviateApplier := weaviate.NewBackendApplier(weaviateClient)

	// The underlying write.Pipeline shares the log writer and runs one
	// Observe per module. Reusing the same embedder adapter as
	// cmd/cortex/observe.go so the FR-051 model-digest datoms appear
	// on ingest entries as well.
	//
	// IMPORTANT: this field set MUST mirror buildObservePipeline in
	// observe.go. Earlier this file omitted Neo4j/Weaviate/Neighbors/
	// LinkProposer entirely, which left ingested entries without
	// concept links (no MENTIONS edges) and without vectors in
	// Weaviate, so cortex recall returned 0 results for everything.
	// See bead cortex-v4g.
	embedder := &observeEmbedder{c: ollamaClient, model: defaultEmbeddingModel}
	writePipe := &write.Pipeline{
		Detector:     detector,
		Registry:     psi.NewRegistry(),
		Log:          writer,
		Embedder:     embedder,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
		Neo4j:        neoApplier,
		Weaviate:     weaviateApplier,
		Neighbors:    &weaviateNeighborFinder{client: weaviateClient},
		LinkProposer: &ollamaLinkProposer{client: generator},
		LinkConfig: write.LinkDerivationConfig{
			ConfidenceFloor:    cfg.LinkDerivation.ConfidenceFloor,
			SimilarCosineFloor: cfg.LinkDerivation.SimilarToCosineFloor,
		},
		LinkTopK:             5,
		ConceptsEnabled:      true,
		ExpectedEmbeddingDim: cfg.Ollama.EmbeddingVectorDim,
	}

	entryWriter := &writePipelineEntryWriter{pipe: writePipe}
	summarizer := &ollamaSummarizer{
		client:       generator,
		sourceBudget: cfg.Ingest.ModuleSourceBudgetBytes,
	}
	appender := &ingestTrailAppender{
		log:          writer,
		actor:        defaultActor(),
		invocationID: ulid.Make().String(),
	}
	stateStore, err := newJSONFileStateStore()
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}

	p := &ingest.Pipeline{
		Walker:        walkerWalk,
		Matrix:        languages.DefaultMatrix(),
		Summarizer:    summarizer,
		Writer:        entryWriter,
		TrailAppender: appender,
		StateStore:    stateStore,
		PostReflect:   nil, // Phase 1: scoped reflection wired after cortex reflect lands
		Now:           func() time.Time { return time.Now().UTC() },
		Logger:        walker.NopLogger{},
		// Concurrency comes from cfg.Ingest.GenerationConcurrency,
		// which Phase 4 (cortex-17p) renamed from ollama_concurrency
		// and made provider-aware: 2 for local ollama (consumer GPU
		// ceiling) and 16 for remote paid-tier providers (Anthropic/
		// OpenAI tier 2+). Operators on higher tiers can raise this
		// in config. The legacy ollama_concurrency YAML key still
		// reads into this field via LegacyOllamaConcurrency for
		// backwards compatibility. ingest.DefaultOllamaConcurrency
		// remains as the internal/ingest fallback if this is zero.
		Concurrency:     cfg.Ingest.GenerationConcurrency,
		SkipPostReflect: true,
		// cortex-ks1: per-package size gate. Budget = num_ctx * 0.6 * 4
		// chars/token leaves room for prompt boilerplate and structured-
		// output response inside the model's context window. Modules
		// exceeding this fall back to per-file granularity automatically.
		MaxModuleBytes: int64(float64(cfg.Ollama.NumCtx) * 0.6 * 4),
		Differ:         gitDiff,
		Retracter:      newIngestRetracter(writer, bolt),
	}
	return p, cleanup, nil
}

// renderIngestResult prints either a human summary or a JSON envelope.
func renderIngestResult(cmd *cobra.Command, res *ingest.Result, jsonFlag bool) error {
	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"project":           res.ProjectName,
			"trail_id":          res.TrailID,
			"entry_ids":         res.EntryIDs,
			"modules_found":     len(res.Modules),
			"skipped_modules":   res.SkippedModules,
			"evicted_modules":   res.EvictedModules,
			"retracted_modules": res.RetractedModules,
			"errors":            res.SummaryErrors,
			"started_at":        res.StartedAt,
			"finished_at":       res.FinishedAt,
		})
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "project=%s modules=%d written=%d skipped=%d errors=%d\n",
		res.ProjectName, len(res.Modules), len(res.EntryIDs),
		len(res.SkippedModules), len(res.SummaryErrors))
	if len(res.EvictedModules) > 0 {
		fmt.Fprintf(w, "evicted=%d (delta detection)\n", len(res.EvictedModules))
	}
	if len(res.RetractedModules) > 0 {
		fmt.Fprintf(w, "retracted=%d (deleted files)\n", len(res.RetractedModules))
	}
	if res.TrailID != "" {
		fmt.Fprintf(w, "trail=%s\n", res.TrailID)
	}
	for _, e := range res.SummaryErrors {
		fmt.Fprintf(w, "  error module=%s reason=%s err=%v\n", e.ModuleID, e.Reason, e.Err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Adapters
// ---------------------------------------------------------------------------

// walkerWalk adapts walker.Walk to ingest.WalkerFunc. It converts each
// walker.FileMeta into a languages.File, which is the unit
// languages.Group consumes. The project-root .cortexignore is
// automatically merged into the walker's ignore matcher alongside the
// project's .gitignore (cortex-8rk). The file is expected to exist by
// the time walkerWalk runs — the cobra RunE handler bootstraps it via
// ensureCortexIgnore before invoking pipeline.Ingest.
func walkerWalk(root string, fn func(languages.File) error) error {
	return walker.Walk(walker.Options{
		ProjectRoot:      root,
		ExtraIgnoreFiles: []string{filepath.Join(root, cortexIgnoreFilename)},
		Logger:           walker.NopLogger{},
	}, func(fm walker.FileMeta) error {
		return fn(languages.File{
			AbsPath: fm.AbsPath,
			RelPath: fm.RelPath,
			Size:    fm.Size,
		})
	})
}

// cortexIgnoreFilename is the per-project ignore file cortex ingest
// honours alongside .gitignore. Same syntax as .gitignore.
const cortexIgnoreFilename = ".cortexignore"

// defaultCortexIgnoreBody is the starter content written to
// <projectRoot>/.cortexignore on first run. Operators own the file
// once it exists; cortex ingest will not overwrite it. The defaults
// deliberately KEEP docs/, *.md, and README.md in scope (they answer
// recall questions about architecture and spec) and exclude IDE
// metadata, local caches, build outputs, and the beads issue tracker
// (which stores tracker state, not source). See cortex-8rk.
//
// Note: internal/walker.alwaysSkipDirs already hardcodes the
// universally-generated directories (node_modules, .next, .nuxt,
// .svelte-kit, .turbo, __pycache__, .pytest_cache, .mypy_cache,
// .venv, .gradle, .tox, .cache) so those are pruned even if an
// operator deletes them from .cortexignore. The entries below
// duplicate that list for discoverability and add file-level
// patterns the walker's directory prune does not cover (*.min.js,
// *.bundle.js, sourcemaps, etc.).
const defaultCortexIgnoreBody = `# .cortexignore — paths cortex ingest skips, in addition to .gitignore.
# Syntax: one glob/prefix per line, same rules as .gitignore.
# Edit to taste; this file is operator-owned once it exists.
#
# The walker also hardcodes a "universally generated" prune list
# (node_modules, .next, .nuxt, .svelte-kit, .turbo, __pycache__,
# .pytest_cache, .mypy_cache, .venv, .gradle, .tox, .cache) so
# removing them from this file does NOT re-enable scanning them.

# IDE / editor metadata
.idea/
.vscode/
.cursor/

# Agent / tooling state
.claude/
.beads/

# Language / package artifacts
node_modules/
vendor/
target/
dist/
build/
out/

# Frontend framework build output
.next/
.nuxt/
.svelte-kit/
.turbo/
.cache/

# Python caches and venvs
__pycache__/
.pytest_cache/
.mypy_cache/
.venv/
venv/
.tox/

# JVM / Gradle
.gradle/

# Generated / minified assets
*.min.js
*.min.css
*.bundle.js
*.bundle.css
*.map

# Test + profile output
coverage.out
*.log
*.tmp
`

// ensureCortexIgnore writes a default .cortexignore at the project
// root if no file is present. An existing file (even a zero-byte one)
// is left untouched so operator customisations survive re-runs. All
// other filesystem errors are surfaced so the caller can decide
// whether to abort the ingest.
func ensureCortexIgnore(projectRoot string) error {
	path := filepath.Join(projectRoot, cortexIgnoreFilename)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(defaultCortexIgnoreBody), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ollamaSummarizer implements ingest.Summarizer by actually reading
// each file in the module (up to a byte budget), handing the
// concatenated source to Ollama via the structured-output
// /api/generate path, and formatting the resulting five-field JSON
// object into a markdown entry body.
//
// The byte budget is chosen so the prompt plus source plus the model's
// response fit inside the configured num_ctx (32K by default for
// qwen3:4b-instruct). At ~3-4 chars per token, ~100KB of code plus
// ~2KB of prompt boilerplate plus ~2KB of expected output leaves
// comfortable headroom. Modules that exceed the budget get each file
// proportionally truncated with a "[truncated N bytes]" marker so the
// model knows the input is incomplete.
//
// See bead cortex-dww for the rationale and cortex-v41 for the eval
// failure mode this replaces.
type ollamaSummarizer struct {
	client llm.Generator
	// sourceBudget, when > 0, overrides the moduleSourceBudget constant.
	// Populated from cfg.Ingest.ModuleSourceBudgetBytes so operators can
	// raise the per-module source cap when they switch to a remote
	// provider with a much larger context window (e.g. 200K+ tokens).
	sourceBudget int
}

// moduleSourceBudget caps the combined source bytes a single module
// contributes to the summarization prompt. Chosen for 32K num_ctx on
// 4-8B q4 models; raise once we verify VRAM on the larger
// configurations. Not a config knob yet — when it becomes one, move
// this to internal/config.
const moduleSourceBudget = 100_000

// moduleSummaryPayload mirrors prompts.ModuleSummarySchema. It is the
// shape Ollama is constrained to emit when GenerateStructured is
// called with that schema; the summarizer unmarshals into this struct
// and then formats the fields into the markdown entry body.
type moduleSummaryPayload struct {
	Summary      string   `json:"summary"`
	Identifiers  []string `json:"identifiers"`
	Algorithms   []string `json:"algorithms"`
	Dependencies []string `json:"dependencies"`
	Searchable   []string `json:"searchable"`
}

func (s *ollamaSummarizer) Summarize(ctx context.Context, req ingest.SummaryRequest) (string, error) {
	source, err := buildModuleSourceBody(req.Module, s.budgetBytes())
	if err != nil {
		return "", fmt.Errorf("read module source: %w", err)
	}
	tmplName, schema := promptForLanguage(req.Module.Language)
	prompt, err := prompts.Render(tmplName, prompts.Data{Body: source})
	if err != nil {
		return "", fmt.Errorf("render %s: %w", tmplName, err)
	}
	out, err := s.client.GenerateStructured(ctx, prompt, schema)
	if err != nil {
		return "", err
	}
	return decodeAndFormatSummary(req.Module, tmplName, out)
}

// budgetBytes returns the configured per-module source budget, or the
// built-in default when unset. The summarizer carries the budget on
// itself (rather than reading from a package-level constant) so that
// ingest.go can wire a config-driven value and tests can override it.
func (s *ollamaSummarizer) budgetBytes() int {
	if s.sourceBudget > 0 {
		return s.sourceBudget
	}
	return moduleSourceBudget
}

// promptForLanguage returns the prompt template name and JSON schema
// the summarizer should use for a module of the given language. Docs
// (.md, .txt) and SQL (.sql) have their own prompts and schemas
// because the module_summary prompt is code-oriented — asking a model
// to report "identifiers, algorithms, dependencies" from a prose
// document or a SQL migration produces garbage (or an empty response
// that crashes the decoder downstream). Every other language falls
// through to the code-oriented module_summary template.
func promptForLanguage(lang languages.Language) (string, json.RawMessage) {
	switch lang {
	case languages.LangDocs:
		return prompts.NameDocSummary, prompts.DocSummarySchema
	case languages.LangSQL:
		return prompts.NameSQLSummary, prompts.SQLSummarySchema
	default:
		return prompts.NameModuleSummary, prompts.ModuleSummarySchema
	}
}

// decodeAndFormatSummary picks the right payload shape for the chosen
// prompt, unmarshals the model's JSON response into it, and renders
// the entry body. On decode failure the error quotes a 200-char
// snippet of the raw response so ops.log carries enough evidence to
// diagnose the failure without a second ingest run.
func decodeAndFormatSummary(mod languages.Module, tmplName, out string) (string, error) {
	snippet := func() string {
		s := out
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		return s
	}
	switch tmplName {
	case prompts.NameDocSummary:
		var p docSummaryPayload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			return "", fmt.Errorf("decode doc_summary response: %w (raw: %q)", err, snippet())
		}
		return formatDocSummaryBody(mod, p), nil
	case prompts.NameSQLSummary:
		var p sqlSummaryPayload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			return "", fmt.Errorf("decode sql_summary response: %w (raw: %q)", err, snippet())
		}
		return formatSQLSummaryBody(mod, p), nil
	default:
		var p moduleSummaryPayload
		if err := json.Unmarshal([]byte(out), &p); err != nil {
			return "", fmt.Errorf("decode module_summary response: %w (raw: %q)", err, snippet())
		}
		return formatModuleSummaryBody(mod, p), nil
	}
}

// buildModuleSourceBody reads each file in the module and concatenates
// them into a single USER_CONTENT block for the summarizer prompt.
// Each file is preceded by a "=== FILE: <relpath> ===" header so the
// model can attribute identifiers back to their file. When the
// combined source would exceed budget, each file's contribution is
// scaled proportionally to its share of the total, and the tail of
// each truncated file is replaced with a clear "... [truncated N
// bytes]" marker. Files that fail to read are recorded inline as
// "=== FILE: <relpath> === (unreadable: <err>)" rather than aborting
// the whole module — a single unreadable file shouldn't black out a
// multi-file module summary.
func buildModuleSourceBody(m languages.Module, budget int) (string, error) {
	type fileRead struct {
		rel  string
		data []byte
		err  error
	}
	reads := make([]fileRead, 0, len(m.Files))
	total := 0
	for _, f := range m.Files {
		data, err := os.ReadFile(f.AbsPath)
		if err != nil {
			reads = append(reads, fileRead{rel: f.RelPath, err: err})
			continue
		}
		reads = append(reads, fileRead{rel: f.RelPath, data: data})
		total += len(data)
	}

	// Allocate a per-file byte cap. If total fits inside the budget,
	// every file is included whole. Otherwise each file gets a share
	// proportional to its original size, floor 256 bytes so tiny
	// files in a huge module still contribute a header and a few
	// lines.
	var caps []int
	if total <= budget {
		caps = make([]int, len(reads))
		for i, r := range reads {
			caps[i] = len(r.data)
		}
	} else {
		caps = make([]int, len(reads))
		const minPerFile = 256
		for i, r := range reads {
			share := int(float64(len(r.data)) / float64(total) * float64(budget))
			if share < minPerFile {
				share = minPerFile
			}
			if share > len(r.data) {
				share = len(r.data)
			}
			caps[i] = share
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "module: %s\nlanguage: %s\n\n", m.ID, m.Language)
	for i, r := range reads {
		fmt.Fprintf(&b, "=== FILE: %s ===\n", r.rel)
		if r.err != nil {
			fmt.Fprintf(&b, "(unreadable: %v)\n\n", r.err)
			continue
		}
		cap := caps[i]
		if cap >= len(r.data) {
			b.Write(r.data)
		} else {
			b.Write(r.data[:cap])
			fmt.Fprintf(&b, "\n... [truncated %d bytes]\n", len(r.data)-cap)
		}
		b.WriteString("\n\n")
	}
	return b.String(), nil
}

// docSummaryPayload is the decoded shape of the doc_summary prompt's
// JSON output. Markdown / plain-text documents are prose and facts,
// not code, so the fields capture WHAT the document says rather than
// what symbols it declares. Topics are the document's subject matter;
// entities are named actors, products, specs, or cross-references
// the document talks ABOUT; links are any URLs or doc-path references
// the document cites; searchable is natural-language query phrases.
type docSummaryPayload struct {
	Summary    string   `json:"summary"`
	Topics     []string `json:"topics"`
	Entities   []string `json:"entities"`
	Links      []string `json:"links"`
	Searchable []string `json:"searchable"`
}

// sqlSummaryPayload is the decoded shape of the sql_summary prompt's
// JSON output. SQL files are schema-shape + intent; the fields
// capture the tables the file creates or touches, the operations it
// performs, the column-level facts it pins, and query phrases a
// reader might use to find it.
type sqlSummaryPayload struct {
	Summary    string   `json:"summary"`
	Tables     []string `json:"tables"`
	Operations []string `json:"operations"`
	Columns    []string `json:"columns"`
	Searchable []string `json:"searchable"`
}

// formatDocSummaryBody renders the doc payload into a markdown entry
// body. Section order mirrors formatModuleSummaryBody so recall output
// looks consistent across kinds: summary → sections → trimmed.
func formatDocSummaryBody(m languages.Module, p docSummaryPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Document %s (%s).\n\n", m.ID, m.Language)
	if s := strings.TrimSpace(p.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	writeSection := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", heading)
		for _, it := range items {
			it = strings.TrimSpace(it)
			if it == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}
	writeSection("Topics", p.Topics)
	writeSection("Entities", p.Entities)
	writeSection("Links", p.Links)
	writeSection("Searchable", p.Searchable)
	return strings.TrimRight(b.String(), "\n")
}

// formatSQLSummaryBody renders the sql payload into a markdown entry
// body. Same section layout as the other two formatters.
func formatSQLSummaryBody(m languages.Module, p sqlSummaryPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SQL %s (%s).\n\n", m.ID, m.Language)
	if s := strings.TrimSpace(p.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	writeSection := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", heading)
		for _, it := range items {
			it = strings.TrimSpace(it)
			if it == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}
	writeSection("Tables", p.Tables)
	writeSection("Operations", p.Operations)
	writeSection("Columns", p.Columns)
	writeSection("Searchable", p.Searchable)
	return strings.TrimRight(b.String(), "\n")
}

// formatModuleSummaryBody renders the five-field payload into the
// markdown entry body that write.Pipeline.Observe will embed and
// concept-extract. The body leads with the prose summary (so the
// embedding picks up natural language) and follows with one section
// per list, each bullet pulled from the model's output. Empty lists
// are omitted so short modules don't carry empty headings.
//
// Section order is fixed: Identifiers → Algorithms → Dependencies →
// Searchable. Keep it stable so the entry body is diffable across
// re-ingests and recall results are visually consistent.
func formatModuleSummaryBody(m languages.Module, p moduleSummaryPayload) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Module %s (%s).\n\n", m.ID, m.Language)
	if s := strings.TrimSpace(p.Summary); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	writeSection := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", heading)
		for _, it := range items {
			it = strings.TrimSpace(it)
			if it == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", it)
		}
		b.WriteString("\n")
	}
	writeSection("Identifiers", p.Identifiers)
	writeSection("Algorithms", p.Algorithms)
	writeSection("Dependencies", p.Dependencies)
	writeSection("Searchable", p.Searchable)
	return strings.TrimRight(b.String(), "\n")
}

// writePipelineEntryWriter implements ingest.EntryWriter by delegating
// to write.Pipeline.Observe. Each ingested module becomes one
// Observation entry carrying the ingest-origin facets required by the
// spec (domain=code, project=<project>, module=<id>).
type writePipelineEntryWriter struct {
	pipe *write.Pipeline
}

func (w *writePipelineEntryWriter) WriteModule(ctx context.Context, req ingest.EntryRequest) (string, error) {
	res, err := w.pipe.Observe(ctx, write.ObserveRequest{
		Body: req.Body,
		Kind: "Observation",
		Facets: map[string]string{
			"domain":  "code",
			"project": req.ProjectName,
			"module":  req.ModuleID,
			"source":  "ingest",
		},
	})
	if err != nil {
		if res != nil && res.EntryID != "" {
			return res.EntryID, err
		}
		return "", err
	}
	return res.EntryID, nil
}

// ingestTrailAppender implements ingest.TrailAppender by writing a
// two-group trail directly to the log. The trail entity id is the
// synthesized "trail:ingest:<project>:<rfc3339>" the pipeline hands us;
// trail.Begin is not reused because it mints its own ULID id.
type ingestTrailAppender struct {
	log          *log.Writer
	actor        string
	invocationID string
}

func (a *ingestTrailAppender) AppendTrail(ctx context.Context, req ingest.TrailRequest) error {
	if a.log == nil {
		return errors.New("ingest: nil log writer")
	}
	if req.TrailID == "" {
		return errs.Validation("MISSING_TRAIL_ID",
			"ingest trail append requires a non-empty trail id", nil)
	}

	// Begin group: kind/agent/name/started_at.
	beginTx := ulid.Make().String()
	beginTs := req.StartedAt.UTC().Format(time.RFC3339Nano)
	beginGroup, err := a.buildGroup(beginTx, beginTs, req.TrailID, []kv{
		{trail.AttrKind, trail.KindTrail},
		{trail.AttrAgent, "ingest"},
		{trail.AttrName, "ingest:" + req.ProjectName},
		{trail.AttrStartedAt, beginTs},
	})
	if err != nil {
		return errs.Operational("DATOM_BUILD_FAILED",
			"could not construct ingest trail begin datoms", err)
	}
	if _, err := a.log.Append(beginGroup); err != nil {
		return errs.Operational("LOG_APPEND_FAILED",
			"failed to append ingest trail begin", err)
	}

	// End group: ended_at + summary.
	endTx := ulid.Make().String()
	endTs := req.FinishedAt.UTC().Format(time.RFC3339Nano)
	summary := fmt.Sprintf("ingest %s: %d entries written", req.ProjectName, len(req.EntryIDs))
	endGroup, err := a.buildGroup(endTx, endTs, req.TrailID, []kv{
		{trail.AttrEndedAt, endTs},
		{trail.AttrSummary, summary},
	})
	if err != nil {
		return errs.Operational("DATOM_BUILD_FAILED",
			"could not construct ingest trail end datoms", err)
	}
	if _, err := a.log.Append(endGroup); err != nil {
		return errs.Operational("LOG_APPEND_FAILED",
			"failed to append ingest trail end", err)
	}
	return nil
}

type kv struct {
	attr  string
	value any
}

// buildGroup assembles one sealed datom group for the ingest trail.
// Mirrors the internal/trail groupBuilder shape but lives here because
// trail.Begin/End cannot be reused (they mint their own trail ids).
func (a *ingestTrailAppender) buildGroup(tx, ts, entity string, fields []kv) ([]datom.Datom, error) {
	group := make([]datom.Datom, 0, len(fields))
	for _, f := range fields {
		raw, err := json.Marshal(f.value)
		if err != nil {
			return nil, fmt.Errorf("marshal %s: %w", f.attr, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        a.actor,
			Op:           datom.OpAdd,
			E:            entity,
			A:            f.attr,
			V:            raw,
			Src:          trail.Source,
			InvocationID: a.invocationID,
		}
		if err := d.Seal(); err != nil {
			return nil, fmt.Errorf("seal %s: %w", f.attr, err)
		}
		group = append(group, d)
	}
	return group, nil
}

// jsonFileStateStore persists per-project ingest state under
// ~/.cortex/state/ingest/<project>.json. It satisfies the spec's
// "sole source of truth for has this module already been ingested?"
// requirement without adding a database dependency in Phase 1.
type jsonFileStateStore struct {
	dir string
}

func newJSONFileStateStore() (*jsonFileStateStore, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, errs.Operational("HOME_UNAVAILABLE",
			"could not resolve user home directory for ingest state", err)
	}
	dir := filepath.Join(home, ".cortex", "state", "ingest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errs.Operational("STATE_DIR_CREATE_FAILED",
			"could not create ingest state directory", err)
	}
	return &jsonFileStateStore{dir: dir}, nil
}

func (s *jsonFileStateStore) path(project string) string {
	// Project names may contain characters that are unfriendly in file
	// names; replace path separators with underscores so the on-disk
	// layout stays flat and predictable.
	safe := strings.ReplaceAll(project, string(filepath.Separator), "_")
	safe = strings.ReplaceAll(safe, "/", "_")
	return filepath.Join(s.dir, safe+".json")
}

func (s *jsonFileStateStore) Read(_ context.Context, project string) (ingest.ProjectState, bool, error) {
	p := s.path(project)
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ingest.ProjectState{}, false, nil
		}
		return ingest.ProjectState{}, false, fmt.Errorf("read %s: %w", p, err)
	}
	var state ingest.ProjectState
	if err := json.Unmarshal(data, &state); err != nil {
		return ingest.ProjectState{}, false, fmt.Errorf("decode %s: %w", p, err)
	}
	return state, true, nil
}

func (s *jsonFileStateStore) Write(_ context.Context, state ingest.ProjectState) error {
	p := s.path(state.ProjectName)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Delta detection adapters (FR-023)
// ---------------------------------------------------------------------------

// gitDiff implements ingest.DiffFunc by running git diff --name-status
// between two commits. It parses the output into changed (A/M/R) and
// deleted (D) relative paths.
func gitDiff(projectRoot, oldSHA, newSHA string) (ingest.DiffResult, error) {
	cmd := exec.Command("git", "diff", "--name-status", oldSHA+".."+newSHA)
	cmd.Dir = projectRoot
	out, err := cmd.Output()
	if err != nil {
		return ingest.DiffResult{}, fmt.Errorf("git diff %s..%s: %w", oldSHA, newSHA, err)
	}
	var result ingest.DiffResult
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		// Format: "D\tpath" or "M\tpath" or "R100\told\tnew"
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		switch {
		case status == "D":
			result.Deleted = append(result.Deleted, parts[1])
		case status == "A", status == "M":
			result.Changed = append(result.Changed, parts[1])
		case strings.HasPrefix(status, "R"):
			// Rename: old path is deleted, new path is changed.
			result.Deleted = append(result.Deleted, parts[1])
			if len(parts) == 3 {
				result.Changed = append(result.Changed, parts[2])
			}
		}
	}
	return result, nil
}

// ingestRetracter implements ingest.Retracter by looking up entries
// with the given module facet in Neo4j and writing OpRetract datoms
// through the log. When Neo4j is unavailable (boltErr was non-nil at
// build time), retraction is a no-op — the module is still evicted
// from CompletedModuleIDs so a future full-ingest cleans up.
type ingestRetracter struct {
	log  *log.Writer
	bolt *neo4j.BoltClient
}

func newIngestRetracter(logWriter *log.Writer, bolt *neo4j.BoltClient) *ingestRetracter {
	return &ingestRetracter{log: logWriter, bolt: bolt}
}

func (r *ingestRetracter) RetractModules(ctx context.Context, projectName string, moduleIDs []string) error {
	if r.bolt == nil {
		// No Neo4j → cannot look up entry IDs by module facet. The
		// module is already evicted from CompletedModuleIDs, so a
		// future full re-ingest will not see stale entries. Log the
		// skip but do not fail the run.
		return nil
	}
	for _, modID := range moduleIDs {
		entryIDs, err := r.findEntriesByModule(ctx, projectName, modID)
		if err != nil {
			return fmt.Errorf("find entries for module %s: %w", modID, err)
		}
		for _, eid := range entryIDs {
			if err := r.retractEntry(ctx, eid, modID); err != nil {
				return fmt.Errorf("retract entry %s (module %s): %w", eid, modID, err)
			}
		}
	}
	return nil
}

// findEntriesByModule queries Neo4j for Entry nodes whose facet.module
// and facet.project properties match.
func (r *ingestRetracter) findEntriesByModule(ctx context.Context, projectName, moduleID string) ([]string, error) {
	cypher := `MATCH (n:Entry)
WHERE n.` + "`facet.module`" + ` = $module AND n.` + "`facet.project`" + ` = $project
  AND (n.retracted IS NULL OR n.retracted = false)
RETURN n.entry_id AS eid`
	rows, err := r.bolt.QueryGraph(ctx, cypher, map[string]any{
		"module":  moduleID,
		"project": projectName,
	})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, row := range rows {
		if eid, ok := row["eid"].(string); ok && eid != "" {
			ids = append(ids, eid)
		}
	}
	return ids, nil
}

// retractEntry writes one OpRetract datom for the given entry.
func (r *ingestRetracter) retractEntry(ctx context.Context, entryID, moduleID string) error {
	tx := ulid.Make().String()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	reasonVal, _ := json.Marshal("incremental re-ingest: module " + moduleID + " deleted")
	nullVal, _ := json.Marshal(nil)
	group := []datom.Datom{
		{
			Tx: tx, Ts: ts, Actor: defaultActor(),
			Op: datom.OpRetract, E: entryID, A: "exists",
			V: nullVal, Src: "retract", InvocationID: tx,
		},
		{
			Tx: tx, Ts: ts, Actor: defaultActor(),
			Op: datom.OpAdd, E: entryID, A: "retract.reason",
			V: reasonVal, Src: "retract", InvocationID: tx,
		},
	}
	for i := range group {
		if err := group[i].Seal(); err != nil {
			return fmt.Errorf("seal datom: %w", err)
		}
	}
	_, err := r.log.Append(group)
	return err
}
