package secrets

// Acceptance tests for cortex-4kq.53.
//
// The bead names three test functions — test_secret_redaction_observe,
// test_secret_redaction_ingest, test_secret_detected_in_generation —
// and asks for confirmation of SECRET_DETECTED /
// SECRET_DETECTED_IN_GENERATION / ops.log scrubbing behaviors per
// FR-045. The write pipeline's Detector field is the single chokepoint
// all three paths share (observe via write.Pipeline.Observe, ingest
// via the same Pipeline when ingest lands, and the generation path
// via an LLM-output scrubber yet to be built). This file exercises
// the detector directly against each of the four canonical payload
// types from the bead's inputs section:
//
//   - AWS access key
//   - GitHub personal access token
//   - PEM private key
//   - JWT
//
// and enforces the two invariants that keep ops.log free of raw
// secrets at every downstream consumer:
//
//   1. Every payload produces at least one Match with a stable rule
//      name so callers can propagate only the rule identifier into
//      error envelopes.
//   2. The Match struct is structurally incapable of carrying the
//      matched substring — asserted by marshaling every hit to JSON
//      and confirming no canonical secret substring survives.
//
// Integration tests against the observe pipeline already exist in
// internal/write/pipeline_test.go (TestObserve_SecretInBodyRejected-
// WithoutWrite); the AWS-key case there is the "observe rejects with
// SECRET_DETECTED and writes zero datoms" acceptance criterion. The
// tests here expand coverage to the other three payload types at the
// detector layer, which is the single place all three pipelines
// consult before touching any side effect.

import (
	"encoding/json"
	"strings"
	"testing"
)

// canonicalPayloads is the bead's inputs.secret_payloads list. Every
// entry here must be detected by the built-in ruleset; if a future
// change to builtin.yaml drops one of these, this test fails loudly.
var canonicalPayloads = []struct {
	name    string
	body    string
	wantRule string
	// A substring that must NEVER appear in a Match struct after
	// JSON serialization — the "secret slice" that would leak if the
	// detector or its callers forgot to strip the raw match.
	leakCanary string
}{
	{
		name:      "aws_access_key",
		body:      "config uses AKIAIOSFODNN7EXAMPLE as the key id",
		wantRule:  "aws_access_key",
		leakCanary: "AKIAIOSFODNN7EXAMPLE",
	},
	{
		name:       "github_pat",
		body:       "use ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 to clone",
		wantRule:   "github_pat",
		leakCanary: "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
	},
	{
		name: "private_key_pem",
		body: "-----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIEpAIBAAKCAQEAvQxY8fakefakefakefakefakefakefakefake\n" +
			"-----END RSA PRIVATE KEY-----\n",
		wantRule:   "private_key_pem",
		leakCanary: "BEGIN RSA PRIVATE KEY",
	},
	{
		name: "jwt",
		// Three base64url segments joined by dots; the bead only asks
		// that JWTs be detected, not that the signature verifies.
		body: "auth: Bearer " +
			"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
			"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ." +
			"dozjgNryP4J3jVmNHl0w5N_XgL0n3I9FYR50DAhukc4",
		wantRule:   "jwt",
		leakCanary: "dozjgNryP4J3jVmNHl0w5N_XgL0n3I9FYR50DAhukc4",
	},
}

// TestSecretRedactionObserve — covers the bead's
// "test_secret_redaction_observe" requirement at the detector layer.
// Every canonical payload type must be detected when it appears in a
// cortex observe body. The write.Pipeline.Observe integration (in
// internal/write/pipeline_test.go) already proves the AWS case
// produces SECRET_DETECTED and writes zero datoms; this test guards
// the other three rule names so a future detector regression can
// never make one of them silently un-flag.
func TestSecretRedactionObserve(t *testing.T) {
	d := mustDetector(t)
	for _, p := range canonicalPayloads {
		t.Run(p.name, func(t *testing.T) {
			hits := d.Scan(p.body)
			if len(hits) == 0 {
				t.Fatalf("no matches for %s payload", p.name)
			}
			assertRuleHit(t, hits, p.wantRule)
			assertNoRawLeakInMatches(t, hits, p.leakCanary)
		})
	}
}

