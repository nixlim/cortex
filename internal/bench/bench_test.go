package bench

// Tests for cortex-4kq.55. The bench harness is a pure library: the
// CLI command wraps it, but all scoring logic and JSON shape live
// here. These tests exercise the acceptance criteria from the bead
// without touching the real write/recall/reflect pipelines — fake
// operations with programmable latencies stand in.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOp is an Operation that sleeps for a fixed duration on every
// call, optionally returning an error on a scheduled call index. It
// is the workhorse for latency-envelope tests — the "latency" is the
// sleep, so the harness's time.Since measurement will see it.
type fakeOp struct {
	name     string
	latency  time.Duration
	failEvery int
	calls    atomic.Int64
}

func (f *fakeOp) Name() string { return f.name }

func (f *fakeOp) Run(ctx context.Context) error {
	n := f.calls.Add(1)
	if f.failEvery > 0 && int(n)%f.failEvery == 0 {
		return errors.New("synthetic op failure")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.latency):
		return nil
	}
}

// shortSequence keeps iteration counts small so the test suite stays
// fast; the default (200/200/20/5) would take minutes under 1ms ops.
func shortSequence() ScriptedSequence {
	return ScriptedSequence{
		OpRecall:        5,
		OpObserve:       5,
		OpReflectDryRun: 2,
		OpAnalyzeDryRun: 1,
	}
}

// TestRunner_P1DevSmallPassingProfile is the primary acceptance test:
// a P1-dev small-corpus run where every op comes in well under its
// envelope must exit 0 and write a latest.json file.
func TestRunner_P1DevSmallPassingProfile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "latest.json")

	ops := []Operation{
		&fakeOp{name: OpRecall, latency: 1 * time.Millisecond},
		&fakeOp{name: OpObserve, latency: 1 * time.Millisecond},
		&fakeOp{name: OpReflectDryRun, latency: 1 * time.Millisecond},
		&fakeOp{name: OpAnalyzeDryRun, latency: 1 * time.Millisecond},
	}
	cfg := Config{
		Profile:    P1DevProfile(CorpusSmall),
		Operations: ops,
		Sequence:   shortSequence(),
		OutputPath: out,
	}

	runner := NewRunner()
	report, err := runner.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !report.Passed {
		t.Errorf("report.Passed = false, want true; failing: %v", report.FailingOperations)
	}
	if code := report.ExitCode(); code != 0 {
		t.Errorf("ExitCode = %d, want 0", code)
	}
	// Every op should have a result, with p95 under its envelope.
	for _, name := range []string{OpRecall, OpObserve, OpReflectDryRun, OpAnalyzeDryRun} {
		res, ok := report.Operations[name]
		if !ok {
			t.Errorf("missing op result %q", name)
			continue
		}
		if res.Count == 0 {
			t.Errorf("%s Count = 0", name)
		}
		if res.Envelope == 0 {
			t.Errorf("%s Envelope = 0", name)
		}
		if res.P95 > res.Envelope {
			t.Errorf("%s P95 %v > envelope %v", name, res.P95, res.Envelope)
		}
		if !res.Passed {
			t.Errorf("%s Passed = false", name)
		}
	}

	// latest.json must be written and parse back into the same shape.
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read latest.json: %v", err)
	}
	var round Report
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal latest.json: %v", err)
	}
	if round.Profile != ProfileP1Dev {
		t.Errorf("round-tripped profile = %q, want %q", round.Profile, ProfileP1Dev)
	}
	if !round.Passed {
		t.Errorf("round-tripped Passed = false")
	}
}

// TestRunner_FailingEnvelopeExitsNonZero covers acceptance criterion
// 4: a failing envelope exits 1 and names the failing operation in
// the JSON output.
func TestRunner_FailingEnvelopeExitsNonZero(t *testing.T) {
	// Tiny envelope, much smaller than the op's 20ms latency.
	tinyProfile := Profile{
		Name:   ProfileP1Dev,
		Corpus: CorpusSmall,
		Envelopes: map[string]Envelope{
			OpRecall: {P95Budget: 1 * time.Millisecond},
		},
	}
	cfg := Config{
		Profile:    tinyProfile,
		Operations: []Operation{&fakeOp{name: OpRecall, latency: 20 * time.Millisecond}},
		Sequence:   ScriptedSequence{OpRecall: 3},
	}
	report, err := NewRunner().Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Passed {
		t.Errorf("Passed = true, want false")
	}
	if report.ExitCode() != 1 {
		t.Errorf("ExitCode = %d, want 1", report.ExitCode())
	}
	if len(report.FailingOperations) != 1 || report.FailingOperations[0] != OpRecall {
		t.Errorf("FailingOperations = %v, want [%s]", report.FailingOperations, OpRecall)
	}
	if report.Operations[OpRecall].Passed {
		t.Errorf("%s Passed = true, want false", OpRecall)
	}
}

