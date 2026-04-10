// cmd/cortex/observe_test.go covers the CORTEX_TRAIL_ID env var
// fallback (cortex-0ci): if --trail is not provided, resolveTrailID
// must return the env var so agents chaining observe inside a trail
// session do not lose context.
package main

import "testing"

func TestResolveTrailID(t *testing.T) {
	t.Setenv("CORTEX_TRAIL_ID", "")
	if got := resolveTrailID(""); got != "" {
		t.Fatalf("empty flag + empty env: want %q, got %q", "", got)
	}

	t.Setenv("CORTEX_TRAIL_ID", "trl_from_env")
	if got := resolveTrailID(""); got != "trl_from_env" {
		t.Fatalf("empty flag + env set: want %q, got %q", "trl_from_env", got)
	}

	if got := resolveTrailID("trl_from_flag"); got != "trl_from_flag" {
		t.Fatalf("flag set must win over env: want %q, got %q", "trl_from_flag", got)
	}

	t.Setenv("CORTEX_TRAIL_ID", "  trl_padded  ")
	if got := resolveTrailID(""); got != "trl_padded" {
		t.Fatalf("env value must be trimmed: want %q, got %q", "trl_padded", got)
	}
}
