// Package errs implements the Cortex standard error envelope and exit
// codes.
//
// Per the spec (FR-036 and the Error flows section):
//
//	exit 0 → success
//	exit 1 → operational failure (runtime, dependency, timeout, not-found)
//	exit 2 → validation / usage failure
//
// When `--json` is requested, a validation error is emitted as
//
//	{"error":{"code":"<CODE>","message":"<message>","details":{...}}}
//
// to stderr. Operational errors never include raw backend messages, stack
// traces, or host file paths in stderr or the JSON envelope — those details
// live only in ops.log. The package is the single sanctioned write path
// for stderr error output to make that invariant enforceable.
package errs

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
)

// Kind classifies a Cortex error for exit-code mapping.
type Kind int

const (
	// KindSuccess is the zero value and should not appear in actual errors.
	KindSuccess Kind = iota
	// KindOperational maps to exit code 1.
	KindOperational
	// KindValidation maps to exit code 2.
	KindValidation
)

// Exit codes, fixed by the spec.
const (
	ExitSuccess     = 0
	ExitOperational = 1
	ExitValidation  = 2
)

// ExitCode returns the process exit code for a Kind.
func (k Kind) ExitCode() int {
	switch k {
	case KindOperational:
		return ExitOperational
	case KindValidation:
		return ExitValidation
	default:
		return ExitSuccess
	}
}

// Error is a typed Cortex error that knows how to emit an envelope and
// what exit code it maps to.
type Error struct {
	Kind    Kind
	Code    string         // short UPPER_SNAKE_CASE identifier
	Message string         // human-readable one-liner, safe to show
	Details map[string]any // optional structured context (pre-sanitized)
	// Cause is the raw underlying error. It is never written to stderr or
	// the JSON envelope; callers route it to ops.log only.
	Cause error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Unwrap returns the underlying cause for errors.Is/errors.As.
func (e *Error) Unwrap() error { return e.Cause }

// Validation builds a validation-kind error.
func Validation(code, msg string, details map[string]any) *Error {
	return &Error{Kind: KindValidation, Code: code, Message: msg, Details: details}
}

// Operational builds an operational-kind error.
func Operational(code, msg string, cause error) *Error {
	return &Error{Kind: KindOperational, Code: code, Message: msg, Cause: cause}
}

// Envelope is the wire shape of a JSON error envelope.
type Envelope struct {
	Error envelopeBody `json:"error"`
}

type envelopeBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// forbiddenPatterns catches information disclosure that must never leak
// into stderr or the JSON envelope. These patterns are checked at write
// time and a hit causes the offending substring to be replaced with
// "[redacted]" before emission.
var forbiddenPatterns = []*regexp.Regexp{
	regexp.MustCompile(`/Users/[A-Za-z0-9._-]+`),
	regexp.MustCompile(`panic:`),
	regexp.MustCompile(`runtime\.`),
	regexp.MustCompile(`goroutine \d+`),
}

func scrub(s string) string {
	for _, re := range forbiddenPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// Emit writes the error to stderr.
//
// If jsonMode is true and the error is a validation failure, a JSON
// envelope is written; otherwise a human-readable single line is written.
// Operational errors are deliberately terse on stderr (code + message only)
// because their raw details belong in ops.log. The returned int is the
// exit code the caller should use.
func Emit(stderr io.Writer, err error, jsonMode bool) int {
	e, ok := err.(*Error)
	if !ok {
		// Unknown errors are treated as operational to fail safely.
		e = &Error{Kind: KindOperational, Code: "INTERNAL", Message: scrub(err.Error())}
	}

	switch e.Kind {
	case KindValidation:
		if jsonMode {
			env := Envelope{envelopeBody{
				Code:    e.Code,
				Message: scrub(e.Message),
				Details: scrubDetails(e.Details),
			}}
			b, _ := json.Marshal(env)
			fmt.Fprintln(stderr, string(b))
		} else {
			fmt.Fprintf(stderr, "cortex: %s: %s\n", e.Code, scrub(e.Message))
		}
	default:
		// Operational. Never include Cause on stderr.
		if jsonMode {
			env := Envelope{envelopeBody{
				Code:    e.Code,
				Message: scrub(e.Message),
			}}
			b, _ := json.Marshal(env)
			fmt.Fprintln(stderr, string(b))
		} else {
			fmt.Fprintf(stderr, "cortex: %s: %s\n", e.Code, scrub(e.Message))
		}
	}
	return e.Kind.ExitCode()
}

func scrubDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			out[k] = scrub(s)
		} else {
			out[k] = v
		}
	}
	return out
}