// TestP1CIAppliesMultiplier — the CI profile doubles every envelope.
func TestP1CIAppliesMultiplier(t *testing.T) {
	dev := P1DevProfile(CorpusSmall)
	ci := P1CIProfile(CorpusSmall)
	for name, devEnv := range dev.Envelopes {
		ciEnv, ok := ci.Envelopes[name]
		if !ok {
			t.Errorf("P1-ci missing op %q", name)
			continue
		}
		if ciEnv.P95Budget != devEnv.P95Budget*2 {
			t.Errorf("%s: ci budget %v != dev %v * 2", name, ciEnv.P95Budget, devEnv.P95Budget)
		}
	}
	if ci.Name != ProfileP1CI {
		t.Errorf("P1CIProfile.Name = %q, want %q", ci.Name, ProfileP1CI)
	}
	if ci.Corpus != CorpusSmall {
		t.Errorf("P1CIProfile.Corpus = %q, want %q", ci.Corpus, CorpusSmall)
	}
}

// TestP1DevProfile_CorpusSizes — the spec fixes the envelope table at
// 2s/3s read/write for small and 3.5s/5s for medium. A drift in those
// numbers should fail this test loudly so we notice on the next spec
// re-read.
func TestP1DevProfile_CorpusSizes(t *testing.T) {
	small := P1DevProfile(CorpusSmall)
	if small.Envelopes[OpRecall].P95Budget != 2*time.Second {
		t.Errorf("small recall budget = %v, want 2s", small.Envelopes[OpRecall].P95Budget)
	}
	if small.Envelopes[OpObserve].P95Budget != 3*time.Second {
		t.Errorf("small observe budget = %v, want 3s", small.Envelopes[OpObserve].P95Budget)
	}
	medium := P1DevProfile(CorpusMedium)
	if medium.Envelopes[OpRecall].P95Budget != 3500*time.Millisecond {
		t.Errorf("medium recall budget = %v, want 3.5s", medium.Envelopes[OpRecall].P95Budget)
	}
	if medium.Envelopes[OpObserve].P95Budget != 5*time.Second {
		t.Errorf("medium observe budget = %v, want 5s", medium.Envelopes[OpObserve].P95Budget)
	}
}

// TestRunner_ErrorsExcludedFromPercentiles — an op that returns
// errors should have those calls counted in Errors, not in the
// latency samples. This matters because an errored call is typically
// faster than a successful one (early return) and would skew p95
// downward if it polluted the sample set.
func TestRunner_ErrorsExcludedFromPercentiles(t *testing.T) {
	op := &fakeOp{name: OpRecall, latency: 5 * time.Millisecond, failEvery: 2}
	cfg := Config{
		Profile:    P1DevProfile(CorpusSmall),
		Operations: []Operation{op},
		Sequence:   ScriptedSequence{OpRecall: 4},
	}
	report, err := NewRunner().Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := report.Operations[OpRecall]
	if res.Errors != 2 {
		t.Errorf("Errors = %d, want 2", res.Errors)
	}
	if res.Count != 2 {
		t.Errorf("Count (successful samples) = %d, want 2", res.Count)
	}
}

