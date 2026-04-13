// cmd/cortex/recall_test.go covers the FR-015 reinforcement append
// path: appendReinforcementDatoms must seal each datom and write the
// whole group to the segment writer in one Append call. The unit
// test exercises the function directly with a real *log.Writer
// pointed at a t.TempDir() so the round-trip through the segment
// reader proves the datoms actually landed on disk.
//
// Spec references:
//
//	docs/spec/cortex-spec.md FR-015 (recall reinforcement)
//	bead cortex-4kq.36, code-review fix CRIT-007
package main

import (
	"encoding/json"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/recall"
)

func TestResolveSimFloorStrict_DisableViaZero(t *testing.T) {
	cases := []struct {
		name string
		in   config.RetrievalConfig
		want float64
	}{
		{
			name: "both zero disables gate",
			in:   config.RetrievalConfig{},
			want: 0,
		},
		{
			name: "legacy relevance_floor promoted",
			in:   config.RetrievalConfig{RelevanceFloor: 0.5},
			want: 0.5,
		},
		{
			name: "explicit sim_floor_strict wins",
			in: config.RetrievalConfig{
				RelevanceGate: config.RelevanceGateConfig{SimFloorStrict: 0.6},
			},
			want: 0.6,
		},
		{
			name: "gate beats legacy when both set",
			in: config.RetrievalConfig{
				RelevanceFloor: 0.5,
				RelevanceGate:  config.RelevanceGateConfig{SimFloorStrict: 0.7},
			},
			want: 0.7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSimFloorStrict(tc.in)
			if got != tc.want {
				t.Fatalf("resolveSimFloorStrict = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAppendReinforcementDatoms_LandsOnDisk constructs a fake recall
// response with three reinforcement datoms (one per FR-015 attribute:
// base_activation, retrieval_count, last_retrieved_at) and verifies
// that appendReinforcementDatoms seals them and appends them to the
// segment writer. The test reads the segment back via log.NewReader
// and asserts every input datom is present, sealed, and in the
// segment.
func TestAppendReinforcementDatoms_LandsOnDisk(t *testing.T) {
	dir := t.TempDir()
	writer, err := log.NewWriter(dir)
	if err != nil {
		t.Fatalf("log.NewWriter: %v", err)
	}

	tx := ulid.Make().String()
	invocation := ulid.Make().String()
	rawFloat, _ := json.Marshal(0.42)
	rawInt, _ := json.Marshal(7)
	rawTime, _ := json.Marshal("2026-04-10T03:00:00Z")

	res := &recall.Response{
		ReinforcementDatoms: []datom.Datom{
			{Tx: tx, Ts: "2026-04-10T03:00:00Z", Actor: "tester", Op: datom.OpAdd,
				E: "entry:01HXTEST", A: "base_activation", V: rawFloat,
				Src: "recall", InvocationID: invocation},
			{Tx: tx, Ts: "2026-04-10T03:00:00Z", Actor: "tester", Op: datom.OpAdd,
				E: "entry:01HXTEST", A: "retrieval_count", V: rawInt,
				Src: "recall", InvocationID: invocation},
			{Tx: tx, Ts: "2026-04-10T03:00:00Z", Actor: "tester", Op: datom.OpAdd,
				E: "entry:01HXTEST", A: "last_retrieved_at", V: rawTime,
				Src: "recall", InvocationID: invocation},
		},
	}

	if err := appendReinforcementDatoms(writer, res); err != nil {
		t.Fatalf("appendReinforcementDatoms: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	// Read the segment back via the merge reader. We expect the three
	// reinforcement datoms in the order they were appended (Append is a
	// single transaction group, so the reader emits them contiguously).
	matches, err := readSegments(t, dir)
	if err != nil {
		t.Fatalf("read segments: %v", err)
	}
	got := map[string]bool{}
	for _, d := range matches {
		if d.Tx == tx && d.Src == "recall" {
			got[d.A] = true
			if d.Checksum == "" {
				t.Errorf("datom %s not sealed", d.A)
			}
		}
	}
	for _, want := range []string{"base_activation", "retrieval_count", "last_retrieved_at"} {
		if !got[want] {
			t.Errorf("missing reinforcement datom for %s", want)
		}
	}
}

// TestAppendReinforcementDatoms_NilAndEmpty verifies the no-op
// branches: a nil response and an empty datom slice both return nil
// without touching the writer. We pass a nil writer to prove the
// function never dereferences it on the no-op paths.
func TestAppendReinforcementDatoms_NilAndEmpty(t *testing.T) {
	if err := appendReinforcementDatoms(nil, nil); err != nil {
		t.Errorf("nil response: got %v, want nil", err)
	}
	if err := appendReinforcementDatoms(nil, &recall.Response{}); err != nil {
		t.Errorf("empty datoms: got %v, want nil", err)
	}
}

// readSegments enumerates the segment dir, opens a reader over the
// healthy survivors, and returns every datom it sees. Local helper so
// the test does not have to import the merge-reader plumbing inline.
func readSegments(t *testing.T, dir string) ([]datom.Datom, error) {
	t.Helper()
	report, err := log.Load(dir, log.LoadOptions{})
	if err != nil {
		return nil, err
	}
	r, err := log.NewReader(report.Healthy)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []datom.Datom
	for {
		d, ok, err := r.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, d)
	}
}
