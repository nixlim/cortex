// Offline-operation end-to-end test for cortex-4kq.57.
//
// This test proves that the entire observe → ingest → recall → reflect →
// analyze flow runs with zero non-loopback outbound TCP connections.
// It re-uses the hermetic fakes defined alongside the cross-project
// value test (cortex-4kq.56) and adds offline-specific fakes for the
// observe (write.Pipeline) and reflect (reflect.Pipeline) entry points.
//
// AC mapping (cortex-4kq.57):
//
//   - cortex observe exits zero with no outbound internet traffic
//   - cortex recall, reflect, ingest, and analyze each exit zero under
//     the same conditions
//   - A test seam asserts zero non-loopback outbound TCP connections
//     during the test run
//   - The test covers a small fixture corpus
//
// The "test seam" is a custom net.Dialer.Control hook installed on
// http.DefaultTransport for the duration of the test. Every dial is
// inspected; if the destination address is anything other than a
// loopback IP, the dial fails AND the test records a violation. Since
// none of the pipelines under test reach a real backend (every adapter
// is faked), the dial counter MUST stay at zero.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Local-first guarantees"
//	bead cortex-4kq.57

package e2e

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/actr"
	"github.com/nixlim/cortex/internal/analyze"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/ingest"
	"github.com/nixlim/cortex/internal/languages"
	"github.com/nixlim/cortex/internal/recall"
	"github.com/nixlim/cortex/internal/reflect"
	"github.com/nixlim/cortex/internal/write"
)

// dialMonitor counts every connect attempt that targets a non-loopback
// destination. It is installed as the Control function on the dialer
// backing http.DefaultTransport for the lifetime of the test.
type dialMonitor struct {
	violations atomic.Int64
	addrs      atomic.Value // []string of offending dials, for diagnostics
}

func (m *dialMonitor) record(addr string) {
	m.violations.Add(1)
	prev, _ := m.addrs.Load().([]string)
	m.addrs.Store(append(prev, addr))
}

// installNetworkGuard replaces http.DefaultTransport with one whose
// dialer rejects every non-loopback address. The previous transport is
// restored on test cleanup. Returning the monitor lets the test assert
// the final violation count.
func installNetworkGuard(t *testing.T) *dialMonitor {
	t.Helper()
	mon := &dialMonitor{}

	dialer := &net.Dialer{
		Timeout: 1 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				host = address
			}
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				mon.record(address)
				return &net.OpError{
					Op:  "dial",
					Net: network,
					Err: &offlineViolation{addr: address},
				}
			}
			return nil
		},
	}

	prev := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   1 * time.Second,
		ResponseHeaderTimeout: 1 * time.Second,
	}
	t.Cleanup(func() { http.DefaultTransport = prev })
	return mon
}

type offlineViolation struct{ addr string }

func (o *offlineViolation) Error() string { return "offline guard rejected dial to " + o.addr }

// -- additional fakes specific to the offline test ------------------

// fakeWriteLog captures every datom group the write pipeline emits and
// satisfies write.LogAppender without touching disk.
type fakeWriteLog struct{ groups [][]datom.Datom }

func (f *fakeWriteLog) Append(group []datom.Datom) (string, error) {
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	if len(group) == 0 {
		return ulid.Make().String(), nil
	}
	return group[0].Tx, nil
}

// fakeReflectLog satisfies reflect.LogAppender. It is intentionally a
// distinct type from fakeWriteLog so each pipeline records its own
// groups for clarity in failure diagnostics.
type fakeReflectLog struct{ groups [][]datom.Datom }

func (f *fakeReflectLog) Append(group []datom.Datom) (string, error) {
	cp := make([]datom.Datom, len(group))
	copy(cp, group)
	f.groups = append(f.groups, cp)
	if len(group) == 0 {
		return ulid.Make().String(), nil
	}
	return group[0].Tx, nil
}

// fakeWatermark satisfies reflect.WatermarkStore using an in-memory
// cursor.
type fakeWatermark struct{ tx string }

