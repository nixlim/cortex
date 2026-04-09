package actr

import (
	"math"
	"testing"
	"time"
)

func TestBaseActivationSingleEncoding10sAgo(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	tj := now.Add(-10 * time.Second)
	got := BaseActivation(now, []time.Time{tj}, DefaultDecayExponent)
	// B = ln(10^-0.5)
	want := math.Log(math.Pow(10, -0.5))
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("B(e) = %v, want %v (diff %v)", got, want, got-want)
	}
}

func TestImportanceCrossProjectBoost(t *testing.T) {
	plain := ImportanceScore(Importance{})
	cp := ImportanceScore(Importance{CrossProject: true})
	if delta := cp - plain; math.Abs(delta-0.20) > 1e-12 {
		t.Errorf("cross_project delta = %v, want 0.20", delta)
	}
}

func TestWeightsOrderAndValues(t *testing.T) {
	w := DefaultWeights()
	if w.Base != 0.3 || w.PPR != 0.3 || w.Similarity != 0.3 || w.Importance != 0.1 {
		t.Errorf("default weights wrong: %+v", w)
	}
	in := Inputs{Base: 1, PPR: 2, Similarity: 3, Importance: 4}
	got := Activation(in, w)
	want := 0.3*1 + 0.3*2 + 0.3*3 + 0.1*4
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Activation = %v, want %v", got, want)
	}
}

func TestVisibilityInclusiveThreshold(t *testing.T) {
	if !Visible(0.05, DefaultVisibilityThreshold) {
		t.Error("0.05 must be visible (>= inclusive)")
	}
	if Visible(0.04999999, DefaultVisibilityThreshold) {
		t.Error("below threshold must be hidden")
	}
	if Visible(0.0, DefaultVisibilityThreshold) {
		t.Error("0.0 (evicted) must be hidden")
	}
}

func TestActivationBatch1000Under10Milliseconds(t *testing.T) {
	const N = 1000
	now := time.Unix(1_000_000, 0)
	batch := make([]Inputs, N)
	stamps := make([][]time.Time, N)
	for i := 0; i < N; i++ {
		stamps[i] = []time.Time{now.Add(-time.Duration(i+1) * time.Second)}
		batch[i] = Inputs{
			PPR:        0.5,
			Similarity: 0.7,
			Importance: ImportanceScore(Importance{CrossProject: i%2 == 0}),
		}
	}
	w := DefaultWeights()

	start := time.Now()
	out := make([]float64, N)
	for i := 0; i < N; i++ {
		batch[i].Base = BaseActivation(now, stamps[i], DefaultDecayExponent)
		out[i] = Activation(batch[i], w)
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Millisecond {
		t.Errorf("1000-entry batch took %v, budget 10ms", elapsed)
	}
	// Sanity: output is populated.
	if len(out) != N {
		t.Fatal("output length mismatch")
	}
}
