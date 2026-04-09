package write

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeProposer returns a pre-canned proposal set (or error) without
// touching any external service. Tests use it to drive every scoring
// branch deterministically.
type fakeProposer struct {
	proposals []LinkProposal
	err       error
	calls     int
}

func (f *fakeProposer) Propose(_ context.Context, _ string, _ []LinkCandidate) ([]LinkProposal, error) {
	f.calls++
	return f.proposals, f.err
}

// defaultCfg mirrors the production defaults from
// docs/spec/cortex-spec.md §"Configuration Defaults".
func defaultCfg() LinkDerivationConfig {
	return LinkDerivationConfig{
		ConfidenceFloor:    0.60,
		SimilarCosineFloor: 0.75,
	}
}

// TestDeriveLinks_SimilarToAboveBothFloorsPersists covers AC1:
// confidence 0.70 + cosine 0.80 + SIMILAR_TO → one accepted link.
func TestDeriveLinks_SimilarToAboveBothFloorsPersists(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.80},
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:NEIGHBOR1", LinkType: LinkTypeSimilarTo, Confidence: 0.70},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 1 {
		t.Fatalf("accepted: got %d want 1", len(got))
	}
	if got[0].Proposal.TargetEntryID != "entry:NEIGHBOR1" {
		t.Fatalf("target: %+v", got[0])
	}
	if got[0].Cosine != 0.80 {
		t.Fatalf("cosine passthrough: %v", got[0].Cosine)
	}

	// And the datom builder turns it into a link.SIMILAR_TO datom
	// attached to the source entry with the expected payload.
	ds, err := BuildLinkDatoms("entry:SRC", "01HTX0000000000000000000000", "2026-04-10T00:00:00Z", "actor", "01HINV000000000000000000000", got)
	if err != nil {
		t.Fatalf("BuildLinkDatoms: %v", err)
	}
	if len(ds) != 1 {
		t.Fatalf("datoms: got %d want 1", len(ds))
	}
	if ds[0].A != "link.SIMILAR_TO" {
		t.Fatalf("attr: %s", ds[0].A)
	}
	if ds[0].E != "entry:SRC" {
		t.Fatalf("src: %s", ds[0].E)
	}
	if ds[0].Checksum == "" {
		t.Fatalf("datom not sealed")
	}
	if err := ds[0].Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(string(ds[0].V), "entry:NEIGHBOR1") {
		t.Fatalf("payload missing target: %s", string(ds[0].V))
	}
}

// TestDeriveLinks_SimilarToBelowCosineDropped covers AC2:
// confidence 0.70 + cosine 0.70 + SIMILAR_TO → zero links (the
// similar-to floor is 0.75 and 0.70 fails it even though the generic
// confidence floor is cleared).
func TestDeriveLinks_SimilarToBelowCosineDropped(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.70},
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:NEIGHBOR1", LinkType: LinkTypeSimilarTo, Confidence: 0.70},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("accepted: got %d want 0", len(got))
	}
}

// TestDeriveLinks_ProposerErrorYieldsZeroLinks covers AC3 for the
// "proposer errored" case: an unparseable LLM body is expected to
// arrive here either as proposer error or empty proposals, and either
// way the source entry must still commit with zero derived links. We
// prove the zero-links half here; the pipeline-level "entry still
// commits" invariant is enforced by Pipeline.Observe, which never
// calls DeriveLinks.
func TestDeriveLinks_ProposerErrorYieldsZeroLinks(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.95},
	}
	prop := &fakeProposer{err: errors.New("malformed LLM JSON: unexpected token")}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("accepted despite proposer error: %d", len(got))
	}
	// And BuildLinkDatoms on an empty set yields nil without error.
	ds, err := BuildLinkDatoms("entry:SRC", "tx", "ts", "actor", "inv", got)
	if err != nil || ds != nil {
		t.Fatalf("BuildLinkDatoms on empty: ds=%v err=%v", ds, err)
	}
}

// TestDeriveLinks_ProposerNilResultYieldsZeroLinks is the second half
// of AC3: a proposer that returns (nil, nil) — the shape a quiet
// swallow of malformed JSON produces — also results in zero links.
func TestDeriveLinks_ProposerNilResultYieldsZeroLinks(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.95},
	}
	prop := &fakeProposer{proposals: nil}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("accepted despite nil proposal set: %d", len(got))
	}
}

