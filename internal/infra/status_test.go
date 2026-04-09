package infra

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes that satisfy the status probe interfaces.
// ---------------------------------------------------------------------------

type fakeStatusWeaviate struct {
	readyErr   error
	version    string
	versionErr error
}

func (f *fakeStatusWeaviate) Ready(ctx context.Context) error { return f.readyErr }
func (f *fakeStatusWeaviate) Version(ctx context.Context) (string, error) {
	return f.version, f.versionErr
}

type fakeStatusNeo4j struct {
	pingErr    error
	version    string
	versionErr error
}

func (f *fakeStatusNeo4j) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeStatusNeo4j) Version(ctx context.Context) (string, error) {
	return f.version, f.versionErr
}

type fakeStatusOllama struct {
	pingErr    error
	version    string
	versionErr error
}

func (f *fakeStatusOllama) Ping(ctx context.Context) error { return f.pingErr }
func (f *fakeStatusOllama) Version(ctx context.Context) (string, error) {
	return f.version, f.versionErr
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex status --json returns a JSON object with keys
// weaviate, neo4j, ollama each containing status and version."
// ---------------------------------------------------------------------------

func TestCheckAllHealthyJSONShape(t *testing.T) {
	opts := StatusOptions{
		Weaviate: &fakeStatusWeaviate{version: "1.36.9"},
		Neo4j:    &fakeStatusNeo4j{version: "5.24.0"},
		Ollama:   &fakeStatusOllama{version: "0.1.40"},
		Timeout:  200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)

	if r.Weaviate.Status != StatusHealthy || r.Weaviate.Version != "1.36.9" {
		t.Errorf("weaviate = %+v, want healthy 1.36.9", r.Weaviate)
	}
	if r.Neo4j.Status != StatusHealthy || r.Neo4j.Version != "5.24.0" {
		t.Errorf("neo4j = %+v, want healthy 5.24.0", r.Neo4j)
	}
	if r.Ollama.Status != StatusHealthy || r.Ollama.Version != "0.1.40" {
		t.Errorf("ollama = %+v, want healthy 0.1.40", r.Ollama)
	}

	// Ensure the JSON object literally carries the three top-level keys
	// the spec acceptance names.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"weaviate", "neo4j", "ollama"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, string(b))
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "A stopped Neo4j yields weaviate status healthy and neo4j
// status down."
// ---------------------------------------------------------------------------

func TestCheckPartialNeo4jDown(t *testing.T) {
	opts := StatusOptions{
		Weaviate: &fakeStatusWeaviate{version: "1.36.9"},
		Neo4j:    &fakeStatusNeo4j{pingErr: errors.New("neo4j: connection refused")},
		Ollama:   &fakeStatusOllama{version: "0.1.40"},
		Timeout:  200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)

	if r.Weaviate.Status != StatusHealthy {
		t.Errorf("weaviate = %+v, want healthy", r.Weaviate)
	}
	if r.Neo4j.Status != StatusDown {
		t.Errorf("neo4j = %+v, want down", r.Neo4j)
	}
	if r.Neo4j.Error == "" {
		t.Errorf("neo4j.Error should be populated when down")
	}
	if r.Ollama.Status != StatusHealthy {
		t.Errorf("ollama = %+v, want healthy", r.Ollama)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex status --json output is parseable and deterministic
// across successive calls against the same state."
// ---------------------------------------------------------------------------

func TestCheckDeterministicOutput(t *testing.T) {
	build := func() Report {
		opts := StatusOptions{
			Weaviate: &fakeStatusWeaviate{version: "1.36.9"},
			Neo4j:    &fakeStatusNeo4j{version: "5.24.0"},
			Ollama:   &fakeStatusOllama{version: "0.1.40"},
			Timeout:  200 * time.Millisecond,
		}
		return Check(context.Background(), opts)
	}
	a := build()
	b := build()
	// Strip the elapsed-ms field which is expected to drift across
	// calls; everything else must match exactly.
	a.ElapsedMS = 0
	b.ElapsedMS = 0
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("non-deterministic output:\n%s\n%s", string(ja), string(jb))
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex status does not perform deep doctor checks
// (verified by elapsed time < 2 seconds)."
// ---------------------------------------------------------------------------

func TestCheckElapsedUnderTwoSeconds(t *testing.T) {
	// Even with every probe timing out, Check must return well under
	// 2 seconds because its internal Timeout caps the wall clock.
	opts := StatusOptions{
		Weaviate: &fakeStatusWeaviate{readyErr: errors.New("timeout")},
		Neo4j:    &fakeStatusNeo4j{pingErr: errors.New("timeout")},
		Ollama:   &fakeStatusOllama{pingErr: errors.New("timeout")},
		Timeout:  100 * time.Millisecond,
	}
	start := time.Now()
	_ = Check(context.Background(), opts)
	elapsed := time.Since(start)
	if elapsed >= 2*time.Second {
		t.Errorf("Check elapsed = %v, want < 2s", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Defensive coverage
// ---------------------------------------------------------------------------

func TestCheckDegradedWhenVersionFails(t *testing.T) {
	opts := StatusOptions{
		Weaviate: &fakeStatusWeaviate{version: "", versionErr: errors.New("meta: 503")},
		Neo4j:    &fakeStatusNeo4j{version: "5.24.0"},
		Ollama:   &fakeStatusOllama{version: "0.1.40"},
		Timeout:  200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)
	if r.Weaviate.Status != StatusDegraded {
		t.Errorf("weaviate = %+v, want degraded", r.Weaviate)
	}
}

func TestCheckDiskUsageWalksCortexHome(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.log"), []byte("abcde"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.log"), []byte("xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := StatusOptions{
		Weaviate:   &fakeStatusWeaviate{version: "1"},
		Neo4j:      &fakeStatusNeo4j{version: "1"},
		Ollama:     &fakeStatusOllama{version: "1"},
		CortexHome: dir,
		Timeout:    200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)
	if r.DiskUsageBytes != 8 {
		t.Errorf("DiskUsageBytes = %d, want 8", r.DiskUsageBytes)
	}
}

func TestCheckOptionalLogFields(t *testing.T) {
	watermark := uint64(12345)
	count := int64(67)
	opts := StatusOptions{
		Weaviate:     &fakeStatusWeaviate{version: "1"},
		Neo4j:        &fakeStatusNeo4j{version: "1"},
		Ollama:       &fakeStatusOllama{version: "1"},
		LogWatermark: func() (uint64, error) { return watermark, nil },
		EntryCount:   func() (int64, error) { return count, nil },
		Timeout:      200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)
	if r.LogWatermark == nil || *r.LogWatermark != watermark {
		t.Errorf("LogWatermark = %v, want %d", r.LogWatermark, watermark)
	}
	if r.EntryCount == nil || *r.EntryCount != count {
		t.Errorf("EntryCount = %v, want %d", r.EntryCount, count)
	}
}

func TestCheckOptionalFieldsOmittedWhenAbsent(t *testing.T) {
	opts := StatusOptions{
		Weaviate: &fakeStatusWeaviate{version: "1"},
		Neo4j:    &fakeStatusNeo4j{version: "1"},
		Ollama:   &fakeStatusOllama{version: "1"},
		Timeout:  200 * time.Millisecond,
	}
	r := Check(context.Background(), opts)
	b, _ := json.Marshal(r)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	if _, ok := raw["log_watermark"]; ok {
		t.Errorf("log_watermark should be omitted when absent: %s", string(b))
	}
	if _, ok := raw["entry_count"]; ok {
		t.Errorf("entry_count should be omitted when absent: %s", string(b))
	}
}

func TestShallowErrStringTrimsNewlinesAndLength(t *testing.T) {
	got := shallowErrString(errors.New("line1\nline2\nline3"))
	if got != "line1" {
		t.Errorf("shallowErrString(newlines) = %q, want line1", got)
	}
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	got = shallowErrString(errors.New(string(long)))
	if len(got) > 170 {
		t.Errorf("shallowErrString long = %d chars, want truncated", len(got))
	}
}
