// Package ingest implements the cortex ingest command's core
// orchestration: walk a project, group files into modules by the
// configured language strategy matrix, summarize each module with
// Ollama, write one episodic entry per module through the standard
// write pipeline, synthesize an ingest trail, and optionally trigger
// scoped post-ingest reflection.
//
// The package is deliberately factored as an orchestrator that takes
// narrow interfaces for every side effect:
//
//   - Summarizer produces a module body from the module's files.
//   - EntryWriter appends one module entry through the standard write
//     path (observe). Tests drop in a fake that records inputs.
//   - TrailAppender writes the synthesized ingest trail datoms.
//   - StateStore reads and writes per-project ingest state so
//     re-running the command is idempotent and resume works.
//   - PostReflect runs scoped reflection over the ingest window.
//
// The CLI (cmd/cortex/ingest.go) wires these to the real log,
// language matrix, Ollama client, and write.Pipeline. The package
// itself is filesystem-touching only via the walker; every other
// dependency is an interface.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-2 (BDD ingest scenarios)
//	docs/spec/cortex-spec.md §"Configuration Defaults" (ingest.*)
//	docs/spec/cortex-spec.md FR-022, FR-023, FR-024, FR-049
//	bead cortex-4kq.51
package ingest

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/languages"
	"github.com/nixlim/cortex/internal/walker"
)

// Spec defaults from docs/spec/cortex-spec.md §"Configuration Defaults".
const (
	DefaultOllamaConcurrency = 4
	DefaultPostIngestReflect = true
)

// Module mirrors languages.Module but is repeated here so callers
// that only import internal/ingest do not have to know the languages
// package. It is the unit of work the ingest pipeline produces one
// entry for.
type Module = languages.Module

// SummaryRequest is the input the Summarizer receives for one module.
// It includes the project name so multi-project summarizers can scope
// model prompts.
type SummaryRequest struct {
	ProjectName string
	Module      Module
}

// Summarizer turns a module's file set into a short, one-entry body
// suitable for cortex observe. A nil return with no error is treated
// as SUMMARIZER_EMPTY and the module is skipped (not a pipeline
// failure — a single bad module should not abort a multi-thousand-
// module ingest).
type Summarizer interface {
	Summarize(ctx context.Context, req SummaryRequest) (string, error)
}

// EntryRequest is the normalized shape passed to EntryWriter for one
// module. It carries everything the observe write pipeline needs
// without leaking write.ObserveRequest into the ingest package and
// creating a dependency cycle with internal/write.
type EntryRequest struct {
	ProjectName string
	ModuleID    string
	Body        string
	Files       []string // relative paths, for provenance
}

// EntryWriter appends one ingested module as an episodic entry
// through the shared write pipeline. The returned EntryID is the same
// prefixed ULID cortex observe would return.
type EntryWriter interface {
	WriteModule(ctx context.Context, req EntryRequest) (entryID string, err error)
}