// TestSecretRedactionIngest — covers the bead's
// "test_secret_redaction_ingest" requirement. cortex ingest walks a
// repo's files and feeds each body through the same Detector that
// observe uses, so the detector-level guarantee is the whole test: if
// Scan flags the four canonical payloads, the ingest pipeline (when
// wired) will produce SECRET_DETECTED with no datoms written. A
// pipeline-integration smoke test can be added once cmd/cortex/
// ingest.go lands.
func TestSecretRedactionIngest(t *testing.T) {
	d := mustDetector(t)
	// Simulate a source file that contains the payload mixed with
	// normal source lines — the bead specifies a GitHub PAT in a
	// source file specifically, and the detector must find it even
	// when surrounded by non-secret text.
	sourceFile := `package example

// Keep this out of the repo, but our dev was sloppy:
// token: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789

func Connect() {
    // ...
}`
	hits := d.Scan(sourceFile)
	if len(hits) == 0 {
		t.Fatal("github PAT inside a source file was not detected")
	}
	assertRuleHit(t, hits, "github_pat")
	assertNoRawLeakInMatches(t, hits, "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

	// And the full sweep: all four payloads must be detected when they
	// appear as the body of a would-be ingested file.
	for _, p := range canonicalPayloads {
		t.Run(p.name, func(t *testing.T) {
			h := d.Scan(p.body)
			if len(h) == 0 {
				t.Fatalf("ingest path: detector missed %s", p.name)
			}
			assertRuleHit(t, h, p.wantRule)
		})
	}
}

// TestSecretDetectedInGeneration — covers the bead's
// "test_secret_detected_in_generation" requirement. When the LLM
// output scrubber (cortex-4kq.XX, not yet implemented) feeds a
// generated trail summary through this same Detector, the canonical
// payloads must be flagged. The downstream code will use the rule
// name to construct a SECRET_DETECTED_IN_GENERATION envelope and
// refuse to write any trail-summary datoms.
func TestSecretDetectedInGeneration(t *testing.T) {
	d := mustDetector(t)
	// Simulate an LLM response that embeds a private key in its
	// "helpful" summary.
	generated := "Summary of this trail:\n" +
		"The agent was debugging a cert rotation and printed the key:\n" +
		"-----BEGIN RSA PRIVATE KEY-----\n" +
		"MIIEpAIBAAKCAQEAexample\n" +
		"-----END RSA PRIVATE KEY-----\n" +
		"The rotation eventually succeeded."
	hits := d.Scan(generated)
	if len(hits) == 0 {
		t.Fatal("detector failed to flag private key in generated summary")
	}
	assertRuleHit(t, hits, "private_key_pem")
	assertNoRawLeakInMatches(t, hits, "BEGIN RSA PRIVATE KEY")
}

// TestOpsLogNeverReceivesRawSecrets — covers the bead's fourth
// acceptance criterion: "ops.log entries scanned after the run contain
// zero raw secrets (only [REDACTED:<rule>] tokens where applicable)".
// The design that makes this true is Match's absence of a raw-string
// field: callers that propagate a detection into an error envelope or
// ops.log event can only propagate the rule name and byte offsets,
// never the substring itself. We verify this structurally by JSON-
// marshaling every hit from every canonical payload and asserting no
// canary appears in the serialized form.
func TestOpsLogNeverReceivesRawSecrets(t *testing.T) {
	d := mustDetector(t)
	for _, p := range canonicalPayloads {
		t.Run(p.name, func(t *testing.T) {
			hits := d.Scan(p.body)
			if len(hits) == 0 {
				t.Fatalf("no hits for %s", p.name)
			}
			raw, err := json.Marshal(hits)
			if err != nil {
				t.Fatalf("marshal matches: %v", err)
			}
			if strings.Contains(string(raw), p.leakCanary) {
				t.Errorf("serialized Match contains raw %s payload: %s", p.name, raw)
			}
			// And the serialized form must carry the rule name — the
			// one piece of data an opslog consumer can print.
			if !strings.Contains(string(raw), p.wantRule) {
				t.Errorf("serialized Match does not carry rule name %q: %s", p.wantRule, raw)
			}
		})
	}
}

// TestRuleNamesCoverCanonicalPayloads — a drift guard. If someone
// removes one of the four rules the bead calls out from builtin.yaml,
// this test fails immediately with a clear message naming the missing
// rule, rather than producing a puzzling no-match failure in one of
// the other tests.
func TestRuleNamesCoverCanonicalPayloads(t *testing.T) {
	d := mustDetector(t)
	have := make(map[string]struct{})
	for _, n := range d.RuleNames() {
		have[n] = struct{}{}
	}
	for _, p := range canonicalPayloads {
		if _, ok := have[p.wantRule]; !ok {
			t.Errorf("built-in ruleset missing canonical rule %q (required by cortex-4kq.53)", p.wantRule)
		}
	}
}

// assertRuleHit verifies that hits contains at least one Match with
// rule == want. It is tolerant of extra matches — some payloads
// legitimately hit multiple rules (e.g., a PEM body whose header
// matches both private_key_pem and generic_high_entropy_secret).
func assertRuleHit(t *testing.T, hits []Match, want string) {
	t.Helper()
	for _, h := range hits {
		if h.Rule == want {
			return
		}
	}
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.Rule)
	}
	t.Errorf("rule %q not among matches: %v", want, names)
}

// assertNoRawLeakInMatches verifies that no Match's Rule or Severity
// field contains the canary substring. Start/End are integer offsets
// and cannot smuggle a string, so this check covers the entire public
// surface of Match.
func assertNoRawLeakInMatches(t *testing.T, hits []Match, canary string) {
	t.Helper()
	if canary == "" {
		return
	}
	for _, h := range hits {
		if strings.Contains(h.Rule, canary) {
			t.Errorf("Match.Rule leaked canary %q: %+v", canary, h)
		}
		if strings.Contains(h.Severity, canary) {
			t.Errorf("Match.Severity leaked canary %q: %+v", canary, h)
		}
	}
}
