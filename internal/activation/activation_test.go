package activation

import (
	"testing"
	"time"
)

// TestSeed_FreshEntryHasInitialValues covers AC1: a freshly written
// entry has base_activation == 1.0 and retrieval_count == 0.
func TestSeed_FreshEntryHasInitialValues(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(now)
	if s.BaseActivation != 1.0 {
		t.Fatalf("BaseActivation: got %v want 1.0", s.BaseActivation)
	}
	if s.RetrievalCount != 0 {
		t.Fatalf("RetrievalCount: got %d want 0", s.RetrievalCount)
	}
	if !s.EncodingAt.Equal(now) {
		t.Fatalf("EncodingAt: got %v want %v", s.EncodingAt, now)
	}
	// Current at the encoding instant equals BaseActivation — the
	// (1+age)^-d clamp keeps the function continuous at age=0.
	if got := s.Current(now, DefaultDecayExponent); got != 1.0 {
		t.Fatalf("Current at encoding: got %v want 1.0", got)
	}
	if !s.Visible(now, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("fresh entry not visible")
	}
}

// TestDecay_OneMillionSecondsBelowThreshold covers AC2: after 1 million
// seconds with zero retrievals, base_activation is below 0.05 and the
// entry is absent from default recall.
func TestDecay_OneMillionSecondsBelowThreshold(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded)
	later := encoded.Add(1_000_000 * time.Second)
	val := s.Current(later, DefaultDecayExponent)
	if val >= VisibilityThreshold {
		t.Fatalf("Current after 1M seconds: got %v want < %v", val, VisibilityThreshold)
	}
	if s.Visible(later, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("entry still visible after 1M seconds")
	}
}

// TestReinforce_RaisesBelowThresholdEntry covers AC3: a retrieval
// event on a below-threshold entry raises base_activation above the
// threshold (unless the entry is evicted).
func TestReinforce_RaisesBelowThresholdEntry(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded)
	later := encoded.Add(1_000_000 * time.Second)
	if s.Visible(later, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("precondition: entry should be invisible before reinforcement")
	}
	s2 := s.Reinforce(later)
	if s2.RetrievalCount != 1 {
		t.Fatalf("RetrievalCount after reinforce: got %d want 1", s2.RetrievalCount)
	}
	if !s2.LastRetrievedAt.Equal(later) {
		t.Fatalf("LastRetrievedAt: got %v want %v", s2.LastRetrievedAt, later)
	}
	if s2.BaseActivation != 1.0 {
		t.Fatalf("BaseActivation after reinforce: got %v want 1.0", s2.BaseActivation)
	}
	if !s2.Visible(later, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("entry still invisible after reinforcement")
	}
	// Current value immediately after the reinforce event is exactly
	// InitialBaseActivation because the reference time just moved.
	if got := s2.Current(later, DefaultDecayExponent); got != 1.0 {
		t.Fatalf("Current immediately after reinforce: got %v want 1.0", got)
	}
}

// TestReinforce_EvictedIsNoop confirms the AC3 carve-out: reinforcing
// an evicted entry does NOT raise it above threshold. Eviction is
// sticky until unevict.
func TestReinforce_EvictedIsNoop(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded).Evict()
	later := encoded.Add(10 * time.Second)
	s2 := s.Reinforce(later)
	if s2.RetrievalCount != 0 {
		t.Fatalf("evicted Reinforce advanced count: %d", s2.RetrievalCount)
	}
	if s2.BaseActivation != 0.0 {
		t.Fatalf("evicted Reinforce raised base: %v", s2.BaseActivation)
	}
	if s2.Visible(later, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("evicted entry became visible after reinforcement")
	}
}

