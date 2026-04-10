// Package bench implements the cortex bench harness described in
// docs/spec/cortex-spec.md §"cortex bench".
//
// The harness is structured around four things:
//
//  1. A Profile (P1-dev or P1-ci) that declares envelope budgets for
//     each operation. P1-ci applies a 2x multiplier to the authoritative
//     P1-dev envelopes — regression detection only, not envelope
//     validation.
//  2. An Operation interface the harness calls to execute one unit of
//     work. Four named operations are expected (recall, observe,
//     reflect_dry_run, analyze_dry_run) but any set can be registered.
//     The real commands inject closures wrapping their pipelines;
//     tests inject deterministic fakes with programmable latencies.
//  3. A Runner that executes the scripted sequence, collects per-op
//     latency samples, computes p50/p95/p99, and compares each against
//     the profile's envelopes to produce an overall pass/fail.
//  4. A Report with the JSON-serialisable shape the spec mandates for
//     ~/.cortex/bench/latest.json. An exit code of 0 means every op
//     passed; 1 means at least one failed, and the failing op is named
//     in Report.FailingOperations.
//
// Wiring note: this package does not contain a cobra command. The
// cmd/cortex/bench.go subcommand (ops-dev) will construct a Runner
// with closures over the real write/recall/reflect pipelines and call
// Run. Keeping the harness a pure library means all the scoring logic
// can be unit-tested without a full stack.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ProfileName identifies an envelope profile.
type ProfileName string

const (
	ProfileP1Dev ProfileName = "P1-dev"
	ProfileP1CI  ProfileName = "P1-ci"
)

// CorpusName identifies a fixture corpus size.
type CorpusName string

const (
	CorpusSmall  CorpusName = "small"
	CorpusMedium CorpusName = "medium"
)

// Operation names the harness recognises. Tests and callers use these
// as dictionary keys rather than bare strings so a typo fails at
// compile time.
const (
	OpRecall           = "recall"
	OpObserve          = "observe"
	OpReflectDryRun    = "reflect_dry_run"
	OpAnalyzeDryRun    = "analyze_dry_run"
)

// Operation is one benchmarkable unit of work. The harness calls Run
// repeatedly (Count times per the ScriptedSequence) and records each
// call's wall-clock latency.
type Operation interface {
	// Name returns the stable operation key used in the report's
	// Operations map and in envelope lookups.
	Name() string

	// Run executes one unit of work. The context is derived from the
	// Runner's parent context; callers should honour it for graceful
	// shutdown. Returning a non-nil error counts the call as an error
	// sample (excluded from p50/p95/p99) but does NOT abort the run —
	// a flaky backend should not short-circuit the whole benchmark,
	// only mark the op as degraded in the report.
	Run(ctx context.Context) error
}

// OperationFunc is a convenience adapter for Operation when the
// implementation is a bare closure (the common case for tests and for
// the CLI command that wraps a pipeline method).
type OperationFunc struct {
	OpName string
	Fn     func(ctx context.Context) error
}

// Name returns OperationFunc.OpName so the harness can group samples.
func (o OperationFunc) Name() string { return o.OpName }

// Run calls OperationFunc.Fn.
func (o OperationFunc) Run(ctx context.Context) error { return o.Fn(ctx) }

// Envelope is the allowed p95 budget for one operation under one
// profile. A zero Budget means "no envelope" — the operation is
// benchmarked for reporting purposes only and cannot fail the run.
type Envelope struct {
	P95Budget time.Duration
}

// Profile bundles a profile name, the corpus size that profile
// targets, and the per-operation envelopes.
type Profile struct {
	Name      ProfileName
	Corpus    CorpusName
	Envelopes map[string]Envelope
}

// ScriptedSequence defines how many iterations each named operation
// should run. The spec fixes these at 200 recall / 200 observe / 20
// reflect / 5 analyze; tests override with smaller numbers.
type ScriptedSequence map[string]int

// DefaultScriptedSequence is the spec-mandated count table used by
// the production CLI. Tests can override via Config.Sequence.
func DefaultScriptedSequence() ScriptedSequence {
	return ScriptedSequence{
		OpRecall:        200,
		OpObserve:       200,
		OpReflectDryRun: 20,
		OpAnalyzeDryRun: 5,
	}
}

// P1DevProfile returns the authoritative envelope table for the
// P1-dev hardware profile at the given corpus size. The numbers are
// transcribed from docs/spec/cortex-spec.md §"Corpus Sizes".
func P1DevProfile(corpus CorpusName) Profile {
	var readP95, writeP95 time.Duration
	switch corpus {
	case CorpusSmall:
		readP95 = 2 * time.Second
		writeP95 = 3 * time.Second
	case CorpusMedium:
		readP95 = 3500 * time.Millisecond
		writeP95 = 5 * time.Second
	default:
		readP95 = 2 * time.Second
		writeP95 = 3 * time.Second
	}
	return Profile{
		Name:   ProfileP1Dev,
		Corpus: corpus,
		Envelopes: map[string]Envelope{
			OpRecall:        {P95Budget: readP95},
			OpObserve:       {P95Budget: writeP95},
			OpReflectDryRun: {P95Budget: writeP95 * 2}, // reflection is batch work
			OpAnalyzeDryRun: {P95Budget: writeP95 * 3}, // analyze is cross-project
		},
	}
}

// P1CIProfile returns the CI profile, which is the same envelope
// table as P1-dev but with a 2x multiplier applied per the spec's
// "P1-ci | Regression detection only ... envelopes get a 2x
// multiplier" note.
func P1CIProfile(corpus CorpusName) Profile {
	dev := P1DevProfile(corpus)
	env := make(map[string]Envelope, len(dev.Envelopes))
	for k, v := range dev.Envelopes {
		env[k] = Envelope{P95Budget: v.P95Budget * 2}
	}
	return Profile{
		Name:      ProfileP1CI,
		Corpus:    dev.Corpus,
		Envelopes: env,
	}
}