func (f *fakeWatermark) ReadReflectionWatermark(_ context.Context) (string, error) {
	return f.tx, nil
}

func (f *fakeWatermark) WriteReflectionWatermark(_ context.Context, tx string) error {
	f.tx = tx
	return nil
}

// fakeReflectSource yields one cluster whose three exemplars carry
// distinct timestamps so it crosses every reflection threshold.
type fakeReflectSource struct{ store *fixtureStore }

func (f fakeReflectSource) Candidates(_ context.Context, _ string) ([]reflect.ClusterCandidate, error) {
	exemplars := make([]reflect.ExemplarRef, 0, len(f.store.order))
	for i, id := range f.store.order {
		exemplars = append(exemplars, reflect.ExemplarRef{
			EntryID:   id,
			Timestamp: time.Date(2026, 4, 10, 0, i, 0, 0, time.UTC),
			Tx:        ulid.Make().String(),
		})
	}
	if len(exemplars) < 3 {
		// Pad with synthesized exemplars so the threshold check passes
		// even when the corpus is smaller than DefaultMinClusterSize.
		for i := len(exemplars); i < 3; i++ {
			exemplars = append(exemplars, reflect.ExemplarRef{
				EntryID:   "entry:" + ulid.Make().String(),
				Timestamp: time.Date(2026, 4, 10, 0, i, 0, 0, time.UTC),
				Tx:        ulid.Make().String(),
			})
		}
	}
	return []reflect.ClusterCandidate{{
		ID:                    "cluster:offline-pattern",
		Exemplars:             exemplars,
		AveragePairwiseCosine: 0.9,
		DistinctTimestamps:    len(exemplars),
		MDLRatio:              1.5,
	}}, nil
}

// fakeReflectProposer returns a deterministic frame for any cluster.
type fakeReflectProposer struct{}

func (fakeReflectProposer) Propose(_ context.Context, c reflect.ClusterCandidate) (*reflect.Frame, error) {
	ids := make([]string, 0, len(c.Exemplars))
	for _, e := range c.Exemplars {
		ids = append(ids, e.EntryID)
	}
	return &reflect.Frame{
		FrameID: "frame:" + ulid.Make().String(),
		Type:    "BugPattern",
		Slots: map[string]any{
			"title": "Offline cluster",
		},
		Exemplars: ids,
	}, nil
}

// -- the test --------------------------------------------------------

