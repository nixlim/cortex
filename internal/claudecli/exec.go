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

	// --max-turns 2 lets the assistant close cleanly after a
	// StructuredOutput tool_use: turn 1 emits the tool_use; the
	// runtime injects "Structured output provided successfully" as a
	// tool_result; turn 2 lets the assistant finish with
	// stop_reason=end_turn. With --max-turns 1, even successful
	// structured-output calls surface as is_error=true with
	// subtype=error_max_turns because the assistant was cut off before
	// it could stop cleanly.
	args := []string{
		"-p", req.Prompt,
		"--output-format", "json",
		"--permission-mode", "plan",
		"--tools", "",
		"--max-turns", "2",
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

// cliEnvelope is the normalised result of parsing whatever shape the
// claude CLI emitted on stdout. Cortex coalesces two on-the-wire
// formats into this one struct:
//
//   - Legacy single-object envelope:
//     {"result":"...","structured_output":{...},"is_error":false,...}
//   - Current array-of-events (Claude Code ≥2.x):
//     [{"type":"system",...},{"type":"assistant",...},{"type":"result",...}]
//
// Only the fields cortex harvests are decoded; unknown ones are
// ignored so a CLI version bump does not break us.
type cliEnvelope struct {
	Result           string
	StructuredOutput json.RawMessage
	SessionID        string
	CostUSD          float64
	IsError          bool
	Subtype          string // e.g. "error_max_turns"; only meaningful on array format
	Usage            struct {
		InputTokens  int64
		OutputTokens int64
	}
}

// parseJSONEnvelope decodes the stdout JSON envelope. Auto-detects
// array-of-events vs single-object format so the wrapper works across
// claude CLI versions without a configuration knob.
func parseJSONEnvelope(stdout []byte) (cliEnvelope, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return cliEnvelope{}, fmt.Errorf("empty stdout")
	}
	if trimmed[0] == '[' {
		return parseEventStream(trimmed)
	}
	return parseLegacyEnvelope(trimmed)
}

// legacyEnvelope is the claude ≤1.x single-object shape.
type legacyEnvelope struct {
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	SessionID        string          `json:"session_id,omitempty"`
	CostUSD          float64         `json:"cost,omitempty"`
	CostUSDLegacy    float64         `json:"cost_usd,omitempty"` // older CLI versions
	IsError          bool            `json:"is_error,omitempty"`
	Usage            struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

func parseLegacyEnvelope(stdout []byte) (cliEnvelope, error) {
	var legacy legacyEnvelope
	if err := json.Unmarshal(stdout, &legacy); err != nil {
		return cliEnvelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	if legacy.CostUSD == 0 && legacy.CostUSDLegacy != 0 {
		legacy.CostUSD = legacy.CostUSDLegacy
	}
	env := cliEnvelope{
		Result:           legacy.Result,
		StructuredOutput: legacy.StructuredOutput,
		SessionID:        legacy.SessionID,
		CostUSD:          legacy.CostUSD,
		IsError:          legacy.IsError,
	}
	env.Usage.InputTokens = legacy.Usage.InputTokens
	env.Usage.OutputTokens = legacy.Usage.OutputTokens
	return env, nil
}

// eventEnvelope is one entry in the array the current claude CLI
// emits with --output-format json. Only the fields we consume are
// typed; everything else rides in json.RawMessage so we can pull it
// out event-by-event.
type eventEnvelope struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`

	// Fields populated only on {"type":"result"} entries.
	SessionID    string  `json:"session_id,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Result       string  `json:"result,omitempty"`
	Usage        struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage,omitempty"`
}

// parseEventStream walks the array-of-events format, harvesting the
// structured output from assistant tool_use blocks named
// "StructuredOutput" and the metadata from the terminal result event.
// A successful structured-output call that ends with
// subtype=error_max_turns is treated as success: the structured
// output was delivered before the turn limit bit.
func parseEventStream(stdout []byte) (cliEnvelope, error) {
	var events []eventEnvelope
	if err := json.Unmarshal(stdout, &events); err != nil {
		return cliEnvelope{}, fmt.Errorf("decode event stream: %w", err)
	}
	var env cliEnvelope
	for _, ev := range events {
		switch ev.Type {
		case "assistant":
			if so, ok := extractStructuredOutput(ev.Message); ok && env.StructuredOutput == nil {
				env.StructuredOutput = so
			}
		case "result":
			env.SessionID = ev.SessionID
			env.Result = ev.Result
			env.IsError = ev.IsError
			env.Subtype = ev.Subtype
			env.CostUSD = ev.TotalCostUSD
			if env.CostUSD == 0 && ev.CostUSD != 0 {
				env.CostUSD = ev.CostUSD
			}
			env.Usage.InputTokens = ev.Usage.InputTokens
			env.Usage.OutputTokens = ev.Usage.OutputTokens
		}
	}
	// A max-turns termination AFTER the model has already emitted the
	// StructuredOutput tool_use is functionally a success — we have
	// the schema-validated JSON we asked for.
	if env.IsError && env.Subtype == "error_max_turns" && env.StructuredOutput != nil {
		env.IsError = false
	}
	return env, nil
}

// extractStructuredOutput pulls the first StructuredOutput tool_use
// input from an assistant message, JSON-encoded. Returns (nil, false)
// if the message does not carry one.
func extractStructuredOutput(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, false
	}
	for _, blk := range msg.Content {
		if blk.Type == "tool_use" && blk.Name == "StructuredOutput" && len(blk.Input) > 0 {
			return blk.Input, true
		}
	}
	return nil, false
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
