// Package walker implements the repository walker used by cortex ingest.
//
// Acceptance criteria (cortex-4kq.18 / task ingest-walker):
//   - A symlink pointing to a path outside the project root is skipped with
//     a warning written to ops.log.
//   - A file larger than 262144 bytes is skipped and does not appear in the
//     iterator.
//   - Files matching .gitignore patterns are skipped.
//   - ~/.cortex/ and deny-list paths never appear in the iterator.
//
// Spec references:
//   - docs/spec/cortex-spec.md "Integration Boundaries — File System":
//     "Cortex MUST NOT follow symlinks outside the project root by default.
//      Cortex MUST NOT ingest files from ~/.cortex/ or other Cortex internal
//      directories."
//   - config.ingest.module_size_limit_bytes default 262144.
//
// The ops.log writer (internal/opslog) is not yet in the tree. This file
// accepts a Logger interface so the walker compiles today and can be wired
// to the real ops.log writer once it lands (TODO: ops.log integration).
package walker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultModuleSizeLimitBytes mirrors config.Ingest.ModuleSizeLimitBytes.
// Duplicated here to avoid a walker→config dependency cycle and to make the
// walker's acceptance tests self-contained.
const DefaultModuleSizeLimitBytes int64 = 262144

// alwaysSkipDirs names directory basenames that are pruned from the walk
// unconditionally, BEFORE any .gitignore / .cortexignore / DenyList check.
// These fall into two buckets:
//
//  1. VCS metadata (.git, .hg, .svn) — never source code, and letting them
//     leak into ingest pollutes memory with binary index objects.
//  2. Universally generated or vendored directories (node_modules, .next,
//     .nuxt, .svelte-kit, .turbo, __pycache__, .venv, .gradle, .mypy_cache,
//     .pytest_cache). These are machine-written by package managers or
//     build tools and never contain human-authored source worth recalling.
//     Including them balloons walk time (Next.js .next/dev/server/chunks
//     alone can be thousands of 250KB JS blobs) and floods the summarizer
//     with minified/transpiled noise that drowns out signal.
//
// This list is hardcoded rather than config-driven because there is no
// plausible project where a directory with one of these basenames holds
// source the operator wants indexed. If an operator needs the escape
// hatch (e.g. ingesting the implementation of a package manager itself),
// they can fork the walker. Cheaper than letting .next/ctx leak into
// every recall result.
//
// Operators can still EXPAND the skip set per-project via .cortexignore;
// this list is a floor, not a ceiling.
var alwaysSkipDirs = map[string]struct{}{
	".git":         {},
	".hg":          {},
	".svn":         {},
	"node_modules": {},
	".next":        {},
	".nuxt":        {},
	".svelte-kit":  {},
	".turbo":       {},
	".cache":       {},
	"__pycache__":  {},
	".pytest_cache": {},
	".mypy_cache":  {},
	".venv":        {},
	".gradle":      {},
	".tox":         {},
}

// FileMeta describes one walked file passed to the consumer callback.
type FileMeta struct {
	// AbsPath is the canonical (EvalSymlinks-resolved) absolute path.
	AbsPath string
	// RelPath is the path relative to Options.ProjectRoot, always forward-slashed.
	RelPath string
	// Size is the file size in bytes.
	Size int64
	// Mode is the file mode reported by os.Lstat.
	Mode fs.FileMode
}

// Logger is the minimal interface the walker uses to emit warnings.
//
// It is intentionally a superset of what internal/opslog will expose: the
// walker calls Warn(event, fields) where event is a stable string code
// ("WALKER_SYMLINK_ESCAPE", "WALKER_FILE_TOO_LARGE", etc.) and fields is a
// small map of contextual key/value pairs. The ops.log writer, once
// available, will satisfy this interface directly.
//
// TODO(cortex-4kq.18): swap call sites to the real ops.log writer.
type Logger interface {
	Warn(event string, fields map[string]any)
}

// NopLogger discards all warnings. Useful in tests and in code paths that
// have not yet been wired to ops.log.
type NopLogger struct{}

// Warn implements Logger by doing nothing.
func (NopLogger) Warn(string, map[string]any) {}

