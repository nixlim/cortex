package infra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/errs"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeDocker struct {
	pingErr    error
	composeErr error
	pings      int32
	composeUps int32
}

func (f *fakeDocker) Ping(ctx context.Context) error {
	atomic.AddInt32(&f.pings, 1)
	return f.pingErr
}

func (f *fakeDocker) ComposeUp(ctx context.Context, _ string) error {
	atomic.AddInt32(&f.composeUps, 1)
	return f.composeErr
}

func (f *fakeDocker) ComposeDown(context.Context, string, bool) error { return nil }
func (f *fakeDocker) ImageExists(context.Context, string) (bool, error) {
	return true, nil
}
func (f *fakeDocker) Build(context.Context, string, string, string) error { return nil }

// fakeWeaviate becomes ready on the Nth Ready call (readyAfter). A
// value of 1 means "ready on first call".
type fakeWeaviate struct {
	readyAfter int
	calls      int32
}

func (f *fakeWeaviate) Ready(ctx context.Context) error {
	n := int(atomic.AddInt32(&f.calls, 1))
	if n >= f.readyAfter {
		return nil
	}
	return errors.New("weaviate: not ready yet")
}

type fakeNeo4j struct {
	readyAfter int
	pings      int32
	gdsAvail   bool
	gdsErr     error
}

func (f *fakeNeo4j) Ping(ctx context.Context) error {
	n := int(atomic.AddInt32(&f.pings, 1))
	if n >= f.readyAfter {
		return nil
	}
	return errors.New("neo4j: not ready yet")
}

func (f *fakeNeo4j) GDSAvailable(context.Context) (bool, error) {
	return f.gdsAvail, f.gdsErr
}

type fakeOllama struct {
	pingAfter int
	pings     int32
	models    []string
	listErr   error
}

func (f *fakeOllama) Ping(ctx context.Context) error {
	n := int(atomic.AddInt32(&f.pings, 1))
	if n >= f.pingAfter {
		return nil
	}
	return errors.New("ollama: not reachable yet")
}

func (f *fakeOllama) ListModels(context.Context) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.models, nil
}

// newOpts builds a reasonable default UpOptions using a temp config
// path, tight probe interval, and a short startup budget so tests run
// fast.
func newOpts(t *testing.T) UpOptions {
	t.Helper()
	dir := t.TempDir()
	return UpOptions{
		ComposeFile:     "docker/docker-compose.yaml",
		ConfigPath:      filepath.Join(dir, "config.yaml"),
		StartupBudget:   2 * time.Second,
		ProbeInterval:   5 * time.Millisecond,
		EmbeddingModel:  "nomic-embed-text",
		GenerationModel: "qwen3:4b-instruct",
	}
}

// requireErrorCode asserts that err is an *errs.Error whose Code
// matches want and returns the typed error for further inspection.
func requireErrorCode(t *testing.T, err error, want string) *errs.Error {
	t.Helper()
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %T: %v", err, err)
	}
	if e.Code != want {
		t.Fatalf("error code = %q, want %q (message=%q)", e.Code, want, e.Message)
	}
	return e
}

// ---------------------------------------------------------------------------
// Acceptance: "Against a stopped stack, cortex up returns zero in under
// 90 seconds with both backends ready."
// ---------------------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	opts := newOpts(t)
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 2}
	opts.Neo4j = &fakeNeo4j{readyAfter: 2, gdsAvail: true}
	opts.Ollama = &fakeOllama{
		pingAfter: 1,
		models:    []string{"nomic-embed-text:latest", "qwen3:4b-instruct"},
	}

	if err := Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	// First-run password file must be owner-only (0600).
	info, err := os.Stat(opts.ConfigPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %v, want 0600", perm)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "Against a stack where Neo4j fails to become ready within
// 60 seconds, cortex up exits 1 with STARTUP_BUDGET_EXCEEDED or the
// relevant per-service error code."
// ---------------------------------------------------------------------------

func TestRunBudgetExceededOnNeo4j(t *testing.T) {
	opts := newOpts(t)
	opts.StartupBudget = 150 * time.Millisecond
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1_000_000, gdsAvail: true} // never ready
	opts.Ollama = &fakeOllama{pingAfter: 1, models: []string{"nomic-embed-text", "qwen3:4b-instruct"}}

	err := Run(context.Background(), opts)
	e := requireErrorCode(t, err, CodeStartupBudgetExceeded)
	_ = e
}

// ---------------------------------------------------------------------------
// Acceptance: "First-run cortex up generates a random Neo4j password
// and stores it in ~/.cortex/config.yaml with mode 0600."
// ---------------------------------------------------------------------------

func TestEnsureNeo4jPasswordFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	pw, generated, err := EnsureNeo4jPassword(path)
	if err != nil {
		t.Fatalf("EnsureNeo4jPassword: %v", err)
	}
	if !generated {
		t.Fatalf("expected generated=true on first run")
	}
	if pw == "" {
		t.Fatalf("expected non-empty password")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600", perm)
	}

	// Second run is idempotent: returns the same password without
	// re-generating.
	pw2, generated2, err := EnsureNeo4jPassword(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if generated2 {
		t.Errorf("expected generated=false on second run")
	}
	if pw2 != pw {
		t.Errorf("password drifted between runs: %q → %q", pw, pw2)
	}
}

