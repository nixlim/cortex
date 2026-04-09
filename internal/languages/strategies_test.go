package languages

import (
	"sort"
	"testing"
)

// buildFiles constructs a []File with AbsPath == RelPath so Classify runs
// against the same extension data in both places.
func buildFiles(rels ...string) []File {
	out := make([]File, 0, len(rels))
	for _, r := range rels {
		out = append(out, File{AbsPath: r, RelPath: r})
	}
	return out
}

func modIDs(mods []Module) []string {
	out := make([]string, 0, len(mods))
	for _, m := range mods {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Acceptance: "A Go project with two packages foo and bar produces exactly
// two modules."
// ---------------------------------------------------------------------------

func TestGoPerPackageGroupsByDirectory(t *testing.T) {
	files := buildFiles(
		"foo/a.go",
		"foo/b.go",
		"foo/c.go",
		"bar/d.go",
		"bar/e.go",
	)
	mods := Group(files, DefaultMatrix())
	if got := len(mods); got != 2 {
		t.Fatalf("expected 2 modules, got %d: %+v", got, mods)
	}

	var fooMod, barMod *Module
	for i := range mods {
		switch {
		case mods[i].ID == "go:per-package:bar":
			barMod = &mods[i]
		case mods[i].ID == "go:per-package:foo":
			fooMod = &mods[i]
		}
	}
	if fooMod == nil || barMod == nil {
		t.Fatalf("expected both foo and bar modules, got IDs %v", modIDs(mods))
	}
	if len(fooMod.Files) != 3 {
		t.Errorf("foo module should have 3 files, got %d", len(fooMod.Files))
	}
	if len(barMod.Files) != 2 {
		t.Errorf("bar module should have 2 files, got %d", len(barMod.Files))
	}
	if fooMod.Strategy != StrategyPerPackage || fooMod.Language != LangGo {
		t.Errorf("foo module metadata wrong: %+v", fooMod)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A Python project with three .py files produces three modules
// (per-file)."
// ---------------------------------------------------------------------------

func TestPythonPerFileProducesOnePerFile(t *testing.T) {
	files := buildFiles("a.py", "sub/b.py", "sub/c.py")
	mods := Group(files, DefaultMatrix())
	if len(mods) != 3 {
		t.Fatalf("expected 3 modules, got %d: %+v", len(mods), mods)
	}
	for _, m := range mods {
		if m.Language != LangPython {
			t.Errorf("module %q language = %q, want python", m.ID, m.Language)
		}
		if m.Strategy != StrategyPerFile {
			t.Errorf("module %q strategy = %q, want per-file", m.ID, m.Strategy)
		}
		if len(m.Files) != 1 {
			t.Errorf("module %q should have exactly 1 file, got %d", m.ID, len(m.Files))
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A C++ project with foo.h and foo.cpp pairs produces one
// module."
// ---------------------------------------------------------------------------

func TestCCppPerPairGroupsHeaderAndSource(t *testing.T) {
	files := buildFiles(
		"src/foo.h",
		"src/foo.cpp",
		"src/bar.h",
		"src/bar.cpp",
	)
	mods := Group(files, DefaultMatrix())
	if len(mods) != 2 {
		t.Fatalf("expected 2 modules (foo + bar), got %d: %+v", len(mods), mods)
	}
	for _, m := range mods {
		if m.Language != LangCCpp {
			t.Errorf("module %q language = %q, want c_cpp", m.ID, m.Language)
		}
		if m.Strategy != StrategyPerPair {
			t.Errorf("module %q strategy = %q, want per-pair", m.ID, m.Strategy)
		}
		if len(m.Files) != 2 {
			t.Errorf("module %q should have 2 files (.h + .cpp), got %d", m.ID, len(m.Files))
		}
	}
}

func TestSingleCppFileStillFormsOneModule(t *testing.T) {
	files := buildFiles("src/only.cpp")
	mods := Group(files, DefaultMatrix())
	if len(mods) != 1 {
		t.Fatalf("expected 1 module, got %d", len(mods))
	}
	if mods[0].Strategy != StrategyPerPair {
		t.Errorf("strategy = %q, want per-pair", mods[0].Strategy)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A file with an unknown extension falls back to per-file and
// appears as its own module."
// ---------------------------------------------------------------------------

func TestUnknownExtensionFallsBackToPerFile(t *testing.T) {
	files := buildFiles("README.md", "config.ini", "data.dat")
	mods := Group(files, DefaultMatrix())
	if len(mods) != 3 {
		t.Fatalf("expected 3 fallback modules, got %d", len(mods))
	}
	for _, m := range mods {
		if m.Language != LangUnknown {
			t.Errorf("unknown-ext module language = %q, want unknown", m.Language)
		}
		if m.Strategy != StrategyPerFile {
			t.Errorf("fallback strategy = %q, want per-file", m.Strategy)
		}
		if len(m.Files) != 1 {
			t.Errorf("fallback module should have exactly 1 file, got %d", len(m.Files))
		}
	}
}

// ---------------------------------------------------------------------------
// Classifier coverage for every declared language.
// ---------------------------------------------------------------------------

func TestClassifyMapsAllDeclaredLanguages(t *testing.T) {
	cases := map[string]Language{
		"main.go":       LangGo,
		"App.java":      LangJava,
		"main.kt":       LangKotlin,
		"build.kts":     LangKotlin,
		"util.py":       LangPython,
		"types.pyi":     LangPython,
		"index.js":      LangJavaScriptTypeScript,
		"app.mjs":       LangJavaScriptTypeScript,
		"legacy.cjs":    LangJavaScriptTypeScript,
		"component.jsx": LangJavaScriptTypeScript,
		"module.ts":     LangJavaScriptTypeScript,
		"view.tsx":      LangJavaScriptTypeScript,
		"lib.rs":        LangRust,
		"Program.cs":    LangCSharp,
		"gem.rb":        LangRuby,
		"header.h":      LangCCpp,
		"source.c":      LangCCpp,
		"obj.cpp":       LangCCpp,
		"other.cxx":     LangCCpp,
		"nope.unknown":  LangUnknown,
		"noext":         LangUnknown,
	}
	for path, want := range cases {
		if got := Classify(path); got != want {
			t.Errorf("Classify(%q) = %q, want %q", path, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Matrix strategy lookup falls back correctly.
// ---------------------------------------------------------------------------

func TestMatrixFallback(t *testing.T) {
	m := Matrix{
		Strategies: map[Language]string{LangGo: StrategyPerPackage},
		Fallback:   StrategyPerFile,
	}
	if got := m.Strategy(LangGo); got != StrategyPerPackage {
		t.Errorf("Strategy(go) = %q, want per-package", got)
	}
	if got := m.Strategy(LangPython); got != StrategyPerFile {
		t.Errorf("Strategy(python) missing entry should fall back to per-file, got %q", got)
	}
	if got := m.Strategy(LangUnknown); got != StrategyPerFile {
		t.Errorf("Strategy(unknown) = %q, want per-file fallback", got)
	}
}

func TestMatrixEmptyFallbackIsPerFile(t *testing.T) {
	var m Matrix
	if got := m.Strategy(LangGo); got != StrategyPerFile {
		t.Errorf("zero-value Matrix should return per-file, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Group determinism: input order does not affect output.
// ---------------------------------------------------------------------------

func TestGroupIsDeterministic(t *testing.T) {
	a := buildFiles("bar/d.go", "foo/a.go", "foo/b.go", "bar/c.go")
	b := buildFiles("foo/b.go", "bar/c.go", "foo/a.go", "bar/d.go")
	ma := Group(a, DefaultMatrix())
	mb := Group(b, DefaultMatrix())
	if len(ma) != len(mb) {
		t.Fatalf("module count mismatch: %d vs %d", len(ma), len(mb))
	}
	for i := range ma {
		if ma[i].ID != mb[i].ID {
			t.Errorf("module[%d] ID mismatch: %q vs %q", i, ma[i].ID, mb[i].ID)
		}
		if len(ma[i].Files) != len(mb[i].Files) {
			t.Errorf("module[%d] file count mismatch", i)
			continue
		}
		for j := range ma[i].Files {
			if ma[i].Files[j].RelPath != mb[i].Files[j].RelPath {
				t.Errorf("module[%d] file[%d] rel path mismatch: %q vs %q",
					i, j, ma[i].Files[j].RelPath, mb[i].Files[j].RelPath)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Rust per-module approximation groups by directory.
// ---------------------------------------------------------------------------

func TestRustPerModuleGroupsByDirectory(t *testing.T) {
	files := buildFiles("src/foo/mod.rs", "src/foo/helper.rs", "src/bar/mod.rs")
	mods := Group(files, DefaultMatrix())
	if len(mods) != 2 {
		t.Fatalf("expected 2 rust modules, got %d: %+v", len(mods), mods)
	}
	for _, m := range mods {
		if m.Language != LangRust {
			t.Errorf("language = %q, want rust", m.Language)
		}
		if m.Strategy != StrategyPerModule {
			t.Errorf("strategy = %q, want per-module", m.Strategy)
		}
	}
}

// ---------------------------------------------------------------------------
// Java per-class: one module per file.
// ---------------------------------------------------------------------------

func TestJavaPerClassOneModulePerFile(t *testing.T) {
	files := buildFiles("src/com/foo/A.java", "src/com/foo/B.java")
	mods := Group(files, DefaultMatrix())
	if len(mods) != 2 {
		t.Fatalf("expected 2 java modules, got %d", len(mods))
	}
	for _, m := range mods {
		if m.Strategy != StrategyPerClass {
			t.Errorf("strategy = %q, want per-class", m.Strategy)
		}
		if len(m.Files) != 1 {
			t.Errorf("per-class module should have 1 file, got %d", len(m.Files))
		}
	}
}
