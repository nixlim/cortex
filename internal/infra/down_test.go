package infra

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nixlim/cortex/internal/errs"
)

// downFakeDocker is a DockerRunner that records ComposeDown calls.
// It lives alongside fakeDocker (up_test.go) because the cortex down
// tests need richer assertions — specifically the removeVolumes flag
// actually passed to ComposeDown and the compose-file argument — that
// the up fakes do not expose.
type downFakeDocker struct {
	composeDownErr  error
	composeDownArgs struct {
		file   string
		purge  bool
		called int32
	}
}

func (f *downFakeDocker) Ping(context.Context) error                   { return nil }
func (f *downFakeDocker) ComposeUp(context.Context, string, map[string]string) error {
	return nil
}
func (f *downFakeDocker) ImageExists(context.Context, string) (bool, error) {
	return true, nil
}
func (f *downFakeDocker) Build(context.Context, string, string, string) error { return nil }

func (f *downFakeDocker) ComposeDown(_ context.Context, file string, removeVolumes bool) error {
	atomic.AddInt32(&f.composeDownArgs.called, 1)
	f.composeDownArgs.file = file
	f.composeDownArgs.purge = removeVolumes
	return f.composeDownErr
}

// newDownOpts builds a DownOptions pre-wired with a fresh fake docker.
// Each test composes its own Purge / Confirm on top.
func newDownOpts() (*downFakeDocker, DownOptions) {
	d := &downFakeDocker{}
	return d, DownOptions{
		ComposeFile: "docker/docker-compose.yaml",
		Docker:      d,
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex down stops both containers and leaves
// cortex_weaviate_data and cortex_neo4j_data volumes present."
// ---------------------------------------------------------------------------

func TestDownDefaultPreservesVolumes(t *testing.T) {
	d, opts := newDownOpts()

	if err := Down(context.Background(), opts); err != nil {
		t.Fatalf("Down returned %v, want nil", err)
	}
	if d.composeDownArgs.called != 1 {
		t.Errorf("ComposeDown called %d times, want 1", d.composeDownArgs.called)
	}
	if d.composeDownArgs.purge {
		t.Errorf("ComposeDown purge = true, want false (default cortex down must preserve volumes)")
	}
	if d.composeDownArgs.file != "docker/docker-compose.yaml" {
		t.Errorf("ComposeDown file = %q, want docker/docker-compose.yaml", d.composeDownArgs.file)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: "cortex down --purge prompts for confirmation and on yes
// removes both volumes."
// ---------------------------------------------------------------------------

func TestDownPurgeYesRemovesVolumes(t *testing.T) {
	d, opts := newDownOpts()
	opts.Purge = true

	var promptSeen string
	opts.Confirm = func(prompt string) (bool, error) {
		promptSeen = prompt
		return true, nil
	}

	if err := Down(context.Background(), opts); err != nil {
		t.Fatalf("Down returned %v, want nil", err)
	}
	if !d.composeDownArgs.purge {
		t.Errorf("ComposeDown purge = false, want true on --purge with yes")
	}
	if !strings.Contains(promptSeen, "cortex_weaviate_data") ||
		!strings.Contains(promptSeen, "cortex_neo4j_data") {
		t.Errorf("purge prompt does not name the target volumes: %q", promptSeen)
	}
	// Guard against accidental log.d mention: the prompt should
	// explicitly state the log is NOT affected.
	if !strings.Contains(promptSeen, "NOT affected") {
		t.Errorf("purge prompt missing the 'log NOT affected' reassurance: %q", promptSeen)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: operator can decline --purge and no destructive call is made.
// ---------------------------------------------------------------------------

func TestDownPurgeNoIsCancelled(t *testing.T) {
	d, opts := newDownOpts()
	opts.Purge = true
	opts.Confirm = func(string) (bool, error) { return false, nil }

	err := Down(context.Background(), opts)
	requireErrorCode(t, err, CodePurgeCancelled)
	if d.composeDownArgs.called != 0 {
		t.Errorf("ComposeDown called %d times after cancellation, want 0",
			d.composeDownArgs.called)
	}
}

func TestDownPurgeConfirmErrorIsCancelled(t *testing.T) {
	d, opts := newDownOpts()
	opts.Purge = true
	opts.Confirm = func(string) (bool, error) { return false, errors.New("stdin closed") }

	err := Down(context.Background(), opts)
	requireErrorCode(t, err, CodePurgeCancelled)
	if d.composeDownArgs.called != 0 {
		t.Errorf("ComposeDown called %d times after confirm error, want 0",
			d.composeDownArgs.called)
	}
}

// ---------------------------------------------------------------------------
// Acceptance: a purge without an interactive callback is refused rather
// than silently proceeding. Defense in depth: the CLI always supplies a
// real callback, but Down must not trust that.
// ---------------------------------------------------------------------------

func TestDownPurgeWithoutConfirmIsMisconfigured(t *testing.T) {
	d, opts := newDownOpts()
	opts.Purge = true
	opts.Confirm = nil

	err := Down(context.Background(), opts)
	requireErrorCode(t, err, CodeDownMisconfigured)
	if d.composeDownArgs.called != 0 {
		t.Errorf("ComposeDown called %d times without confirm, want 0",
			d.composeDownArgs.called)
	}
}

// ---------------------------------------------------------------------------
// Docker compose-down failure is surfaced as COMPOSE_DOWN_FAILED with
// the cause chained.
// ---------------------------------------------------------------------------

func TestDownComposeFailurePropagates(t *testing.T) {
	d, opts := newDownOpts()
	d.composeDownErr = errors.New("compose: network busy")

	err := Down(context.Background(), opts)
	requireErrorCode(t, err, CodeComposeDownFailed)
	var e *errs.Error
	if !errors.As(err, &e) || e.Cause == nil {
		t.Errorf("expected chained cause, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation: missing docker adapter or compose file are misconfiguration.
// ---------------------------------------------------------------------------

func TestDownMissingAdapterIsMisconfigured(t *testing.T) {
	err := Down(context.Background(), DownOptions{ComposeFile: "x.yaml"})
	requireErrorCode(t, err, CodeDownMisconfigured)
}

func TestDownMissingComposeFileIsMisconfigured(t *testing.T) {
	err := Down(context.Background(), DownOptions{Docker: &downFakeDocker{}})
	requireErrorCode(t, err, CodeDownMisconfigured)
}
