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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/ingest"
	"github.com/nixlim/cortex/internal/languages"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/prompts"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/trail"
	"github.com/nixlim/cortex/internal/walker"
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
			cfg, segDir, err := loadIngestConfig()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			pipeline, cleanup, err := buildIngestPipeline(cfg, segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

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
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"project":               project,
					"found":                 ok,
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
			cfg, segDir, err := loadIngestConfig()
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			pipeline, cleanup, err := buildIngestPipeline(cfg, segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

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
	return cmd
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
// adapters. The cleanup closure closes the log writer.
func buildIngestPipeline(cfg config.Config, segDir string) (*ingest.Pipeline, func(), error) {
	writer, err := log.NewWriter(segDir)
	if err != nil {
		return nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}
	cleanup := func() { _ = writer.Close() }

	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		cleanup()
		return nil, func() {}, errs.Operational("SECRETS_INIT_FAILED",
			"could not initialize secret detector", err)
	}

	ollamaClient := ollama.NewHTTPClient(ollama.Config{
		Endpoint:              cfg.Endpoints.Ollama,
		EmbeddingModel:        defaultEmbeddingModel,
		GenerationModel:       defaultGenerationModel,
		EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
		LinkDerivationTimeout: time.Duration(cfg.Timeouts.LinkDerivationSeconds) * time.Second,
		NumCtx:                cfg.Ollama.NumCtx,
	})

	// The underlying write.Pipeline shares the log writer and runs one
	// Observe per module. Reusing the same embedder adapter as
	// cmd/cortex/observe.go so the FR-051 model-digest datoms appear
	// on ingest entries as well.
	embedder := &observeEmbedder{c: ollamaClient, model: defaultEmbeddingModel}
	writePipe := &write.Pipeline{
		Detector:     detector,
		Registry:     psi.NewRegistry(),
		Log:          writer,
		Embedder:     embedder,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
	}

	entryWriter := &writePipelineEntryWriter{pipe: writePipe}
	summarizer := &ollamaSummarizer{client: ollamaClient}
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
		Walker:          walkerWalk,
		Matrix:          languages.DefaultMatrix(),
		Summarizer:      summarizer,
		Writer:          entryWriter,
		TrailAppender:   appender,
		StateStore:      stateStore,
		PostReflect:     nil, // Phase 1: scoped reflection wired after cortex reflect lands
		Now:             func() time.Time { return time.Now().UTC() },
		Logger:          walker.NopLogger{},
		Concurrency:     ingest.DefaultOllamaConcurrency,
		SkipPostReflect: true,
	}
	return p, cleanup, nil
}

// renderIngestResult prints either a human summary or a JSON envelope.
func renderIngestResult(cmd *cobra.Command, res *ingest.Result, jsonFlag bool) error {
	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"project":         res.ProjectName,
			"trail_id":        res.TrailID,
			"entry_ids":       res.EntryIDs,
			"modules_found":   len(res.Modules),
			"skipped_modules": res.SkippedModules,
			"errors":          res.SummaryErrors,
			"started_at":      res.StartedAt,
			"finished_at":     res.FinishedAt,
		})
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "project=%s modules=%d written=%d skipped=%d errors=%d\n",
		res.ProjectName, len(res.Modules), len(res.EntryIDs),
		len(res.SkippedModules), len(res.SummaryErrors))
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
// languages.Group consumes.
func walkerWalk(root string, fn func(languages.File) error) error {
	return walker.Walk(walker.Options{
		ProjectRoot: root,
		Logger:      walker.NopLogger{},
	}, func(fm walker.FileMeta) error {
		return fn(languages.File{
			AbsPath: fm.AbsPath,
			RelPath: fm.RelPath,
		})
	})
}

// ollamaSummarizer implements ingest.Summarizer using a rendered
// module_summary prompt and the Ollama Generate endpoint. The summary
// body is the concatenated list of relative file paths followed by a
// short "files:" preamble — the current Phase 1 shape the prompts
// template is designed for (opaque code block). File contents are not
// streamed to the LLM in Phase 1 because module_summary.tmpl renders
// a single USER_CONTENT block; a follow-up can include trimmed file
// bodies once the prompts layer supports multi-file rendering.
type ollamaSummarizer struct {
	client *ollama.HTTPClient
}

func (s *ollamaSummarizer) Summarize(ctx context.Context, req ingest.SummaryRequest) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "module: %s\nlanguage: %s\nfiles:\n", req.Module.ID, req.Module.Language)
	for _, f := range req.Module.Files {
		fmt.Fprintf(&b, "  - %s\n", f.RelPath)
	}
	prompt, err := prompts.Render(prompts.NameModuleSummary, prompts.Data{Body: b.String()})
	if err != nil {
		return "", fmt.Errorf("render module_summary: %w", err)
	}
	out, err := s.client.Generate(ctx, prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
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
