package reflect

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/datom"
)

// --- fakes -----------------------------------------------------------

type fakeSource struct {
	candidates []ClusterCandidate
	err        error
	calls      int
	lastSince  string
}

func (f *fakeSource) Candidates(_ context.Context, sinceTx string) ([]ClusterCandidate, error) {
	f.calls++
	f.lastSince = sinceTx
	if f.err != nil {
		return nil, f.err
	}
	// Filter candidates whose youngest exemplar tx is > sinceTx so
	// the resume test can simulate the watermark advancement.
	if sinceTx == "" {
		return f.candidates, nil
	}
	var out []ClusterCandidate
	for _, c := range f.candidates {
		if youngestTx(c) > sinceTx {
			out = append(out, c)
		}
	}
	return out, nil
}

func youngestTx(c ClusterCandidate) string {
	var max string
	for _, e := range c.Exemplars {
		if e.Tx > max {
			max = e.Tx
		}
	}
	return max
}

type fakeProposer struct {
	frame *Frame
	err   error
	calls int
}

func (f *fakeProposer) Propose(_ context.Context, c ClusterCandidate) (*Frame, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.frame != nil {
		// Return a per-cluster frame so each accepted cluster gets a
		// distinct frame id and exemplar list.
		fr := *f.frame
		fr.FrameID = "frame:" + c.ID
		exs := make([]string, 0, len(c.Exemplars))
		for _, e := range c.Exemplars {
			exs = append(exs, e.EntryID)
		}
		fr.Exemplars = exs
		return &fr, nil
	}
	return nil, nil
}

type fakeWatermark struct {
	value   string
	writes  []string
	readErr error
}

func (f *fakeWatermark) ReadReflectionWatermark(_ context.Context) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.value, nil
}
func (f *fakeWatermark) WriteReflectionWatermark(_ context.Context, tx string) error {
	f.writes = append(f.writes, tx)
	f.value = tx
	return nil
}

type fakeLog struct {
	groups [][]datom.Datom
}

func (f *fakeLog) Append(group []datom.Datom) (string, error) {
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	if len(group) > 0 {
		return group[0].Tx, nil
	}
	return "", nil
}

// --- helpers ---------------------------------------------------------

func qualifyingCluster(id string, exemplars int, baseTx string) ClusterCandidate {
	exs := make([]ExemplarRef, exemplars)
	for i := 0; i < exemplars; i++ {
		exs[i] = ExemplarRef{
			EntryID:   "entry:" + id + "-" + string(rune('A'+i)),
			Timestamp: time.Unix(int64(1_700_000_000+i*100), 0).UTC(),
			Tx:        baseTx + "-" + string(rune('A'+i)),
		}
	}
	return ClusterCandidate{
		ID:                    id,
		Exemplars:             exs,
		AveragePairwiseCosine: 0.80, // > floor 0.65
		DistinctTimestamps:    exemplars,
		MDLRatio:              1.5, // > 1.3
	}
}

func newPipeline(src ClusterSource, prop FrameProposer, wm WatermarkStore, log LogAppender) *Pipeline {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &Pipeline{
		Source:       src,
		Proposer:     prop,
		Watermark:    wm,
		Log:          log,
		Now:          func() time.Time { return now },
		Actor:        "test",
		InvocationID: "01HTESTINVOCATION0000000000",
	}
}

// --- tests -----------------------------------------------------------

// TestReflect_TwoExemplarClusterRejected covers AC1: a 2-exemplar
// cluster is rejected with reason BELOW_MIN_CLUSTER_SIZE.
func TestReflect_TwoExemplarClusterRejected(t *testing.T) {
	c := qualifyingCluster("c1", 2, "01TX")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{"name": "x"}}}
	wm := &fakeWatermark{}
	log := &fakeLog{}
	p := newPipeline(src, prop, wm, log)

	res, err := p.Reflect(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("outcomes: got %d want 1", len(res.Outcomes))
	}
	if res.Outcomes[0].Accepted {
		t.Fatalf("2-exemplar cluster accepted")
	}
	if res.Outcomes[0].Reason != ReasonBelowMinClusterSize {
		t.Fatalf("reason: got %s want BELOW_MIN_CLUSTER_SIZE", res.Outcomes[0].Reason)
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on rejected cluster")
	}
	if prop.calls != 0 {
		t.Fatalf("proposer invoked on rejected cluster: %d calls", prop.calls)
	}
}

