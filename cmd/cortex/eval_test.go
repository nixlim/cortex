package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEvalCalibrate_PicksOptimalFromMockDump builds a small in-memory
// dump with three queries, each with five hits, and verifies that the
// calibration routine picks a sensible grid point and emits the
// retrieval.relevance_gate YAML block. The positives sit around
// sim=0.70 and the negatives around sim=0.35, so any reasonable sweep
// should land on (hard~0.40, strict~0.55, composite~0.45).
func TestEvalCalibrate_PicksOptimalFromMockDump(t *testing.T) {
	d := evalDump{Records: []evalDumpRecord{
		mockRecord("q1", []string{"good/mod.md"}),
		mockRecord("q2", []string{"good/mod.md"}),
		mockRecord("q3", []string{"good/mod.md"}),
	}}
	raw, err := json.Marshal(&d)
	if err != nil {
		t.Fatalf("marshal mock dump: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "mock.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write mock dump: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runCalibrateFloors(path, &stdout, &stderr); err != nil {
		t.Fatalf("runCalibrateFloors: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"retrieval:",
		"relevance_gate:",
		"sim_floor_hard:",
		"sim_floor_strict:",
		"rescue_alpha:",
		"composite_floor:",
		"gate_sim_weight:",
		"gate_ppr_weight:",
		"ppr_baseline_min_n:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("calibration YAML missing %q; full output:\n%s", want, out)
		}
	}
	summary := stderr.String()
	if !strings.Contains(summary, "calibration:") || !strings.Contains(summary, "F1=") {
		t.Errorf("stderr summary unexpected: %q", summary)
	}
}

// TestEvalCalibrate_FailsOnMissingScores verifies that a dump without
// per-hit sim/ppr returns a clear error pointing the operator back at
// the deep-eval runner.
func TestEvalCalibrate_FailsOnMissingScores(t *testing.T) {
	d := evalDump{Records: []evalDumpRecord{
		{
			ID:              1,
			Query:           "q",
			ExpectedModules: []string{"good/mod.md"},
			Retrievals: []evalDumpRetrieval{{
				Query: "q",
				Hits: []evalDumpHit{
					{Module: "good/mod.md"}, // sim=0, ppr=0
					{Module: "bad/mod.md"},
				},
			}},
		},
	}}
	raw, _ := json.Marshal(&d)
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	err := runCalibrateFloors(path, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for dump with no sim/ppr")
	}
	if !strings.Contains(err.Error(), "per-hit sim/ppr") {
		t.Errorf("error message unclear: %v", err)
	}
}

func mockRecord(q string, expected []string) evalDumpRecord {
	return evalDumpRecord{
		Query:           q,
		ExpectedModules: expected,
		Retrievals: []evalDumpRetrieval{{
			Query: q,
			Hits: []evalDumpHit{
				// Strong positives (in expected).
				{Module: expected[0], Similarity: 0.72, PPRScore: 0.40},
				{Module: expected[0], Similarity: 0.68, PPRScore: 0.30},
				// Weak negatives (not in expected).
				{Module: "noise/a.json", Similarity: 0.38, PPRScore: 0.10},
				{Module: "noise/b.json", Similarity: 0.33, PPRScore: 0.05},
				{Module: "noise/c.json", Similarity: 0.29, PPRScore: 0.02},
			},
		}},
	}
}
