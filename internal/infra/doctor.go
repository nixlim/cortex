package infra

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Doctor check-result status constants. These are the three distinct
// outcomes the spec asks for in "reports each check separately with
// duration and remediation guidance". A warn is non-fatal but surfaced
// to the operator (e.g. Leiden unavailable → Louvain fallback).
const (
	CheckPass = "pass"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Stable error codes emitted by doctor checks. The subset that also
// exists in up.go (DOCKER_UNREACHABLE, WEAVIATE_NOT_READY, ...) is
// intentionally reused so operators see the same vocabulary across
// cortex up and cortex doctor output.
const (
	CodeHostRAMBelowFloor    = "HOST_RAM_BELOW_FLOOR"
	CodeHostDiskLow          = "HOST_DISK_LOW"
	CodeHostPermissionsWrong = "HOST_PERMISSIONS_WRONG"
	CodeHostPortBusy         = "HOST_PORT_BUSY"
	CodeHostUlimitLow        = "HOST_ULIMIT_LOW"
	CodeLogQuarantine        = "LOG_QUARANTINE_PRESENT"
	CodeOllamaNotLoopback    = "OLLAMA_NOT_LOOPBACK"
)

// DefaultDoctorParallelism matches doctor.parallelism in the config
// default table (docs/spec/cortex-spec.md line 301).
const DefaultDoctorParallelism = 4

// DefaultDoctorQuickTimeout is the total wall-clock budget for
// `cortex doctor --quick`. The spec pins this at 5s (line 302) and
// doctor enforces it via a context deadline that wraps the per-check
// context used in quick mode.
const DefaultDoctorQuickTimeout = 5 * time.Second

// DefaultDoctorQuickPerCheck bounds each individual quick-mode check
// so a single slow probe cannot eat the entire 5s budget. The spec
// says quick checks are "bounded to <500 ms individual time" (bead
// constraint + cortex-spec.md §"Time and Timeout Budgets").
const DefaultDoctorQuickPerCheck = 500 * time.Millisecond

// MinimumHostRAMBytes is the 12 GB floor from the Host Prerequisites
// table (cortex-spec.md line 523) and FR-059.
const MinimumHostRAMBytes = uint64(12) * 1024 * 1024 * 1024

// CheckResult is the per-check output row. All fields are flat and
// JSON-friendly so `cortex doctor --json` produces a shape the spec's
// acceptance tests can pattern-match.
type CheckResult struct {
	Name        string `json:"name"`
	Status      string `json:"status"`            // pass | warn | fail
	DurationMS  int64  `json:"duration_ms"`
	Code        string `json:"code,omitempty"`    // stable error code on warn/fail
	Message     string `json:"message,omitempty"` // one-line explanation
	Remediation string `json:"remediation,omitempty"`
}

// DoctorCheck is the narrow surface every doctor probe implements.
// The Doctor runner treats checks opaquely and only cares about Name
// (for logging + dedup), Quick (to decide whether to skip in quick
// mode), and Run (the actual work). The name is prefixed to avoid
// collision with the infra.Check status function.
type DoctorCheck interface {
	Name() string
	// Quick reports whether this check should run in `cortex doctor
	// --quick` mode. Quick checks must return within
	// DefaultDoctorQuickPerCheck; slow checks (segment scan, deep GDS
	// probe) return false and are skipped in quick mode.
	Quick() bool
	Run(ctx context.Context) CheckResult
}

// checkFunc adapts a function to the DoctorCheck interface. It exists
// so built-in checks can be declared as closures without each one
// needing its own named struct.
type checkFunc struct {
	name  string
	quick bool
	run   func(context.Context) CheckResult
}

func (c checkFunc) Name() string                        { return c.name }
func (c checkFunc) Quick() bool                         { return c.quick }
func (c checkFunc) Run(ctx context.Context) CheckResult { return c.run(ctx) }

// NewCheck is the public constructor for a DoctorCheck built from a
// closure. It is exported because cmd/cortex/doctor.go assembles the
// default check set by composing small functions against live
// adapters.
func NewCheck(name string, quick bool, run func(context.Context) CheckResult) DoctorCheck {
	return checkFunc{name: name, quick: quick, run: run}
}

// DoctorReport is the top-level shape of `cortex doctor --json`. It
// carries every CheckResult plus counts derived from them, so the CLI
// exit code can be computed without re-walking the slice.
type DoctorReport struct {
	Mode        string        `json:"mode"` // "quick" or "full"
	ElapsedMS   int64         `json:"elapsed_ms"`
	Parallelism int           `json:"parallelism"`
	Checks      []CheckResult `json:"checks"`
	Passes      int           `json:"passes"`
	Warns       int           `json:"warns"`
	Fails       int           `json:"fails"`
}

// HasFailures reports whether any check returned fail. The CLI uses
// this to pick exit code 1 (fail) vs 0 (all pass or warn-only). Warns
// are non-fatal by spec intent: the Leiden→Louvain fallback is a
// warn, and the benchmark still ships green.
func (r DoctorReport) HasFailures() bool { return r.Fails > 0 }

// DoctorOptions wires RunDoctor to its checks and timing knobs.
type DoctorOptions struct {
	Checks           []DoctorCheck
	Quick            bool
	Parallelism      int
	QuickTimeout     time.Duration
	QuickPerCheck    time.Duration
	FullCheckTimeout time.Duration
	Clock            func() time.Time
}

// RunDoctor executes every eligible check according to the mode and
// assembles a DoctorReport. RunDoctor never returns an error: every
// per-check failure is encoded into its CheckResult so the caller
// always gets a complete picture. Use DoctorReport.HasFailures to
// decide whether the process should exit non-zero.
func RunDoctor(ctx context.Context, opts DoctorOptions) DoctorReport {
	if opts.Parallelism <= 0 {
		opts.Parallelism = DefaultDoctorParallelism
	}
	if opts.QuickTimeout <= 0 {
		opts.QuickTimeout = DefaultDoctorQuickTimeout
	}
	if opts.QuickPerCheck <= 0 {
		opts.QuickPerCheck = DefaultDoctorQuickPerCheck
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}

	// Filter checks by mode. Quick mode keeps only fast checks; full
	// mode keeps everything.
	eligible := make([]DoctorCheck, 0, len(opts.Checks))
	for _, c := range opts.Checks {
		if opts.Quick && !c.Quick() {
			continue
		}
		eligible = append(eligible, c)
	}

	start := opts.Clock()

	// Quick mode wraps the entire run in a single 5s deadline so no
	// amount of stuck probes can blow past the spec budget. Full mode
	// has no hard cap (spec: "no hard total bound") but individual
	// checks may still be cancelled through the caller's ctx.
	runCtx := ctx
	if opts.Quick {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, opts.QuickTimeout)
		defer cancel()
	}

	// Worker pool. Each worker pulls from the check channel and
	// publishes into the result channel. Parallelism is bounded by the
	// configured value.
	checkCh := make(chan DoctorCheck)
	resultCh := make(chan CheckResult, len(eligible))

	var wg sync.WaitGroup
	workers := opts.Parallelism
	if workers > len(eligible) {
		workers = len(eligible)
	}
	if workers < 1 && len(eligible) > 0 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range checkCh {
				resultCh <- runOneCheck(runCtx, c, opts.Quick, opts.QuickPerCheck, opts.FullCheckTimeout, opts.Clock)
			}
		}()
	}
	go func() {
		for _, c := range eligible {
			checkCh <- c
		}
		close(checkCh)
	}()
	wg.Wait()
	close(resultCh)

	results := make([]CheckResult, 0, len(eligible))
	for r := range resultCh {
		results = append(results, r)
	}
	// Sort by name so the JSON output is deterministic across runs
	// regardless of worker scheduling.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	report := DoctorReport{
		Mode:        modeString(opts.Quick),
		Checks:      results,
		Parallelism: opts.Parallelism,
	}
	for _, r := range results {
		switch r.Status {
		case CheckPass:
			report.Passes++
		case CheckWarn:
			report.Warns++
		case CheckFail:
			report.Fails++
		}
	}
	report.ElapsedMS = opts.Clock().Sub(start).Milliseconds()
	return report
}

// runOneCheck invokes a single check under its mode-specific deadline
// and returns the resulting CheckResult. Duration is measured around
// the Run call so the operator can see which checks are expensive.
func runOneCheck(
	parent context.Context,
	c DoctorCheck,
	quick bool,
	quickPerCheck, fullPerCheck time.Duration,
	clock func() time.Time,
) CheckResult {
	ctx := parent
	if quick {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, quickPerCheck)
		defer cancel()
	} else if fullPerCheck > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(parent, fullPerCheck)
		defer cancel()
	}
	start := clock()
	result := c.Run(ctx)
	if result.Name == "" {
		result.Name = c.Name()
	}
	result.DurationMS = clock().Sub(start).Milliseconds()
	return result
}

// modeString is a single-line helper so the JSON field is a readable
// literal rather than a bool-to-string inline.
func modeString(quick bool) string {
	if quick {
		return "quick"
	}
	return "full"
}

// MarshalJSON produces the canonical on-wire shape of a DoctorReport.
// It is a thin wrapper around encoding/json to centralize the type so
// tests can compare byte-for-byte output.
func (r DoctorReport) MarshalJSON() ([]byte, error) {
	type alias DoctorReport
	return json.Marshal(alias(r))
}
