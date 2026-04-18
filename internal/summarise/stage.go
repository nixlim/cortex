package summarise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nixlim/cortex/internal/claudecli"
)

// FrameSchemaVersion is the version string stamped into every frame
// the summariser produces. Matches the `version` field of
// internal/frames/builtin/community_brief.json and project_brief.json.
const FrameSchemaVersion = "v1"

// defaultConcurrency is the fan-out used when Config.Concurrency is
// zero. 2 is a deliberately low ceiling: the CLI itself serialises
// on provider rate limits, and over-fanning just piles up rate-limit
// retries.
const defaultConcurrency = 2

// defaultCallTimeout bounds one per-community LLM call. A cluster of
// a few dozen short observations typically returns in < 30s; 120s
// is a generous ceiling that still fails fast on a hung provider.
const defaultCallTimeout = 120 * time.Second

// Config tunes the stage. Zero-value is valid; the stage fills in
// defaults for unset fields.
type Config struct {
	// Runner executes the Claude Code CLI subprocess. Required.
	Runner claudecli.Runner

	// Model, if non-empty, is forwarded to the CLI via --model.
	Model string

	// Concurrency is the number of per-community LLM calls allowed
	// in flight simultaneously. <=0 uses defaultConcurrency.
	Concurrency int

	// CallTimeout bounds one CLI call. <=0 uses defaultCallTimeout.
	CallTimeout time.Duration

	// Now is a clock seam for tests. nil defaults to time.Now.
	Now func() time.Time
}

// Stage is the event-driven summariser. One Stage can serve many
// analyze runs; it holds no per-run state.
type Stage struct {
	cfg Config
}

// New constructs a Stage. It returns an error if required
// dependencies are missing.
func New(cfg Config) (*Stage, error) {
	if cfg.Runner == nil {
		return nil, errors.New("summarise: Runner is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = defaultCallTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Stage{cfg: cfg}, nil
}

// Summarise walks `communities`, skipping those whose membership
// hash matches `prior`, summarising the rest, and stitching the
// current set of CommunityBriefs into one ProjectBrief. The returned
// Report always contains one CommunityResult per input community
// (status = summarised | skipped | failed). Report.Frames lists the
// NEW frames the caller should persist, in write order.
//
// A per-community failure is isolated: the Report records it, the
// prior brief (if any) is retained by the caller (we emit no
// replacement frame), and the stitch still runs over whatever set
// of briefs is current.
//
// The stitch itself can also fail independently; Report.StitchErr
// carries the reason and Report.ProjectBrief is nil.
func (s *Stage) Summarise(ctx context.Context, project string, communities []Community, prior map[CommunityID]PriorBrief) (Report, error) {
	if project == "" {
		return Report{}, errors.New("summarise: project is required")
	}
	results := make([]CommunityResult, len(communities))

	// Fixed-size worker pool so we respect Concurrency without
	// pulling in errgroup. Each worker pulls the next index off a
	// channel and writes its CommunityResult into results[i].
	type job struct{ idx int }
	jobs := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < s.cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.idx] = s.processCommunity(ctx, project, communities[j.idx], prior)
			}
		}()
	}
	for i := range communities {
		select {
		case jobs <- job{idx: i}:
		case <-ctx.Done():
			// Context cancellation: stop enqueuing, let workers drain.
			goto done
		}
	}
done:
	close(jobs)
	wg.Wait()

	report := Report{Communities: results}
	for i := range results {
		if results[i].Status == StatusSummarised && results[i].Frame != nil {
			report.Frames = append(report.Frames, *results[i].Frame)
		}
	}

	// Stitch over the CURRENT set of briefs: prior briefs that were
	// skipped this run are still the canonical brief for their
	// community, so they contribute to the project view. We
	// assemble the stitch input from (a) newly-summarised briefs'
	// slots and (b) a lightweight projection of each prior brief we
	// have on hand. Communities that failed this run and had no
	// prior brief are excluded from the stitch input — they are
	// uncovered; coverage_ratio reflects this.
	stitchInput, covered := assembleStitchInput(communities, results, prior)
	if len(stitchInput) == 0 {
		// Nothing to stitch (all communities failed and had no
		// prior brief). Return the report as-is; the caller will
		// see no ProjectBrief.
		return report, nil
	}

	pb, stitchErr := s.runStitch(ctx, project, stitchInput, covered, len(communities))
	if stitchErr != nil {
		report.StitchErr = stitchErr
		return report, nil
	}
	report.ProjectBrief = pb
	report.Frames = append(report.Frames, *pb)
	return report, nil
}

// processCommunity is the per-community worker body. Errors are
// captured into the CommunityResult; the caller never sees them as
// Go errors so one bad community cannot abort the whole pass.
func (s *Stage) processCommunity(ctx context.Context, project string, c Community, prior map[CommunityID]PriorBrief) CommunityResult {
	start := s.cfg.Now()
	hash := MembershipHash(c.EntryIDs)
	res := CommunityResult{CommunityID: c.ID, MembershipHash: hash}

	if p, ok := prior[c.ID]; ok && p.MembershipHash == hash {
		res.Status = StatusSkipped
		res.DurationMS = since(s.cfg.Now, start)
		return res
	}

	prompt := buildCommunityPrompt(project, c, hash)
	req := claudecli.Request{
		Prompt:     prompt,
		SchemaJSON: CommunityBriefSchema,
		Model:      s.cfg.Model,
		Timeout:    s.cfg.CallTimeout,
	}
	resp, err := s.cfg.Runner.Run(ctx, req)
	res.DurationMS = since(s.cfg.Now, start)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return res
	}
	frame, err := frameFromStructuredOutput(resp, "CommunityBrief", c.EntryIDs, hash, c.ID)
	if err != nil {
		res.Status = StatusFailed
		res.Err = err
		return res
	}
	res.Status = StatusSummarised
	res.Frame = frame
	return res
}