// Config is the per-run input to Runner.Run.
type Config struct {
	Profile    Profile
	Operations []Operation
	Sequence   ScriptedSequence // nil ⇒ DefaultScriptedSequence()

	// OutputPath is the file path where the JSON report is written.
	// Empty string disables file output (used by tests that assert
	// on the in-memory Report). The CLI command passes
	// ~/.cortex/bench/latest.json here.
	OutputPath string
}

// OperationResult is the per-operation summary written into the
// report. Latency fields are nanosecond durations so the JSON shape is
// numeric and easy to diff across runs.
type OperationResult struct {
	Name       string        `json:"name"`
	Count      int           `json:"count"`
	Errors     int           `json:"errors"`
	P50        time.Duration `json:"p50_ns"`
	P95        time.Duration `json:"p95_ns"`
	P99        time.Duration `json:"p99_ns"`
	Throughput float64       `json:"throughput_per_sec"`
	Envelope   time.Duration `json:"envelope_p95_ns"`
	Passed     bool          `json:"passed"`
}

// Report is the JSON-serialisable bench output.
type Report struct {
	Profile           ProfileName                 `json:"profile"`
	Corpus            CorpusName                  `json:"corpus"`
	StartedAt         time.Time                   `json:"started_at"`
	CompletedAt       time.Time                   `json:"completed_at"`
	Operations        map[string]*OperationResult `json:"operations"`
	FailingOperations []string                    `json:"failing_operations"`
	Passed            bool                        `json:"passed"`
}

// ExitCode returns the process-level exit code for this report: 0 on
// overall pass, 1 on fail. The spec's acceptance criteria use this
// code to gate CI runs.
func (r *Report) ExitCode() int {
	if r.Passed {
		return 0
	}
	return 1
}

// Runner executes a bench configuration and produces a Report.
type Runner struct {
	// Now is the clock used for StartedAt/CompletedAt. Tests pin it.
	Now func() time.Time
}

// NewRunner returns a default runner with a real clock.
func NewRunner() *Runner {
	return &Runner{Now: func() time.Time { return time.Now().UTC() }}
}

// Run executes the scripted sequence for every operation in cfg and
// returns the populated Report. Errors returned from an Operation.Run
// increment the per-op Errors counter but do not abort the run; a
// context cancellation (e.g., SIGINT via the shutdown handle)
// short-circuits the remaining iterations and the report is still
// computed from whatever samples were collected.
func (r *Runner) Run(ctx context.Context, cfg Config) (*Report, error) {
	seq := cfg.Sequence
	if seq == nil {
		seq = DefaultScriptedSequence()
	}
	now := r.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	report := &Report{
		Profile:    cfg.Profile.Name,
		Corpus:     cfg.Profile.Corpus,
		StartedAt:  now(),
		Operations: make(map[string]*OperationResult, len(cfg.Operations)),
	}

	for _, op := range cfg.Operations {
		name := op.Name()
		count := seq[name]
		if count <= 0 {
			// Operations not in the scripted sequence still get an
			// entry with count 0 so the report surface is stable.
			report.Operations[name] = &OperationResult{Name: name}
			continue
		}
		samples := make([]time.Duration, 0, count)
		errCount := 0
		wallStart := time.Now()
		for i := 0; i < count; i++ {
			if err := ctx.Err(); err != nil {
				// Shutdown requested — stop iterating this op and move on
				// to the next (which will also short-circuit, but we let
				// the loop handle it rather than returning early so the
				// report's Operations map is populated for every op).
				break
			}
			start := time.Now()
			err := op.Run(ctx)
			elapsed := time.Since(start)
			if err != nil {
				errCount++
				continue
			}
			samples = append(samples, elapsed)
		}
		wallElapsed := time.Since(wallStart)

		env := cfg.Profile.Envelopes[name].P95Budget
		res := &OperationResult{
			Name:     name,
			Count:    len(samples),
			Errors:   errCount,
			Envelope: env,
		}
		if len(samples) > 0 {
			res.P50 = percentile(samples, 0.50)
			res.P95 = percentile(samples, 0.95)
			res.P99 = percentile(samples, 0.99)
			if wallElapsed > 0 {
				res.Throughput = float64(len(samples)) / wallElapsed.Seconds()
			}
		}
		res.Passed = env == 0 || (len(samples) > 0 && res.P95 <= env)
		if !res.Passed {
			report.FailingOperations = append(report.FailingOperations, name)
		}
		report.Operations[name] = res
	}

	report.CompletedAt = now()
	report.Passed = len(report.FailingOperations) == 0

	if cfg.OutputPath != "" {
		if err := writeJSON(cfg.OutputPath, report); err != nil {
			// A JSON-write failure does not alter the pass/fail
			// verdict — the report is the source of truth — but it
			// does surface as a runner-level error so the CLI can
			// warn. Operators rely on latest.json for CI diffing,
			// so silently dropping it would be worse than noisy.
			return report, fmt.Errorf("bench: write %s: %w", cfg.OutputPath, err)
		}
	}
	return report, nil
}

// percentile returns the p-th percentile (0..1) of samples. Samples
// are sorted in place; callers that care about the original order
// should pass a copy. An empty slice returns zero — the caller has
// already guarded against that above.
func percentile(samples []time.Duration, p float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// Nearest-rank method: index = ceil(p * N) - 1, clamped.
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// writeJSON marshals the report and writes it to path, creating the
// parent directory (with 0700 mode per spec) if necessary.
func writeJSON(path string, report *Report) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
