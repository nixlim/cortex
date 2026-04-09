package walker

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// captureLogger records every Warn call so acceptance tests can assert that
// a specific event fired with a specific path. It is the test-side stand-in
// for the real internal/opslog writer.
type captureLogger struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	Event  string
	Fields map[string]any
}

func (c *captureLogger) Warn(event string, fields map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := make(map[string]any, len(fields))
	for k, v := range fields {
		copied[k] = v
	}
	c.events = append(c.events, capturedEvent{Event: event, Fields: copied})
}

func (c *captureLogger) has(event string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Event == event {
			return true
		}
	}
	return false
}

func (c *captureLogger) hasWithFieldContains(event, field, substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Event != event {
			continue
		}
		v, ok := e.Fields[field]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok && strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// collect runs Walk and returns every emitted RelPath in sorted order along
// with the capture logger it installed.
func collect(t *testing.T, opts Options) ([]string, *captureLogger) {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = &captureLogger{}
	}
	var out []string
	if err := Walk(opts, func(fm FileMeta) error {
		out = append(out, fm.RelPath)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	sort.Strings(out)
	return out, opts.Logger.(*captureLogger)
}

// writeFile is a test helper that ensures parent dirs exist and writes data.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A symlink pointing to a path outside the project root is
// skipped with a warning written to ops.log."
// ---------------------------------------------------------------------------

func TestSymlinkEscapeIsSkippedAndLogged(t *testing.T) {
	tmp := t.TempDir()
	project := filepath.Join(tmp, "proj")
	external := filepath.Join(tmp, "external")
	writeFile(t, filepath.Join(project, "keep.txt"), []byte("hi"))
	writeFile(t, filepath.Join(external, "secret.txt"), []byte("leak"))
	if err := os.Symlink(filepath.Join(external, "secret.txt"), filepath.Join(project, "escape.txt")); err != nil {
		t.Skipf("symlink not supported on this fs: %v", err)
	}

	logger := &captureLogger{}
	files, _ := collect(t, Options{ProjectRoot: project, Logger: logger})

	for _, f := range files {
		if f == "escape.txt" {
			t.Errorf("escape symlink appeared in walker output: %q", f)
		}
	}
	if !contains(files, "keep.txt") {
		t.Errorf("expected keep.txt in output, got %v", files)
	}
	if !logger.has("WALKER_SYMLINK_ESCAPE") {
		t.Errorf("expected WALKER_SYMLINK_ESCAPE warning; got events %+v", logger.events)
	}
}

func TestSymlinkWithinRootIsIncluded(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "real.txt"), []byte("ok"))
	if err := os.Symlink(filepath.Join(tmp, "real.txt"), filepath.Join(tmp, "alias.txt")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	files, logger := collect(t, Options{ProjectRoot: tmp})
	if !contains(files, "alias.txt") {
		t.Errorf("in-root symlink should be followed; files=%v", files)
	}
	if logger.has("WALKER_SYMLINK_ESCAPE") {
		t.Errorf("no escape warning expected; events=%+v", logger.events)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A file larger than 262144 bytes is skipped and does not
// appear in the iterator."
// ---------------------------------------------------------------------------

func TestOversizedFileIsSkipped(t *testing.T) {
	tmp := t.TempDir()
	big := make([]byte, DefaultModuleSizeLimitBytes+1)
	small := []byte("ok")
	writeFile(t, filepath.Join(tmp, "big.bin"), big)
	writeFile(t, filepath.Join(tmp, "small.txt"), small)

	files, logger := collect(t, Options{ProjectRoot: tmp})
	if contains(files, "big.bin") {
		t.Errorf("oversized file leaked into output: %v", files)
	}
	if !contains(files, "small.txt") {
		t.Errorf("small file missing from output: %v", files)
	}
	if !logger.has("WALKER_FILE_TOO_LARGE") {
		t.Errorf("expected WALKER_FILE_TOO_LARGE warning; got %+v", logger.events)
	}
}

func TestCustomSizeLimitOverridesDefault(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.txt"), make([]byte, 100))
	writeFile(t, filepath.Join(tmp, "b.txt"), make([]byte, 50))

	files, _ := collect(t, Options{ProjectRoot: tmp, ModuleSizeLimitBytes: 75})
	if contains(files, "a.txt") {
		t.Errorf("100-byte file should exceed 75-byte limit")
	}
	if !contains(files, "b.txt") {
		t.Errorf("50-byte file should be kept under 75-byte limit")
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "Files matching .gitignore patterns are skipped."
// ---------------------------------------------------------------------------

func TestGitignorePatternsAreSkipped(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".gitignore"), []byte("*.log\nbuild/\nsecret.txt\n"))
	writeFile(t, filepath.Join(tmp, "keep.go"), []byte("package x"))
	writeFile(t, filepath.Join(tmp, "drop.log"), []byte("noisy"))
	writeFile(t, filepath.Join(tmp, "build", "artifact.o"), []byte("obj"))
	writeFile(t, filepath.Join(tmp, "secret.txt"), []byte("secret"))

	files, _ := collect(t, Options{ProjectRoot: tmp})

	if contains(files, "drop.log") {
		t.Errorf("*.log should be ignored; files=%v", files)
	}
	if contains(files, "build/artifact.o") {
		t.Errorf("build/ should be pruned; files=%v", files)
	}
	if contains(files, "secret.txt") {
		t.Errorf("secret.txt literal should be ignored; files=%v", files)
	}
	if !contains(files, "keep.go") {
		t.Errorf("keep.go should be present; files=%v", files)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "~/.cortex/ and deny-list paths never appear in the iterator."
// ---------------------------------------------------------------------------

func TestCortexHomeIsExcluded(t *testing.T) {
	tmp := t.TempDir()
	cortexHome := filepath.Join(tmp, ".cortex")
	writeFile(t, filepath.Join(cortexHome, "config.yaml"), []byte("x"))
	writeFile(t, filepath.Join(tmp, "app.go"), []byte("package x"))

	files, logger := collect(t, Options{
		ProjectRoot: tmp,
		CortexHome:  cortexHome,
	})
	for _, f := range files {
		if strings.HasPrefix(f, ".cortex/") {
			t.Errorf("walker yielded cortex-internal path: %q", f)
		}
	}
	if !contains(files, "app.go") {
		t.Errorf("app.go should be present; files=%v", files)
	}
	if !logger.has("WALKER_CORTEX_INTERNAL") {
		t.Errorf("expected WALKER_CORTEX_INTERNAL warning; got %+v", logger.events)
	}
}

func TestDenyListExcludesMatchedPaths(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.txt"), []byte("a"))
	writeFile(t, filepath.Join(tmp, "secret.env"), []byte("b"))
	writeFile(t, filepath.Join(tmp, "vendor", "lib.go"), []byte("package vendor"))

	files, logger := collect(t, Options{
		ProjectRoot: tmp,
		DenyList:    []string{"*.env", "vendor"},
	})
	if contains(files, "secret.env") {
		t.Errorf("secret.env should be deny-listed; files=%v", files)
	}
	if contains(files, "vendor/lib.go") {
		t.Errorf("vendor/ should be deny-listed; files=%v", files)
	}
	if !contains(files, "a.txt") {
		t.Errorf("a.txt should remain; files=%v", files)
	}
	if !logger.has("WALKER_DENY_LISTED") {
		t.Errorf("expected WALKER_DENY_LISTED warning; got %+v", logger.events)
	}
}

// ---------------------------------------------------------------------------
// Defensive / API tests.
// ---------------------------------------------------------------------------

func TestMissingProjectRootIsError(t *testing.T) {
	err := Walk(Options{}, func(FileMeta) error { return nil })
	if err == nil {
		t.Fatal("expected error for empty ProjectRoot")
	}
}

func TestCallbackErrorPropagates(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.txt"), []byte("a"))
	sentinel := errors.New("stop")
	err := Walk(Options{ProjectRoot: tmp}, func(FileMeta) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

func TestNopLoggerIsDefault(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.txt"), []byte("a"))
	// Should not panic with nil Logger; walker must default to NopLogger.
	_ = Walk(Options{ProjectRoot: tmp}, func(FileMeta) error { return nil })
}

func TestHiddenVCSDirsArePruned(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".git", "HEAD"), []byte("ref: refs/heads/main"))
	writeFile(t, filepath.Join(tmp, "main.go"), []byte("package main"))

	files, _ := collect(t, Options{ProjectRoot: tmp})
	for _, f := range files {
		if strings.HasPrefix(f, ".git/") {
			t.Errorf(".git directory should be pruned; got %q", f)
		}
	}
	if !contains(files, "main.go") {
		t.Errorf("main.go should be present; files=%v", files)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
