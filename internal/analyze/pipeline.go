// Package analyze implements cortex analyze --find-patterns, the
// cross-project pattern finder.
//
// Unlike cortex reflect (which runs over any episodic cluster), this
// command accepts only clusters that span at least two distinct
// projects and whose single-project share stays below a configurable
// cap. Accepted frames are marked cross_project=true, their
// importance is boosted by a fixed delta, and a full community
// refresh is triggered after the writes land.
//
// The pipeline mirrors internal/reflect in shape: the caller owns
// cluster materialization, this package evaluates the thresholds,
// asks an LLM proposer, builds datom groups, and persists them. Every
// side effect is behind a narrow interface so the CLI can wire real
// adapters while tests drive the whole flow with fakes.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-4 (cross-project analysis BDDs)
//	docs/spec/cortex-spec.md §"Configuration Defaults" (analyze.*)
//	bead cortex-4kq.52
package analyze

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
	DefaultMinProjects         = 2
	DefaultMaxSharePerProject  = 0.70
	DefaultAnalysisMDLRatio    = 1.15
	DefaultImportanceBoost     = 0.20
	DefaultFrameSchemaVersion  = "v1"
)

// RejectionReason is the canonical code for a rejected cluster.
type RejectionReason string

const (
	ReasonSingleProject         RejectionReason = "SINGLE_PROJECT"
	ReasonProjectShareExceeded  RejectionReason = "PROJECT_SHARE_EXCEEDED"
	ReasonBelowAnalysisMDLRatio RejectionReason = "BELOW_ANALYSIS_MDL_RATIO"
	ReasonLLMRejected           RejectionReason = "LLM_REJECTED"
	ReasonEmptyAfterFilter      RejectionReason = "EMPTY_AFTER_MIGRATED_FILTER"
)

// ExemplarRef is the minimal per-exemplar projection this pipeline
// needs: the entry id (for DERIVED_FROM edges), the owning project
// (for the two-project / share thresholds), and a migrated flag
// (excluded by default per AC4, included with --include-migrated).
type ExemplarRef struct {
	EntryID  string
	Project  string
	Migrated bool
}

// ClusterCandidate is one input to the filter. Unlike reflect, this
// package does not evaluate cluster-size or cosine-floor thresholds —
// those are the reflect contract. Analyze only cares about cross-
// project distribution and the relaxed MDL ratio.
type ClusterCandidate struct {
	ID        string
	Exemplars []ExemplarRef
	MDLRatio  float64
}

// Frame is the proposer's output, ready to be turned into datoms.
// cross_project is always set true for accepted frames; the proposer
// does not need to know the flag exists.
type Frame struct {
	FrameID       string
	Type          string
	Slots         map[string]any
	Exemplars     []string // entry ids
	Projects      []string // distinct project ids spanned
	SchemaVersion string
	Importance    float64 // pre-boost importance from the proposer
}

// FrameProposer is the LLM-backed proposer. Nil return with no error
// counts as LLM_REJECTED.
type FrameProposer interface {
	Propose(ctx context.Context, cluster ClusterCandidate) (*Frame, error)
}

// ClusterSource enumerates candidate clusters. The analyze pipeline
// does NOT advance a watermark per frame; full cross-project runs are
// infrequent and operator-initiated, so the source returns the whole
// candidate universe on every call.
type ClusterSource interface {
	Candidates(ctx context.Context) ([]ClusterCandidate, error)
}

// LogAppender mirrors the narrow interface reflect uses.
type LogAppender interface {
	Append(group []datom.Datom) (string, error)
}

// CommunityRefresher triggers the full-graph community refresh that
// the spec requires after cross-project writes. A nil refresher is
// treated as "no refresh".
type CommunityRefresher interface {
	Refresh(ctx context.Context) error
}

// CandidateOutcome records what happened to one candidate.
type CandidateOutcome struct {
	Cluster  ClusterCandidate
	Accepted bool
	Reason   RejectionReason
	Frame    *Frame
}

// Result is the full output of one Analyze call.
type Result struct {
	Accepted         []*Frame
	Outcomes         []CandidateOutcome
	FrameDatoms      []datom.Datom
	CommunityRefresh bool // true if a refresh was triggered
}

// RunOptions carries the CLI flags.
type RunOptions struct {
	DryRun          bool
	Explain         bool
	IncludeMigrated bool // --include-migrated (AC4)
}

// Pipeline orchestrates one cortex analyze invocation.
type Pipeline struct {
	Source    ClusterSource
	Proposer  FrameProposer
	Log       LogAppender
	Community CommunityRefresher

	MinProjects        int
	MaxSharePerProject float64
	MDLRatio           float64
	ImportanceBoost    float64

	Now          func() time.Time
	Actor        string
	InvocationID string
}

func (p *Pipeline) fillDefaults() {
	if p.MinProjects <= 0 {
		p.MinProjects = DefaultMinProjects
	}
	if p.MaxSharePerProject <= 0 {
		p.MaxSharePerProject = DefaultMaxSharePerProject
	}
	if p.MDLRatio <= 0 {
		p.MDLRatio = DefaultAnalysisMDLRatio
	}
	if p.ImportanceBoost <= 0 {
		p.ImportanceBoost = DefaultImportanceBoost
	}
	if p.Now == nil {
		p.Now = func() time.Time { return time.Now().UTC() }
	}
}

