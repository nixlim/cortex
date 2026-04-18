package claudecli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ExecRunner is the production Runner that invokes the `claude`
// binary as a subprocess. It is safe for concurrent use from multiple
// goroutines — each Run call spawns a fresh subprocess with an
// independent exec.Cmd.
type ExecRunner struct {
	// Command is the `claude` binary name or absolute path. Empty
	// defaults to "claude" (resolved via PATH).
	Command string

	// ExtraArgs are appended after the built-in flag set. Useful for
	// pinning MCP config files or passing per-environment flags the
	// caller wants every invocation to carry.
	ExtraArgs []string

	// newCmd is a seam for tests. When non-nil, it is called in
	// place of exec.CommandContext. Production callers leave this
	// nil.
	newCmd func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// Run executes the CLI one-shot with the fixed reasoning-only flag
// set described in the package doc:
//
//	claude -p <prompt> --output-format json --permission-mode plan
//	       --tools "" --max-turns 1 [--model ...] [--json-schema ...]
//
// It returns a Response populated from the CLI's JSON stdout, or a
// typed error (see the Err* variables) plus Response.Stderr for
// diagnosis.
func (r *ExecRunner) Run(ctx context.Context, req Request) (Response, error) {
	cmdName := r.Command
	if cmdName == "" {
		cmdName = "claude"
	}

	args := []string{
		"-p", req.Prompt,
		"--output-format", "json",
		"--permission-mode", "plan",
		"--tools", "",
		"--max-turns", "1",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.SchemaJSON != "" {
		args = append(args, "--json-schema", req.SchemaJSON)
	}
	args = append(args, r.ExtraArgs...)

	// Apply the per-request timeout as a bounded context deriving
	// from the parent; the parent's cancel still propagates.
	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	newCmd := r.newCmd
	if newCmd == nil {
		newCmd = exec.CommandContext
	}
	cmd := newCmd(runCtx, cmdName, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	resp := Response{
		Stderr:     stderr.String(),
		DurationMS: elapsed.Milliseconds(),
	}

	if runErr != nil {
		// Deadline/cancellation take precedence over exit-code
		// interpretation so callers see ErrTimeout rather than
		// ErrCLICrash for a killed subprocess.
		if runCtx.Err() == context.DeadlineExceeded {
			return resp, fmt.Errorf("%w: elapsed=%v", ErrTimeout, elapsed)
		}
		if err := classifyStderr(resp.Stderr); err != nil {
			return resp, err
		}
		return resp, fmt.Errorf("%w: %v: %s", ErrCLICrash, runErr, truncate(resp.Stderr, 512))
	}

	parsed, err := parseJSONEnvelope(stdout.Bytes())
	if err != nil {
		return resp, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	resp.StructuredOutput = parsed.StructuredOutput
	resp.Result = parsed.Result
	resp.SessionID = parsed.SessionID
	resp.CostUSD = parsed.CostUSD
	resp.InputTokens = parsed.Usage.InputTokens
	resp.OutputTokens = parsed.Usage.OutputTokens

	if parsed.IsError {
		// The CLI exited 0 but its envelope reports an error — this
		// can happen for model-side refusals. Surface as a crash so
		// the caller treats it uniformly with non-zero exits.
		return resp, fmt.Errorf("%w: cli reported is_error=true: %s", ErrCLICrash, truncate(parsed.Result, 256))
	}

	return resp, nil
}

// cliEnvelope mirrors the shape of `claude -p --output-format json`
// output. Only the fields cortex harvests are decoded; unknown fields
// are ignored so a CLI version bump does not break us.
type cliEnvelope struct {
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	SessionID       string          `json:"session_id,omitempty"`
	CostUSD         float64         `json:"cost,omitempty"`
	CostUSDLegacy   float64         `json:"cost_usd,omitempty"` // older CLI versions
	IsError         bool            `json:"is_error,omitempty"`
	Usage           struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// parseJSONEnvelope decodes the stdout JSON envelope. It coalesces
// the current `cost` and legacy `cost_usd` fields so we survive the
// field rename observed between Claude Code CLI versions.
func parseJSONEnvelope(stdout []byte) (cliEnvelope, error) {
	var env cliEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return env, fmt.Errorf("decode envelope: %w", err)
	}
	if env.CostUSD == 0 && env.CostUSDLegacy != 0 {
		env.CostUSD = env.CostUSDLegacy
	}
	return env, nil
}

// classifyStderr inspects the subprocess's stderr for patterns that
// indicate a retryable error. It returns a typed Err* sentinel or
// nil if no known pattern matched (caller falls back to ErrCLICrash).
func classifyStderr(stderr string) error {
	lo := strings.ToLower(stderr)
	switch {
	case strings.Contains(lo, "context length"),
		strings.Contains(lo, "context window"),
		strings.Contains(lo, "token limit"),
		strings.Contains(lo, "maximum context"):
		return fmt.Errorf("%w: %s", ErrContextOverflow, truncate(stderr, 256))
	case strings.Contains(lo, "rate limit"),
		strings.Contains(lo, "429"),
		strings.Contains(lo, "too many requests"):
		return fmt.Errorf("%w: %s", ErrRateLimited, truncate(stderr, 256))
	}
	return nil
}

// truncate bounds diagnostic strings so a 10 MB stderr dump doesn't
// land in our logs verbatim.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