// TestReflect_QualifyingClusterEmitsDerivedFromEdges covers AC2: a
// qualifying cluster produces a frame with DERIVED_FROM edges to
// every exemplar.
func TestReflect_QualifyingClusterEmitsDerivedFromEdges(t *testing.T) {
	c := qualifyingCluster("c1", 4, "01TX")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{"name": "p"}}}
	wm := &fakeWatermark{}
	log := &fakeLog{}
	p := newPipeline(src, prop, wm, log)

	res, err := p.Reflect(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted: got %d want 1", len(res.Accepted))
	}
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
	derived := 0
	for _, d := range log.groups[0] {
		if d.A == "DERIVED_FROM" {
			derived++
		}
	}
	if derived != 4 {
		t.Fatalf("DERIVED_FROM edges: got %d want 4", derived)
	}
	// Every datom in the group must share one tx and be sealed.
	tx := log.groups[0][0].Tx
	for _, d := range log.groups[0] {
		if d.Tx != tx {
			t.Fatalf("tx mismatch in frame group: %s vs %s", d.Tx, tx)
		}
		if d.Checksum == "" {
			t.Fatalf("frame datom not sealed: %+v", d)
		}
		if err := d.Verify(); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
}

// TestReflect_ResumeAfterInterrupt covers AC3: an interrupted run
// that wrote 3 of 5 proposed frames resumes on the next invocation
// without reprocessing the 3 already-written frames. We simulate the
// interrupt by running the pipeline once with only the first 3
// candidates, then running it again with all 5 — the resume call
// should only process the trailing 2.
func TestReflect_ResumeAfterInterrupt(t *testing.T) {
	all := []ClusterCandidate{
		qualifyingCluster("c1", 3, "01TX01"),
		qualifyingCluster("c2", 3, "01TX02"),
		qualifyingCluster("c3", 3, "01TX03"),
		qualifyingCluster("c4", 3, "01TX04"),
		qualifyingCluster("c5", 3, "01TX05"),
	}
	wm := &fakeWatermark{}

	// First pass: only the first 3 are visible (simulate that the
	// run was killed before c4/c5 were observed). The shared
	// watermark store records the writes.
	src1 := &fakeSource{candidates: all[:3]}
	prop1 := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{}}}
	log1 := &fakeLog{}
	p1 := newPipeline(src1, prop1, wm, log1)
	res1, err := p1.Reflect(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("first Reflect: %v", err)
	}
	if len(res1.Accepted) != 3 {
		t.Fatalf("first pass: accepted %d want 3", len(res1.Accepted))
	}
	if len(wm.writes) != 3 {
		t.Fatalf("first pass: watermark writes %d want 3 (per-frame)", len(wm.writes))
	}
	// Watermark should now point at the youngest tx of c3 (one of
	// its exemplars), which is the latest log Append return.
	firstFinalWatermark := wm.value
	if firstFinalWatermark == "" {
		t.Fatalf("watermark not advanced after first pass")
	}

	// Second pass: source has all 5 but filters by sinceTx so only
	// the un-watermarked ones come back. The proposer's call count
	// proves nothing from c1/c2/c3 is reprocessed.
	src2 := &fakeSource{candidates: all}
	prop2 := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{}}}
	log2 := &fakeLog{}
	p2 := newPipeline(src2, prop2, wm, log2)
	res2, err := p2.Reflect(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("resume Reflect: %v", err)
	}
	if src2.lastSince == "" {
		t.Fatalf("source did not receive a sinceTx on resume")
	}
	if prop2.calls > 2 {
		t.Fatalf("resume reprocessed already-written candidates: %d proposer calls", prop2.calls)
	}
	if len(res2.Accepted) != 2 {
		t.Fatalf("resume accepted %d want 2", len(res2.Accepted))
	}
}

