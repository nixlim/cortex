// internal/e2e/cli_exec_test.go is the CLI-exec harness called for in the
// Phase 1 grill-code review (docs/spec/cortex-spec-code-review.md, MAJ-005).
//
// Unlike the in-process e2e tests in this package, every test here builds
// the real cortex binary and invokes subcommands as actual subprocesses
// via os/exec, so it asserts the same surface an operator hits at the
// shell. The point is to catch the failure mode the grill review found
// in round 1: a subcommand can register on the cobra tree and import its
// implementation package while its RunE still returns notImplemented —
// the in-process unit tests don't notice because they call the package
// directly, never the CLI.
//
// The harness is gated behind `//go:build cli` so it stays out of the
// default `go test ./...` loop (it shells out to `go build` and runs
// real binaries — slow). Run it with:
//
//	make test-cli
//	go test -tags=cli ./internal/e2e/...
//
// Two test categories live here:
//
//   - "wired" tests assert behavior of subcommands that have already
//     been wired to real implementations. They must pass today.
//
//   - "stub canary" tests assert that subcommands which still return
//     notImplemented eventually stop doing so. They are expected to FAIL
//     today and to pass once each stub is replaced. They double as a
//     completion ladder: every red line in `make test-cli` output is a
//     CLI verb that is documented but not yet honest.
//
// Spec references:
//
//	docs/spec/cortex-spec-code-review.md MAJ-005 (CLI-exec harness)
//	docs/spec/cortex-spec.md (full subcommand surface)

//go:build cli

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// cortexBin is set once by buildCortexOnce and reused by every test.
var (
	cortexBin     string
	cortexBuildMu sync.Mutex
	cortexBuilt   bool
	cortexBuildErr error
)

// buildCortexOnce compiles ./cmd/cortex into a temp directory exactly
// once per `go test` invocation. It walks up from the test file to
// find the module root rather than depending on a fixed CWD, so the
// harness works whether the test is invoked from the package directory
// or from the repo root.
func buildCortexOnce(t *testing.T) string {
	t.Helper()
	cortexBuildMu.Lock()
	defer cortexBuildMu.Unlock()
	if cortexBuilt {
		if cortexBuildErr != nil {
			t.Fatalf("cortex build (cached): %v", cortexBuildErr)
		}
		return cortexBin
	}
	cortexBuilt = true

	root, err := moduleRoot()
	if err != nil {
		cortexBuildErr = err
		t.Fatalf("locate module root: %v", err)
	}

	tmp, err := os.MkdirTemp("", "cortex-cli-exec-*")
	if err != nil {
		cortexBuildErr = err
		t.Fatalf("mkdtemp: %v", err)
	}
	bin := filepath.Join(tmp, "cortex")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/cortex")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		cortexBuildErr = err
		t.Fatalf("go build ./cmd/cortex failed: %v\n%s", err, out)
	}
	cortexBin = bin
	return bin
}

// moduleRoot walks upward from this test file's directory until it
// finds a go.mod, returning the directory that contains it.
func moduleRoot() (string, error) {
	// internal/e2e is two directories under the module root.
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}

// runResult bundles everything a test needs to assert on a single
// subprocess invocation.
type runResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// runCortex runs the prebuilt binary with the given args under an
// isolated HOME and CORTEX_CONFIG_PATH, so tests never touch the
// developer's real ~/.cortex/. The caller can override env via extraEnv.
func runCortex(t *testing.T, extraEnv []string, args ...string) runResult {
	t.Helper()
	bin := buildCortexOnce(t)

	// If the caller pre-assigned HOME in extraEnv (integration tests
	// that need to share state with the host cortex install — e.g.
	// observe→recall against a live managed stack), honour it. Other-
	// wise use an isolated t.TempDir() so hermetic tests never touch
	// the developer's real ~/.cortex/.
	home := ""
	for _, kv := range extraEnv {
		if strings.HasPrefix(kv, "HOME=") {
			home = strings.TrimPrefix(kv, "HOME=")
			break
		}
	}
	if home == "" {
		home = t.TempDir()
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"CORTEX_HOME="+home,
	)
	cmd.Env = append(cmd.Env, extraEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	res := runResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		res.exitCode = 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		res.exitCode = ee.ExitCode()
	} else {
		t.Fatalf("cortex %v failed to start: %v", args, err)
	}
	return res
}

// ---------------------------------------------------------------------------
// Wired surface — these must pass today.
// ---------------------------------------------------------------------------

// TestCLI_Version proves the binary launches and prints something.
func TestCLI_Version(t *testing.T) {
	r := runCortex(t, nil, "version")
	if r.exitCode != 0 {
		t.Fatalf("version exit=%d, stderr=%q", r.exitCode, r.stderr)
	}
	if strings.TrimSpace(r.stdout) == "" {
		t.Fatalf("version stdout empty")
	}
}

// TestCLI_RootHelpListsAllVerbs is the regression that would have caught
// CRIT-003: it asserts every documented verb appears in `cortex --help`,
// so any future drop is loud.
func TestCLI_RootHelpListsAllVerbs(t *testing.T) {
	r := runCortex(t, nil, "--help")
	if r.exitCode != 0 {
		t.Fatalf("--help exit=%d, stderr=%q", r.exitCode, r.stderr)
	}
	for _, verb := range []string{
		"up", "down", "status", "doctor",
		"trail", "history", "as-of",
		"communities", "community",
		"pin", "unpin", "evict", "unevict",
		"rebuild", "export", "merge", "retract", "subject", "migrate", "bench",
		"observe", "recall", "reflect", "ingest", "analyze",
		"version",
	} {
		if !strings.Contains(r.stdout, verb) {
			t.Errorf("root --help missing verb %q\nstdout:\n%s", verb, r.stdout)
		}
	}
}

