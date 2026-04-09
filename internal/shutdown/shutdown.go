// Package shutdown provides the graceful-shutdown primitive that Cortex's
// long-running commands (reflect, analyze, ingest, recall, observe) install at
// entry. It wraps signal.NotifyContext with two extra behaviours the spec
// requires for cortex-4kq.54:
//
//  1. The first SIGINT or SIGTERM cancels the returned context, logs a
//     SHUTDOWN_REQUESTED event, and lets the caller run its drain path
//     (commit or discard the current transaction group, flush watermarks,
//     close adapters).
//  2. A second signal received within the grace window (5 seconds by spec)
//     short-circuits the drain path and calls the configured HardExit
//     function immediately. The HardExit callback exists so tests can
//     observe the second-signal path without actually terminating the
//     process, and so production code can route through os.Exit(1).
//
// The package does not import internal/opslog directly — it takes a narrow
// Logger interface so tests can substitute a fake and so we don't pin the
// ops.log writer's concrete shape here.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Lifecycle and Signals" (SHUTDOWN_REQUESTED)
//	docs/spec/cortex-spec.md §"Exit Codes" (code 1 on graceful shutdown)
package shutdown

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DefaultGraceWindow is the span during which a second signal is
// interpreted as "exit now, stop trying to drain". The spec fixes
// this at five seconds.
const DefaultGraceWindow = 5 * time.Second

// ExitCodeGraceful is the exit code a command should return after a
// successful graceful shutdown. The spec (§Exit Codes) assigns code 1
// to this path so shell scripts can distinguish it from a normal
// clean exit (0) and a bug-class crash (2+).
const ExitCodeGraceful = 1

// Logger is the narrow opslog.Writer surface this package needs. Only
// a single method is required: recording the SHUTDOWN_REQUESTED event.
// Callers typically pass a thin adapter that wraps their opslog.Writer.
type Logger interface {
	LogShutdownRequested(signal string)
}

// LoggerFunc adapts a bare function to Logger, so callers that already
// have a closure over an opslog.Writer don't need to declare a type.
type LoggerFunc func(signal string)

func (f LoggerFunc) LogShutdownRequested(signal string) { f(signal) }

// Options configures Install. All fields have spec-compliant defaults
// when zero.
type Options struct {
	// Logger receives the SHUTDOWN_REQUESTED event on the first signal.
	// Nil is allowed — the shutdown still works, it just isn't logged
	// (useful when ops.log setup itself is what the command is
	// testing).
	Logger Logger

	// GraceWindow overrides DefaultGraceWindow. Tests pass a very short
	// window to verify the double-signal path without stalling the run.
	GraceWindow time.Duration

	// HardExit is called from the signal goroutine when a second signal
	// arrives inside GraceWindow. Production code should pass
	// func(code int) { os.Exit(code) }; tests pass a recorder.
	//
	// Leaving this nil panics on the second-signal path — we refuse to
	// silently swallow the "exit now" request because it's the user's
	// last resort when a drain has hung.
	HardExit func(code int)

	// Signals overrides the default set (SIGINT, SIGTERM). Kept as a
	// seam mainly for tests that want to use SIGUSR1 to avoid
	// disturbing the real test harness.
	Signals []os.Signal

	// signalSource is an unexported test seam: when non-nil, Install
	// reads signal events from this channel instead of wiring up
	// signal.Notify. Tests use it to drive the state machine without
	// touching real OS signal delivery (Go's runtime uses signals for
	// goroutine preemption, so real-signal tests are flaky under
	// some sandboxes).
	signalSource <-chan os.Signal
}

// Handle is returned by Install. The caller should `defer h.Stop()`
// at the top of the command so the signal goroutine is torn down on
// the non-signal exit path.
type Handle struct {
	ctx        context.Context
	cancel     context.CancelFunc
	stopSignal func()
	stopCh     chan struct{}
	stopOnce   sync.Once
	firstSeen  atomic.Bool
	firedOnce  sync.Once
	done       chan struct{}
}

// Context returns the cancellable context that will be Done() as soon
// as the first signal is observed. Long-running commands use this as
// their top-level context and pass it into every blocking call.
func (h *Handle) Context() context.Context { return h.ctx }

// ShutdownRequested reports whether the first signal has arrived. The
// drain path uses this to choose between "commit the current tx group"
// (first signal received) and "continue normally" (not yet received).
func (h *Handle) ShutdownRequested() bool { return h.firstSeen.Load() }

// Stop disconnects from the signal source and releases the goroutine.
// It is safe to call multiple times. Stop does NOT cancel the context
// on its own; callers that want to cancel a still-running command on
// the non-signal exit path should call cancel() from the context they
// derived from Handle.Context().
func (h *Handle) Stop() {
	h.stopOnce.Do(func() {
		h.stopSignal()
		close(h.stopCh)
	})
	<-h.done
	h.cancel()
}

// Install wires SIGINT+SIGTERM handlers over the parent context and
// returns a Handle the caller can use to observe the shutdown state.
// It is safe to call exactly once per command invocation; calling it
// twice in the same process concurrently will deliver signals to both
// handles (Go's signal package fans out notifications) but no
// soundness property is violated.
func Install(parent context.Context, opts Options) *Handle {
	signals := opts.Signals
	if len(signals) == 0 {
		signals = []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	}
	grace := opts.GraceWindow
	if grace <= 0 {
		grace = DefaultGraceWindow
	}

	ctx, cancel := context.WithCancel(parent)

	var sigCh <-chan os.Signal
	var stopSignal func()
	if opts.signalSource != nil {
		sigCh = opts.signalSource
		stopSignal = func() {}
	} else {
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, signals...)
		sigCh = ch
		stopSignal = func() { signal.Stop(ch) }
	}

	h := &Handle{
		ctx:        ctx,
		cancel:     cancel,
		stopSignal: stopSignal,
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
	}

	go func() {
		defer close(h.done)
		var firstAt time.Time
		for {
			select {
			case <-h.stopCh:
				// Stop() was called — tear down cleanly. We use a
				// dedicated stop channel rather than ctx.Done()
				// because the first-signal branch below also cancels
				// ctx, and we need to stay alive after that to see
				// a possible second signal within the grace window.
				return
			case <-parent.Done():
				// Parent cancellation is a hard teardown from the
				// caller side. We do not drain further signals.
				return
			case sig, ok := <-sigCh:
				if !ok {
					return
				}
				if !h.firstSeen.Swap(true) {
					firstAt = time.Now()
					h.firedOnce.Do(func() {
						if opts.Logger != nil {
							opts.Logger.LogShutdownRequested(sig.String())
						}
						cancel()
					})
					continue
				}
				// Second signal. Honor it only if it arrives inside
				// the grace window — a signal that comes much later
				// (e.g., an hour after the first) is almost certainly
				// a new user action, not a "please hurry up", and we
				// treat it the same way (hard exit) because the
				// drain should have completed long before.
				if time.Since(firstAt) <= grace {
					exit := opts.HardExit
					if exit == nil {
						panic("shutdown: second signal received but HardExit is nil")
					}
					exit(ExitCodeGraceful)
					return
				}
				// Outside grace window — treat as a fresh first signal.
				// This matches user intuition: a second Ctrl+C an hour
				// later is not a panic button, it's a re-request.
				firstAt = time.Now()
			}
		}
	}()

	return h
}
