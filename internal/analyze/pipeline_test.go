package analyze

import (
	"context"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/datom"
)

// --- fakes -----------------------------------------------------------

type fakeSource struct {
	candidates []ClusterCandidate
	err        error
}

func (f *fakeSource) Candidates(_ context.Context) ([]ClusterCandidate, error) {
	return f.candidates, f.err
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
	if f.frame == nil {
		return nil, nil
	}
	fr := *f.frame
	fr.FrameID = "frame:" + c.ID
	exs := make([]string, 0, len(c.Exemplars))
	for _, e := range c.Exemplars {
		exs = append(exs, e.EntryID)
	}
	fr.Exemplars = exs
	return &fr, nil
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

type fakeCommunity struct {
	refreshCalls int
}

func (f *fakeCommunity) Refresh(_ context.Context) error {
	f.refreshCalls++
	return nil
}

func newPipeline(src ClusterSource, prop FrameProposer, log LogAppender, com CommunityRefresher) *Pipeline {
	now := time.Unix(1_700_000_000, 0).UTC()
	return &Pipeline{
		Source:       src,
		Proposer:     prop,
		Log:          log,
		Community:    com,
		Now:          func() time.Time { return now },
		Actor:        "test",
		InvocationID: "01HTESTINVOCATION0000000000",
	}
}

// crossProjectCluster returns a cluster with balanced exemplars across
// three projects — 4/3/3 is both above min_projects and under the
// 70% share cap.
func crossProjectCluster(id string) ClusterCandidate {
	exs := []ExemplarRef{
		{EntryID: "entry:A1", Project: "projA"},
		{EntryID: "entry:A2", Project: "projA"},
		{EntryID: "entry:A3", Project: "projA"},
		{EntryID: "entry:A4", Project: "projA"},
		{EntryID: "entry:B1", Project: "projB"},
		{EntryID: "entry:B2", Project: "projB"},
		{EntryID: "entry:B3", Project: "projB"},
		{EntryID: "entry:C1", Project: "projC"},
		{EntryID: "entry:C2", Project: "projC"},
		{EntryID: "entry:C3", Project: "projC"},
	}
	return ClusterCandidate{ID: id, Exemplars: exs, MDLRatio: 1.3}
}

// --- tests -----------------------------------------------------------

// TestAnalyze_SingleProjectRejected covers AC1: a cluster whose
// exemplars all come from one project is rejected with reason
// SINGLE_PROJECT.
func TestAnalyze_SingleProjectRejected(t *testing.T) {
	c := ClusterCandidate{
		ID: "c1",
		Exemplars: []ExemplarRef{
			{EntryID: "entry:A", Project: "projA"},
			{EntryID: "entry:B", Project: "projA"},
			{EntryID: "entry:C", Project: "projA"},
		},
		MDLRatio: 1.3,
	}
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern"}}
	log := &fakeLog{}
	com := &fakeCommunity{}
	p := newPipeline(src, prop, log, com)

	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("outcomes: got %d want 1", len(res.Outcomes))
	}
	if res.Outcomes[0].Accepted {
		t.Fatalf("single-project cluster accepted")
	}
	if res.Outcomes[0].Reason != ReasonSingleProject {
		t.Fatalf("reason: got %s want SINGLE_PROJECT", res.Outcomes[0].Reason)
	}
	if prop.calls != 0 {
		t.Fatalf("proposer invoked on rejected cluster")
	}
	if len(log.groups) != 0 {
		t.Fatalf("log touched on rejected cluster")
	}
	if com.refreshCalls != 0 {
		t.Fatalf("community refresh on zero accepted: %d", com.refreshCalls)
	}
}

// TestAnalyze_ProjectShareExceededRejected covers AC2: 8 of 10
// exemplars from one project → PROJECT_SHARE_EXCEEDED (8/10 = 0.80 > 0.70).
func TestAnalyze_ProjectShareExceededRejected(t *testing.T) {
	exs := make([]ExemplarRef, 0, 10)
	for i := 0; i < 8; i++ {
		exs = append(exs, ExemplarRef{EntryID: "entry:A", Project: "projA"})
	}
	exs = append(exs,
		ExemplarRef{EntryID: "entry:B1", Project: "projB"},
		ExemplarRef{EntryID: "entry:B2", Project: "projB"},
	)
	c := ClusterCandidate{ID: "c1", Exemplars: exs, MDLRatio: 1.3}
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "Principle"}}
	p := newPipeline(src, prop, &fakeLog{}, nil)

	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Outcomes[0].Reason != ReasonProjectShareExceeded {
		t.Fatalf("reason: got %s want PROJECT_SHARE_EXCEEDED", res.Outcomes[0].Reason)
	}
	if prop.calls != 0 {
		t.Fatalf("proposer invoked on over-share cluster")
	}
}

