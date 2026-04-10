package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nixlim/cortex/internal/languages"
)

// --- fakes -----------------------------------------------------------

// fakeWalker replays a fixed file list. The project root is ignored —
// tests supply the exact files they want grouped.
type fakeWalker struct{ files []languages.File }

func (f *fakeWalker) walk(_ string, fn func(languages.File) error) error {
	for _, file := range f.files {
		if err := fn(file); err != nil {
			return err
		}
	}
	return nil
}

type fakeSummarizer struct {
	calls int
	err   error
}

func (f *fakeSummarizer) Summarize(_ context.Context, req SummaryRequest) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "summary of " + req.Module.ID, nil
}

type fakeWriter struct {
	writes []EntryRequest
	err    error
}

func (f *fakeWriter) WriteModule(_ context.Context, req EntryRequest) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.writes = append(f.writes, req)
	return "entry:" + req.ModuleID, nil
}

type fakeTrail struct {
	appended []TrailRequest
}

func (f *fakeTrail) AppendTrail(_ context.Context, req TrailRequest) error {
	f.appended = append(f.appended, req)
	return nil
}

type fakeState struct {
	byProject map[string]ProjectState
	writes    int
}

func (f *fakeState) Read(_ context.Context, project string) (ProjectState, bool, error) {
	if f.byProject == nil {
		return ProjectState{}, false, nil
	}
	s, ok := f.byProject[project]
	return s, ok, nil
}

func (f *fakeState) Write(_ context.Context, s ProjectState) error {
	if f.byProject == nil {
		f.byProject = map[string]ProjectState{}
	}
	f.byProject[s.ProjectName] = s
	f.writes++
	return nil
}

type fakeReflect struct {
	scopes []string
	err    error
}

func (f *fakeReflect) ReflectScope(_ context.Context, trailID string) error {
	f.scopes = append(f.scopes, trailID)
	return f.err
}

// --- helpers ---------------------------------------------------------

// threePackageGoFixture returns the file list of a Go project with
// three packages: foo, bar, and baz. languages.Group with the default
// matrix produces one module per package (per-package strategy).
func threePackageGoFixture() []languages.File {
	return []languages.File{
		{AbsPath: "/root/foo/a.go", RelPath: "foo/a.go"},
		{AbsPath: "/root/foo/b.go", RelPath: "foo/b.go"},
		{AbsPath: "/root/bar/c.go", RelPath: "bar/c.go"},
		{AbsPath: "/root/baz/d.go", RelPath: "baz/d.go"},
	}
}

func newTestPipeline(files []languages.File) (*Pipeline, *fakeSummarizer, *fakeWriter, *fakeTrail, *fakeState, *fakeReflect) {
	w := &fakeWalker{files: files}
	sum := &fakeSummarizer{}
	wr := &fakeWriter{}
	tr := &fakeTrail{}
	st := &fakeState{}
	rf := &fakeReflect{}
	now := time.Unix(1_700_000_000, 0).UTC()
	p := &Pipeline{
		Walker:        w.walk,
		Matrix:        languages.DefaultMatrix(),
		Summarizer:    sum,
		Writer:        wr,
		TrailAppender: tr,
		StateStore:    st,
		PostReflect:   rf,
		Now:           func() time.Time { return now },
		Concurrency:   2,
	}
	return p, sum, wr, tr, st, rf
}

// --- tests -----------------------------------------------------------

// TestIngest_ThreePackagesYieldThreeEntriesAndOneTrail covers AC1.
func TestIngest_ThreePackagesYieldThreeEntriesAndOneTrail(t *testing.T) {
	p, _, wr, tr, st, _ := newTestPipeline(threePackageGoFixture())
	res, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.Modules) != 3 {
		t.Fatalf("modules grouped: got %d want 3", len(res.Modules))
	}
	if len(res.EntryIDs) != 3 {
		t.Fatalf("entries written: got %d want 3", len(res.EntryIDs))
	}
	if len(wr.writes) != 3 {
		t.Fatalf("writer calls: got %d want 3", len(wr.writes))
	}
	if len(tr.appended) != 1 {
		t.Fatalf("trail append: got %d want 1", len(tr.appended))
	}
	if !strings.HasPrefix(tr.appended[0].TrailID, "trail:ingest:fixture:") {
		t.Fatalf("trail id: %s", tr.appended[0].TrailID)
	}
	if len(tr.appended[0].EntryIDs) != 3 {
		t.Fatalf("trail entry ids: got %d want 3", len(tr.appended[0].EntryIDs))
	}
	// State must record all three module ids for future idempotence.
	saved := st.byProject["fixture"]
	if len(saved.CompletedModuleIDs) != 3 {
		t.Fatalf("state completed ids: got %d want 3", len(saved.CompletedModuleIDs))
	}
	if saved.TotalEntriesWritten != 3 {
		t.Fatalf("state total entries: got %d want 3", saved.TotalEntriesWritten)
	}
}

