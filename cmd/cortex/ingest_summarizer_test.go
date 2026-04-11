package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nixlim/cortex/internal/languages"
)

// TestBuildModuleSourceBody_FitsWithinBudget asserts that when the
// combined source fits inside the byte budget, every file is included
// verbatim — no truncation marker appears and each file's full
// contents are preserved. This is the common case for typical cortex
// modules at 32K num_ctx.
func TestBuildModuleSourceBody_FitsWithinBudget(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	aAbs := write("a.go", "package a\nconst DefaultX = 42\n")
	bAbs := write("b.go", "package a\nfunc Leiden() {}\n")

	mod := languages.Module{
		ID:       "go:one-module-per-dir:example",
		Language: languages.LangGo,
		Files: []languages.File{
			{AbsPath: aAbs, RelPath: "a.go"},
			{AbsPath: bAbs, RelPath: "b.go"},
		},
	}

	body, err := buildModuleSourceBody(mod, 100_000)
	if err != nil {
		t.Fatalf("buildModuleSourceBody: %v", err)
	}
	if !strings.Contains(body, "=== FILE: a.go ===") {
		t.Error("missing a.go header")
	}
	if !strings.Contains(body, "=== FILE: b.go ===") {
		t.Error("missing b.go header")
	}
	if !strings.Contains(body, "const DefaultX = 42") {
		t.Error("missing a.go content (verbatim)")
	}
	if !strings.Contains(body, "func Leiden()") {
		t.Error("missing b.go content (verbatim)")
	}
	if strings.Contains(body, "[truncated") {
		t.Error("unexpected truncation marker for within-budget module")
	}
}

// TestBuildModuleSourceBody_TruncatesOversize asserts that when a
// module exceeds the byte budget, each file is truncated proportional
// to its share of the total and a clear "[truncated N bytes]" marker
// is written after the included prefix.
func TestBuildModuleSourceBody_TruncatesOversize(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 4_000)
	p := filepath.Join(dir, "big.go")
	if err := os.WriteFile(p, []byte(big), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mod := languages.Module{
		ID:       "go:single:big",
		Language: languages.LangGo,
		Files: []languages.File{
			{AbsPath: p, RelPath: "big.go"},
		},
	}
	body, err := buildModuleSourceBody(mod, 1_000)
	if err != nil {
		t.Fatalf("buildModuleSourceBody: %v", err)
	}
	if !strings.Contains(body, "[truncated") {
		t.Errorf("expected truncation marker in body, got:\n%s", body)
	}
	if strings.Count(body, "x") >= 4_000 {
		t.Errorf("expected body to contain fewer than all 4000 xs (truncated), got %d", strings.Count(body, "x"))
	}
}

// TestBuildModuleSourceBody_UnreadableFileNoted asserts that a file
// that fails to read does not abort the whole module — it is recorded
// inline with "(unreadable: ...)" and subsequent files are still
// processed.
func TestBuildModuleSourceBody_UnreadableFileNoted(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.go")
	if err := os.WriteFile(good, []byte("package a\n"), 0o600); err != nil {
		t.Fatalf("write good: %v", err)
	}
	bad := filepath.Join(dir, "does-not-exist.go")

	mod := languages.Module{
		ID:       "go:mixed",
		Language: languages.LangGo,
		Files: []languages.File{
			{AbsPath: bad, RelPath: "does-not-exist.go"},
			{AbsPath: good, RelPath: "good.go"},
		},
	}
	body, err := buildModuleSourceBody(mod, 100_000)
	if err != nil {
		t.Fatalf("buildModuleSourceBody: %v", err)
	}
	if !strings.Contains(body, "(unreadable:") {
		t.Error("missing unreadable marker for missing file")
	}
	if !strings.Contains(body, "package a") {
		t.Error("subsequent file content dropped after unreadable file")
	}
}

// TestFormatModuleSummaryBody_AllSectionsPresent asserts the happy
// path: prose first, then sections in fixed order (Identifiers,
// Algorithms, Dependencies, Searchable), each with bullet items.
func TestFormatModuleSummaryBody_AllSectionsPresent(t *testing.T) {
	mod := languages.Module{
		ID:       "go:pkg:internal/ingest",
		Language: languages.LangGo,
	}
	p := moduleSummaryPayload{
		Summary:      "Orchestrates the cortex ingest sequence.",
		Identifiers:  []string{"DefaultOllamaConcurrency=4", "func Ingest"},
		Algorithms:   []string{"ulid"},
		Dependencies: []string{"github.com/nixlim/cortex/internal/walker"},
		Searchable:   []string{"module summarization pipeline"},
	}
	body := formatModuleSummaryBody(mod, p)

	mustContain := []string{
		"Module go:pkg:internal/ingest (go).",
		"Orchestrates the cortex ingest sequence.",
		"## Identifiers",
		"- DefaultOllamaConcurrency=4",
		"- func Ingest",
		"## Algorithms",
		"- ulid",
		"## Dependencies",
		"- github.com/nixlim/cortex/internal/walker",
		"## Searchable",
		"- module summarization pipeline",
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("body missing %q; got:\n%s", s, body)
		}
	}
	// Section order check: Identifiers before Algorithms before
	// Dependencies before Searchable.
	order := []string{"## Identifiers", "## Algorithms", "## Dependencies", "## Searchable"}
	prev := -1
	for _, h := range order {
		idx := strings.Index(body, h)
		if idx <= prev {
			t.Errorf("section %q out of order (idx=%d, prev=%d)", h, idx, prev)
		}
		prev = idx
	}
}

// TestFormatModuleSummaryBody_EmptySectionsOmitted asserts that empty
// arrays in the payload do not produce empty headings — short modules
// with no algorithms, dependencies, or searchable hints get a clean
// body with just prose and whatever sections actually have content.
func TestFormatModuleSummaryBody_EmptySectionsOmitted(t *testing.T) {
	mod := languages.Module{
		ID:       "go:pkg:internal/config",
		Language: languages.LangGo,
	}
	p := moduleSummaryPayload{
		Summary:     "Holds the defaults struct.",
		Identifiers: []string{"NumCtx int"},
	}
	body := formatModuleSummaryBody(mod, p)
	if !strings.Contains(body, "## Identifiers") {
		t.Error("Identifiers section should be present")
	}
	for _, s := range []string{"## Algorithms", "## Dependencies", "## Searchable"} {
		if strings.Contains(body, s) {
			t.Errorf("unexpected empty section %q in body:\n%s", s, body)
		}
	}
}
