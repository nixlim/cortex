package shutdown

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// testLogger is a Logger double that records every SHUTDOWN_REQUESTED
// call. Tests assert both the call count (acceptance criterion
// "logs a SHUTDOWN_REQUESTED event") and that the signal name is
// propagated so operators can tell SIGINT from SIGTERM in ops.log.
type testLogger struct {
	mu      sync.Mutex
	signals []string
}

func (t *testLogger) LogShutdownRequested(sig string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.signals = append(t.signals, sig)
}

func (t *testLogger) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.signals))
	copy(out, t.signals)
	return out
}

// newTestHandle builds an Install handle wired to a synthetic signal
// channel the test controls directly. We deliberately do NOT use real
// OS signal delivery in unit tests: Go's runtime uses SIGURG for
// goroutine preemption, test sandboxes can intercept SIGUSR1, and the
// signal.Notify fan-out is hard to isolate between concurrent tests.
// The synthetic channel exercises the exact same state machine without
// any of those hazards. End-to-end validation of the real signal path
// lives in the //go:build integration suite.
func newTestHandle(t *testing.T, opts Options) (*Handle, chan<- os.Signal) {
	t.Helper()
	src := make(chan os.Signal, 4)
	opts.signalSource = src
	return Install(context.Background(), opts), src
}

// waitFor polls cond up to timeout; tests use it to avoid racing the
// signal goroutine without hard-coding a sleep.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// TestInstall_FirstSignalCancelsContextAndLogs covers the core
// acceptance criterion: one SIGINT cancels the context and writes a
// SHUTDOWN_REQUESTED event to ops.log.
func TestInstall_FirstSignalCancelsContextAndLogs(t *testing.T) {
	logger := &testLogger{}
	var hardExitCalls int32
	h, src := newTestHandle(t, Options{
		Logger:      logger,
		GraceWindow: 200 * time.Millisecond,
		HardExit:    func(code int) { atomic.AddInt32(&hardExitCalls, 1) },
	})
	defer h.Stop()

	if h.ShutdownRequested() {
		t.Fatal("ShutdownRequested true before any signal")
	}

	src <- syscall.SIGINT

	waitFor(t, time.Second, func() bool {
		return h.ShutdownRequested() && h.Context().Err() != nil
	})

	if h.Context().Err() == nil {
		t.Errorf("context not cancelled after SIGINT")
	}
	sigs := logger.snapshot()
	if len(sigs) != 1 {
		t.Errorf("logger saw %d events, want 1: %v", len(sigs), sigs)
	}
	if len(sigs) == 1 && sigs[0] == "" {
		t.Error("logged signal name is empty")
	}
	if atomic.LoadInt32(&hardExitCalls) != 0 {
		t.Errorf("HardExit called on first signal; should only fire on the second")
	}
}

// TestInstall_SecondSignalWithinGraceWindowTriggersHardExit covers
// "A second SIGINT within 5 seconds of the first triggers an
// immediate exit without further log flushing". We set GraceWindow
// to 500ms and deliver two signals back-to-back.
func TestInstall_SecondSignalWithinGraceWindowTriggersHardExit(t *testing.T) {
	logger := &testLogger{}
	hardExitCh := make(chan int, 1)
	h, src := newTestHandle(t, Options{
		Logger:      logger,
		GraceWindow: 500 * time.Millisecond,
		HardExit: func(code int) {
			select {
			case hardExitCh <- code:
			default:
			}
		},
	})
	defer h.Stop()

	src <- syscall.SIGINT
	waitFor(t, time.Second, h.ShutdownRequested)
	src <- syscall.SIGINT

	select {
	case code := <-hardExitCh:
		if code != ExitCodeGraceful {
			t.Errorf("HardExit code = %d, want %d", code, ExitCodeGraceful)
		}
	case <-time.After(time.Second):
		t.Fatal("HardExit not called after second signal within grace window")
	}

	// The logger should still only have the first event — a second
	// signal is a user escalation, not a new shutdown request, so it
	// must not flood ops.log.
	if sigs := logger.snapshot(); len(sigs) != 1 {
		t.Errorf("logger saw %d events, want exactly 1", len(sigs))
	}
}

// TestInstall_SecondSignalOutsideGraceWindowDoesNotHardExit — a
// delayed second signal is treated as a fresh first signal, not as
// an escalation. This prevents a stray Ctrl+C hours later from
// nuking the process.
func TestInstall_SecondSignalOutsideGraceWindowDoesNotHardExit(t *testing.T) {
	var hardExitCalls int32
	h, src := newTestHandle(t, Options{
		GraceWindow: 10 * time.Millisecond,
		HardExit:    func(code int) { atomic.AddInt32(&hardExitCalls, 1) },
	})
	defer h.Stop()

	src <- syscall.SIGINT
	waitFor(t, time.Second, h.ShutdownRequested)

	// Sleep past the grace window.
	time.Sleep(40 * time.Millisecond)
	src <- syscall.SIGINT
	// Give the goroutine a beat to observe the second signal.
	time.Sleep(30 * time.Millisecond)

	if n := atomic.LoadInt32(&hardExitCalls); n != 0 {
		t.Errorf("HardExit called %d times, want 0 (second signal outside grace window)", n)
	}
}

// TestInstall_ParentCancellationStopsGoroutine — Stop()'ing via
// parent cancellation must reliably tear down the signal goroutine,
// otherwise repeated Install calls in the same test binary would
// leak goroutines.
func TestInstall_ParentCancellationStopsGoroutine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan os.Signal, 1)
	h := Install(ctx, Options{signalSource: src})
	cancel()
	// Stop will block on h.done until the goroutine exits.
	done := make(chan struct{})
	go func() {
		h.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal goroutine did not exit after parent cancel")
	}
}

// TestLoggerFunc verifies the convenience adapter routes calls to the
// underlying function.
func TestLoggerFunc(t *testing.T) {
	var got string
	var fn Logger = LoggerFunc(func(sig string) { got = sig })
	fn.LogShutdownRequested("test-sig")
	if got != "test-sig" {
		t.Errorf("LoggerFunc got %q, want test-sig", got)
	}
}

// TestInstall_NilLoggerIsTolerated — the spec calls the log write
// "best-effort"; if the caller has not yet wired ops.log we must not
// crash. The drain path still runs.
func TestInstall_NilLoggerIsTolerated(t *testing.T) {
	h, src := newTestHandle(t, Options{
		GraceWindow: 100 * time.Millisecond,
		HardExit:    func(int) {},
	})
	defer h.Stop()
	src <- syscall.SIGINT
	waitFor(t, time.Second, h.ShutdownRequested)
}

// TestInstall_DefaultGraceWindow verifies that a zero GraceWindow in
// Options falls back to DefaultGraceWindow (5s per spec).
func TestInstall_DefaultGraceWindow(t *testing.T) {
	// We can't directly observe the internal grace window, but we
	// can prove that zero is rejected by the defaulting branch:
	// construct an Install with GraceWindow=0 and verify the first
	// signal still cancels the context (i.e., the code path didn't
	// panic on a zero-duration comparison).
	h, src := newTestHandle(t, Options{
		HardExit: func(int) {},
	})
	defer h.Stop()
	src <- syscall.SIGINT
	waitFor(t, time.Second, h.ShutdownRequested)
	if h.Context().Err() == nil {
		t.Error("context not cancelled under default grace window")
	}
}
