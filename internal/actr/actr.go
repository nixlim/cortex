// Package actr implements the Cortex composite activation formula.
//
// From docs/spec/cortex-spec.md ("ACT-R Activation Formula"):
//
//	activation(e, q) = w_base * B(e)
//	                 + w_ppr  * PPR(e)
//	                 + w_sim  * sim(e, q)
//	                 + w_imp  * I(e)
//
// with default weights w_base=0.3, w_ppr=0.3, w_sim=0.3, w_imp=0.1,
// B(e) = ln(sum t_j^{-d}) and d = retrieval.activation.decay_exponent
// (default 0.5).
//
// The package deliberately works in plain float64s and takes no dependency
// on config. Callers pass in the weights explicitly so rebuild-time and
// retrieval-time code paths can both use the same primitive.
package actr

import (
	"math"
	"time"
)

// DefaultDecayExponent is the spec default for d in B(e) = ln(sum t_j^{-d}).
const DefaultDecayExponent = 0.5

// DefaultVisibilityThreshold is the inclusive floor used for default
// retrieval visibility: entries with base_activation >= 0.05 are visible.
const DefaultVisibilityThreshold = 0.05

// Weights are the composite weights w_base, w_ppr, w_sim, w_imp.
type Weights struct {
	Base       float64
	PPR        float64
	Similarity float64
	Importance float64
}

// DefaultWeights returns the spec defaults: 0.3, 0.3, 0.3, 0.1 in the
// canonical base/PPR/sim/importance order.
func DefaultWeights() Weights {
	return Weights{Base: 0.3, PPR: 0.3, Similarity: 0.3, Importance: 0.1}
}

// Importance describes the per-entry importance inputs.
type Importance struct {
	// CrossProject adds +0.20 to I(e) when true.
	CrossProject bool
	// FacetBoost is an additional additive boost from type- or facet-
	// based importance rules. Callers may leave it zero.
	FacetBoost float64
}

// ImportanceScore computes I(e) = 0.0 base + 0.20 if cross-project
// + facet boost.
func ImportanceScore(i Importance) float64 {
	v := i.FacetBoost
	if i.CrossProject {
		v += 0.20
	}
	return v
}

// BaseActivation returns ACT-R base-level activation:
//
//	B(e) = ln(sum_j t_j^{-d})
//
// now is the reference time against which each retrieval timestamp is aged;
// t_j is then (now - timestamps[j]).Seconds(). Zero or negative ages are
// clamped to a tiny positive value so t_j^{-d} is finite. An empty
// timestamps slice returns math.Inf(-1) — B is undefined without at least
// one encoding or retrieval event.
func BaseActivation(now time.Time, timestamps []time.Time, decay float64) float64 {
	if len(timestamps) == 0 {
		return math.Inf(-1)
	}
	var sum float64
	for _, tj := range timestamps {
		age := now.Sub(tj).Seconds()
		if age <= 0 {
			age = 1e-9
		}
		sum += math.Pow(age, -decay)
	}
	if sum <= 0 {
		return math.Inf(-1)
	}
	return math.Log(sum)
}

// Inputs groups the four composite terms of the activation formula.
type Inputs struct {
	Base       float64 // B(e)
	PPR        float64 // PPR(e)
	Similarity float64 // sim(e, q)
	Importance float64 // I(e)
}

// Activation computes the composite activation score for a single entry
// given the four term values and the weights.
func Activation(in Inputs, w Weights) float64 {
	return w.Base*in.Base +
		w.PPR*in.PPR +
		w.Similarity*in.Similarity +
		w.Importance*in.Importance
}

// Visible reports whether base_activation passes the inclusive visibility
// threshold used for default retrieval.
func Visible(baseActivation, threshold float64) bool {
	return baseActivation >= threshold
}
