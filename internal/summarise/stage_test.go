package summarise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/claudecli"
)

// fakeRunner is a test-only Runner. Each Run consults the `respond`
// func against the prompt contents, so tests can return per-community
// schema-conforming outputs, simulate errors, and observe call order.
type fakeRunner struct {
	mu      sync.Mutex
	calls   int32
	respond func(req claudecli.Request) (claudecli.Response, error)
}

func (f *fakeRunner) Run(_ context.Context, req claudecli.Request) (claudecli.Response, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	r := f.respond
	f.mu.Unlock()
	return r(req)
}

func fixedTime() time.Time {
	return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
}

func mkEntry(id, body string) Entry {
	return Entry{ID: id, Kind: "Observation", Body: body, TS: fixedTime()}
}

func mkCommunity(id string, entryIDs []string) Community {
	entries := make([]Entry, len(entryIDs))
	for i, eid := range entryIDs {
		entries[i] = mkEntry(eid, "body of "+eid)
	}
	return Community{ID: CommunityID(id), EntryIDs: entryIDs, Entries: entries}
}

// communityBriefJSON renders a valid CommunityBrief payload with the
// supplied community_id + membership_hash echoed back. Tests use this
// to simulate a well-behaved LLM.
func communityBriefJSON(t *testing.T, communityID, hash, theme string, exemplar string) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"community_id":      communityID,
		"membership_hash":   hash,
		"theme_label":       theme,
		"canonical_insight": "Something an engineer can act on.",
		"exemplar_entry_id": exemplar,
		"summary":           "A summary long enough to satisfy the schema minLength of twenty characters.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func projectBriefJSON(t *testing.T) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"project":            "cortex",
		"generated_at":       fixedTime().UTC().Format(time.RFC3339),
		"community_ids":      []string{"c1", "c2"},
		"stitched_narrative": "A short map of the project that threads the themes together.",
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStage_SummariseFreshCommunities(t *testing.T) {
	runner := &fakeRunner{
		respond: func(req claudecli.Request) (claudecli.Response, error) {
			// Extract community_id from the prompt's MUST-set line so
			// the fake echoes the right value back in structured_output.
			switch {
			case strings.Contains(req.Prompt, "community_id=c1"):
				return claudecli.Response{
					StructuredOutput: communityBriefJSON(t, "c1", MembershipHash([]string{"e1", "e2"}), "ingest resilience", "e1"),
				}, nil
			case strings.Contains(req.Prompt, "community_id=c2"):
				return claudecli.Response{
					StructuredOutput: communityBriefJSON(t, "c2", MembershipHash([]string{"e3", "e4"}), "community detection", "e3"),
				}, nil
			case strings.Contains(req.Prompt, "ProjectBrief"):
				// Covered by the "else" — this branch is the stitch prompt.
				return claudecli.Response{StructuredOutput: projectBriefJSON(t)}, nil
			}
			return claudecli.Response{StructuredOutput: projectBriefJSON(t)}, nil
		},
	}
	stage, err := New(Config{Runner: runner, Now: fixedTime})
	if err != nil {
		t.Fatal(err)
	}
	communities := []Community{
		mkCommunity("c1", []string{"e1", "e2"}),
		mkCommunity("c2", []string{"e3", "e4"}),
	}
	report, err := stage.Summarise(context.Background(), "cortex", communities, nil)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if len(report.Communities) != 2 {
		t.Fatalf("expected 2 community results, got %d", len(report.Communities))
	}
	for _, r := range report.Communities {
		if r.Status != StatusSummarised {
			t.Errorf("community %s: status=%s want summarised", r.CommunityID, r.Status)
		}
	}
	// 2 community frames + 1 project brief = 3 frames total.
	if len(report.Frames) != 3 {
		t.Errorf("expected 3 frames (2 community + 1 stitch), got %d", len(report.Frames))
	}
	if report.ProjectBrief == nil {
		t.Error("ProjectBrief should be present")
	}
	if report.ProjectBrief.Slots["project"] != "cortex" {
		t.Errorf("stage should force project slot = \"cortex\", got %v", report.ProjectBrief.Slots["project"])
	}
	if got, _ := report.ProjectBrief.Slots["coverage_ratio"].(float64); got != 1.0 {
		t.Errorf("coverage_ratio = %v, want 1.0 (all 2/2 covered)", got)
	}
}