// TestOfflineOperation_AllPipelines drives observe, ingest, recall,
// reflect, and analyze through their orchestrators with the network
// guard installed. Each pipeline must return a nil error and the dial
// monitor must record zero violations.
func TestOfflineOperation_AllPipelines(t *testing.T) {
	monitor := installNetworkGuard(t)
	ctx := context.Background()

	// ---- shared fixture corpus ------------------------------------
	store := newFixtureStore()
	projectA := "fixtures/offline-alpha"
	projectB := "fixtures/offline-bravo"

	embedA := []float32{1.0, 0.7, 0.1, 0.0}
	embedB := []float32{1.0, 0.0, 0.1, 0.7}
	queryVec := []float32{1.0, 0.35, 0.1, 0.35}

	writer := &fakeEntryWriter{
		store: store,
		projectEmbed: map[string][]float32{
			projectA: embedA,
			projectB: embedB,
		},
	}
	trailAppender := &fakeTrailAppender{}
	stateStore := &fakeStateStore{}

	// ---- ingest both projects -------------------------------------
	for _, p := range []struct {
		name string
		root string
		body string
	}{
		{projectA, "/tmp/offline-alpha", "Offline-only retry pattern in alpha"},
		{projectB, "/tmp/offline-bravo", "Offline-only retry pattern in bravo"},
	} {
		walker := &fakeWalker{files: []languages.File{
			{AbsPath: p.root + "/main.go", RelPath: "main.go"},
		}}
		pipeline := &ingest.Pipeline{
			Walker:          walker.walk,
			Matrix:          languages.DefaultMatrix(),
			Summarizer:      summarizerByProject(p.name, p.body),
			Writer:          writer,
			TrailAppender:   trailAppender,
			StateStore:      stateStore,
			Now:             func() time.Time { return time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC) },
			Concurrency:     1,
			SkipPostReflect: true,
		}
		res, err := pipeline.Ingest(ctx, ingest.Request{ProjectRoot: p.root, ProjectName: p.name})
		if err != nil {
			t.Fatalf("ingest %s: %v", p.name, err)
		}
		if len(res.EntryIDs) == 0 {
			t.Fatalf("ingest %s wrote zero entries", p.name)
		}
	}

	// ---- observe a single episodic frame --------------------------
	writeLog := &fakeWriteLog{}
	observer := &write.Pipeline{
		Log:          writeLog,
		Now:          func() time.Time { return time.Date(2026, 4, 10, 0, 1, 0, 0, time.UTC) },
		Actor:        "e2e",
		InvocationID: ulid.Make().String(),
	}
	obsResult, err := observer.Observe(ctx, write.ObserveRequest{
		Body: "Local observation while the network is unreachable.",
		Kind: "Observation",
		Facets: map[string]string{
			"domain":  "engineering",
			"project": projectA,
		},
	})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if obsResult == nil || obsResult.EntryID == "" {
		t.Fatal("observe returned empty result")
	}
	if len(writeLog.groups) == 0 {
		t.Fatal("observe did not produce a datom group")
	}

	// ---- recall ---------------------------------------------------
	now := time.Date(2026, 4, 10, 0, 5, 0, 0, time.UTC)
	recallPipeline := &recall.Pipeline{
		Concepts:     fakeConcepts{},
		Seeds:        fakeSeeds{store: store},
		PPR:          fakePPR{},
		Loader:       &fakeLoader{store: store, now: now},
		Embedder:     fakeEmbedder{vec: queryVec},
		Context:      fakeContext{},
		Now:          func() time.Time { return now },
		Actor:        "e2e",
		InvocationID: ulid.Make().String(),
		Weights:      actr.DefaultWeights(),
	}
	resp, err := recallPipeline.Recall(ctx, recall.Request{Query: "offline retry pattern"})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("recall returned zero results")
	}

	// ---- reflect --------------------------------------------------
	reflectLog := &fakeReflectLog{}
	reflectPipeline := &reflect.Pipeline{
		Source:       fakeReflectSource{store: store},
		Proposer:     fakeReflectProposer{},
		Watermark:    &fakeWatermark{},
		Log:          reflectLog,
		Now:          func() time.Time { return now },
		Actor:        "e2e",
		InvocationID: ulid.Make().String(),
	}
	reflectRes, err := reflectPipeline.Reflect(ctx, reflect.RunOptions{})
	if err != nil {
		t.Fatalf("reflect: %v", err)
	}
	if len(reflectRes.Accepted) == 0 {
		var reasons []string
		for _, o := range reflectRes.Outcomes {
			reasons = append(reasons, string(o.Reason))
		}
		t.Fatalf("reflect accepted zero frames; reasons=%v", reasons)
	}

	// ---- analyze --------------------------------------------------
	analyzeLog := &fakeAnalyzeLog{}
	refresher := &fakeRefresher{}
	analyzePipeline := &analyze.Pipeline{
		Source:       fakeClusterSource{store: store},
		Proposer:     fakeProposer{},
		Log:          analyzeLog,
		Community:    refresher,
		Now:          func() time.Time { return now },
		Actor:        "e2e",
		InvocationID: ulid.Make().String(),
	}
	analyzeRes, err := analyzePipeline.Analyze(ctx, analyze.RunOptions{})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if len(analyzeRes.Accepted) == 0 {
		t.Fatal("analyze accepted zero frames")
	}

	// ---- the AC: zero non-loopback dials --------------------------
	if v := monitor.violations.Load(); v != 0 {
		offenders, _ := monitor.addrs.Load().([]string)
		t.Errorf("offline guard tripped %d times: %v", v, offenders)
	}

	t.Logf("OK: observe, ingest, recall, reflect, analyze all succeeded with %d non-loopback dials",
		monitor.violations.Load())
}