// Options configures a Walk invocation.
type Options struct {
	// ProjectRoot is the absolute path to the repository root. Required.
	ProjectRoot string

	// ModuleSizeLimitBytes is the inclusive upper bound for file size.
	// Files strictly greater than this value are skipped with a warning.
	// Zero means "use DefaultModuleSizeLimitBytes".
	ModuleSizeLimitBytes int64

	// DenyList is a list of glob patterns evaluated against the forward-
	// slashed path relative to ProjectRoot. A file whose RelPath matches
	// any pattern is skipped.
	//
	// Patterns use filepath.Match semantics (with forward slashes).
	// Upstream note: task graph says deny_list source is "config", but
	// config.IngestConfig does not yet expose a DenyList field. The
	// walker accepts it explicitly here so it can be wired later without
	// touching the walker's API.
	DenyList []string

	// ExtraIgnoreFiles is a list of additional gitignore-syntax files
	// whose rules are merged into the same matcher the walker builds
	// from the project-root .gitignore. Paths are absolute. Files that
	// do not exist are silently skipped (unlike the .gitignore path
	// itself, a missing extra-ignore file is a first-run condition the
	// caller typically handles by creating the file). Used by the
	// ingest CLI to pass the project-root .cortexignore. See cortex-8rk.
	ExtraIgnoreFiles []string

	// CortexHome is the path that must be excluded unconditionally. When
	// empty, it is resolved from $HOME/.cortex.
	CortexHome string

	// Logger receives structured warnings for skipped files and symlinks.
	// If nil, NopLogger is used.
	Logger Logger
}

// Walk traverses opts.ProjectRoot and calls fn for every eligible file.
//
// Eligibility rules:
//  1. Symlinks whose resolved target escapes ProjectRoot are skipped with
//     WALKER_SYMLINK_ESCAPE.
//  2. Files larger than ModuleSizeLimitBytes are skipped with
//     WALKER_FILE_TOO_LARGE.
//  3. Files whose RelPath matches any DenyList glob are skipped with
//     WALKER_DENY_LISTED.
//  4. Files inside CortexHome (canonical resolution) are skipped with
//     WALKER_CORTEX_INTERNAL.
//  5. Files matching any .gitignore pattern (project-root .gitignore) are
//     skipped with WALKER_GITIGNORED. (.gitignore support is minimal and
//     line-oriented; extended pattern support is a Phase 2 concern and is
//     tracked alongside cortex-4kq.18 acceptance.)
//
// fn may return filepath.SkipDir to prune the current subtree or
// filepath.SkipAll / any other error to terminate the walk.
func Walk(opts Options, fn func(FileMeta) error) error {
	if opts.ProjectRoot == "" {
		return errors.New("walker: ProjectRoot is required")
	}

	root, err := filepath.Abs(opts.ProjectRoot)
	if err != nil {
		return fmt.Errorf("walker: resolve project root: %w", err)
	}
	rootCanonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("walker: eval project root symlinks: %w", err)
	}

	sizeLimit := opts.ModuleSizeLimitBytes
	if sizeLimit <= 0 {
		sizeLimit = DefaultModuleSizeLimitBytes
	}

	logger := opts.Logger
	if logger == nil {
		logger = NopLogger{}
	}

	cortexHome := opts.CortexHome
	if cortexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cortexHome = filepath.Join(home, ".cortex")
		}
	}
	var cortexHomeCanonical string
	if cortexHome != "" {
		if resolved, err := filepath.EvalSymlinks(cortexHome); err == nil {
			cortexHomeCanonical = resolved
		} else {
			cortexHomeCanonical = cortexHome
		}
	}

	// Load project-root .gitignore if present. Nested .gitignore support
	// is deferred; acceptance exercises the root-level case.
	var ignore gitignoreMatcher
	if err := ignore.loadFromFile(filepath.Join(rootCanonical, ".gitignore")); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("WALKER_GITIGNORE_READ_ERROR", map[string]any{
			"path":  filepath.Join(rootCanonical, ".gitignore"),
			"error": err.Error(),
		})
	}
	// Merge any caller-supplied extra ignore files (e.g. .cortexignore)
	// into the same matcher. A missing file is silently skipped — the
	// first-run bootstrap that creates the file is the caller's
	// responsibility. Other read errors are surfaced via the logger but
	// do not abort the walk. See cortex-8rk.
	for _, extra := range opts.ExtraIgnoreFiles {
		if err := ignore.loadFromFile(extra); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("WALKER_EXTRA_IGNORE_READ_ERROR", map[string]any{
				"path":  extra,
				"error": err.Error(),
			})
		}
	}

	return filepath.WalkDir(rootCanonical, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			logger.Warn("WALKER_WALK_ERROR", map[string]any{
				"path":  path,
				"error": walkErr.Error(),
			})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Always skip VCS metadata and universally-generated directories
		// without consulting .gitignore so the iterator never sees
		// machine-written source, even for repos that don't list these
		// paths in .gitignore / .cortexignore. See alwaysSkipDirs.
		if d.IsDir() {
			if _, skip := alwaysSkipDirs[filepath.Base(path)]; skip {
				return filepath.SkipDir
			}
		}

		// Canonicalize and compute rel path (forward slashes).
		rel, err := filepath.Rel(rootCanonical, path)
		if err != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)

		// Deny list / cortex-home / gitignore filtering applies to both
		// directories (so we can prune early) and files.
		if matched, _ := matchAny(opts.DenyList, relSlash); matched {
			logger.Warn("WALKER_DENY_LISTED", map[string]any{"path": relSlash})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if cortexHomeCanonical != "" && isWithin(path, cortexHomeCanonical) {
			logger.Warn("WALKER_CORTEX_INTERNAL", map[string]any{"path": path})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if ignore.match(relSlash, d.IsDir()) {
			logger.Warn("WALKER_GITIGNORED", map[string]any{"path": relSlash})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// For non-directories, stat via Lstat so we can detect symlinks
		// before following them.
		info, err := os.Lstat(path)
		if err != nil {
			logger.Warn("WALKER_LSTAT_ERROR", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
			return nil
		}

		// Symlink handling: never follow a symlink that escapes root.
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				logger.Warn("WALKER_BROKEN_SYMLINK", map[string]any{
					"path":  path,
					"error": err.Error(),
				})
				return nil
			}
			if !isWithin(resolved, rootCanonical) {
				logger.Warn("WALKER_SYMLINK_ESCAPE", map[string]any{
					"path":   path,
					"target": resolved,
				})
				return nil
			}
			// Safe symlink: re-stat the target so Size reflects the real file.
			targetInfo, err := os.Stat(resolved)
			if err != nil {
				logger.Warn("WALKER_SYMLINK_STAT_ERROR", map[string]any{
					"path":   path,
					"target": resolved,
					"error":  err.Error(),
				})
				return nil
			}
			info = targetInfo
		}

		if info.IsDir() {
			return nil
		}

		if info.Size() > sizeLimit {
			logger.Warn("WALKER_FILE_TOO_LARGE", map[string]any{
				"path":  relSlash,
				"size":  info.Size(),
				"limit": sizeLimit,
			})
			return nil
		}

		return fn(FileMeta{
			AbsPath: path,
			RelPath: relSlash,
			Size:    info.Size(),
			Mode:    info.Mode(),
		})
	})
}

