// Package reflect implements the Cortex reflection pipeline.
//
// cortex reflect promotes episodic clusters into typed semantic /
// procedural frames. The pipeline:
//
//  1. Reads the per-frame reflection watermark from the watermark
//     store.
//  2. Asks the cluster source for candidates whose youngest exemplar
//     was committed AFTER the watermark.
//  3. Evaluates the four threshold rules:
//       - cluster size           >= reflection.min_cluster_size (3)
//       - distinct timestamps    >= reflection.min_distinct_timestamps (2)
//       - average pairwise cos   >= reflection.avg_pairwise_cosine_floor (0.65)
//       - MDL compression ratio  >= reflection.mdl_compression_ratio (1.3)
//  4. Asks the LLM proposer for a frame.
//  5. Appends the frame's datoms (frame node + DERIVED_FROM edges).
//  6. Advances the watermark PER ACCEPTED FRAME so an interrupted
//     run resumes without reprocessing already-written frames (AC3).
//
// --dry-run/--explain runs steps 1-4 but skips 5 and 6, so the
// outcome list still records every accepted/rejected candidate with
// its rejection reason while the log is left untouched.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-3 (reflection BDD scenarios)
//	docs/spec/cortex-spec.md §"Configuration Defaults" (reflection.*)
//	bead cortex-4kq.44
package reflect

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// Spec defaults from docs/spec/cortex-spec.md §"Configuration Defaults".
const (
	DefaultMinClusterSize        = 3
	DefaultMinDistinctTimestamps = 2
	DefaultCosineFloor           = 0.65
	DefaultMDLRatio              = 1.3

	// FrameSchemaVersion is the schema version recorded on every
	// frame asserted by reflect. Bumped on schema breaking changes.
	FrameSchemaVersion = "v1"
)

// RejectionReason is the canonical code attached to a rejected
// candidate cluster. The CLI surfaces these with --explain.
type RejectionReason string

const (
	ReasonBelowMinClusterSize     RejectionReason = "BELOW_MIN_CLUSTER_SIZE"
	ReasonInsufficientTimestamps  RejectionReason = "INSUFFICIENT_TIMESTAMPS"
	ReasonBelowCosineFloor        RejectionReason = "BELOW_COSINE_FLOOR"
	ReasonBelowMDLRatio           RejectionReason = "BELOW_MDL_RATIO"
	ReasonLLMRejected             RejectionReason = "LLM_REJECTED"
)

// ExemplarRef is the minimal projection of an episodic entry the
// pipeline needs to count, score, and link clusters.
type ExemplarRef struct {
	EntryID   string
	Timestamp time.Time
	Tx        string // tx of the exemplar's source datom
}

// ClusterCandidate is one input to the threshold filter. It is the
// shape the cluster source produces; reflection neither runs Leiden
// nor materializes its own clusters in this package — that is the
// caller's job.
type ClusterCandidate struct {
	ID                    string
	Exemplars             []ExemplarRef
	AveragePairwiseCosine float64
	DistinctTimestamps    int
	MDLRatio              float64
}

// Frame is the proposer's output, ready to be turned into datoms.
// Slots is type-specific (BugPattern has different slots than
// Principle); the package treats them opaquely.
type Frame struct {
	FrameID       string
	Type          string
	Slots         map[string]any
	Exemplars     []string // entry ids the frame DERIVED_FROM
	SchemaVersion string
}

// FrameProposer is the LLM-backed scorer/asserter. A nil return with
// no error counts as ReasonLLMRejected; an error is treated the same
// way (the spec calls for graceful skip on reflection failures).
type FrameProposer interface {
	Propose(ctx context.Context, cluster ClusterCandidate) (*Frame, error)
}

// ClusterSource enumerates candidate clusters whose youngest
// exemplar tx is strictly greater than the supplied watermark. An
// empty watermark means "from the beginning".
type ClusterSource interface {
	Candidates(ctx context.Context, sinceTx string) ([]ClusterCandidate, error)
}

// WatermarkStore reads and updates the per-frame reflection
// watermark. Implementations typically store the watermark as a
// labeled node in Neo4j; the package only needs the two operations.
type WatermarkStore interface {
	ReadReflectionWatermark(ctx context.Context) (string, error)
	WriteReflectionWatermark(ctx context.Context, tx string) error
}

