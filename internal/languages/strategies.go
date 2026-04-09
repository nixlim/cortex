// Package languages implements the built-in ingest language-strategy matrix
// from docs/spec/cortex-spec.md "Configuration Defaults — ingest.default_strategy".
//
// Responsibilities (cortex-4kq.25 / task ingest-language-strategies):
//   - Classify a walked file by extension to a built-in language.
//   - Select the configured strategy for that language (per-package, per-class,
//     per-file, per-module, per-pair, per-class-or-module) or the fallback.
//   - Group a stream of walked files into Modules according to strategy.
//
// Acceptance (task graph):
//   - A Go project with two packages foo and bar produces exactly two modules.
//   - A Python project with three .py files produces three modules.
//   - A C++ project with foo.h and foo.cpp pairs produces one module.
//   - A file with an unknown extension falls back to per-file.
//
// This package is pure (no I/O, no logger). The walker streams FileMeta into
// it and it returns grouped Modules. Tests will live alongside once the
// cortex-4kq.25 bead is claimed.
package languages

import (
	"path/filepath"
	"sort"
	"strings"
)

// Language is a stable identifier for a built-in language group.
type Language string

const (
	LangGo                   Language = "go"
	LangJava                 Language = "java"
	LangKotlin               Language = "kotlin"
	LangPython               Language = "python"
	LangJavaScriptTypeScript Language = "javascript_typescript"
	LangRust                 Language = "rust"
	LangCSharp               Language = "csharp"
	LangRuby                 Language = "ruby"
	LangCCpp                 Language = "c_cpp"
	LangUnknown              Language = "unknown"
)

// Strategy names from the spec's default_strategy table.
const (
	StrategyPerPackage       = "per-package"
	StrategyPerClass         = "per-class"
	StrategyPerFile          = "per-file"
	StrategyPerModule        = "per-module"
	StrategyPerPair          = "per-pair"
	StrategyPerClassOrModule = "per-class-or-module"
)

// Matrix maps a Language to the strategy name to use for it, with a
// fallback strategy for files whose extension does not match any known
// language.
//
// A concrete Matrix is usually derived from config.Ingest.DefaultStrategy,
// but the languages package does not import internal/config to avoid a
// dependency cycle. Callers build the matrix at the ingest boundary via
// MatrixFromDefaults or by hand in tests.
type Matrix struct {
	Strategies map[Language]string
	Fallback   string
}

// DefaultMatrix returns the Phase 1 built-in defaults from cortex-spec.md.
// It is the canonical fallback for tests and for code paths that do not
// have a config.Config instance available.
func DefaultMatrix() Matrix {
	return Matrix{
		Strategies: map[Language]string{
			LangGo:                   StrategyPerPackage,
			LangJava:                 StrategyPerClass,
			LangKotlin:               StrategyPerFile,
			LangPython:               StrategyPerFile,
			LangJavaScriptTypeScript: StrategyPerFile,
			LangRust:                 StrategyPerModule,
			LangCSharp:               StrategyPerClass,
			LangRuby:                 StrategyPerClassOrModule,
			LangCCpp:                 StrategyPerPair,
		},
		Fallback: StrategyPerFile,
	}
}

// Classify maps a file path to a built-in Language by extension.
// Unknown extensions map to LangUnknown.
func Classify(path string) Language {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return LangGo
	case ".java":
		return LangJava
	case ".kt", ".kts":
		return LangKotlin
	case ".py", ".pyi":
		return LangPython
	case ".js", ".mjs", ".cjs", ".jsx", ".ts", ".tsx":
		return LangJavaScriptTypeScript
	case ".rs":
		return LangRust
	case ".cs":
		return LangCSharp
	case ".rb":
		return LangRuby
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".hxx":
		return LangCCpp
	}
	return LangUnknown
}

// Strategy returns the strategy name the matrix has configured for lang,
// or the matrix fallback when there is no explicit entry (including
// LangUnknown).
func (m Matrix) Strategy(lang Language) string {
	if s, ok := m.Strategies[lang]; ok && s != "" {
		return s
	}
	if m.Fallback != "" {
		return m.Fallback
	}
	return StrategyPerFile
}

// File is the minimal shape the grouper needs from the walker. It is a
// structural subset of walker.FileMeta and kept duplicated to avoid an
// internal/languages → internal/walker dependency that would pin the
// package ordering.
type File struct {
	AbsPath string
	RelPath string // forward-slashed, relative to project root
}

// Module is a group of one or more files that belong together under some
// language strategy.
type Module struct {
	// ID is a stable identifier for the module, unique within one Group call.
	// Format: "<language>:<strategy>:<key>".
	ID string
	// Language is the detected language, or LangUnknown for the fallback bucket.
	Language Language
	// Strategy is the strategy name that produced this grouping.
	Strategy string
	// Files are the member files sorted lexicographically by RelPath.
	Files []File
}

// Group partitions files into Modules according to the matrix.
//
// The function is deterministic: input order does not affect output; files
// are always sorted within a module and modules are sorted by ID.
func Group(files []File, m Matrix) []Module {
	// Copy so we don't mutate the caller's slice.
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RelPath < sorted[j].RelPath })

	byKey := map[string]*Module{}
	keyOrder := []string{}

	addTo := func(key string, lang Language, strategy string, f File) {
		mod, ok := byKey[key]
		if !ok {
			mod = &Module{
				ID:       key,
				Language: lang,
				Strategy: strategy,
			}
			byKey[key] = mod
			keyOrder = append(keyOrder, key)
		}
		mod.Files = append(mod.Files, f)
	}

	for _, f := range sorted {
		lang := Classify(f.AbsPath)
		strategy := m.Strategy(lang)

		var key string
		switch strategy {
		case StrategyPerPackage:
			// Group all same-language files in the same directory.
			dir := filepath.ToSlash(filepath.Dir(f.RelPath))
			key = string(lang) + ":" + strategy + ":" + dir

		case StrategyPerModule:
			// For Rust the "module" boundary is a directory containing a
			// mod.rs or lib.rs sibling; approximate by directory in Phase 1.
			dir := filepath.ToSlash(filepath.Dir(f.RelPath))
			key = string(lang) + ":" + strategy + ":" + dir

		case StrategyPerPair:
			// C/C++ header+source pair: group by directory + basename
			// (without extension) so foo.h and foo.cpp land together.
			dir := filepath.ToSlash(filepath.Dir(f.RelPath))
			stem := strings.TrimSuffix(filepath.Base(f.RelPath), filepath.Ext(f.RelPath))
			key = string(lang) + ":" + strategy + ":" + dir + "/" + stem

		case StrategyPerClass:
			// Approximate one-class-per-file. Java/C#/Kotlin conventions
			// generally co-locate one public class per file; refining this
			// to parse for multiple top-level classes is Phase 2.
			key = string(lang) + ":" + strategy + ":" + f.RelPath

		case StrategyPerClassOrModule:
			// Ruby: approximate as per-file in Phase 1.
			key = string(lang) + ":" + strategy + ":" + f.RelPath

		case StrategyPerFile:
			fallthrough
		default:
			key = string(lang) + ":" + StrategyPerFile + ":" + f.RelPath
		}

		addTo(key, lang, strategy, f)
	}

	out := make([]Module, 0, len(keyOrder))
	sort.Strings(keyOrder)
	for _, k := range keyOrder {
		mod := byKey[k]
		sort.Slice(mod.Files, func(i, j int) bool {
			return mod.Files[i].RelPath < mod.Files[j].RelPath
		})
		out = append(out, *mod)
	}
	return out
}
