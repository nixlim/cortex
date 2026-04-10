// internal/e2e/observe_recall_roundtrip_test.go is the round-3 grill
// recommendation #4: an end-to-end test that runs `cortex observe`
// followed by `cortex recall` against the real binary, and asserts
// the recall surfaces at least one hit. Without this guard, both
// CRIT-008 (Neo4j applier MERGEd on `id` while every reader filtered
// on `entry_id`, returning zero rows) and CRIT-009 (Weaviate applier
// hardcoded a nil vector, leaving cosine rerank inert) shipped
// undetected because the in-process unit tests never invoked the
// observe→recall path through the CLI.
//
// Why a separate file with a stricter build tag.
//
// The base CLI-exec harness in cli_exec_test.go is gated behind
// `//go:build cli` and only requires `go build` plus a writable
// HOME — every test there is hermetic. The observe→recall round-
// trip is fundamentally NOT hermetic: observe needs a live Ollama
// generation+embedding endpoint to fill the kind/facet/body datoms,
// a live Neo4j to persist the entry node, and a live Weaviate to
// hold the body vector for cosine recall. Running it inside the
// default `cli` tag would turn `make test-cli` into a service-
// dependent suite, which is the wrong contract.
//
// So this file adds a SECOND build tag — `integration` — on top
// of `cli`. The test only runs when both tags are set:
//
//	go test -tags='cli integration' ./internal/e2e/...
//
// The expectation is that `cortex up` (or its docker-compose
// equivalent) has been run beforehand and Neo4j/Weaviate/Ollama are
// reachable on their default endpoints. The test will fail loudly
// (with the underlying connection error in stderr) if any backend
// is missing, which is the right behavior for an integration suite:
// silent skip would defeat the round-3 grill point of the test.
//
// Spec references:
//
//	docs/spec/cortex-spec-code-review.md round-3 #4 recommendation
//	docs/spec/cortex-spec-code-review.md CRIT-008
//	docs/spec/cortex-spec-code-review.md CRIT-009

//go:build cli && integration

package e2e

import (
	"strings"
	"testing"
)

// TestCLI_ObserveRecallRoundtrip is the end-to-end CRIT-008/009
// regression guard. It writes one entry via `cortex observe` and
// then queries it back via `cortex recall`, asserting that the
// recall response contains at least one hit. The chosen body and
// query share a distinctive lexical token ("cortex-roundtrip-token")
// so the lexical-only fallback path can satisfy the assertion if
// cosine rerank degrades, AND so the cosine rerank path has a
// strong semantic signal. Either way, "results=0" means a wiring
// regression, not a relevance miss.
//
// The test runs each subprocess under an isolated HOME provided by
// runCortex, so the staging segment dir, watermarks, and config
// path are scoped to t.TempDir() and never touch the developer's
// real cortex install. Backends (Neo4j/Weaviate/Ollama) are NOT
// isolated — they are external services and the test reuses
// whatever the current operator has configured. The integration
// tag exists precisely to communicate that contract.
func TestCLI_ObserveRecallRoundtrip(t *testing.T) {
	const token = "cortex-roundtrip-token"
	const body = "round-3 regression entry: " + token + " — must come back via observe→recall"
	const query = token

	// Step 1: observe. A non-zero exit here is a hard fail; the
	// purpose of the integration tag is that the operator has
	// already brought the backends up.
	obs := runCortex(t, nil, "observe", body)
	if obs.exitCode != 0 {
		t.Fatalf("observe exit=%d\nstdout=%q\nstderr=%q",
			obs.exitCode, obs.stdout, obs.stderr)
	}

	// Step 2: recall. Same hard-fail contract on exit code.
	rec := runCortex(t, nil, "recall", query)
	if rec.exitCode != 0 {
		t.Fatalf("recall exit=%d\nstdout=%q\nstderr=%q",
			rec.exitCode, rec.stdout, rec.stderr)
	}

	// Step 3: assert at least one hit. The exact framing of "hit"
	// is brittle to recall's output format, so the assertion is
	// deliberately loose: the original token must appear somewhere
	// in the recall output. If recall returns zero rows the token
	// will not appear; if recall returns a hit the body (which
	// contains the token) will be in the rendered row.
	combined := rec.stdout + rec.stderr
	if !strings.Contains(combined, token) {
		t.Fatalf("recall returned no hits matching the observed entry — "+
			"observe→recall wiring regression (CRIT-008/009 class)\n"+
			"recall stdout:\n%s\nrecall stderr:\n%s",
			rec.stdout, rec.stderr)
	}
}