// Acceptance criterion (1): idle log = zero LLM calls. A second run
// over the same communities with prior briefs matching the current
// membership hash should SKIP every community and run only the
// stitch call.
func TestStage_IdempotentSkipOnUnchangedHash(t *testing.T) {
	var communityCalls int32
	runner := &fakeRunner{
		respond: func(req claudecli.Request) (claudecli.Response, error) {
			if strings.Contains(req.Prompt, "community_id=") {
				atomic.AddInt32(&communityCalls, 1)
			}
			return claudecli.Response{StructuredOutput: projectBriefJSON(t)}, nil
		},
	}
	stage, _ := New(Config{Runner: runner, Now: fixedTime})
	communities := []Community{
		mkCommunity("c1", []string{"e1", "e2"}),
		mkCommunity("c2", []string{"e3", "e4"}),
	}
	prior := map[CommunityID]PriorBrief{
		"c1": {CommunityID: "c1", MembershipHash: MembershipHash([]string{"e1", "e2"}), FrameID: "frame:c1:prior"},
		"c2": {CommunityID: "c2", MembershipHash: MembershipHash([]string{"e3", "e4"}), FrameID: "frame:c2:prior"},
	}
	report, err := stage.Summarise(context.Background(), "cortex", communities, prior)
	if err != nil {
		t.Fatalf("Summarise: %v", err)
	}
	if communityCalls != 0 {
		t.Errorf("expected zero per-community LLM calls with matching hashes; got %d", communityCalls)
	}
	for _, r := range report.Communities {
		if r.Status != StatusSkipped {
			t.Errorf("community %s: status=%s want skipped", r.CommunityID, r.Status)
		}
	}
	// No new community frames, but the stitch still ran.
	if len(report.Frames) != 1 {
		t.Errorf("expected 1 frame (stitch only), got %d", len(report.Frames))
	}
}

// Acceptance criterion (2): single entry change in one community
// causes exactly ONE per-community recompute + one stitch.
func TestStage_SingleCommunityRecomputeOnHashChange(t *testing.T) {
	var c1Calls, c2Calls int32
	runner := &fakeRunner{
		respond: func(req claudecli.Request) (claudecli.Response, error) {
			switch {
			case strings.Contains(req.Prompt, "community_id=c1"):
				atomic.AddInt32(&c1Calls, 1)
				return claudecli.Response{StructuredOutput: communityBriefJSON(t, "c1", MembershipHash([]string{"e1", "e2", "e5"}), "ingest", "e1")}, nil
			case strings.Contains(req.Prompt, "community_id=c2"):
				atomic.AddInt32(&c2Calls, 1)
				return claudecli.Response{StructuredOutput: communityBriefJSON(t, "c2", MembershipHash([]string{"e3", "e4"}), "community detection", "e3")}, nil
			}
			return claudecli.Response{StructuredOutput: projectBriefJSON(t)}, nil
		},
	}
	stage, _ := New(Config{Runner: runner, Now: fixedTime})
	communities := []Community{
		mkCommunity("c1", []string{"e1", "e2", "e5"}), // c1 gained e5 since last run
		mkCommunity("c2", []string{"e3", "e4"}),
	}
	prior := map[CommunityID]PriorBrief{
		"c1": {CommunityID: "c1", MembershipHash: MembershipHash([]string{"e1", "e2"})}, // stale
		"c2": {CommunityID: "c2", MembershipHash: MembershipHash([]string{"e3", "e4"})}, // current
	}
	report, _ := stage.Summarise(context.Background(), "cortex", communities, prior)
	if c1Calls != 1 {
		t.Errorf("c1 recompute: got %d calls, want 1", c1Calls)
	}
	if c2Calls != 0 {
		t.Errorf("c2 should have been skipped: got %d calls", c2Calls)
	}
	// Map results by id so order doesn't matter (workers race).
	byID := map[CommunityID]Status{}
	for _, r := range report.Communities {
		byID[r.CommunityID] = r.Status
	}
	if byID["c1"] != StatusSummarised {
		t.Errorf("c1 status: got %s want summarised", byID["c1"])
	}
	if byID["c2"] != StatusSkipped {
		t.Errorf("c2 status: got %s want skipped", byID["c2"])
	}
}

