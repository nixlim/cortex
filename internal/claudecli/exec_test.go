package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubScript writes an executable shell script to a temp dir that
// emulates the `claude -p` invocation. The script:
//   - writes whatever `stdout` argument was given (literal bytes) to stdout
//   - writes `stderr` to stderr
//   - exits with `code`
//
// Tests pass a path to this script via ExecRunner.Command so the
// runner goes through the real exec.CommandContext path without
// depending on a real `claude` binary.
func stubScript(t *testing.T, stdout, stderr string, code int) string {
	t.Helper()
	dir := t.TempDir()
	// The stub is intentionally argv-agnostic: we don't verify the
	// exact flag set the runner chose (that's the wrapper's business),
	// only that it produces a process, captures stdout/stderr, and
	// handles the exit code and parse result.
	body := "#!/bin/sh\n" +
		"cat <<'EOF'\n" + stdout + "\nEOF\n" +
		"echo '" + strings.ReplaceAll(stderr, "'", "'\\''") + "' 1>&2\n" +
		"exit " + itoa(code) + "\n"
	path := filepath.Join(dir, "claude")
	if err := writeExecutable(t, path, body); err != nil {
		t.Fatalf("stub: %v", err)
	}
	return path
}

func TestExecRunner_SuccessEnvelope(t *testing.T) {
	stdout := `{"result":"ok","structured_output":{"foo":"bar"},"session_id":"sess-1","cost":0.001234,"usage":{"input_tokens":42,"output_tokens":7}}`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	resp, err := r.Run(context.Background(), Request{Prompt: "p", SchemaJSON: `{"type":"object"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SessionID != "sess-1" {
		t.Errorf("session_id: got %q want %q", resp.SessionID, "sess-1")
	}
	if resp.CostUSD != 0.001234 {
		t.Errorf("cost: got %v want 0.001234", resp.CostUSD)
	}
	if resp.InputTokens != 42 || resp.OutputTokens != 7 {
		t.Errorf("tokens: got in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
	if resp.StructuredOutput == nil {
		t.Fatal("structured_output missing")
	}
	var so map[string]string
	if err := json.Unmarshal(resp.StructuredOutput, &so); err != nil {
		t.Fatalf("structured_output not json: %v", err)
	}
	if so["foo"] != "bar" {
		t.Errorf("structured_output contents wrong: %v", so)
	}
}

func TestExecRunner_LegacyCostField(t *testing.T) {
	stdout := `{"result":"ok","cost_usd":0.05}`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	resp, err := r.Run(context.Background(), Request{Prompt: "p"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.CostUSD != 0.05 {
		t.Errorf("legacy cost_usd should populate CostUSD: got %v", resp.CostUSD)
	}
}

func TestExecRunner_InvalidJSON(t *testing.T) {
	r := &ExecRunner{Command: stubScript(t, "not json at all", "", 0)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("expected ErrInvalidJSON, got %v", err)
	}
}

func TestExecRunner_ContextOverflowClassified(t *testing.T) {
	r := &ExecRunner{Command: stubScript(t, "", "Error: prompt exceeded maximum context window for this model", 1)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("expected ErrContextOverflow, got %v", err)
	}
}

func TestExecRunner_RateLimitClassified(t *testing.T) {
	r := &ExecRunner{Command: stubScript(t, "", "HTTP 429: Too Many Requests", 1)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
}

func TestExecRunner_UnclassifiedCrash(t *testing.T) {
	r := &ExecRunner{Command: stubScript(t, "", "something else went wrong", 2)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrCLICrash) {
		t.Fatalf("expected ErrCLICrash, got %v", err)
	}
}

func TestExecRunner_Timeout(t *testing.T) {
	// A script that sleeps longer than the request timeout. We use
	// the newCmd seam here to run /bin/sh with a deliberate sleep
	// rather than the stub (which exits immediately).
	r := &ExecRunner{
		newCmd: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 2")
		},
	}
	_, err := r.Run(context.Background(), Request{
		Prompt:  "p",
		Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestExecRunner_IsErrorEnvelope(t *testing.T) {
	stdout := `{"result":"model refused","is_error":true}`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrCLICrash) {
		t.Fatalf("is_error=true should surface as ErrCLICrash, got %v", err)
	}
}

// TestExecRunner_EventStreamArray exercises the current claude CLI
// output format: a JSON array of events. Structured output lives on
// an assistant tool_use block; metadata lives on a terminal result
// event. This is the shape cortex-8sr's summariser actually depends
// on at runtime — the legacy single-object envelope is a historical
// fallback.
func TestExecRunner_EventStreamArray(t *testing.T) {
	stdout := `[
{"type":"system","subtype":"init","session_id":"sess-42"},
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"StructuredOutput","input":{"label":"hi","ok":true}}]}},
{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}},
{"type":"result","subtype":"success","is_error":false,"session_id":"sess-42","total_cost_usd":0.012,"usage":{"input_tokens":123,"output_tokens":4}}
]`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	resp, err := r.Run(context.Background(), Request{Prompt: "p", SchemaJSON: `{"type":"object"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.SessionID != "sess-42" {
		t.Errorf("session_id: got %q", resp.SessionID)
	}
	if resp.CostUSD != 0.012 {
		t.Errorf("cost: got %v want 0.012", resp.CostUSD)
	}
	if resp.InputTokens != 123 || resp.OutputTokens != 4 {
		t.Errorf("tokens: in=%d out=%d", resp.InputTokens, resp.OutputTokens)
	}
	var so map[string]any
	if err := json.Unmarshal(resp.StructuredOutput, &so); err != nil {
		t.Fatalf("structured_output not parseable: %v", err)
	}
	if so["label"] != "hi" || so["ok"] != true {
		t.Errorf("structured_output contents: got %v", so)
	}
}

// TestExecRunner_EventStreamMaxTurnsSuccess: when the CLI reports
// is_error=true with subtype=error_max_turns, but a StructuredOutput
// tool_use already emitted the schema-validated JSON, the wrapper
// MUST surface success — the payload we asked for was delivered,
// the assistant just didn't get another turn to say "done."
func TestExecRunner_EventStreamMaxTurnsSuccess(t *testing.T) {
	stdout := `[
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"StructuredOutput","input":{"label":"ok"}}]}},
{"type":"user","message":{"content":[{"type":"tool_result","content":"Structured output provided successfully"}]}},
{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s","total_cost_usd":0.001}
]`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	resp, err := r.Run(context.Background(), Request{Prompt: "p", SchemaJSON: `{"type":"object"}`})
	if err != nil {
		t.Fatalf("max_turns WITH structured output must be success, got %v", err)
	}
	if resp.StructuredOutput == nil {
		t.Fatal("structured_output should have been captured from the tool_use block")
	}
}

// TestExecRunner_EventStreamGenuineError: is_error=true on the result
// event WITHOUT an accompanying structured output is still a crash.
func TestExecRunner_EventStreamGenuineError(t *testing.T) {
	stdout := `[{"type":"result","subtype":"error_during_execution","is_error":true,"result":"model refused"}]`
	r := &ExecRunner{Command: stubScript(t, stdout, "", 0)}
	_, err := r.Run(context.Background(), Request{Prompt: "p"})
	if !errors.Is(err, ErrCLICrash) {
		t.Fatalf("expected ErrCLICrash, got %v", err)
	}
}

// helpers

func itoa(n int) string {
	// avoid pulling strconv just for tests
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

func writeExecutable(t *testing.T, path, body string) error {
	t.Helper()
	return writeFile(path, body, 0o755)
}
