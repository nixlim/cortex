// Package activation manages the persisted activation state of a
// Cortex entry across its lifetime: seeding on write, decaying in the
// absence of retrievals, reinforcement on recall, pinning, and
// eviction.
//
// Relationship to internal/actr:
//
//	internal/actr computes the composite retrieval score
//	activation(e, q) = 0.3*B(e) + 0.3*PPR(e) + 0.3*sim + 0.1*I(e)
//	at query time. This package owns the persisted B(e)-like field
//	(base_activation) that enters that formula as Inputs.Base.
//
// Divergence from the literal ACT-R formula:
//
//	The spec requires fresh entries to seed base_activation=1.0
//	(see docs/spec/cortex-spec.md US-12 scenario "New writes seed
//	initial activation" and line 622). Literal ACT-R B(e) = ln(t^-d)
//	is not 1.0 for any finite t, so the spec's "stored base_activation
//	is the current value of B(e)" must be read pragmatically: the
//	stored field starts at 1.0 and decays between reinforcement
//	events via a bounded power-law factor `(1+age)^-d` where age is
//	seconds since the last event (encoding or retrieval). That
//	decay, with the default d=0.5, drives a lone encoding event
//	below the 0.05 visibility threshold well before the 1M-second
//	budget required by cortex-4kq.42 AC2.
//
// The package is zero-dependency (only math + time) so tests run in
// microseconds and the write pipeline can import it freely.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"ACT-R Activation Formula"
//	docs/spec/cortex-spec.md §"Boundary conditions" (visibility threshold)
//	docs/spec/cortex-spec.md US-12, US-9 (pin/unpin/evict/unevict)
//	bead cortex-4kq.42
package activation

import (
	"math"
	"time"
)

// DefaultDecayExponent mirrors retrieval.activation.decay_exponent (0.5).
const DefaultDecayExponent = 0.5

// InitialBaseActivation is the seed value written at encoding time.
// Matches the spec BDD scenario "New writes seed initial activation".
const InitialBaseActivation = 1.0

// VisibilityThreshold is the inclusive floor used by default recall.
// Duplicates internal/actr.DefaultVisibilityThreshold on purpose so
// callers of this package do not need to pull in the actr import.
const VisibilityThreshold = 0.05

// State is the persisted activation snapshot for one entry. Every
// field is independently represented as an LWW datom in the log;
// State exists to give the write and recall pipelines a typed view
// over those datoms without threading four named arguments through
// every function.
type State struct {
	// EncodingAt is the initial-encoding event timestamp, written once
	// at observe time and never mutated. It is the minimum possible
	// reference time for the decay calculation.
	EncodingAt time.Time

	// BaseActivation is the stored LWW value. Seeded to
	// InitialBaseActivation on write; refreshed to InitialBaseActivation
	// on every reinforcement; forced to 0.0 on eviction.
	BaseActivation float64

	// RetrievalCount is the number of recall events that have surfaced
	// this entry. Monotonically non-decreasing under reinforcement;
	// unchanged by eviction (eviction blocks further reinforcement).
	RetrievalCount int

	// LastRetrievedAt is the zero time until the first reinforcement.
	// Acts as the decay reference point after the first recall: subsequent
	// decays are measured from LastRetrievedAt rather than EncodingAt.
	LastRetrievedAt time.Time

	// Pinned reports whether cortex pin has been applied. Pinned entries
	// never fall below PinActivation regardless of age, satisfying AC4.
	Pinned bool

	// PinActivation is the activation value captured at pin time. Only
	// meaningful while Pinned is true.
	PinActivation float64

	// Evicted is true after cortex evict has forced BaseActivation to
	// 0.0. Blocks reinforcement via Reinforce and forces Visible to
	// return false regardless of the decayed value.
	Evicted bool
}

// Seed builds the initial state for a freshly written entry. AC1:
// base_activation == InitialBaseActivation, retrieval_count == 0.
func Seed(encodingAt time.Time) State {
	return State{
		EncodingAt:     encodingAt,
		BaseActivation: InitialBaseActivation,
		RetrievalCount: 0,
	}
}

// referenceTime returns the "most recent event" timestamp the decay
// math is measured from. For an entry that has never been retrieved
// this is EncodingAt; after the first reinforcement it is
// LastRetrievedAt. Using the later of the two timestamps preserves
// the invariant that reinforcement "resets" the decay clock.
func (s State) referenceTime() time.Time {
	if s.LastRetrievedAt.After(s.EncodingAt) {
		return s.LastRetrievedAt
	}
	return s.EncodingAt
}

// Current returns the activation value at wall-clock time `now` with
// the given decay exponent. Evicted entries return 0.0. Pinned entries
// never fall below their PinActivation floor. Non-pinned entries decay
// via `BaseActivation * (1 + age_seconds)^-decay`.
//
// The `(1 + age)` clamp (as opposed to bare `age^-d`) exists so the
// function is continuous at the reference instant: Current(s, s.EncodingAt, d)
// equals BaseActivation rather than the pole of age^-d at age=0.
func (s State) Current(now time.Time, decay float64) float64 {
	if s.Evicted {
		return 0.0
	}
	ref := s.referenceTime()
	age := now.Sub(ref).Seconds()
	if age < 0 {
		age = 0
	}
	val := s.BaseActivation * math.Pow(1+age, -decay)
	if s.Pinned && val < s.PinActivation {
		val = s.PinActivation
	}
	return val
}

// Visible reports whether the entry clears the inclusive visibility
// threshold at time `now`. Evicted entries are never visible; pinned
// entries remain visible so long as PinActivation >= threshold.
func (s State) Visible(now time.Time, decay, threshold float64) bool {
	if s.Evicted {
		return false
	}
	return s.Current(now, decay) >= threshold
}

// Reinforce records a retrieval event at time `now`. The stored
// BaseActivation is refreshed to InitialBaseActivation (a full
// reinforcement boost), RetrievalCount increments, and LastRetrievedAt
// advances. Evicted entries are a no-op per AC3: "unless the entry is
// evicted".
func (s State) Reinforce(now time.Time) State {
	if s.Evicted {
		return s
	}
	s.BaseActivation = InitialBaseActivation
	s.RetrievalCount++
	s.LastRetrievedAt = now
	return s
}

// Pin freezes the current decayed activation as the new PinActivation
// floor and marks the entry as pinned. Spec scenario "pinned entries
// do not decay below their pin-time activation" (AC4) is enforced by
// Current's pin clamp, not by suppressing decay outright — the stored
// BaseActivation still decays naturally so unpinning produces the
// expected lower value.
func (s State) Pin(now time.Time, decay float64) State {
	s.PinActivation = s.Current(now, decay)
	s.Pinned = true
	return s
}

// Unpin removes the pin without touching the stored BaseActivation.
// The entry returns to normal decay behavior on the next Current call.
func (s State) Unpin() State {
	s.Pinned = false
	s.PinActivation = 0
	return s
}

// Evict forces BaseActivation to 0.0, sets the Evicted flag, and
// leaves RetrievalCount unchanged. Spec: "cortex evict ... forces
// base_activation to 0.0, writes a sticky evicted_at attribute, and
// blocks reinforcement from raising the entry above the visibility
// threshold".
func (s State) Evict() State {
	s.Evicted = true
	s.BaseActivation = 0.0
	return s
}

// Unevict clears the Evicted flag. The caller is responsible for
// writing the evicted_at_retracted datom; this package only manages
// the in-memory state transition. After unevict the entry is back in
// the reinforcement loop but its BaseActivation remains 0.0 until a
// Reinforce event lifts it.
func (s State) Unevict() State {
	s.Evicted = false
	return s
}