// TestAnalyze_AcceptedFrameHasCrossProjectAndMultiProjectDerivedFrom
// covers AC3: accepted cross-project frames carry cross_project=true
// and their DERIVED_FROM edges reference entries in ≥2 distinct
// projects.
func TestAnalyze_AcceptedFrameHasCrossProjectAndMultiProjectDerivedFrom(t *testing.T) {
	c := crossProjectCluster("c1")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "Principle", Importance: 0.3}}
	log := &fakeLog{}
	com := &fakeCommunity{}
	p := newPipeline(src, prop, log, com)

	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("accepted: got %d want 1", len(res.Accepted))
	}
	accepted := res.Accepted[0]
	if len(accepted.Projects) < 2 {
		t.Fatalf("accepted frame projects: got %d want ≥2", len(accepted.Projects))
	}
	// Importance boosted by +0.20 over proposer value (0.3 → 0.5).
	if accepted.Importance < 0.49 || accepted.Importance > 0.51 {
		t.Fatalf("importance boost: got %f want ≈0.5", accepted.Importance)
	}

	// Inspect the emitted datom group.
	if len(log.groups) != 1 {
		t.Fatalf("log groups: got %d want 1", len(log.groups))
	}
	group := log.groups[0]
	hasCrossProject := false
	derivedCount := 0
	for _, d := range group {
		switch d.A {
		case "frame.cross_project":
			hasCrossProject = true
			if string(d.V) != "true" {
				t.Errorf("cross_project value: got %s want true", string(d.V))
			}
		case "DERIVED_FROM":
			derivedCount++
		}
	}
	if !hasCrossProject {
		t.Fatalf("frame.cross_project datom missing")
	}
	if derivedCount != len(c.Exemplars) {
		t.Fatalf("DERIVED_FROM count: got %d want %d", derivedCount, len(c.Exemplars))
	}
	// Every datom must be sealed and share the same tx.
	tx := group[0].Tx
	for _, d := range group {
		if d.Tx != tx {
			t.Fatalf("tx mismatch in frame group")
		}
		if d.Checksum == "" {
			t.Fatalf("datom not sealed: %+v", d)
		}
		if d.Src != "analyze" {
			t.Errorf("src: got %s want analyze", d.Src)
		}
	}
	// Community refresh must run after the write.
	if com.refreshCalls != 1 {
		t.Fatalf("community refresh: got %d want 1", com.refreshCalls)
	}
	if !res.CommunityRefresh {
		t.Fatalf("result.CommunityRefresh not set")
	}
}

// TestAnalyze_MigratedEntriesExcludedByDefault covers AC4: migrated
// entries are excluded unless --include-migrated is passed. We build
// a cluster whose cross-project signal only exists via migrated
// exemplars, so filtering them out collapses the cluster to a single
// project and triggers SINGLE_PROJECT (or empties it entirely).
func TestAnalyze_MigratedEntriesExcludedByDefault(t *testing.T) {
	c := ClusterCandidate{
		ID: "c1",
		Exemplars: []ExemplarRef{
			{EntryID: "entry:A1", Project: "projA"},
			{EntryID: "entry:A2", Project: "projA"},
			{EntryID: "entry:A3", Project: "projA"},
			// These two would satisfy cross-project, but they are migrated.
			{EntryID: "entry:B1", Project: "projB", Migrated: true},
			{EntryID: "entry:B2", Project: "projB", Migrated: true},
		},
		MDLRatio: 1.3,
	}
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: &Frame{Type: "BugPattern"}}
	p := newPipeline(src, prop, &fakeLog{}, nil)

	// Default run: migrated filtered out, cluster collapses to one project.
	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze default: %v", err)
	}
	if res.Outcomes[0].Accepted {
		t.Fatalf("cluster accepted when migrated should have been filtered")
	}
	if res.Outcomes[0].Reason != ReasonSingleProject {
		t.Fatalf("default-mode reason: got %s want SINGLE_PROJECT",
			res.Outcomes[0].Reason)
	}

	// Same cluster with --include-migrated: should accept.
	prop2 := &fakeProposer{frame: &Frame{Type: "BugPattern"}}
	p2 := newPipeline(src, prop2, &fakeLog{}, &fakeCommunity{})
	res2, err := p2.Analyze(context.Background(), RunOptions{IncludeMigrated: true})
	if err != nil {
		t.Fatalf("Analyze --include-migrated: %v", err)
	}
	if len(res2.Accepted) != 1 {
		t.Fatalf("--include-migrated: accepted %d want 1", len(res2.Accepted))
	}
}