// Acceptance criterion (4): per-community failure does not abort the
// pass. The stitch still runs over whatever succeeded + any prior
// briefs we can carry over.
func TestStage_PerCommunityFailureIsolated(t *testing.T) {
	runner := &fakeRunner{
		respond: func(req claudecli.Request) (claudecli.Response, error) {
			switch {
			case strings.Contains(req.Prompt, "community_id=c1"):
				return claudecli.Response{}, fmt.Errorf("transient network: %w", claudecli.ErrRateLimited)
			case strings.Contains(req.Prompt, "community_id=c2"):
				return claudecli.Response{StructuredOutput: communityBriefJSON(t, "c2", MembershipHash([]string{"e3", "e4"}), "analytics", "e3")}, nil
			}
			return claudecli.Response{StructuredOutput: projectBriefJSON(t)}, nil
		},
	}
	stage, _ := New(Config{Runner: runner, Now: fixedTime})
	communities := []Community{
		mkCommunity("c1", []string{"e1", "e2"}),
		mkCommunity("c2", []string{"e3", "e4"}),
	}
	report, err := stage.Summarise(context.Background(), "cortex", communities, nil)
	if err != nil {
		t.Fatalf("Summarise should not return an error on per-community failure: %v", err)
	}
	byID := map[CommunityID]CommunityResult{}
	for _, r := range report.Communities {
		byID[r.CommunityID] = r
	}
	if byID["c1"].Status != StatusFailed {
		t.Errorf("c1 status: got %s want failed", byID["c1"].Status)
	}
	if !errors.Is(byID["c1"].Err, claudecli.ErrRateLimited) {
		t.Errorf("c1 err should wrap ErrRateLimited: got %v", byID["c1"].Err)
	}
	if byID["c2"].Status != StatusSummarised {
		t.Errorf("c2 status: got %s want summarised", byID["c2"].Status)
	}
	// Stitch still happened, over c2 only.
	if report.ProjectBrief == nil {
		t.Error("stitch should run over the one successful community")
	}
	if report.StitchErr != nil {
		t.Errorf("stitch should not have failed: %v", report.StitchErr)
	}
	// Coverage is 1/2 (one community summarised, one failed with no prior).
	if got, _ := report.ProjectBrief.Slots["coverage_ratio"].(float64); got != 0.5 {
		t.Errorf("coverage_ratio: got %v want 0.5", got)
	}
}

func TestStage_RejectsHashMismatchFromModel(t *testing.T) {
	runner := &fakeRunner{
		respond: func(req claudecli.Request) (claudecli.Response, error) {
			// Model returns a mismatched membership_hash ("model
			// hallucination"); stage must reject the brief rather
			// than pollute the log with a wrong-hash CommunityBrief.
			return claudecli.Response{StructuredOutput: communityBriefJSON(t, "c1", "deadbeef"+strings.Repeat("0", 56), "x", "e1")}, nil
		},
	}
	stage, _ := New(Config{Runner: runner, Now: fixedTime})
	communities := []Community{mkCommunity("c1", []string{"e1"})}
	report, _ := stage.Summarise(context.Background(), "cortex", communities, nil)
	if len(report.Communities) != 1 {
		t.Fatalf("want 1 result, got %d", len(report.Communities))
	}
	r := report.Communities[0]
	if r.Status != StatusFailed {
		t.Errorf("status: got %s want failed", r.Status)
	}
	if r.Err == nil || !strings.Contains(r.Err.Error(), "membership_hash") {
		t.Errorf("err should mention membership_hash mismatch: %v", r.Err)
	}
}

func TestStage_ProjectRequired(t *testing.T) {
	stage, _ := New(Config{Runner: &fakeRunner{}, Now: fixedTime})
	_, err := stage.Summarise(context.Background(), "", nil, nil)
	if err == nil {
		t.Fatal("empty project should return an error")
	}
}

func TestStage_RequiresRunner(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("Config without Runner should error")
	}
}

func TestMembershipHash_OrderIndependent(t *testing.T) {
	a := MembershipHash([]string{"z", "a", "m"})
	b := MembershipHash([]string{"a", "m", "z"})
	if a != b {
		t.Errorf("hash should be order-independent: %s vs %s", a, b)
	}
	// Sanity: different sets hash differently.
	c := MembershipHash([]string{"a", "m"})
	if a == c {
		t.Error("different sets must hash differently")
	}
}