// TestIngest_ReRunWritesNothing covers AC2: running ingest twice
// against an unchanged project writes zero new entries on the second
// run.
func TestIngest_ReRunWritesNothing(t *testing.T) {
	p, sum, wr, tr, st, _ := newTestPipeline(threePackageGoFixture())
	req := Request{ProjectRoot: "/root", ProjectName: "fixture"}

	if _, err := p.Ingest(context.Background(), req); err != nil {
		t.Fatalf("first run: %v", err)
	}
	firstWrites := len(wr.writes)
	firstTrails := len(tr.appended)
	firstSummaries := sum.calls

	res2, err := p.Ingest(context.Background(), req)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res2.EntryIDs) != 0 {
		t.Fatalf("second run wrote entries: got %d want 0", len(res2.EntryIDs))
	}
	if len(wr.writes) != firstWrites {
		t.Fatalf("second run called writer: got %d delta", len(wr.writes)-firstWrites)
	}
	if len(tr.appended) != firstTrails {
		t.Fatalf("second run appended trail: got %d delta", len(tr.appended)-firstTrails)
	}
	if sum.calls != firstSummaries {
		t.Fatalf("second run summarized: got %d delta", sum.calls-firstSummaries)
	}
	if len(res2.SkippedModules) != 3 {
		t.Fatalf("skipped modules: got %d want 3", len(res2.SkippedModules))
	}
	if st.byProject["fixture"].TotalEntriesWritten != 3 {
		t.Fatalf("state total should stay at 3 after no-op run, got %d",
			st.byProject["fixture"].TotalEntriesWritten)
	}
}

// TestIngest_StatusReportsLastRun covers AC3: cortex ingest status
// reports the last ingested commit SHA, timestamp, and counts.
func TestIngest_StatusReportsLastRun(t *testing.T) {
	p, _, _, _, st, _ := newTestPipeline(threePackageGoFixture())
	req := Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
		CommitSHA:   "abc123",
	}
	if _, err := p.Ingest(context.Background(), req); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	state, ok, err := p.Status(context.Background(), "fixture")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !ok {
		t.Fatalf("Status: want ok=true")
	}
	if state.LastCommitSHA != "abc123" {
		t.Fatalf("LastCommitSHA: got %s want abc123", state.LastCommitSHA)
	}
	if state.LastIngestedAt.IsZero() {
		t.Fatalf("LastIngestedAt not set")
	}
	if state.TotalEntriesWritten != 3 {
		t.Fatalf("TotalEntriesWritten: got %d want 3", state.TotalEntriesWritten)
	}
	if state.LastTrailID == "" {
		t.Fatalf("LastTrailID empty")
	}
	// Sanity: unknown project returns ok=false, no error.
	_, ok2, err := p.Status(context.Background(), "unknown")
	if err != nil {
		t.Fatalf("Status unknown: %v", err)
	}
	if ok2 {
		t.Fatalf("Status unknown: want ok=false")
	}
	_ = st
}

// TestIngest_ResumeProcessesOnlyMissingModules covers AC4: resume
// processes only missing modules and does not duplicate already-
// written entries. We simulate a partial ingest by pre-seeding the
// state store with two of three module ids completed.
func TestIngest_ResumeProcessesOnlyMissingModules(t *testing.T) {
	p, sum, wr, tr, st, _ := newTestPipeline(threePackageGoFixture())
	// Pre-seed state: foo and bar already ingested. baz remains.
	// The module ids come from languages.Group with the Go
	// per-package strategy: "go:per-package:<dir>".
	st.byProject = map[string]ProjectState{
		"fixture": {
			ProjectName: "fixture",
			CompletedModuleIDs: []string{
				"go:per-package:foo",
				"go:per-package:bar",
			},
			TotalEntriesWritten: 2,
		},
	}
	res, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
		Resume:      true,
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(res.EntryIDs) != 1 {
		t.Fatalf("resume wrote: got %d want 1", len(res.EntryIDs))
	}
	if len(res.SkippedModules) != 2 {
		t.Fatalf("skipped: got %d want 2", len(res.SkippedModules))
	}
	// Only one writer call and one summarizer call — not three.
	if sum.calls != 1 {
		t.Fatalf("summarizer calls: got %d want 1", sum.calls)
	}
	if len(wr.writes) != 1 {
		t.Fatalf("writer calls: got %d want 1", len(wr.writes))
	}
	if wr.writes[0].ModuleID != "go:per-package:baz" {
		t.Fatalf("writer target: got %s want baz module", wr.writes[0].ModuleID)
	}
	// One trail (covering only the one new entry).
	if len(tr.appended) != 1 {
		t.Fatalf("trail appended: got %d want 1", len(tr.appended))
	}
	if len(tr.appended[0].EntryIDs) != 1 {
		t.Fatalf("trail entry ids: got %d want 1", len(tr.appended[0].EntryIDs))
	}
	// State is now complete: 3 module ids, 2+1=3 entries total.
	saved := st.byProject["fixture"]
	if len(saved.CompletedModuleIDs) != 3 {
		t.Fatalf("state completed ids after resume: got %d want 3",
			len(saved.CompletedModuleIDs))
	}
	if saved.TotalEntriesWritten != 3 {
		t.Fatalf("state total entries after resume: got %d want 3",
			saved.TotalEntriesWritten)
	}
}

