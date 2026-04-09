package infra

import (
	"context"

	"github.com/nixlim/cortex/internal/errs"
)

// Stable error codes emitted by Down. Each one maps to a distinct
// failure mode the cortex down acceptance tests assert on.
const (
	CodeComposeDownFailed = "COMPOSE_DOWN_FAILED"
	CodePurgeCancelled    = "PURGE_CANCELLED"
	CodeDownMisconfigured = "DOWN_MISCONFIGURED"
)

// DownOptions wires Down to a concrete environment. Like UpOptions,
// every host-touching dependency is an injection seam so Down can be
// exercised end-to-end without a real docker daemon.
type DownOptions struct {
	// ComposeFile is the docker-compose file Down will `compose down`.
	ComposeFile string

	// Purge, when true, asks Docker to also remove named volumes. This
	// is the only in-band destructive operation on managed-service
	// storage and is gated by Confirm below.
	Purge bool

	// Docker is the injected docker-CLI adapter.
	Docker DockerRunner

	// Confirm is the interactive gate for --purge. When Purge is true,
	// Down calls Confirm with a warning string and only proceeds if the
	// callback returns (true, nil). A (false, nil) return is treated as
	// operator cancellation and mapped to PURGE_CANCELLED. A non-nil
	// error surfaces as the cause of PURGE_CANCELLED. Confirm is never
	// called when Purge is false.
	Confirm func(prompt string) (bool, error)
}

// purgeWarning is the exact prompt Down asks the operator to accept
// before `docker compose down -v` is called. It names the volumes the
// spec lists in §"Volume Topology and Persistence" so the operator is
// given a concrete picture of what will be destroyed.
const purgeWarning = "cortex down --purge will permanently remove named volumes " +
	"cortex_weaviate_data and cortex_neo4j_data. " +
	"The datom log at ~/.cortex/log.d/ is NOT affected. " +
	"Proceed? [y/N]: "

// Down stops the managed Docker stack. When opts.Purge is true, Down
// first calls opts.Confirm to obtain operator consent, then invokes
// `docker compose down -v`. When Purge is false, named volumes are
// left intact so a subsequent `cortex up` resumes against the same
// data (spec §"Volume Topology and Persistence", SC-020).
//
// Down never touches files under ~/.cortex/log.d/: the datom log is
// authoritative and must survive every managed-service operation,
// including --purge. This guarantee is trivially maintained because
// Down has no knowledge of the log directory — it only speaks to
// DockerRunner.ComposeDown.
//
// A nil return indicates ComposeDown succeeded. A non-nil return is
// always an *errs.Error whose Code is one of the stable constants
// above.
func Down(ctx context.Context, opts DownOptions) error {
	if opts.Docker == nil {
		return errs.Validation(CodeDownMisconfigured,
			"cortex down requires a docker adapter", nil)
	}
	if opts.ComposeFile == "" {
		return errs.Validation(CodeDownMisconfigured,
			"cortex down requires a compose file path", nil)
	}

	if opts.Purge {
		if opts.Confirm == nil {
			// Refuse to proceed rather than silently destroy volumes
			// without an operator gate. The CLI wire-up in
			// cmd/cortex/down.go always supplies a real confirm
			// callback, so this branch only fires in tests or
			// mis-configured callers — and failing loudly is the
			// correct behavior there.
			return errs.Validation(CodeDownMisconfigured,
				"cortex down --purge requires an interactive confirmation callback", nil)
		}
		ok, err := opts.Confirm(purgeWarning)
		if err != nil {
			return errs.Operational(CodePurgeCancelled,
				"operator confirmation for --purge could not be read", err)
		}
		if !ok {
			return errs.Operational(CodePurgeCancelled,
				"cortex down --purge cancelled by operator; volumes were not removed", nil)
		}
	}

	if err := opts.Docker.ComposeDown(ctx, opts.ComposeFile, opts.Purge); err != nil {
		return errs.Operational(CodeComposeDownFailed,
			"docker compose down failed", err)
	}
	return nil
}