// TrailRequest is the input that produces a synthesized ingest trail.
// The trail id format is fixed by the spec:
// "trail:ingest:<project>:<rfc3339-timestamp>".
type TrailRequest struct {
	ProjectName string
	TrailID     string
	EntryIDs    []string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// TrailAppender writes the synthesized ingest trail. The trail is a
// single unit of work (not per-module) so a crash mid-walk leaves the
// log without a half-formed trail.
type TrailAppender interface {
	AppendTrail(ctx context.Context, req TrailRequest) error
}

// ProjectState is what the StateStore persists between runs. It is
// the sole source of truth for "has this module already been
// ingested?". The store implementation decides where this lives —
// a labelled node in Neo4j, a sqlite row, or a JSON file under
// ~/.cortex/state/ingest/<project>.json.
type ProjectState struct {
	ProjectName         string
	LastCommitSHA       string
	// RunStartedAt records the wall-clock time the current (or most
	// recent) Ingest call began. It is persisted incrementally as the
	// run progresses so `cortex ingest status` in another terminal can
	// tell the difference between "stale state from last night's run"
	// and "run in progress right now". The run is considered in
	// progress when RunStartedAt.After(LastIngestedAt). See cortex-so5.
	RunStartedAt        time.Time
	LastIngestedAt      time.Time
	CompletedModuleIDs  []string // superset across all runs
	LastTrailID         string
	TotalEntriesWritten int
}

// Has reports whether the given module id has already been ingested.
func (s ProjectState) Has(moduleID string) bool {
	for _, id := range s.CompletedModuleIDs {
		if id == moduleID {
			return true
		}
	}
	return false
}

// RunInProgress returns true when a run is currently executing. A run
// is in progress once RunStartedAt has been set on the latest write
// and LastIngestedAt has not yet caught up. Used by
// `cortex ingest status` to show "N of M modules done" versus
// "last completed at ...".
func (s ProjectState) RunInProgress() bool {
	if s.RunStartedAt.IsZero() {
		return false
	}
	return s.RunStartedAt.After(s.LastIngestedAt)
}

// StateStore reads and writes per-project ingest state. Missing state
// for a fresh project returns (ProjectState{}, false, nil).
type StateStore interface {
	Read(ctx context.Context, project string) (state ProjectState, ok bool, err error)
	Write(ctx context.Context, state ProjectState) error
}

// PostReflect is the optional scoped reflection callback. It is
// invoked exactly once at the end of a successful ingest with the
// trail id so it can scope the candidate window. A nil PostReflect is
// skipped silently.
type PostReflect interface {
	ReflectScope(ctx context.Context, trailID string) error
}

// Request is one cortex ingest invocation. All fields except
// ProjectRoot and ProjectName are optional.
type Request struct {
	ProjectRoot string
	ProjectName string
	CommitSHA   string // optional, for state tracking
	Concurrency int    // 0 → DefaultOllamaConcurrency
	DryRun      bool   // skip writes but still walk/group/summarize
	Resume      bool   // explicit resume (same semantics as idempotent re-run)
	Analyze     bool   // --analyze; not implemented in Phase 1 core
}

// Result is the full output of one Ingest call. EntryIDs lists only
// entries WRITTEN during this run (not the superset across all runs).
type Result struct {
	ProjectName      string
	TrailID          string // empty when no new entries were written
	EntryIDs         []string
	Modules          []Module
	SkippedModules   []string // module ids that were already ingested
	EvictedModules   []string // module ids evicted by delta detection
	RetractedModules []string // module ids retracted (all files deleted)
	SummaryErrors    []ModuleError
	StartedAt        time.Time
	FinishedAt       time.Time
	State            ProjectState // the state written back at the end
}

// ModuleError records a per-module failure (summarizer error, empty
// summary, write rejected). Per-module failures do not abort the run.
type ModuleError struct {
	ModuleID string
	Reason   string
	Err      error
}

// Pipeline orchestrates one cortex ingest invocation.
type Pipeline struct {
	Walker          WalkerFunc // nil → real walker.Walk
	Matrix          languages.Matrix
	Summarizer      Summarizer
	Writer          EntryWriter
	TrailAppender   TrailAppender
	StateStore      StateStore
	PostReflect     PostReflect
	Now             func() time.Time
	Logger          walker.Logger
	Concurrency     int
	SkipPostReflect bool // honored when true; otherwise DefaultPostIngestReflect
	// Progress is an optional per-module completion callback. nil =
	// silent (preserves Phase 1 behavior). When set, Ingest invokes
	// it from the worker goroutine exactly once per module after all
	// summarize + write work for that module has completed, passing a
	// ProgressEvent with a monotonic DoneCount relative to TotalCount.
	// Callback code should be cheap; it runs inline on the worker
	// goroutine and blocks the slot until it returns. See cortex-so5.
	Progress func(ProgressEvent)
	// MaxModuleBytes is the per-package size gate from cortex-ks1.
	// When > 0, any non-per-file module whose summed File.Size exceeds
	// this value is split into one per-file module per file before the
	// summarizer ever sees it. The wiring in cmd/cortex/ingest.go
	// computes the value as int64(NumCtx * 0.6 * 4) so a 32K context
	// model gets ~78KB of source budget — enough to absorb prompt
	// overhead and the structured-output response. Zero disables the
	// gate (default; preserves existing tests and small projects).
	MaxModuleBytes int64
	// Differ is the optional git-diff seam for incremental re-ingest
	// (FR-023). When non-nil and the request carries a CommitSHA that
	// differs from state.LastCommitSHA, Ingest diffs the two commits
	// to find changed and deleted files, evicts affected modules from
	// CompletedModuleIDs so they are re-processed, and retracts entries
	// for modules whose files were all deleted. nil = no delta
	// detection (preserves Phase 1 full-walk idempotent behavior).
	Differ DiffFunc
	// Retracter emits retraction datoms for deleted-file modules.
	// Required when Differ is set; ignored otherwise.
	Retracter Retracter
}

// ProgressEvent carries one per-module completion signal from the
// ingest pipeline to a Progress callback. Fields:
//
//   - ModuleID / Language:        identify the module
//   - FileCount / ByteCount:      module source size fed to the summarizer
//   - Elapsed:                    wall-clock from Summarize call start to
//                                 the end of the write step (or to the
//                                 failure point, if Err != nil)
//   - Err:                        nil on success, the specific error
//                                 returned by the summarizer or writer
//                                 otherwise. Matches the ModuleError
//                                 that would be attached to Result
//                                 for the same module.
//   - DoneCount / TotalCount:     monotonic progress against the todo
//                                 set. DoneCount is incremented under
//                                 an atomic counter so two concurrent
//                                 workers never see the same value.
//                                 TotalCount is len(todo) for this run.
type ProgressEvent struct {
	ModuleID   string
	Language   languages.Language
	FileCount  int
	ByteCount  int64
	Elapsed    time.Duration
	Err        error
	DoneCount  int
	TotalCount int
}

// WalkerFunc is the seam used by tests to replace the filesystem
// walker with a fixture-driven producer. Production wires it to
// walker.Walk via a tiny adapter.
type WalkerFunc func(root string, fn func(languages.File) error) error

// DiffResult holds the output of a git diff between two commits.
// Changed lists relative paths of files that were added or modified.
// Deleted lists relative paths of files that were removed.
type DiffResult struct {
	Changed []string
	Deleted []string
}

// DiffFunc computes the set of changed and deleted files between two
// commits in a project root. Production wires it to git diff
// --name-status; tests supply a canned result.
type DiffFunc func(projectRoot, oldSHA, newSHA string) (DiffResult, error)

// Retracter emits retraction datoms for entries that were derived from
// modules that no longer exist (all constituent files deleted). The
// implementation looks up entries by their module facet and writes
// OpRetract datoms.
type Retracter interface {
	RetractModules(ctx context.Context, projectName string, moduleIDs []string) error
}

// Ingest runs the full ingest sequence. Return values follow the
// Cortex error envelope: validation failures become KindValidation,
// operational failures become KindOperational, and per-module errors
// are attached to Result.SummaryErrors so callers can report partial
// success without the whole command exiting non-zero.
func (p *Pipeline) Ingest(ctx context.Context, req Request) (*Result, error) {
	if req.ProjectRoot == "" {
		return nil, errs.Validation("MISSING_PROJECT_ROOT",
			"cortex ingest requires a project path", nil)
	}
	if req.ProjectName == "" {
		return nil, errs.Validation("MISSING_PROJECT_NAME",
			"cortex ingest requires --project", nil)
	}
	p.fillDefaults()

	state, _, err := p.StateStore.Read(ctx, req.ProjectName)
	if err != nil {
		return nil, errs.Operational("STATE_READ_FAILED",
			"could not read ingest state", err)
	}
	if state.ProjectName == "" {
		state.ProjectName = req.ProjectName
	}

	started := p.Now()
	res := &Result{
		ProjectName: req.ProjectName,
		StartedAt:   started,
		State:       state,
	}

	// --- delta detection (FR-023) ----------------------------------------
	// When both old and new commit SHAs are available and a Differ is
	// wired, diff the two commits to find changed/deleted files, map
	// them to module IDs via the same grouping logic, evict affected
	// modules from CompletedModuleIDs so they are re-processed, and
	// retract entries for modules whose files were all deleted.
	if p.Differ != nil && req.CommitSHA != "" && state.LastCommitSHA != "" &&
		req.CommitSHA != state.LastCommitSHA {
		diff, err := p.Differ(req.ProjectRoot, state.LastCommitSHA, req.CommitSHA)
		if err != nil {
			return res, errs.Operational("DIFF_FAILED",
				"could not diff commits for incremental ingest", err)
		}
		evict, retract := p.computeDelta(diff)
		if len(evict) > 0 {
			state.CompletedModuleIDs = removeAll(state.CompletedModuleIDs, evict)
			res.EvictedModules = evict
		}
		if len(retract) > 0 && p.Retracter != nil && !req.DryRun {
			if err := p.Retracter.RetractModules(ctx, req.ProjectName, retract); err != nil {
				return res, errs.Operational("RETRACT_FAILED",
					"could not retract deleted-file modules", err)
			}
			// Also remove retracted modules from completed so stale
			// state does not block future ingests of re-created files.
			state.CompletedModuleIDs = removeAll(state.CompletedModuleIDs, retract)
			res.RetractedModules = retract
		}
	}

	// Stamp the start-of-run marker and persist it immediately so
	// `cortex ingest status` from another terminal can tell a run is
	// in flight even before the first module completes. A best-effort
	// write: if the state store is flaky, the run still proceeds. See
	// cortex-so5.
	state.RunStartedAt = started
	if !req.DryRun {
		_ = p.StateStore.Write(ctx, state)
	}

	// --- walk + group ---------------------------------------------------
	files, err := p.collectFiles(req.ProjectRoot)
	if err != nil {
		return res, errs.Operational("INGEST_WALK_FAILED",
			"could not walk project root", err)
	}
	modules := languages.Group(files, p.Matrix)
	modules = p.splitOversizeModules(modules)
	res.Modules = modules

	// --- filter already-ingested modules (idempotent re-run / resume) ---
	todo := make([]Module, 0, len(modules))
	for _, m := range modules {
		if state.Has(m.ID) {
			res.SkippedModules = append(res.SkippedModules, m.ID)
			continue
		}
		todo = append(todo, m)
	}

	// Short-circuit: nothing to do. Still advances state timestamp so
	// subsequent `status` reports the no-op run time, but writes no
	// new datoms (AC2).
	if len(todo) == 0 {
		state.LastIngestedAt = p.Now()
		if req.CommitSHA != "" {
			state.LastCommitSHA = req.CommitSHA
		}
		if !req.DryRun {
			if err := p.StateStore.Write(ctx, state); err != nil {
				return res, errs.Operational("STATE_WRITE_FAILED",
					"could not persist ingest state", err)
			}
		}
		res.State = state
		res.FinishedAt = p.Now()
		return res, nil
	}

	// --- summarize + write under a bounded worker pool -----------------
	type moduleResult struct {
		module  Module
		entryID string
		err     *ModuleError
	}
	sem := make(chan struct{}, p.Concurrency)
	out := make([]moduleResult, len(todo))
	totalCount := len(todo)
	// doneCount is an atomic monotonic counter so every ProgressEvent
	// that fires from the worker pool sees a distinct DoneCount value.
	// It is incremented right before the callback is invoked.
	var doneCount int64
	// stateMu guards the in-memory ProjectState against concurrent
	// appends from worker goroutines. The workers persist incremental
	// state writes (cortex-so5) so `cortex ingest status` in another
	// terminal can observe progress without waiting for end-of-run.
	var stateMu sync.Mutex
	var wg sync.WaitGroup
	for i := range todo {
		i := i
		m := todo[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			// Measure both the wall-clock elapsed time and the source
			// byte count for the Progress callback. Stat'ing the files
			// up-front is cheap compared to the summarizer call and
			// reports the actual module size (not what the summarizer
			// elected to trim). A single failed stat just contributes
			// zero bytes for that file — the ProgressEvent is still
			// emitted.
			start := p.Now()
			var byteCount int64
			for _, f := range m.Files {
				if st, err := os.Stat(f.AbsPath); err == nil {
					byteCount += st.Size()
				}
			}

			// emitProgress fires the callback (if set) with the
			// current module's outcome. Safe to call exactly once per
			// worker. The monotonic counter bump happens here so the
			// user-visible DoneCount is "number of modules that have
			// finished so far" at the moment the line is printed.
			emitProgress := func(mErr error) {
				if p.Progress == nil {
					return
				}
				next := atomic.AddInt64(&doneCount, 1)
				p.Progress(ProgressEvent{
					ModuleID:   m.ID,
					Language:   m.Language,
					FileCount:  len(m.Files),
					ByteCount:  byteCount,
					Elapsed:    p.Now().Sub(start),
					Err:        mErr,
					DoneCount:  int(next),
					TotalCount: totalCount,
				})
			}

			body, sErr := p.Summarizer.Summarize(ctx, SummaryRequest{
				ProjectName: req.ProjectName,
				Module:      m,
			})
			if sErr != nil {
				out[i] = moduleResult{module: m, err: &ModuleError{
					ModuleID: m.ID,
					Reason:   "SUMMARIZER_FAILED",
					Err:      sErr,
				}}
				emitProgress(sErr)
				return
			}
			if body == "" {
				out[i] = moduleResult{module: m, err: &ModuleError{
					ModuleID: m.ID,
					Reason:   "SUMMARIZER_EMPTY",
				}}
				emitProgress(fmt.Errorf("SUMMARIZER_EMPTY"))
				return
			}
			if req.DryRun {
				out[i] = moduleResult{module: m, entryID: "dry-run:" + m.ID}
				emitProgress(nil)
				return
			}
			entryID, wErr := p.Writer.WriteModule(ctx, EntryRequest{
				ProjectName: req.ProjectName,
				ModuleID:    m.ID,
				Body:        body,
				Files:       fileRelPaths(m),
			})
			if wErr != nil {
				out[i] = moduleResult{module: m, err: &ModuleError{
					ModuleID: m.ID,
					Reason:   "WRITE_FAILED",
					Err:      wErr,
				}}
				emitProgress(wErr)
				return
			}
			out[i] = moduleResult{module: m, entryID: entryID}

			// Incremental state persistence (cortex-so5): append the
			// just-completed module and persist the state file so
			// `cortex ingest status` reflects real-time progress.
			// StateStore.Write failures are surfaced to stderr via
			// the Logger but must not abort the run — the end-of-run
			// authoritative write will also try to persist, and a
			// resume is always idempotent on CompletedModuleIDs.
			if !req.DryRun {
				stateMu.Lock()
				state.CompletedModuleIDs = append(state.CompletedModuleIDs, m.ID)
				snapshot := state
				stateMu.Unlock()
				if err := p.StateStore.Write(ctx, snapshot); err != nil {
					p.Logger.Warn("INGEST_STATE_WRITE_FAILED",
						map[string]any{
							"module": m.ID,
							"error":  err.Error(),
						})
				}
			}

			emitProgress(nil)
		}()
	}
	wg.Wait()

	// Stable ordering: iterate todo (already sorted by languages.Group).
	// Note: CompletedModuleIDs is appended incrementally by the worker
	// goroutines under stateMu (cortex-so5), so we no longer touch it
	// here — only collect per-module results into res.
	for _, r := range out {
		if r.err != nil {
			res.SummaryErrors = append(res.SummaryErrors, *r.err)
			continue
		}
		res.EntryIDs = append(res.EntryIDs, r.entryID)
	}

	// --- synthesize trail ----------------------------------------------
	finished := p.Now()
	if len(res.EntryIDs) > 0 {
		trailID := fmt.Sprintf("trail:ingest:%s:%s",
			req.ProjectName, started.UTC().Format(time.RFC3339Nano))
		res.TrailID = trailID
		if !req.DryRun {
			if err := p.TrailAppender.AppendTrail(ctx, TrailRequest{
				ProjectName: req.ProjectName,
				TrailID:     trailID,
				EntryIDs:    res.EntryIDs,
				StartedAt:   started,
				FinishedAt:  finished,
			}); err != nil {
				return res, errs.Operational("TRAIL_APPEND_FAILED",
					"could not append ingest trail", err)
			}
		}
		state.LastTrailID = trailID
	}

	// --- persist state --------------------------------------------------
	state.LastIngestedAt = finished
	state.TotalEntriesWritten += len(res.EntryIDs)
	if req.CommitSHA != "" {
		state.LastCommitSHA = req.CommitSHA
	}
	// Deduplicate CompletedModuleIDs to keep the state bounded across
	// many re-runs — go guarantees append adds; we compress here.
	state.CompletedModuleIDs = dedupSorted(state.CompletedModuleIDs)
	if !req.DryRun {
		if err := p.StateStore.Write(ctx, state); err != nil {
			return res, errs.Operational("STATE_WRITE_FAILED",
				"could not persist ingest state", err)
		}
	}
	res.State = state
	res.FinishedAt = finished

	// --- post-ingest reflect (opt-out via SkipPostReflect) -------------
	if !req.DryRun && !p.SkipPostReflect && p.PostReflect != nil && res.TrailID != "" {
		if err := p.PostReflect.ReflectScope(ctx, res.TrailID); err != nil {
			// Reflection is advisory; a failure is surfaced but does
			// not invalidate the ingest. Following the spec's
			// "graceful skip on reflection failures" language.
			res.SummaryErrors = append(res.SummaryErrors, ModuleError{
				ModuleID: res.TrailID,
				Reason:   "POST_INGEST_REFLECT_FAILED",
				Err:      err,
			})
		}
	}
	return res, nil
}

// Status returns the persisted state for one project without side
// effects. Used by the `cortex ingest status` subcommand (AC3).
func (p *Pipeline) Status(ctx context.Context, project string) (ProjectState, bool, error) {
	if project == "" {
		return ProjectState{}, false, errs.Validation("MISSING_PROJECT_NAME",
			"cortex ingest status requires --project", nil)
	}
	state, ok, err := p.StateStore.Read(ctx, project)
	if err != nil {
		return ProjectState{}, false, errs.Operational("STATE_READ_FAILED",
			"could not read ingest state", err)
	}
	return state, ok, nil
}

func (p *Pipeline) fillDefaults() {
	if p.Concurrency <= 0 {
		p.Concurrency = DefaultOllamaConcurrency
	}
	if p.Now == nil {
		p.Now = func() time.Time { return time.Now().UTC() }
	}
	if p.Logger == nil {
		p.Logger = walker.NopLogger{}
	}
	if p.Matrix.Fallback == "" && len(p.Matrix.Strategies) == 0 {
		p.Matrix = languages.DefaultMatrix()
	}
}

// collectFiles runs the configured walker and returns the flat file
// list that languages.Group consumes. Production wires WalkerFunc to
// a walker.Walk adapter; tests supply an in-memory slice.
func (p *Pipeline) collectFiles(root string) ([]languages.File, error) {
	if p.Walker != nil {
		var out []languages.File
		err := p.Walker(root, func(f languages.File) error {
			out = append(out, f)
			return nil
		})
		return out, err
	}
	var out []languages.File
	err := walker.Walk(walker.Options{
		ProjectRoot: root,
		Logger:      p.Logger,
	}, func(fm walker.FileMeta) error {
		out = append(out, languages.File{
			AbsPath: fm.AbsPath,
			RelPath: fm.RelPath,
		})
		return nil
	})
	return out, err
}

// splitOversizeModules implements the cortex-ks1 size gate. When
// p.MaxModuleBytes is positive, any non-per-file module whose summed
// File.Size exceeds the budget is replaced by one per-file module per
// constituent file. Modules already at per-file granularity, modules
// under the budget, and modules with zero known sizes (test fixtures
// that did not populate languages.File.Size) pass through unchanged.
//
// The split preserves the per-file ID format languages.Group would
// emit ("<lang>:per-file:<relpath>") so resume state on a future run
// keys correctly even if MaxModuleBytes was different on the prior
// run.
func (p *Pipeline) splitOversizeModules(in []Module) []Module {
	if p.MaxModuleBytes <= 0 || len(in) == 0 {
		return in
	}
	out := make([]Module, 0, len(in))
	for _, m := range in {
		if m.Strategy == languages.StrategyPerFile || len(m.Files) <= 1 {
			out = append(out, m)
			continue
		}
		var total int64
		for _, f := range m.Files {
			total += f.Size
		}
		if total <= p.MaxModuleBytes {
			out = append(out, m)
			continue
		}
		for _, f := range m.Files {
			out = append(out, Module{
				ID:       string(m.Language) + ":" + languages.StrategyPerFile + ":" + f.RelPath,
				Language: m.Language,
				Strategy: languages.StrategyPerFile,
				Files:    []languages.File{f},
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func fileRelPaths(m Module) []string {
	out := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		out = append(out, f.RelPath)
	}
	return out
}

// computeDelta maps a DiffResult (changed + deleted relative paths)
// into two sets of module IDs:
//   - evict: modules that contain at least one changed or deleted file
//     and should be removed from CompletedModuleIDs so the pipeline
//     re-processes them on the current walk.
//   - retract: modules whose constituent files were ALL deleted (no
//     changed files remain in that module). These modules will not
//     appear in the walk so they need explicit retraction datoms.
//
// The mapping reuses languages.Classify + Matrix.Strategy to derive the
// same module ID that languages.Group would produce, without needing
// the files on disk.
func (p *Pipeline) computeDelta(diff DiffResult) (evict, retract []string) {
	// Build fake File entries for changed and deleted paths so we can
	// run them through languages.Group to get the canonical module IDs.
	changedFiles := make([]languages.File, 0, len(diff.Changed))
	for _, rel := range diff.Changed {
		changedFiles = append(changedFiles, languages.File{
			AbsPath: rel, // Classify only uses the extension
			RelPath: rel,
		})
	}
	deletedFiles := make([]languages.File, 0, len(diff.Deleted))
	for _, rel := range diff.Deleted {
		deletedFiles = append(deletedFiles, languages.File{
			AbsPath: rel,
			RelPath: rel,
		})
	}

	// Module IDs touched by changed files.
	changedModIDs := make(map[string]bool)
	for _, m := range languages.Group(changedFiles, p.Matrix) {
		changedModIDs[m.ID] = true
	}

	// Module IDs touched by deleted files.
	deletedModIDs := make(map[string]bool)
	for _, m := range languages.Group(deletedFiles, p.Matrix) {
		deletedModIDs[m.ID] = true
	}

	// Evict = union of changed and deleted module IDs.
	seen := make(map[string]bool)
	for id := range changedModIDs {
		if !seen[id] {
			evict = append(evict, id)
			seen[id] = true
		}
	}
	for id := range deletedModIDs {
		if !seen[id] {
			evict = append(evict, id)
			seen[id] = true
		}
	}
	sort.Strings(evict)

	// Retract = deleted-only modules (no changed files in the same module).
	for id := range deletedModIDs {
		if !changedModIDs[id] {
			retract = append(retract, id)
		}
	}
	sort.Strings(retract)
	return evict, retract
}

// removeAll returns a copy of ids with every element in remove deleted.
func removeAll(ids []string, remove []string) []string {
	drop := make(map[string]bool, len(remove))
	for _, r := range remove {
		drop[r] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !drop[id] {
			out = append(out, id)
		}
	}
	return out
}

// dedupSorted returns a copy of in with duplicates removed, preserving
// ascending order. Used to keep CompletedModuleIDs bounded across
// repeated runs so state size does not grow unbounded.
func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return in
	}
	cp := make([]string, len(in))
	copy(cp, in)
	sort.Strings(cp)
	out := cp[:0]
	var last string
	for i, v := range cp {
		if i == 0 || v != last {
			out = append(out, v)
			last = v
		}
	}
	return out
}