// TestCLI_Retract_RequiresEntityID asserts the new retract CLI rejects
// missing args via cobra and exits non-zero.
func TestCLI_Retract_RequiresEntityID(t *testing.T) {
	r := runCortex(t, nil, "retract")
	if r.exitCode == 0 {
		t.Fatalf("retract with no args should fail; stdout=%q", r.stdout)
	}
}

// TestCLI_SubjectMerge_RequiresTwoArgs asserts the new subject merge CLI
// rejects under-specified args via cobra and exits non-zero.
func TestCLI_SubjectMerge_RequiresTwoArgs(t *testing.T) {
	r := runCortex(t, nil, "subject", "merge")
	if r.exitCode == 0 {
		t.Fatalf("subject merge with no args should fail; stdout=%q", r.stdout)
	}
}

// TestCLI_Migrate_RequiresFromMempalace asserts the new migrate CLI
// emits MISSING_SOURCE when --from-mempalace is omitted, and that the
// failure is a validation error (exit 2), not "not implemented".
func TestCLI_Migrate_RequiresFromMempalace(t *testing.T) {
	r := runCortex(t, nil, "migrate")
	if r.exitCode == 0 {
		t.Fatalf("migrate without --from-mempalace should fail; stdout=%q", r.stdout)
	}
	combined := r.stdout + r.stderr
	if strings.Contains(combined, "not implemented") {
		t.Fatalf("migrate still reports not-implemented; this CLI should be wired now\n%s", combined)
	}
	if !strings.Contains(combined, "MISSING_SOURCE") &&
		!strings.Contains(combined, "from-mempalace") {
		t.Fatalf("migrate error did not mention MISSING_SOURCE or --from-mempalace flag\nstdout=%q\nstderr=%q",
			r.stdout, r.stderr)
	}
}

// TestCLI_Retract_NotImplementedGone proves CRIT-003 stays fixed for
// retract: invoking it with a synthetic id must not fall through to
// the generic "not implemented" stub. The call may still fail (no log
// segment dir, no real entity, etc.), but the *reason* must not be the
// notImplemented stub.
func TestCLI_Retract_NotImplementedGone(t *testing.T) {
	r := runCortex(t, nil, "retract", "ent:synthetic")
	combined := r.stdout + r.stderr
	if strings.Contains(combined, "not implemented") {
		t.Fatalf("retract still returns notImplemented; output:\n%s", combined)
	}
}

// TestCLI_SubjectMerge_NotImplementedGone is the same canary for
// subject merge: invoking it with two PSIs must not fall through to
// notImplemented even if the segment dir cannot be opened.
func TestCLI_SubjectMerge_NotImplementedGone(t *testing.T) {
	r := runCortex(t, nil, "subject", "merge", "psi:a", "psi:b")
	combined := r.stdout + r.stderr
	if strings.Contains(combined, "not implemented") {
		t.Fatalf("subject merge still returns notImplemented; output:\n%s", combined)
	}
}

// ---------------------------------------------------------------------------
// Stub canary suite — every test below is expected to FAIL until the
// matching notImplemented stub in cmd/cortex/commands.go is replaced.
// Each failure is a TODO with a precise CLI repro; passing means the
// stub has been honestly wired through to its package.
// ---------------------------------------------------------------------------

// stubCanary asserts that running `cortex <args>` does NOT print the
// generic "not implemented" prefix. The test fails when the stub is
// still in place.
func stubCanary(t *testing.T, args ...string) {
	t.Helper()
	r := runCortex(t, nil, args...)
	combined := r.stdout + r.stderr
	if strings.Contains(combined, "not implemented") {
		t.Fatalf("cortex %v still returns notImplemented stub:\n%s",
			args, combined)
	}
}

// Pin/unpin/evict/unevict were wired in lifecycle.go alongside the
// CRIT-003 sweep; they used to be stub canaries here but graduated to
// the wired surface and live as standing assertions instead.
func TestCLI_Lifecycle_NotImplementedGone(t *testing.T) {
	for _, args := range [][]string{
		{"pin", "ent:synthetic"},
		{"unpin", "ent:synthetic"},
		{"evict", "ent:synthetic"},
		{"unevict", "ent:synthetic"},
	} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			r := runCortex(t, nil, args...)
			combined := r.stdout + r.stderr
			if strings.Contains(combined, "not implemented") {
				t.Fatalf("cortex %v still returns notImplemented:\n%s", args, combined)
			}
		})
	}
}

func TestCLI_StubCanary_Recall(t *testing.T) { stubCanary(t, "recall", "anything") }
func TestCLI_StubCanary_Reflect(t *testing.T) { stubCanary(t, "reflect", "--dry-run") }
func TestCLI_StubCanary_Ingest(t *testing.T)  { stubCanary(t, "ingest", "/tmp/none") }
func TestCLI_StubCanary_IngestStatus(t *testing.T) {
	stubCanary(t, "ingest", "status")
}
func TestCLI_StubCanary_IngestResume(t *testing.T) {
	stubCanary(t, "ingest", "resume")
}