// isWithin reports whether child lies inside parent after canonicalization.
// Both paths are expected to be absolute.
func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !strings.HasPrefix(rel, "/")
}

// matchAny reports whether any glob in patterns matches p. Patterns use
// filepath.Match semantics on forward-slashed input.
func matchAny(patterns []string, p string) (bool, error) {
	for _, pat := range patterns {
		ok, err := filepath.Match(pat, p)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
		// Also match against the basename so patterns like "*.bin" work
		// without requiring callers to anchor them.
		if ok2, _ := filepath.Match(pat, filepath.Base(p)); ok2 {
			return true, nil
		}
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Minimal .gitignore matcher.
//
// This is intentionally small: it supports literal paths, basename globs, and
// directory suffix patterns ("build/"). It does NOT implement the full
// gitignore specification (negation with "!", nested ignores, character
// classes beyond filepath.Match). That's sufficient for the acceptance
// criteria and keeps the walker stdlib-only. Extended support can be layered
// on later without changing the walker's public API.
// ---------------------------------------------------------------------------

type gitignoreRule struct {
	raw       string // original pattern line
	dirOnly   bool   // trailing slash
	anchored  bool   // leading slash
	pattern   string // normalized pattern
}

type gitignoreMatcher struct {
	rules []gitignoreRule
}

func (g *gitignoreMatcher) loadFromFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			// Negation is not supported in the minimal matcher. Skip.
			continue
		}
		r := gitignoreRule{raw: line}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.anchored = true
			line = strings.TrimPrefix(line, "/")
		}
		r.pattern = line
		g.rules = append(g.rules, r)
	}
	// Stable order for deterministic test output.
	sort.SliceStable(g.rules, func(i, j int) bool { return g.rules[i].raw < g.rules[j].raw })
	return nil
}

func (g *gitignoreMatcher) match(relSlash string, isDir bool) bool {
	if len(g.rules) == 0 {
		return false
	}
	base := filepath.Base(relSlash)
	for _, r := range g.rules {
		if r.dirOnly && !isDir {
			// Directory-only rule still matches files inside that dir via
			// the walker's SkipDir path; for pure file matches we skip.
			continue
		}
		if r.anchored {
			if ok, _ := filepath.Match(r.pattern, relSlash); ok {
				return true
			}
			continue
		}
		// Unanchored: match on full rel path OR basename.
		if ok, _ := filepath.Match(r.pattern, relSlash); ok {
			return true
		}
		if ok, _ := filepath.Match(r.pattern, base); ok {
			return true
		}
		// Also match if pattern appears as any path component.
		for _, part := range strings.Split(relSlash, "/") {
			if ok, _ := filepath.Match(r.pattern, part); ok {
				return true
			}
		}
	}
	return false
}