// runStitch makes the single project-level LLM call. If the stitch
// fails it is isolated: the caller still gets per-community frames.
func (s *Stage) runStitch(ctx context.Context, project string, briefs []map[string]any, covered, total int) (*Frame, error) {
	now := s.cfg.Now()
	prompt := buildProjectPrompt(project, briefs, now)
	resp, err := s.cfg.Runner.Run(ctx, claudecli.Request{
		Prompt:     prompt,
		SchemaJSON: ProjectBriefSchema,
		Model:      s.cfg.Model,
		Timeout:    s.cfg.CallTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("stitch: %w", err)
	}
	// The project brief DERIVED_FROM every community_id it covered;
	// we roll those up from the stitch input.
	exemplars := make([]string, 0, len(briefs))
	for _, b := range briefs {
		if id, ok := b["community_id"].(string); ok && id != "" {
			exemplars = append(exemplars, id)
		}
	}
	frame, err := frameFromStructuredOutput(resp, "ProjectBrief", exemplars, "", CommunityID(""))
	if err != nil {
		return nil, fmt.Errorf("stitch: %w", err)
	}
	// Force the coverage_ratio slot onto the frame regardless of
	// what the model chose, so it agrees with what actually
	// happened this run. The model's estimate is at best a
	// rounded-to-0.1 guess; we know the exact number.
	if total > 0 {
		frame.Slots["coverage_ratio"] = float64(covered) / float64(total)
	}
	// Pin project + generated_at to be authoritative too — the
	// model occasionally paraphrases these fields.
	frame.Slots["project"] = project
	frame.Slots["generated_at"] = now.UTC().Format(time.RFC3339)
	return frame, nil
}

// frameFromStructuredOutput parses a claudecli.Response into a Frame
// of the given type. It asserts that the required community_id and
// membership_hash slots match what we asked for, so a model that
// helpfully "corrects" those values does not produce a
// brief-for-the-wrong-community.
func frameFromStructuredOutput(resp claudecli.Response, frameType string, exemplars []string, expectedHash string, expectedID CommunityID) (*Frame, error) {
	if len(resp.StructuredOutput) == 0 {
		return nil, fmt.Errorf("summarise: empty structured_output for %s", frameType)
	}
	var slots map[string]any
	if err := json.Unmarshal(resp.StructuredOutput, &slots); err != nil {
		return nil, fmt.Errorf("summarise: parse %s slots: %w", frameType, err)
	}
	if frameType == "CommunityBrief" {
		if got, _ := slots["community_id"].(string); got != string(expectedID) {
			return nil, fmt.Errorf("summarise: community_id mismatch: got %q want %q", got, expectedID)
		}
		if got, _ := slots["membership_hash"].(string); got != expectedHash {
			return nil, fmt.Errorf("summarise: membership_hash mismatch: got %q want %q", got, expectedHash)
		}
	}
	return &Frame{
		Type:          frameType,
		Slots:         slots,
		Exemplars:     exemplars,
		SchemaVersion: FrameSchemaVersion,
	}, nil
}

// assembleStitchInput builds the lightweight per-brief projection
// fed into the stitch prompt. Newly-summarised briefs contribute
// their full slot-map; prior briefs that were skipped contribute
// just enough for the stitch to reference them (community_id +
// prior theme_label/concept_tags if we retained them).
//
// Returned (rows, coveredCount) — coveredCount is the number of
// communities represented in rows, used for coverage_ratio.
func assembleStitchInput(communities []Community, results []CommunityResult, prior map[CommunityID]PriorBrief) ([]map[string]any, int) {
	rows := make([]map[string]any, 0, len(results))
	covered := 0
	for i, r := range results {
		switch r.Status {
		case StatusSummarised:
			if r.Frame != nil {
				rows = append(rows, r.Frame.Slots)
				covered++
			}
		case StatusSkipped:
			// Prior brief is still canonical. We don't have its
			// full slots here (loading them would force a Neo4j
			// read path the summariser doesn't own), but we can at
			// least thread the community_id so the stitch knows
			// this cluster exists.
			row := map[string]any{"community_id": string(communities[i].ID)}
			if p, ok := prior[communities[i].ID]; ok && p.FrameID != "" {
				row["prior_frame_id"] = p.FrameID
			}
			rows = append(rows, row)
			covered++
		case StatusFailed:
			// No prior brief and this run failed: uncovered.
			if p, ok := prior[communities[i].ID]; ok {
				row := map[string]any{"community_id": string(communities[i].ID)}
				if p.FrameID != "" {
					row["prior_frame_id"] = p.FrameID
				}
				rows = append(rows, row)
				covered++
			}
		}
	}
	return rows, covered
}

// since is a small seam so the Config.Now override drives duration
// measurements too (important for deterministic tests).
func since(now func() time.Time, start time.Time) int64 {
	return now().Sub(start).Milliseconds()
}
