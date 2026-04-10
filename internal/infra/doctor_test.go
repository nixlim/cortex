package infra

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fake HostProvider
// ---------------------------------------------------------------------------

type fakeHost struct {
	ram     uint64
	ramErr  error
	disk    uint64
	diskErr error
	fd      uint64
	fdErr   error
	busy    map[int]bool
	portErr error
}

func (f *fakeHost) TotalRAMBytes() (uint64, error)             { return f.ram, f.ramErr }
func (f *fakeHost) FreeDiskBytes(string) (uint64, error)       { return f.disk, f.diskErr }
func (f *fakeHost) FDLimit() (uint64, error)                   { return f.fd, f.fdErr }
func (f *fakeHost) PortInUse(p int) (bool, error) {
	if f.portErr != nil {
		return false, f.portErr
	}
	return f.busy[p], nil
}

func healthyHost() *fakeHost {
	return &fakeHost{
		ram:  16 * 1024 * 1024 * 1024,
		disk: 60 * 1024 * 1024 * 1024,
		fd:   8192,
		busy: map[int]bool{},
	}
}

// ---------------------------------------------------------------------------
// Framework: parallelism, quick mode, JSON shape, determinism
// ---------------------------------------------------------------------------

// counterCheck records invocation concurrency so TestDoctorParallelism
// can verify the worker pool obeys the configured bound.
type counterCheck struct {
	name    string
	quick   bool
	inflight *int32
	peak     *int32
	hold     time.Duration
	status   string
}

func (c *counterCheck) Name() string { return c.name }
func (c *counterCheck) Quick() bool  { return c.quick }
func (c *counterCheck) Run(ctx context.Context) CheckResult {
	n := atomic.AddInt32(c.inflight, 1)
	for {
		p := atomic.LoadInt32(c.peak)
		if n <= p || atomic.CompareAndSwapInt32(c.peak, p, n) {
			break
		}
	}
	select {
	case <-time.After(c.hold):
	case <-ctx.Done():
	}
	atomic.AddInt32(c.inflight, -1)
	return CheckResult{Name: c.name, Status: c.status}
}