// TestReflect_DryRunExplainWritesNothing covers AC4: cortex reflect
// --dry-run --explain prints candidate clusters and rejection
// reasons and writes zero datoms.
func TestReflect_DryRunExplainWritesNothing(t *testing.T) {
	candidates := []ClusterCandidate{
		qualifyingCluster("good1", 4, "01TX01"),
		qualifyingCluster("good2", 5, "01TX02"),
		qualifyingCluster("smol", 2, "01TX03"), // BELOW_MIN_CLUSTER_SIZE
	}
	src := &fakeSource{candidates: candidates}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{}}}
	wm := &fakeWatermark{}
	log := &fakeLog{}
	p := newPipeline(src, prop, wm, log)

	res, err := p.Reflect(context.Background(), RunOptions{DryRun: true, Explain: true})
	if err != nil {
		t.Fatalf("Reflect dry-run: %v", err)
	}
	if len(log.groups) != 0 {
		t.Fatalf("dry run wrote datoms: %d groups", len(log.groups))
	}
	if len(wm.writes) != 0 {
		t.Fatalf("dry run advanced watermark: %d writes", len(wm.writes))
	}
	if len(res.Outcomes) != 3 {
		t.Fatalf("outcomes: got %d want 3", len(res.Outcomes))
	}
	// Two qualifying clusters are accepted (in-memory only), one
	// rejected with the size reason.
	accepted := 0
	rejectedReason := ""
	for _, o := range res.Outcomes {
		if o.Accepted {
			accepted++
		} else {
			rejectedReason = string(o.Reason)
		}
	}
	if accepted != 2 {
		t.Fatalf("dry-run accepted: got %d want 2", accepted)
	}
	if rejectedReason != string(ReasonBelowMinClusterSize) {
		t.Fatalf("rejection reason: got %s", rejectedReason)
	}
}

// TestReflect_BelowCosineFloorRejected covers the cosine threshold path.
func TestReflect_BelowCosineFloorRejected(t *testing.T) {
	c := qualifyingCluster("c1", 4, "01TX")
	c.AveragePairwiseCosine = 0.50 // below 0.65 floor
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "X"}}
	p := newPipeline(src, prop, &fakeWatermark{}, &fakeLog{})
	res, err := p.Reflect(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if res.Outcomes[0].Reason != ReasonBelowCosineFloor {
		t.Fatalf("reason: got %s want BELOW_COSINE_FLOOR", res.Outcomes[0].Reason)
	}
}

// TestReflect_BelowMDLRatioRejected covers the MDL threshold path.
func TestReflect_BelowMDLRatioRejected(t *testing.T) {
	c := qualifyingCluster("c1", 4, "01TX")
	c.MDLRatio = 1.0 // below 1.3 floor
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	p := newPipeline(src, &fakeProposer{frame: &Frame{Type: "X"}}, &fakeWatermark{}, &fakeLog{})
	res, _ := p.Reflect(context.Background(), RunOptions{})
	if res.Outcomes[0].Reason != ReasonBelowMDLRatio {
		t.Fatalf("reason: got %s want BELOW_MDL_RATIO", res.Outcomes[0].Reason)
	}
}

// TestReflect_LLMRejectionRecordsReason covers the proposer-said-no path.
func TestReflect_LLMRejectionRecordsReason(t *testing.T) {
	c := qualifyingCluster("c1", 4, "01TX")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: nil} // nil = no frame
	res, _ := newPipeline(src, prop, &fakeWatermark{}, &fakeLog{}).
		Reflect(context.Background(), RunOptions{})
	if res.Outcomes[0].Reason != ReasonLLMRejected {
		t.Fatalf("reason: got %s want LLM_REJECTED", res.Outcomes[0].Reason)
	}
}

// TestReflect_InsufficientTimestampsRejected covers the
// distinct-timestamps threshold path.
func TestReflect_InsufficientTimestampsRejected(t *testing.T) {
	c := qualifyingCluster("c1", 4, "01TX")
	c.DistinctTimestamps = 1
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	res, _ := newPipeline(src, &fakeProposer{frame: &Frame{Type: "X"}}, &fakeWatermark{}, &fakeLog{}).
		Reflect(context.Background(), RunOptions{})
	if res.Outcomes[0].Reason != ReasonInsufficientTimestamps {
		t.Fatalf("reason: got %s want INSUFFICIENT_TIMESTAMPS", res.Outcomes[0].Reason)
	}
}

// TestReflect_FrameSchemaVersionRecorded asserts every accepted frame
// carries a frame.schema_version datom.
func TestReflect_FrameSchemaVersionRecorded(t *testing.T) {
	c := qualifyingCluster("c1", 3, "01TX")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern", Slots: map[string]any{}}}
	log := &fakeLog{}
	p := newPipeline(src, prop, &fakeWatermark{}, log)
	if _, err := p.Reflect(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	found := false
	for _, d := range log.groups[0] {
		if d.A == "frame.schema_version" {
			found = true
			if !strings.Contains(string(d.V), FrameSchemaVersion) {
				t.Fatalf("schema version: %s", string(d.V))
			}
		}
	}
	if !found {
		t.Fatalf("frame.schema_version datom missing")
	}
}