// TestAnalyze_BelowMDLRatioRejected covers the relaxed MDL threshold
// check (1.15 by default, lower than reflect's 1.3).
func TestAnalyze_BelowMDLRatioRejected(t *testing.T) {
	c := crossProjectCluster("c1")
	c.MDLRatio = 1.10 // below 1.15 relaxed floor
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	p := newPipeline(src, &fakeProposer{frame: &Frame{Type: "X"}}, &fakeLog{}, nil)
	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Outcomes[0].Reason != ReasonBelowAnalysisMDLRatio {
		t.Fatalf("reason: got %s want BELOW_ANALYSIS_MDL_RATIO",
			res.Outcomes[0].Reason)
	}
}

// TestAnalyze_LLMRejectedRecordsReason covers the proposer-said-no path.
func TestAnalyze_LLMRejectedRecordsReason(t *testing.T) {
	c := crossProjectCluster("c1")
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	prop := &fakeProposer{frame: nil}
	p := newPipeline(src, prop, &fakeLog{}, nil)
	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Outcomes[0].Reason != ReasonLLMRejected {
		t.Fatalf("reason: got %s want LLM_REJECTED", res.Outcomes[0].Reason)
	}
}

// TestAnalyze_DryRunWritesNothing confirms --dry-run skips Append and
// community refresh while still producing outcomes.
func TestAnalyze_DryRunWritesNothing(t *testing.T) {
	src := &fakeSource{candidates: []ClusterCandidate{crossProjectCluster("c1")}}
	prop := &fakeProposer{frame: &Frame{Type: "Principle"}}
	log := &fakeLog{}
	com := &fakeCommunity{}
	p := newPipeline(src, prop, log, com)
	res, err := p.Analyze(context.Background(), RunOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Analyze dry-run: %v", err)
	}
	if len(res.Accepted) != 1 {
		t.Fatalf("dry-run accepted: got %d want 1", len(res.Accepted))
	}
	if len(log.groups) != 0 {
		t.Fatalf("dry-run wrote datoms: %d groups", len(log.groups))
	}
	if com.refreshCalls != 0 {
		t.Fatalf("dry-run triggered community refresh: %d", com.refreshCalls)
	}
}

// TestAnalyze_EmptyAfterMigratedFilter covers the edge case where the
// cluster becomes empty after filtering migrated exemplars.
func TestAnalyze_EmptyAfterMigratedFilter(t *testing.T) {
	c := ClusterCandidate{
		ID: "c1",
		Exemplars: []ExemplarRef{
			{EntryID: "entry:X", Project: "projA", Migrated: true},
			{EntryID: "entry:Y", Project: "projB", Migrated: true},
		},
		MDLRatio: 1.3,
	}
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	p := newPipeline(src, &fakeProposer{frame: &Frame{Type: "X"}}, &fakeLog{}, nil)
	res, err := p.Analyze(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if res.Outcomes[0].Reason != ReasonEmptyAfterFilter {
		t.Fatalf("reason: got %s want EMPTY_AFTER_MIGRATED_FILTER",
			res.Outcomes[0].Reason)
	}
}

// TestAnalyze_CommunityRefreshOnlyWhenAccepted confirms the refresh
// does not fire when every candidate is rejected (prevents wasted
// graph work on empty runs).
func TestAnalyze_CommunityRefreshOnlyWhenAccepted(t *testing.T) {
	c := ClusterCandidate{
		ID: "c1",
		Exemplars: []ExemplarRef{
			{EntryID: "entry:A", Project: "projA"},
			{EntryID: "entry:B", Project: "projA"},
		},
		MDLRatio: 1.3,
	}
	src := &fakeSource{candidates: []ClusterCandidate{c}}
	com := &fakeCommunity{}
	p := newPipeline(src, &fakeProposer{frame: &Frame{Type: "X"}}, &fakeLog{}, com)
	if _, err := p.Analyze(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if com.refreshCalls != 0 {
		t.Fatalf("refresh ran on empty-accept run: %d", com.refreshCalls)
	}
}