// TestPin_HoldsFloorOverTime covers AC4: pinned entries do not decay
// below their pin-time activation.
func TestPin_HoldsFloorOverTime(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded)
	// Let the entry age 100 seconds first so the pinned floor is
	// notably less than 1.0 and we can verify the floor holds later.
	pinTime := encoded.Add(100 * time.Second)
	pinnedVal := s.Current(pinTime, DefaultDecayExponent)
	if pinnedVal >= 1.0 {
		t.Fatalf("precondition: pinnedVal should be < 1.0, got %v", pinnedVal)
	}
	s = s.Pin(pinTime, DefaultDecayExponent)
	if !s.Pinned {
		t.Fatalf("Pin did not set flag")
	}
	if s.PinActivation != pinnedVal {
		t.Fatalf("PinActivation: got %v want %v", s.PinActivation, pinnedVal)
	}

	// Far in the future, the unpinned decayed value would be tiny, but
	// the pin floor holds.
	much := encoded.Add(1_000_000 * time.Second)
	got := s.Current(much, DefaultDecayExponent)
	if got < pinnedVal {
		t.Fatalf("Current below pin floor: got %v want >= %v", got, pinnedVal)
	}
	// And pin floors above the visibility threshold keep the entry
	// visible even past the 1M-second mark.
	if pinnedVal >= VisibilityThreshold && !s.Visible(much, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("pinned entry not visible despite floor >= threshold")
	}
}

// TestEvict_ForcesZeroAndBlocksVisibility asserts the spec rule that
// eviction forces base_activation to 0.0 and the entry stops appearing
// in default recall.
func TestEvict_ForcesZeroAndBlocksVisibility(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded).Evict()
	if s.BaseActivation != 0.0 {
		t.Fatalf("BaseActivation after Evict: got %v want 0.0", s.BaseActivation)
	}
	if !s.Evicted {
		t.Fatalf("Evicted flag not set")
	}
	if s.Visible(encoded, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("evicted entry still visible at encoding time")
	}
	if v := s.Current(encoded, DefaultDecayExponent); v != 0.0 {
		t.Fatalf("Current of evicted entry: got %v want 0.0", v)
	}
}

// TestUnevict_ClearsFlag verifies the state transition path used by
// cortex unevict. The stored BaseActivation is unchanged (still 0.0);
// a subsequent Reinforce brings it back up.
func TestUnevict_ClearsFlag(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded).Evict().Unevict()
	if s.Evicted {
		t.Fatalf("Unevict did not clear flag")
	}
	if s.BaseActivation != 0.0 {
		t.Fatalf("Unevict altered base activation: %v", s.BaseActivation)
	}
	// Now Reinforce should work.
	now := encoded.Add(1 * time.Second)
	s = s.Reinforce(now)
	if s.BaseActivation != 1.0 || s.RetrievalCount != 1 {
		t.Fatalf("Reinforce after unevict: %+v", s)
	}
}

// TestThresholdInclusive verifies the boundary condition from the
// spec: base_activation exactly 0.05 is visible.
func TestThresholdInclusive(t *testing.T) {
	s := State{BaseActivation: 0.05, EncodingAt: time.Unix(0, 0)}
	// Query at the reference instant so decay is a no-op.
	if !s.Visible(s.EncodingAt, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("threshold should be inclusive")
	}
}

// TestReinforce_ResetsDecayClock proves that the reinforcement makes
// the decay measurement start over, rather than continuing from the
// encoding time.
func TestReinforce_ResetsDecayClock(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded)
	// Reinforce at t=10_000 seconds — well past the threshold crossing.
	recallAt := encoded.Add(10_000 * time.Second)
	s = s.Reinforce(recallAt)
	// 100 seconds after the recall, the entry should still be visible
	// because the decay clock reset.
	check := recallAt.Add(100 * time.Second)
	if !s.Visible(check, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("entry invisible 100s after reinforcement: %v", s.Current(check, DefaultDecayExponent))
	}
}

// TestPin_UnpinRestoresDecay ensures Unpin removes the floor and the
// entry resumes natural decay.
func TestPin_UnpinRestoresDecay(t *testing.T) {
	encoded := time.Unix(1_700_000_000, 0).UTC()
	s := Seed(encoded).Pin(encoded, DefaultDecayExponent) // pin at floor=1.0
	if s.PinActivation != 1.0 {
		t.Fatalf("PinActivation: got %v want 1.0", s.PinActivation)
	}
	s = s.Unpin()
	if s.Pinned {
		t.Fatalf("Unpin did not clear flag")
	}
	// After 1M seconds unpinned, decay drops below threshold.
	later := encoded.Add(1_000_000 * time.Second)
	if s.Visible(later, DefaultDecayExponent, VisibilityThreshold) {
		t.Fatalf("unpinned entry still visible after 1M seconds")
	}
}
