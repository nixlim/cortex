// Package claudecli wraps one-shot headless invocations of the
// `claude` CLI for cortex's continuous categorised-summarisation pass
// (bead cortex-8sr).
//
// The summariser's job — curating a Leiden community into a brief an
// agent can act on — is materially more than text-in/text-out
// paraphrase: it needs to deduplicate, reconcile conflicts, name
// concepts, and promote a canonical exemplar. Claude Code CLI is
// invoked in reasoning-only mode (plan permission, tools disabled,
// --max-turns 1) because the only thing it needs to do is read the
// provided observations and return a strict-JSON CommunityBrief.
//
// This package is intentionally stateless and one-shot: retry,
// backoff, and per-community error isolation live in the summariser
// above. The Runner interface exists so tests can substitute a fake
// without running a subprocess.
package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Typed errors surface CLI failure modes so the caller can apply
// appropriate retry policy. ExecRunner classifies a subprocess
// failure into one of these based on exit code + stderr patterns
// following the adversarial-spec-system recovery.go pattern.
var (
	// ErrContextOverflow indicates the prompt + input exceeded the
	// model's context window. Retry with reduced input (e.g. drop
	// the lowest-activation entries from the cluster).
	ErrContextOverflow = errors.New("claudecli: context overflow")
	// ErrRateLimited indicates the provider rate-limited the call.
	// Retry with exponential backoff.
	ErrRateLimited = errors.New("claudecli: rate limited")
	// ErrInvalidJSON indicates the CLI returned successfully but the
	// output did not parse as JSON matching the requested schema.
	ErrInvalidJSON = errors.New("claudecli: invalid json output")
	// ErrTimeout indicates the subprocess exceeded its deadline.
	ErrTimeout = errors.New("claudecli: subprocess timeout")
	// ErrCLICrash indicates the subprocess exited non-zero for a
	// reason not otherwise classified. Look at Response.Stderr for
	// diagnosis.
	ErrCLICrash = errors.New("claudecli: cli crashed")
)

// Request is a one-shot headless call to `claude`.
type Request struct {
	// Prompt is passed to the CLI via -p. The entire structured
	// input (the cluster's observations) is embedded inline in the
	// prompt; stdin is not used. Keep well under the model's context
	// window — a 5 KB cluster is fine, a 500 KB cluster will
	// ErrContextOverflow.
	Prompt string

	// SchemaJSON, if non-empty, is passed to the CLI via
	// --json-schema. When set, Response.StructuredOutput is
	// populated with schema-validated JSON; when empty, the caller
	// must parse Response.Result itself.
	SchemaJSON string

	// Model, if non-empty, is passed via --model. Empty falls back
	// to the CLI's configured default (ANTHROPIC_DEFAULT_MODEL env
	// or Claude Code's built-in default).
	Model string

	// Timeout bounds the subprocess wall-clock time. Zero means no
	// timeout beyond the parent context's deadline.
	Timeout time.Duration
}

// Response is the structured result of a successful CLI invocation.
// Any field may be zero if the CLI did not populate it (older
// versions, non-JSON output formats, etc.).
type Response struct {
	// StructuredOutput carries the --json-schema-validated JSON when
	// Request.SchemaJSON was non-empty. Otherwise it is nil and the
	// caller should parse Result instead.
	StructuredOutput json.RawMessage

	// Result is the raw text output the model produced. Populated
	// regardless of whether a schema was supplied.
	Result string

	// SessionID is the Claude Code session ID the CLI emits, useful
	// for correlating telemetry across calls even though we spawn
	// fresh sessions per request.
	SessionID string

	// CostUSD is the reported total cost of the call, in USD. Zero
	// if the CLI did not report it.
	CostUSD float64

	// InputTokens / OutputTokens are harvested from the CLI's usage
	// block when present. Zero means "not reported," not "none used."
	InputTokens  int64
	OutputTokens int64

	// DurationMS is the wall-clock elapsed time of the subprocess as
	// measured by this wrapper.
	DurationMS int64

	// Stderr is the full captured stderr, retained for diagnosis on
	// non-error runs (some CLI warnings still appear on stderr) and
	// for error classification on failure.
	Stderr string
}

// Runner is the minimal interface the summariser depends on.
// ExecRunner is the production implementation; tests substitute a
// fake to avoid spawning subprocesses.
type Runner interface {
	Run(ctx context.Context, req Request) (Response, error)
}