func TestDoctorParallelismBoundsInflight(t *testing.T) {
	var inflight, peak int32
	const n = 12
	checks := make([]DoctorCheck, n)
	for i := 0; i < n; i++ {
		checks[i] = &counterCheck{
			name:     "c" + string(rune('a'+i)),
			quick:    false,
			inflight: &inflight,
			peak:     &peak,
			hold:     10 * time.Millisecond,
			status:   CheckPass,
		}
	}
	RunDoctor(context.Background(), DoctorOptions{
		Checks:      checks,
		Parallelism: 3,
	})
	if peak > 3 {
		t.Errorf("peak inflight = %d, want <= 3", peak)
	}
	if peak < 2 {
		t.Errorf("peak inflight = %d, want >= 2 (parallelism not engaged)", peak)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex doctor --quick completes in under 5 seconds
// regardless of backend state."
// ---------------------------------------------------------------------------

func TestDoctorQuickTimeBudget(t *testing.T) {
	// A check that would hang forever if not cancelled.
	hanging := NewCheck("hanging", true, func(ctx context.Context) CheckResult {
		<-ctx.Done()
		return CheckResult{Name: "hanging", Status: CheckFail, Message: ctx.Err().Error()}
	})
	start := time.Now()
	r := RunDoctor(context.Background(), DoctorOptions{
		Checks:        []DoctorCheck{hanging},
		Quick:         true,
		QuickTimeout:  800 * time.Millisecond,
		QuickPerCheck: 200 * time.Millisecond,
		Parallelism:   1,
	})
	elapsed := time.Since(start)
	if elapsed > 1500*time.Millisecond {
		t.Errorf("quick mode elapsed %v, want <1.5s", elapsed)
	}
	if r.Mode != "quick" {
		t.Errorf("Mode = %q, want quick", r.Mode)
	}
	if r.Fails != 1 {
		t.Errorf("Fails = %d, want 1", r.Fails)
	}
}

// Quick mode must skip checks marked Quick() == false.
func TestDoctorQuickSkipsSlowChecks(t *testing.T) {
	var slowCalls int32
	slow := NewCheck("slow", false, func(context.Context) CheckResult {
		atomic.AddInt32(&slowCalls, 1)
		return CheckResult{Name: "slow", Status: CheckPass}
	})
	fast := NewCheck("fast", true, func(context.Context) CheckResult {
		return CheckResult{Name: "fast", Status: CheckPass}
	})
	r := RunDoctor(context.Background(), DoctorOptions{
		Checks: []DoctorCheck{slow, fast},
		Quick:  true,
	})
	if slowCalls != 0 {
		t.Errorf("slow check invoked %d times in quick mode, want 0", slowCalls)
	}
	if len(r.Checks) != 1 || r.Checks[0].Name != "fast" {
		t.Errorf("quick results = %+v, want [fast]", r.Checks)
	}
}

// JSON output has a top-level checks array + counts and is deterministic.
func TestDoctorJSONShapeAndDeterminism(t *testing.T) {
	build := func() DoctorReport {
		return RunDoctor(context.Background(), DoctorOptions{
			Checks: []DoctorCheck{
				NewCheck("b", true, func(context.Context) CheckResult {
					return CheckResult{Status: CheckPass}
				}),
				NewCheck("a", true, func(context.Context) CheckResult {
					return CheckResult{Status: CheckPass}
				}),
			},
		})
	}
	r1 := build()
	r2 := build()
	r1.ElapsedMS = 0
	r2.ElapsedMS = 0
	for i := range r1.Checks {
		r1.Checks[i].DurationMS = 0
	}
	for i := range r2.Checks {
		r2.Checks[i].DurationMS = 0
	}
	j1, _ := json.Marshal(r1)
	j2, _ := json.Marshal(r2)
	if string(j1) != string(j2) {
		t.Errorf("non-deterministic output:\n%s\n%s", j1, j2)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(j1, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"mode", "checks", "passes", "warns", "fails"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing top-level key %q in %s", k, j1)
		}
	}
	// Checks must be sorted by Name regardless of execution order.
	if r1.Checks[0].Name != "a" || r1.Checks[1].Name != "b" {
		t.Errorf("checks not sorted: %+v", r1.Checks)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "detects host RAM below 12 GB and fails loudly with
// HOST_RAM_BELOW_FLOOR."
// ---------------------------------------------------------------------------

func TestHostRAMCheckBelowFloor(t *testing.T) {
	host := healthyHost()
	host.ram = 8 * 1024 * 1024 * 1024 // 8 GB

	r := HostRAMCheck(host).Run(context.Background())
	if r.Status != CheckFail {
		t.Errorf("status = %q, want fail", r.Status)
	}
	if r.Code != CodeHostRAMBelowFloor {
		t.Errorf("code = %q, want %q", r.Code, CodeHostRAMBelowFloor)
	}
	if r.Remediation == "" {
		t.Error("remediation must be populated on failure")
	}
}

func TestHostRAMCheckAtFloorPasses(t *testing.T) {
	host := healthyHost()
	host.ram = MinimumHostRAMBytes

	r := HostRAMCheck(host).Run(context.Background())
	if r.Status != CheckPass {
		t.Errorf("status = %q, want pass", r.Status)
	}
}

func TestHostRAMCheckReadError(t *testing.T) {
	host := healthyHost()
	host.ramErr = errors.New("sysctl: permission denied")

	r := HostRAMCheck(host).Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeHostRAMBelowFloor {
		t.Errorf("got %+v, want fail+HOST_RAM_BELOW_FLOOR", r)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "on a missing embedding model exits non-zero with
// OLLAMA_MODEL_MISSING in the error list."
// ---------------------------------------------------------------------------

type fakeOllamaReady struct {
	models []string
	err    error
}

func (f *fakeOllamaReady) Ping(context.Context) error              { return nil }
func (f *fakeOllamaReady) ListModels(context.Context) ([]string, error) {
	return f.models, f.err
}

func TestOllamaModelCheckMissing(t *testing.T) {
	ollama := &fakeOllamaReady{models: []string{"qwen3:4b-instruct-2507"}}

	r := OllamaModelCheck(ollama, "nomic-embed-text", "qwen3:4b-instruct-2507").Run(context.Background())
	if r.Status != CheckFail {
		t.Errorf("status = %q, want fail", r.Status)
	}
	if r.Code != CodeOllamaModelMissing {
		t.Errorf("code = %q, want %q", r.Code, CodeOllamaModelMissing)
	}
	if !strings.Contains(r.Remediation, "ollama pull nomic-embed-text") {
		t.Errorf("remediation = %q, want 'ollama pull nomic-embed-text'", r.Remediation)
	}
}

func TestOllamaModelCheckAllPresent(t *testing.T) {
	ollama := &fakeOllamaReady{models: []string{"nomic-embed-text:latest", "qwen3:4b-instruct-2507"}}

	r := OllamaModelCheck(ollama, "nomic-embed-text", "qwen3:4b-instruct-2507").Run(context.Background())
	if r.Status != CheckPass {
		t.Errorf("status = %q, want pass", r.Status)
	}
}

func TestOllamaModelCheckListError(t *testing.T) {
	ollama := &fakeOllamaReady{err: errors.New("connection refused")}

	r := OllamaModelCheck(ollama, "nomic-embed-text", "").Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeOllamaNotReachable {
		t.Errorf("got %+v, want fail+OLLAMA_NOT_REACHABLE", r)
	}
}

// ---------------------------------------------------------------------------
// Host prerequisite checks
// ---------------------------------------------------------------------------

func TestHostDiskCheckUnderFloor(t *testing.T) {
	host := healthyHost()
	host.disk = 5 * 1024 * 1024 * 1024 // 5 GB

	r := HostDiskCheck(host, "/tmp/nowhere").Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeHostDiskLow {
		t.Errorf("got %+v, want fail+HOST_DISK_LOW", r)
	}
}

func TestHostDiskCheckBelowRecommendedWarns(t *testing.T) {
	host := healthyHost()
	host.disk = 20 * 1024 * 1024 * 1024 // between 10 and 50 GB

	r := HostDiskCheck(host, "/tmp").Run(context.Background())
	if r.Status != CheckWarn {
		t.Errorf("got %+v, want warn", r)
	}
}

func TestHostFDLimitUnderFloor(t *testing.T) {
	host := healthyHost()
	host.fd = 1024

	r := HostFDLimitCheck(host).Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeHostUlimitLow {
		t.Errorf("got %+v, want fail+HOST_ULIMIT_LOW", r)
	}
}

func TestHostFDLimitRecommendedWarns(t *testing.T) {
	host := healthyHost()
	host.fd = 4096

	r := HostFDLimitCheck(host).Run(context.Background())
	if r.Status != CheckWarn {
		t.Errorf("got %+v, want warn", r)
	}
}

func TestHostPortsBusyFails(t *testing.T) {
	host := healthyHost()
	host.busy = map[int]bool{7687: true}

	r := HostPortsCheck(host).Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeHostPortBusy {
		t.Errorf("got %+v, want fail+HOST_PORT_BUSY", r)
	}
	if !strings.Contains(r.Message, "7687") {
		t.Errorf("message should name the busy port: %q", r.Message)
	}
}

// ---------------------------------------------------------------------------
// File permissions and log quarantine checks
// ---------------------------------------------------------------------------

func TestFilePermissionsCheckDetectsLooseDir(t *testing.T) {
	dir := t.TempDir()
	// Make the tempdir itself 0755 (wrong: should be 0700).
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)

	r := FilePermissionsCheck(dir).Run(context.Background())
	if r.Status != CheckFail || r.Code != CodeHostPermissionsWrong {
		t.Errorf("got %+v, want fail+HOST_PERMISSIONS_WRONG", r)
	}
}

func TestFilePermissionsCheckPassesOnCleanTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "a")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := FilePermissionsCheck(dir).Run(context.Background())
	if r.Status != CheckPass {
		t.Errorf("got %+v, want pass", r)
	}
}

func TestLogQuarantineNoDirPasses(t *testing.T) {
	dir := t.TempDir()
	r := LogQuarantineCheck(dir).Run(context.Background())
	if r.Status != CheckPass {
		t.Errorf("got %+v, want pass when quarantine missing", r)
	}
}

func TestLogQuarantineWithEntriesWarns(t *testing.T) {
	dir := t.TempDir()
	qdir := filepath.Join(dir, "log.d", ".quarantine")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(qdir, "bad.jsonl"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := LogQuarantineCheck(dir).Run(context.Background())
	if r.Status != CheckWarn || r.Code != CodeLogQuarantine {
		t.Errorf("got %+v, want warn+LOG_QUARANTINE_PRESENT", r)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex doctor --json on a healthy stack exits zero; on
// a missing embedding model exits non-zero."
// ---------------------------------------------------------------------------

func TestDoctorReportHasFailures(t *testing.T) {
	healthy := RunDoctor(context.Background(), DoctorOptions{
		Checks: []DoctorCheck{HostRAMCheck(healthyHost())},
	})
	if healthy.HasFailures() {
		t.Errorf("healthy report should not have failures: %+v", healthy)
	}
	bad := RunDoctor(context.Background(), DoctorOptions{
		Checks: []DoctorCheck{
			OllamaModelCheck(&fakeOllamaReady{}, "nomic-embed-text", ""),
		},
	})
	if !bad.HasFailures() {
		t.Errorf("missing-model report should have failures: %+v", bad)
	}
	// The failure record must carry the stable code in the error list
	// so operators can grep for it in --json output.
	var sawCode bool
	for _, r := range bad.Checks {
		if r.Code == CodeOllamaModelMissing {
			sawCode = true
		}
	}
	if !sawCode {
		t.Errorf("OLLAMA_MODEL_MISSING not in error list: %+v", bad.Checks)
	}
}

// Remediation guidance is populated on every fail result.
func TestDoctorRemediationPresent(t *testing.T) {
	host := healthyHost()
	host.ram = 4 * 1024 * 1024 * 1024

	r := RunDoctor(context.Background(), DoctorOptions{
		Checks: []DoctorCheck{HostRAMCheck(host)},
	})
	for _, c := range r.Checks {
		if c.Status == CheckFail && c.Remediation == "" {
			t.Errorf("fail check %q missing remediation", c.Name)
		}
	}
}