func TestEnsureNeo4jPasswordPreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Seed with an unrelated key — round-tripping must keep it.
	if err := os.WriteFile(path, []byte("retrieval:\n  default_limit: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, generated, err := EnsureNeo4jPassword(path)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("expected password to be generated")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "default_limit") {
		t.Errorf("retrieval.default_limit was dropped during round-trip: %s", s)
	}
	if !strings.Contains(s, "neo4j_password") {
		t.Errorf("neo4j_password was not written: %s", s)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "When the embedding model is absent, cortex up exits 1
// with OLLAMA_MODEL_MISSING and prints the exact ollama pull command."
// ---------------------------------------------------------------------------

func TestRunOllamaEmbeddingModelMissing(t *testing.T) {
	opts := newOpts(t)
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: true}
	opts.Ollama = &fakeOllama{
		pingAfter: 1,
		models:    []string{"some-other-model:latest"},
	}

	err := Run(context.Background(), opts)
	e := requireErrorCode(t, err, CodeOllamaModelMissing)
	if !strings.Contains(e.Message, "ollama pull nomic-embed-text") {
		t.Errorf("error message missing pull command: %q", e.Message)
	}
}

// ---------------------------------------------------------------------------
// Defensive coverage
// ---------------------------------------------------------------------------

func TestRunDockerUnreachable(t *testing.T) {
	opts := newOpts(t)
	opts.Docker = &fakeDocker{pingErr: errors.New("cannot connect to daemon")}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: true}
	opts.Ollama = &fakeOllama{pingAfter: 1, models: []string{"nomic-embed-text", "qwen3:4b-instruct"}}

	err := Run(context.Background(), opts)
	requireErrorCode(t, err, CodeDockerUnreachable)
}

func TestRunComposeFailedShortCircuits(t *testing.T) {
	opts := newOpts(t)
	opts.Docker = &fakeDocker{composeErr: errors.New("compose: port in use")}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: true}
	opts.Ollama = &fakeOllama{pingAfter: 1, models: []string{"nomic-embed-text", "qwen3:4b-instruct"}}

	err := Run(context.Background(), opts)
	requireErrorCode(t, err, CodeComposeFailed)
}

func TestRunGDSUnavailable(t *testing.T) {
	opts := newOpts(t)
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: false}
	opts.Ollama = &fakeOllama{pingAfter: 1, models: []string{"nomic-embed-text", "qwen3:4b-instruct"}}

	err := Run(context.Background(), opts)
	requireErrorCode(t, err, CodeGDSNotAvailable)
}

func TestRunOllamaNotReachableOnProbe(t *testing.T) {
	opts := newOpts(t)
	opts.StartupBudget = 150 * time.Millisecond
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: true}
	opts.Ollama = &fakeOllama{pingAfter: 1_000_000, models: nil}

	err := Run(context.Background(), opts)
	// Either STARTUP_BUDGET_EXCEEDED (budget expired while spinning) or
	// OLLAMA_NOT_REACHABLE (budget not classified) is acceptable per
	// the spec's "STARTUP_BUDGET_EXCEEDED or the relevant per-service
	// error code" language.
	var e *errs.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *errs.Error, got %v", err)
	}
	if e.Code != CodeStartupBudgetExceeded && e.Code != CodeOllamaNotReachable {
		t.Errorf("unexpected code %q (message=%q)", e.Code, e.Message)
	}
}

func TestRunMisconfiguredEmbeddingModel(t *testing.T) {
	opts := newOpts(t)
	opts.EmbeddingModel = ""
	opts.Docker = &fakeDocker{}
	opts.Weaviate = &fakeWeaviate{readyAfter: 1}
	opts.Neo4j = &fakeNeo4j{readyAfter: 1, gdsAvail: true}
	opts.Ollama = &fakeOllama{pingAfter: 1, models: []string{"nomic-embed-text"}}

	err := Run(context.Background(), opts)
	requireErrorCode(t, err, CodeUpMisconfigured)
}

func TestContainsModelAcceptsTagVariants(t *testing.T) {
	cases := []struct {
		installed []string
		want      string
		ok        bool
	}{
		{[]string{"nomic-embed-text:latest"}, "nomic-embed-text", true},
		{[]string{"nomic-embed-text"}, "nomic-embed-text", true},
		{[]string{"qwen3:4b-instruct"}, "qwen3:4b-instruct", true},
		{[]string{"llama3.1:8b"}, "qwen3:4b-instruct", false},
		{[]string{}, "nomic-embed-text", false},
	}
	for _, tc := range cases {
		got := containsModel(tc.installed, tc.want)
		if got != tc.ok {
			t.Errorf("containsModel(%v, %q) = %v, want %v",
				tc.installed, tc.want, got, tc.ok)
		}
	}
}