// LogAppender mirrors the same narrow interface used by the write
// package. The reflect pipeline appends one transaction group per
// accepted frame so a partial run leaves the log in a coherent state.
type LogAppender interface {
	Append(group []datom.Datom) (string, error)
}

// CandidateOutcome records what happened to one candidate during a
// run. Accepted is true only when a frame was proposed AND (in non-
// dry-run mode) successfully appended.
type CandidateOutcome struct {
	Cluster  ClusterCandidate
	Accepted bool
	Reason   RejectionReason // empty for accepted clusters
	Frame    *Frame          // populated for accepted clusters
}

// Result is the full output of one Reflect call.
type Result struct {
	Accepted    []*Frame
	Outcomes    []CandidateOutcome
	FrameDatoms []datom.Datom // for --json output and tests
	Watermark   string        // final watermark after the run
}

// RunOptions carries the CLI flags that change the side-effect
// surface (--dry-run/--explain).
type RunOptions struct {
	DryRun  bool
	Explain bool
}

// Pipeline orchestrates one cortex reflect invocation.
type Pipeline struct {
	Source    ClusterSource
	Proposer  FrameProposer
	Watermark WatermarkStore
	Log       LogAppender

	MinClusterSize        int
	MinDistinctTimestamps int
	CosineFloor           float64
	MDLRatio              float64

	Now          func() time.Time
	Actor        string
	InvocationID string
}

func (p *Pipeline) fillDefaults() {
	if p.MinClusterSize <= 0 {
		p.MinClusterSize = DefaultMinClusterSize
	}
	if p.MinDistinctTimestamps <= 0 {
		p.MinDistinctTimestamps = DefaultMinDistinctTimestamps
	}
	if p.CosineFloor <= 0 {
		p.CosineFloor = DefaultCosineFloor
	}
	if p.MDLRatio <= 0 {
		p.MDLRatio = DefaultMDLRatio
	}
	if p.Now == nil {
		p.Now = func() time.Time { return time.Now().UTC() }
	}
}

// Reflect runs one reflection pass. See the package doc for the
// step-by-step contract.
//
// Per-frame watermark advancement (AC3): the watermark is updated
// AFTER each successful Append. If the process is killed mid-run,
// the next invocation will only see candidates with a tx greater
// than the latest persisted watermark, so already-written frames
// are not reprocessed.
func (p *Pipeline) Reflect(ctx context.Context, opts RunOptions) (*Result, error) {
	p.fillDefaults()

	watermark, err := p.Watermark.ReadReflectionWatermark(ctx)
	if err != nil {
		return nil, errs.Operational("WATERMARK_READ_FAILED",
			"could not read reflection watermark", err)
	}

	candidates, err := p.Source.Candidates(ctx, watermark)
	if err != nil {
		return nil, errs.Operational("CLUSTER_SOURCE_FAILED",
			"could not enumerate cluster candidates", err)
	}

	res := &Result{Watermark: watermark}
	for _, c := range candidates {
		outcome, err := p.evaluate(ctx, c, opts)
		if err != nil {
			return res, err
		}
		res.Outcomes = append(res.Outcomes, outcome)
		if !outcome.Accepted {
			continue
		}
		if opts.DryRun {
			res.Accepted = append(res.Accepted, outcome.Frame)
			continue
		}
		// Build the datom group for this frame and append it as its
		// own transaction. Per-frame tx granularity is what makes
		// AC3's resume-after-interrupt work.
		group, frameTx, err := p.buildFrameGroup(outcome.Frame)
		if err != nil {
			return res, errs.Operational("FRAME_BUILD_FAILED",
				"could not build frame datoms", err)
		}
		appendedTx, err := p.Log.Append(group)
		if err != nil {
			return res, errs.Operational("FRAME_APPEND_FAILED",
				"could not append frame group", err)
		}
		_ = frameTx
		_ = appendedTx
		// AC3: advance watermark per accepted frame. The cursor is
		// the cluster's youngest exemplar tx — that is the value the
		// cluster source compares against to decide which candidates
		// have already been reflected. Using the frame's own tx
		// would be circular: the source has no way to map a frame
		// tx back onto the cluster id space.
		newWatermark := youngestExemplarTx(c)
		if newWatermark > res.Watermark {
			if err := p.Watermark.WriteReflectionWatermark(ctx, newWatermark); err != nil {
				return res, errs.Operational("WATERMARK_WRITE_FAILED",
					"could not advance reflection watermark", err)
			}
			res.Watermark = newWatermark
		}
		res.Accepted = append(res.Accepted, outcome.Frame)
		res.FrameDatoms = append(res.FrameDatoms, group...)
	}

	return res, nil
}