// Analyze runs one cross-project pattern-finding pass. See the
// package doc for the step-by-step contract.
func (p *Pipeline) Analyze(ctx context.Context, opts RunOptions) (*Result, error) {
	p.fillDefaults()

	candidates, err := p.Source.Candidates(ctx)
	if err != nil {
		return nil, errs.Operational("CLUSTER_SOURCE_FAILED",
			"could not enumerate cluster candidates", err)
	}

	res := &Result{}
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
		group, _, err := p.buildFrameGroup(outcome.Frame)
		if err != nil {
			return res, errs.Operational("FRAME_BUILD_FAILED",
				"could not build frame datoms", err)
		}
		if _, err := p.Log.Append(group); err != nil {
			return res, errs.Operational("FRAME_APPEND_FAILED",
				"could not append frame group", err)
		}
		res.Accepted = append(res.Accepted, outcome.Frame)
		res.FrameDatoms = append(res.FrameDatoms, group...)
	}

	// Full community refresh after writes (spec constraint). Skipped
	// on dry run and when no frames were accepted so we don't churn
	// the graph on empty runs.
	if !opts.DryRun && len(res.Accepted) > 0 && p.Community != nil {
		if err := p.Community.Refresh(ctx); err != nil {
			return res, errs.Operational("COMMUNITY_REFRESH_FAILED",
				"community refresh failed after cross-project writes", err)
		}
		res.CommunityRefresh = true
	}

	return res, nil
}

// evaluate applies the four analyze rules to one candidate. The
// returned outcome is always non-nil.
//
// Order of checks matters for test clarity:
//  1. migrated filter (AC4) — may empty the exemplar set entirely
//  2. MDL ratio — cheapest numeric check
//  3. distinct-project count (AC1) — SINGLE_PROJECT
//  4. per-project share (AC2) — PROJECT_SHARE_EXCEEDED
//  5. LLM propose — LLM_REJECTED on nil/error
func (p *Pipeline) evaluate(ctx context.Context, c ClusterCandidate, opts RunOptions) (CandidateOutcome, error) {
	filtered := c
	if !opts.IncludeMigrated {
		kept := make([]ExemplarRef, 0, len(c.Exemplars))
		for _, e := range c.Exemplars {
			if !e.Migrated {
				kept = append(kept, e)
			}
		}
		filtered.Exemplars = kept
		if len(kept) == 0 {
			return CandidateOutcome{Cluster: c, Reason: ReasonEmptyAfterFilter}, nil
		}
	}

	if c.MDLRatio < p.MDLRatio {
		return CandidateOutcome{Cluster: c, Reason: ReasonBelowAnalysisMDLRatio}, nil
	}

	projects, counts := projectDistribution(filtered.Exemplars)
	if len(projects) < p.MinProjects {
		return CandidateOutcome{Cluster: c, Reason: ReasonSingleProject}, nil
	}
	total := float64(len(filtered.Exemplars))
	for _, n := range counts {
		if float64(n)/total > p.MaxSharePerProject {
			return CandidateOutcome{Cluster: c, Reason: ReasonProjectShareExceeded}, nil
		}
	}

	frame, err := p.Proposer.Propose(ctx, filtered)
	if err != nil || frame == nil {
		return CandidateOutcome{Cluster: c, Reason: ReasonLLMRejected}, nil
	}
	if frame.SchemaVersion == "" {
		frame.SchemaVersion = DefaultFrameSchemaVersion
	}
	// Apply the importance boost (+0.20 by default) and record the
	// distinct projects on the frame so the proposer does not have
	// to duplicate the bookkeeping.
	frame.Importance += p.ImportanceBoost
	frame.Projects = projects
	return CandidateOutcome{Cluster: c, Accepted: true, Frame: frame}, nil
}

// projectDistribution returns the distinct project list (sorted for
// determinism) and a count-by-project map.
func projectDistribution(exemplars []ExemplarRef) ([]string, map[string]int) {
	counts := make(map[string]int, len(exemplars))
	for _, e := range exemplars {
		counts[e.Project]++
	}
	projects := make([]string, 0, len(counts))
	for p := range counts {
		projects = append(projects, p)
	}
	// Deterministic ordering — insertion sort suffices; project lists
	// stay small.
	for i := 1; i < len(projects); i++ {
		for j := i; j > 0 && projects[j] < projects[j-1]; j-- {
			projects[j], projects[j-1] = projects[j-1], projects[j]
		}
	}
	return projects, counts
}

// buildFrameGroup assembles the datom group for one accepted frame.
// The group carries:
//
//  1. frame.type
//  2. frame.slots
//  3. frame.schema_version
//  4. frame.cross_project=true (AC3)
//  5. frame.importance (boosted)
//  6. one DERIVED_FROM datom per exemplar (AC3 — must span ≥2 projects)
//
// All datoms share one tx so a partial-write crash leaves either the
// entire frame or none of it.
func (p *Pipeline) buildFrameGroup(f *Frame) ([]datom.Datom, string, error) {
	tx := ulid.Make().String()
	if f.FrameID == "" {
		f.FrameID = "frame:" + ulid.Make().String()
	}
	ts := p.Now().UTC().Format(time.RFC3339Nano)
	group := make([]datom.Datom, 0, 5+len(f.Exemplars))
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
			Src:          "analyze",
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
	if err := add("frame.cross_project", true); err != nil {
		return nil, "", err
	}
	if err := add("frame.importance", f.Importance); err != nil {
		return nil, "", err
	}
	for _, ex := range f.Exemplars {
		if err := add("DERIVED_FROM", ex); err != nil {
			return nil, "", err
		}
	}
	return group, tx, nil
}