// TestRunner_ContextCancellationShortCircuits — if the parent context
// is cancelled (the shutdown handle case), the runner should stop
// iterating the current op and still produce a populated report.
func TestRunner_ContextCancellationShortCircuits(t *testing.T) {
	op := &fakeOp{name: OpRecall, latency: 10 * time.Millisecond}
	cfg := Config{
		Profile:    P1DevProfile(CorpusSmall),
		Operations: []Operation{op},
		Sequence:   ScriptedSequence{OpRecall: 1000}, // would take 10 seconds
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	report, err := NewRunner().Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Should have stopped well short of 1000 iterations.
	if n := op.calls.Load(); n >= 1000 {
		t.Errorf("op called %d times, expected short-circuit", n)
	}
	if report == nil {
		t.Fatal("report is nil after cancellation")
	}
	if _, ok := report.Operations[OpRecall]; !ok {
		t.Errorf("report missing recall op after short-circuit")
	}
}

// TestRunner_OpNotInSequence — an operation registered in
// cfg.Operations but with no count in the sequence produces a zero-
// count result entry (so the report shape stays stable across runs).
func TestRunner_OpNotInSequence(t *testing.T) {
	op := &fakeOp{name: "extra", latency: 0}
	cfg := Config{
		Profile:    Profile{Name: ProfileP1Dev, Corpus: CorpusSmall, Envelopes: map[string]Envelope{}},
		Operations: []Operation{op},
		Sequence:   ScriptedSequence{}, // empty — "extra" has no count
	}
	report, err := NewRunner().Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	res, ok := report.Operations["extra"]
	if !ok {
		t.Fatal("missing zero-count result entry")
	}
	if res.Count != 0 {
		t.Errorf("Count = %d, want 0", res.Count)
	}
	// A not-in-sequence op contributes no samples, so whatever its
	// Passed flag reads is moot — but it must not leak into the
	// run-level FailingOperations list (because the op was simply
	// never run, not run-and-failed).
	for _, f := range report.FailingOperations {
		if f == "extra" {
			t.Errorf("not-in-sequence op leaked into FailingOperations")
		}
	}
}

// TestRunner_ZeroEnvelopeIsReportingOnly — explicitly test that an
// operation with no envelope budget is treated as reporting-only.
func TestRunner_ZeroEnvelopeIsReportingOnly(t *testing.T) {
	prof := Profile{
		Name:   ProfileP1Dev,
		Corpus: CorpusSmall,
		Envelopes: map[string]Envelope{
			OpRecall: {P95Budget: 0}, // no budget
		},
	}
	cfg := Config{
		Profile:    prof,
		Operations: []Operation{&fakeOp{name: OpRecall, latency: 50 * time.Millisecond}},
		Sequence:   ScriptedSequence{OpRecall: 3},
	}
	report, err := NewRunner().Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Errorf("zero-envelope should pass; failing = %v", report.FailingOperations)
	}
}

// TestPercentile_NearestRank — smoke-tests the percentile helper with
// known samples. Inputs 10,20,30,40,50 under the nearest-rank method:
//   p50 → idx = ceil(0.5*5)-1 = 2 → 30
//   p95 → idx = ceil(0.95*5)-1 = 4 → 50
func TestPercentile_NearestRank(t *testing.T) {
	in := []time.Duration{30, 10, 50, 20, 40} // deliberately unsorted
	if got := percentile(in, 0.50); got != 30 {
		t.Errorf("p50 = %v, want 30", got)
	}
	if got := percentile(in, 0.95); got != 50 {
		t.Errorf("p95 = %v, want 50", got)
	}
	if got := percentile(in, 0.99); got != 50 {
		t.Errorf("p99 = %v, want 50", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("p95(nil) = %v, want 0", got)
	}
}

// TestReport_JSONShape — the report must serialize to the exact keys
// the spec mandates for ~/.cortex/bench/latest.json.
func TestReport_JSONShape(t *testing.T) {
	r := &Report{
		Profile: ProfileP1Dev,
		Corpus:  CorpusSmall,
		Operations: map[string]*OperationResult{
			OpRecall: {Name: OpRecall, Count: 1, P50: 1, P95: 2, P99: 3, Envelope: 10, Passed: true},
		},
		Passed: true,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"profile", "corpus", "operations", "failing_operations", "passed"} {
		if _, ok := m[key]; !ok {
			t.Errorf("report JSON missing key %q", key)
		}
	}
	ops := m["operations"].(map[string]interface{})
	rec := ops[OpRecall].(map[string]interface{})
	for _, key := range []string{"name", "count", "p50_ns", "p95_ns", "p99_ns", "envelope_p95_ns", "passed"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("operation JSON missing key %q", key)
		}
	}
}

// TestOperationFunc_Adapter — the closure adapter forwards Name/Run
// correctly.
func TestOperationFunc_Adapter(t *testing.T) {
	called := false
	op := OperationFunc{
		OpName: "x",
		Fn:     func(ctx context.Context) error { called = true; return nil },
	}
	if op.Name() != "x" {
		t.Errorf("Name = %q, want x", op.Name())
	}
	if err := op.Run(context.Background()); err != nil {
		t.Errorf("Run err = %v", err)
	}
	if !called {
		t.Errorf("Fn not called")
	}
}