// youngestExemplarTx returns the lexicographically largest exemplar
// tx in a cluster. ULIDs are designed to be lexicographically sortable
// in tx-creation order, so the max string IS the most recent tx.
func youngestExemplarTx(c ClusterCandidate) string {
	var max string
	for _, e := range c.Exemplars {
		if e.Tx > max {
			max = e.Tx
		}
	}
	return max
}

// evaluate runs the four threshold checks against one candidate and
// asks the proposer if every check passes. The returned outcome is
// always non-nil.
func (p *Pipeline) evaluate(ctx context.Context, c ClusterCandidate, opts RunOptions) (CandidateOutcome, error) {
	if len(c.Exemplars) < p.MinClusterSize {
		return CandidateOutcome{Cluster: c, Reason: ReasonBelowMinClusterSize}, nil
	}
	if c.DistinctTimestamps < p.MinDistinctTimestamps {
		return CandidateOutcome{Cluster: c, Reason: ReasonInsufficientTimestamps}, nil
	}
	if c.AveragePairwiseCosine < p.CosineFloor {
		return CandidateOutcome{Cluster: c, Reason: ReasonBelowCosineFloor}, nil
	}
	if c.MDLRatio < p.MDLRatio {
		return CandidateOutcome{Cluster: c, Reason: ReasonBelowMDLRatio}, nil
	}
	frame, err := p.Proposer.Propose(ctx, c)
	if err != nil || frame == nil {
		return CandidateOutcome{Cluster: c, Reason: ReasonLLMRejected}, nil
	}
	if frame.SchemaVersion == "" {
		frame.SchemaVersion = FrameSchemaVersion
	}
	return CandidateOutcome{Cluster: c, Accepted: true, Frame: frame}, nil
}

// buildFrameGroup turns one frame into a sealed datom group. The
// group contains:
//
//  1. one frame.type datom asserting the frame node
//  2. one frame.slots datom holding the slot map
//  3. one frame.schema_version datom
//  4. one DERIVED_FROM datom per exemplar (AC2)
//
// All datoms share the same tx (the frame's tx), so a partial-write
// crash leaves either the entire frame or none of it.
func (p *Pipeline) buildFrameGroup(f *Frame) ([]datom.Datom, string, error) {
	tx := ulid.Make().String()
	if f.FrameID == "" {
		f.FrameID = "frame:" + ulid.Make().String()
	}
	ts := p.Now().UTC().Format(time.RFC3339Nano)
	group := make([]datom.Datom, 0, 3+len(f.Exemplars))
	add := func(a string, v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", a, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        p.Actor,
			Op:           datom.OpAdd,
			E:            f.FrameID,
			A:            a,
			V:            raw,
			Src:          "reflect",
			InvocationID: p.InvocationID,
		}
		if err := d.Seal(); err != nil {
			return fmt.Errorf("seal %s: %w", a, err)
		}
		group = append(group, d)
		return nil
	}
	if err := add("frame.type", f.Type); err != nil {
		return nil, "", err
	}
	if err := add("frame.slots", f.Slots); err != nil {
		return nil, "", err
	}
	if err := add("frame.schema_version", f.SchemaVersion); err != nil {
		return nil, "", err
	}
	// AC2: every accepted frame writes a DERIVED_FROM edge to every
	// exemplar entry id, so reflection provenance is queryable.
	for _, ex := range f.Exemplars {
		if err := add("DERIVED_FROM", ex); err != nil {
			return nil, "", err
		}
	}
	return group, tx, nil
}
