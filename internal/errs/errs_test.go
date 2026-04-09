package errs

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestValidationJSONEnvelope(t *testing.T) {
	var buf bytes.Buffer
	err := Validation("EMPTY_BODY", "body must not be empty", map[string]any{"field": "body"})
	code := Emit(&buf, err, true)
	if code != 2 {
		t.Errorf("exit code: got %d want 2", code)
	}
	var env Envelope
	if e := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env); e != nil {
		t.Fatalf("output is not a JSON envelope: %v\nraw=%s", e, buf.String())
	}
	if env.Error.Code != "EMPTY_BODY" {
		t.Errorf("code: %q", env.Error.Code)
	}
	if env.Error.Message != "body must not be empty" {
		t.Errorf("message: %q", env.Error.Message)
	}
	if env.Error.Details["field"] != "body" {
		t.Errorf("details missing 'field'")
	}
}

func TestOperationalDoesNotLeakCauseToStderr(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:7687: panic: runtime.goroutine 1 at /Users/alice/secret")
	err := Operational("NEO4J_UNREACHABLE", "Neo4j is not reachable", cause)
	var buf bytes.Buffer
	code := Emit(&buf, err, false)
	if code != 1 {
		t.Errorf("exit code: got %d want 1", code)
	}
	out := buf.String()
	if strings.Contains(out, "dial tcp") {
		t.Errorf("cause leaked into stderr: %q", out)
	}
	if strings.Contains(out, "/Users/alice") {
		t.Errorf("home path leaked into stderr: %q", out)
	}
}

func TestEnvelopeForbiddenSubstrings(t *testing.T) {
	// Deliberately craft a message with forbidden patterns to verify the
	// scrubber strips them.
	msg := "failed at /Users/bob/project — panic: runtime.goroutine 42 blew up"
	err := Validation("WEIRD", msg, map[string]any{"where": "/Users/bob/project"})
	var buf bytes.Buffer
	Emit(&buf, err, true)
	out := buf.String()
	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`/Users/[A-Za-z]+/`),
		regexp.MustCompile(`panic:`),
		regexp.MustCompile(`runtime\.`),
	}
	for _, re := range forbidden {
		if re.MatchString(out) {
			t.Errorf("envelope contains forbidden pattern %q: %s", re, out)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	if KindSuccess.ExitCode() != 0 {
		t.Error("success != 0")
	}
	if KindOperational.ExitCode() != 1 {
		t.Error("operational != 1")
	}
	if KindValidation.ExitCode() != 2 {
		t.Error("validation != 2")
	}
}

func TestUnknownErrorIsOperational(t *testing.T) {
	var buf bytes.Buffer
	code := Emit(&buf, errors.New("plain error"), true)
	if code != 1 {
		t.Errorf("unknown err exit code: %d want 1", code)
	}
}