// TestDeriveLinks_NonSimilarTypeSkipsCosineFloor proves the extra
// cosine floor is specific to SIMILAR_TO: a CAUSES link with a cosine
// well below 0.75 is still persisted so long as confidence clears.
func TestDeriveLinks_NonSimilarTypeSkipsCosineFloor(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.30},
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:NEIGHBOR1", LinkType: "CAUSES", Confidence: 0.65},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 1 {
		t.Fatalf("CAUSES should bypass similar-to cosine floor: got %d", len(got))
	}
}

// TestDeriveLinks_BelowConfidenceFloorDropped asserts the generic
// confidence floor is enforced for every link type.
func TestDeriveLinks_BelowConfidenceFloorDropped(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.99},
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:NEIGHBOR1", LinkType: LinkTypeSimilarTo, Confidence: 0.50},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("confidence floor not enforced: %d", len(got))
	}
}

// TestDeriveLinks_UnknownTargetDropped proves the scorer refuses to
// let the LLM invent a link target that was not in the neighbor set.
func TestDeriveLinks_UnknownTargetDropped(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:NEIGHBOR1", CosineSimilarity: 0.95},
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:HALLUCINATED", LinkType: LinkTypeSimilarTo, Confidence: 0.95},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("hallucinated target accepted: %d", len(got))
	}
}

// TestDeriveLinks_NoCandidatesSkipsProposer asserts the fast path:
// with zero neighbors the proposer is never invoked (saves an Ollama
// round-trip for cold-start writes).
func TestDeriveLinks_NoCandidatesSkipsProposer(t *testing.T) {
	prop := &fakeProposer{}
	got := DeriveLinks(context.Background(), prop, "body", nil, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("no-candidate path emitted links: %d", len(got))
	}
	if prop.calls != 0 {
		t.Fatalf("proposer invoked with empty neighbors: %d calls", prop.calls)
	}
}

// TestDeriveLinks_MultipleProposalsMixedOutcomes exercises the full
// filter across a top-k=5 style proposal set: some clear, some drop
// below the confidence floor, one fails the SIMILAR_TO cosine floor.
func TestDeriveLinks_MultipleProposalsMixedOutcomes(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:A", CosineSimilarity: 0.90}, // passes similar-to
		{TargetEntryID: "entry:B", CosineSimilarity: 0.72}, // fails similar-to cosine
		{TargetEntryID: "entry:C", CosineSimilarity: 0.80}, // passes (CAUSES)
		{TargetEntryID: "entry:D", CosineSimilarity: 0.95}, // dropped: low confidence
		{TargetEntryID: "entry:E", CosineSimilarity: 0.85}, // passes similar-to
	}
	prop := &fakeProposer{proposals: []LinkProposal{
		{TargetEntryID: "entry:A", LinkType: LinkTypeSimilarTo, Confidence: 0.80},
		{TargetEntryID: "entry:B", LinkType: LinkTypeSimilarTo, Confidence: 0.90},
		{TargetEntryID: "entry:C", LinkType: "CAUSES", Confidence: 0.65},
		{TargetEntryID: "entry:D", LinkType: LinkTypeSimilarTo, Confidence: 0.30},
		{TargetEntryID: "entry:E", LinkType: LinkTypeSimilarTo, Confidence: 0.78},
	}}
	got := DeriveLinks(context.Background(), prop, "body", cands, defaultCfg())
	if len(got) != 3 {
		t.Fatalf("accepted: got %d want 3 (A, C, E)", len(got))
	}
	accepted := map[string]bool{}
	for _, l := range got {
		accepted[l.Proposal.TargetEntryID] = true
	}
	for _, want := range []string{"entry:A", "entry:C", "entry:E"} {
		if !accepted[want] {
			t.Errorf("missing expected accepted link: %s", want)
		}
	}
	for _, reject := range []string{"entry:B", "entry:D"} {
		if accepted[reject] {
			t.Errorf("unexpectedly accepted: %s", reject)
		}
	}
}

// TestDeriveLinks_NilProposerYieldsZeroLinks covers the "not wired yet"
// configuration where the Pipeline's LinkProposer field is nil — the
// write path must still function (AC3 spirit: link derivation failures
// never block the source commit).
func TestDeriveLinks_NilProposerYieldsZeroLinks(t *testing.T) {
	cands := []LinkCandidate{
		{TargetEntryID: "entry:X", CosineSimilarity: 0.99},
	}
	got := DeriveLinks(context.Background(), nil, "body", cands, defaultCfg())
	if len(got) != 0 {
		t.Fatalf("nil proposer accepted links: %d", len(got))
	}
}