// TestIngest_MissingProjectRootRejected validates the precondition.
func TestIngest_MissingProjectRootRejected(t *testing.T) {
	p, _, _, _, _, _ := newTestPipeline(nil)
	_, err := p.Ingest(context.Background(), Request{ProjectName: "x"})
	if err == nil || !strings.Contains(err.Error(), "MISSING_PROJECT_ROOT") {
		t.Fatalf("want MISSING_PROJECT_ROOT, got %v", err)
	}
}

// TestIngest_MissingProjectNameRejected validates the precondition.
func TestIngest_MissingProjectNameRejected(t *testing.T) {
	p, _, _, _, _, _ := newTestPipeline(nil)
	_, err := p.Ingest(context.Background(), Request{ProjectRoot: "/root"})
	if err == nil || !strings.Contains(err.Error(), "MISSING_PROJECT_NAME") {
		t.Fatalf("want MISSING_PROJECT_NAME, got %v", err)
	}
}

// TestIngest_SummarizerErrorRecordedNotFatal confirms per-module
// summarizer failures append to Result.SummaryErrors and do not abort
// the run. The other modules still produce entries.
func TestIngest_SummarizerErrorRecordedNotFatal(t *testing.T) {
	p, sum, wr, _, _, _ := newTestPipeline(threePackageGoFixture())
	sum.err = errors.New("ollama timeout")
	res, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.SummaryErrors) != 3 {
		t.Fatalf("summary errors: got %d want 3", len(res.SummaryErrors))
	}
	if len(wr.writes) != 0 {
		t.Fatalf("writer called despite summarizer failing: %d", len(wr.writes))
	}
	if len(res.EntryIDs) != 0 {
		t.Fatalf("entries on all-failed run: %d", len(res.EntryIDs))
	}
}

// TestIngest_DryRunSkipsSideEffects proves --dry-run writes no state,
// no trail, and no datoms but still returns the module plan.
func TestIngest_DryRunSkipsSideEffects(t *testing.T) {
	p, _, wr, tr, st, _ := newTestPipeline(threePackageGoFixture())
	res, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("Ingest dry-run: %v", err)
	}
	if len(res.Modules) != 3 {
		t.Fatalf("dry-run modules: got %d want 3", len(res.Modules))
	}
	if len(wr.writes) != 0 {
		t.Fatalf("dry-run writer touched: %d", len(wr.writes))
	}
	if len(tr.appended) != 0 {
		t.Fatalf("dry-run trail touched: %d", len(tr.appended))
	}
	if st.writes != 0 {
		t.Fatalf("dry-run state touched: %d", st.writes)
	}
}

// TestIngest_PostReflectInvokedWithTrailID confirms the post-ingest
// reflection hook runs with the synthesized trail id when new entries
// were written.
func TestIngest_PostReflectInvokedWithTrailID(t *testing.T) {
	p, _, _, _, _, rf := newTestPipeline(threePackageGoFixture())
	res, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rf.scopes) != 1 {
		t.Fatalf("post-reflect calls: got %d want 1", len(rf.scopes))
	}
	if rf.scopes[0] != res.TrailID {
		t.Fatalf("post-reflect trail: got %s want %s", rf.scopes[0], res.TrailID)
	}
}

// TestIngest_PostReflectSkippedOnNoOp confirms the reflection hook
// does NOT run when there are no new entries (so a no-op resume does
// not trigger empty reflection windows).
func TestIngest_PostReflectSkippedOnNoOp(t *testing.T) {
	p, _, _, _, st, rf := newTestPipeline(threePackageGoFixture())
	st.byProject = map[string]ProjectState{
		"fixture": {
			ProjectName: "fixture",
			CompletedModuleIDs: []string{
				"go:per-package:foo",
				"go:per-package:bar",
				"go:per-package:baz",
			},
		},
	}
	_, err := p.Ingest(context.Background(), Request{
		ProjectRoot: "/root",
		ProjectName: "fixture",
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(rf.scopes) != 0 {
		t.Fatalf("post-reflect ran on no-op: %d calls", len(rf.scopes))
	}
}
