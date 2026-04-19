// cmd/cortex/summarise_adapters.go wires the continuous categorised-
// summarisation pass (bead cortex-8sr, Pass A) into the analyze
// pipeline. It bridges three concerns that the pure internal/summarise
// package deliberately does NOT own:
//
//  1. PriorBriefLoader — read back the most recent CommunityBrief for
//     each :Community so the hash gate can skip unchanged clusters.
//  2. CommunityMaterialiser — turn the post-refresh :Community +
//     IN_COMMUNITY graph into summarise.Community values with entry
//     bodies filled in, so the LLM has something substantive to
//     curate.
//  3. FrameWriter — serialise the summariser's Frame output into
//     datom groups and append them to the log, matching the shape the
//     Neo4j + Weaviate appliers already know how to consume.
//
// The summariserRunner at the bottom of this file composes all three
// behind the narrow analyze.SummariseStage interface so the analyze
// pipeline can fire-and-forget the whole pass.
//
// Entry bodies are read from Neo4j (e.body), not Weaviate. The bead
// design sketch originally proposed per-entry Weaviate GetObject
// calls, but Weaviate's GetObject takes a derived UUID (not the
// cortex_id) while Neo4j already carries the body as a plain property
// that recall's loader reads the same way (see
// cmd/cortex/recall_adapters.go:327). One Cypher round trip beats N
// Weaviate round trips, and no id-derivation is needed.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/summarise"
)

// ---------------------------------------------------------------------------
// 1. PriorBriefLoader — read most-recent CommunityBrief per community
// ---------------------------------------------------------------------------

// neo4jPriorBriefLoader reads the latest CommunityBrief frame for
// every :Community by scanning :Frame nodes whose frame_type property
// equals 'CommunityBrief'. Frame slot data is stored as a JSON-encoded
// string property on the :Frame node (see internal/neo4j/applier.go
// decodeValue: compound JSON collapses to a string), so we parse the
// slot map in Go.
//
// When more than one brief exists for the same community (re-runs
// over time each mint a fresh ULID), the newest write wins — we sort
// by last_tx descending and keep the first occurrence per community.
type neo4jPriorBriefLoader struct {
	client neo4j.Client
}

// Load returns a map from CommunityID → the most recent PriorBrief we
// have for that community. Communities without a prior brief are
// absent from the map; the summariser treats absence as "unsummarised,
// run the LLM".
func (l *neo4jPriorBriefLoader) Load(ctx context.Context) (map[summarise.CommunityID]summarise.PriorBrief, error) {
	const cypher = `
MATCH (f:Frame)
WHERE f.frame_type = 'CommunityBrief' AND f.frame_slots IS NOT NULL
RETURN
  coalesce(f.entry_id, f.id) AS frame_id,
  f.frame_slots              AS slots,
  coalesce(f.last_tx, '')    AS tx
ORDER BY tx DESC
`
	rows, err := l.client.QueryGraph(ctx, cypher, nil)
	if err != nil {
		return nil, fmt.Errorf("summarise: query prior briefs: %w", err)
	}
	out := make(map[summarise.CommunityID]summarise.PriorBrief, len(rows))
	for _, row := range rows {
		frameID, _ := row["frame_id"].(string)
		slotsRaw, _ := row["slots"].(string)
		if slotsRaw == "" {
			continue
		}
		var slots map[string]any
		if err := json.Unmarshal([]byte(slotsRaw), &slots); err != nil {
			// One malformed frame must not blind the whole pass. Skip
			// and let the summariser recompute that community.
			continue
		}
		cid, _ := slots["community_id"].(string)
		if cid == "" {
			continue
		}
		if _, seen := out[summarise.CommunityID(cid)]; seen {
			// Rows are sorted newest-first; keep the first one.
			continue
		}
		hash, _ := slots["membership_hash"].(string)
		out[summarise.CommunityID(cid)] = summarise.PriorBrief{
			CommunityID:    summarise.CommunityID(cid),
			MembershipHash: hash,
			FrameID:        frameID,
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 2. CommunityMaterialiser — turn :Community nodes into summarise.Community
// ---------------------------------------------------------------------------

// neo4jCommunityMaterialiser reads every level-0 :Community and its
// member entries, pulling body + kind + encoding_at from the entry
// node so the summariser has a complete per-cluster input without a
// second round trip to Weaviate.
type neo4jCommunityMaterialiser struct {
	client neo4j.Client
}

// Materialise returns one summarise.Community per level-0 community
// that currently has at least one member with an entry_id. EntryIDs
// are sorted for deterministic membership hashing; Entries parallels
// EntryIDs in the same order.
func (m *neo4jCommunityMaterialiser) Materialise(ctx context.Context) ([]summarise.Community, error) {
	const cypher = `
MATCH (c:Community {level: 0})<-[:IN_COMMUNITY]-(e)
WHERE e.entry_id IS NOT NULL
WITH c, collect({
  entry_id:    e.entry_id,
  kind:        coalesce(e.kind, ''),
  body:        coalesce(e.body, ''),
  encoding_at: e.encoding_at
}) AS members
WHERE size(members) > 0
RETURN
  toString(c.community_id) AS community_id,
  members
`
	rows, err := m.client.QueryGraph(ctx, cypher, nil)
	if err != nil {
		return nil, fmt.Errorf("summarise: query communities: %w", err)
	}
	out := make([]summarise.Community, 0, len(rows))
	for _, row := range rows {
		cid, _ := row["community_id"].(string)
		if cid == "" {
			continue
		}
		members, ok := row["members"].([]any)
		if !ok || len(members) == 0 {
			continue
		}
		// Build a sortable slice so EntryIDs comes back in a stable,
		// caller-friendly order. Summarise.MembershipHash is
		// order-independent, but Entries must parallel EntryIDs 1:1
		// so a sort-by-entry_id before indexing is simplest.
		type memberView struct {
			id, kind, body string
			ts             time.Time
		}
		ms := make([]memberView, 0, len(members))
		for _, raw := range members {
			mm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := mm["entry_id"].(string)
			if id == "" {
				continue
			}
			ms = append(ms, memberView{
				id:   id,
				kind: stringOrEmpty(mm["kind"]),
				body: stringOrEmpty(mm["body"]),
				ts:   parseRowTime(mm["encoding_at"]),
			})
		}
		if len(ms) == 0 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].id < ms[j].id })
		entryIDs := make([]string, len(ms))
		entries := make([]summarise.Entry, len(ms))
		for i, m := range ms {
			entryIDs[i] = m.id
			entries[i] = summarise.Entry{
				ID:   m.id,
				Kind: m.kind,
				Body: m.body,
				TS:   m.ts,
			}
		}
		out = append(out, summarise.Community{
			ID:       summarise.CommunityID(cid),
			EntryIDs: entryIDs,
			Entries:  entries,
		})
	}
	return out, nil
}

func stringOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// 3. FrameWriter — turn summarise.Frame values into appended datom groups
// ---------------------------------------------------------------------------

// logSummariseFrameWriter appends one datom group per summarise.Frame
// to the append-only log. The group shape mirrors analyze.buildFrameGroup
// so the Neo4j + Weaviate appliers (internal/neo4j/applier.go,
// internal/weaviate/applier.go) already know how to reflect the new
// frame into the live graph — no applier changes required.
//
// Per-frame attributes emitted:
//
//	frame.type             — "CommunityBrief" | "ProjectBrief"
//	frame.slots            — the full slot map (marshalled once)
//	frame.schema_version   — summarise.FrameSchemaVersion
//	DERIVED_FROM           — one datom per exemplar entry id (edge)
type logSummariseFrameWriter struct {
	log          logAppenderLike
	actor        string
	invocationID string
	now          func() time.Time
}

// logAppenderLike is the minimal append surface we need. It mirrors
// analyze.LogAppender but is declared locally so a fake can satisfy
// this adapter without dragging in the analyze package for tests.
type logAppenderLike interface {
	Append(group []datom.Datom) (string, error)
}

// Write appends every frame as an independent tx group. Each frame
// gets its own frame_id (minted with a fresh ULID) so multiple briefs
// for the same community over time remain distinguishable in the log
// — the applier still MERGEs by entry_id, so ancient frames are
// superseded naturally by the newest write.
func (w *logSummariseFrameWriter) Write(frames []summarise.Frame) error {
	for i := range frames {
		group, err := w.buildGroup(&frames[i])
		if err != nil {
			return fmt.Errorf("summarise: build frame %d: %w", i, err)
		}
		if _, err := w.log.Append(group); err != nil {
			return fmt.Errorf("summarise: append frame %d: %w", i, err)
		}
	}
	return nil
}

func (w *logSummariseFrameWriter) buildGroup(f *summarise.Frame) ([]datom.Datom, error) {
	tx := ulid.Make().String()
	frameID := "frame:" + ulid.Make().String()
	ts := w.now().UTC().Format(time.RFC3339Nano)

	group := make([]datom.Datom, 0, 3+len(f.Exemplars))
	add := func(attr string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", attr, err)
		}
		d := datom.Datom{
			Tx:           tx,
			Ts:           ts,
			Actor:        w.actor,
			Op:           datom.OpAdd,
			E:            frameID,
			A:            attr,
			V:            raw,
			Src:          "summarise",
			InvocationID: w.invocationID,
		}
		if err := d.Seal(); err != nil {
			return fmt.Errorf("seal %s: %w", attr, err)
		}
		group = append(group, d)
		return nil
	}
	if err := add("frame.type", f.Type); err != nil {
		return nil, err
	}
	if err := add("frame.slots", f.Slots); err != nil {
		return nil, err
	}
	schema := f.SchemaVersion
	if schema == "" {
		schema = summarise.FrameSchemaVersion
	}
	if err := add("frame.schema_version", schema); err != nil {
		return nil, err
	}
	// DERIVED_FROM edges are only emitted when the exemplar looks
	// addressable as an entity (prefix:id shape). ProjectBrief
	// exemplars are bare community IDs which would otherwise create
	// useless :CortexEntity nodes; skipping them keeps the graph
	// clean. Callers who want the ProjectBrief → Community link can
	// read slots.community_ids directly.
	for _, ex := range f.Exemplars {
		if !looksAddressable(ex) {
			continue
		}
		if err := add("derived_from", ex); err != nil {
			return nil, err
		}
	}
	return group, nil
}

// looksAddressable reports whether s looks like a Cortex entity id —
// a prefix:suffix pair where the prefix is a known entity kind. It is
// intentionally conservative: false positives are worse than missing
// edges (a stray :CortexEntity node is harder to clean up than a
// missing DERIVED_FROM).
func looksAddressable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			prefix := s[:i]
			switch prefix {
			case "entry", "frame", "subject", "trail", "community", "psi", "concept":
				return i+1 < len(s)
			}
			return false
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 4. summariserRunner — composes the three adapters behind SummariseStage
// ---------------------------------------------------------------------------

// priorBriefLoader, communityMaterialiser, frameWriter are private
// seams so tests can drive the runner without touching Neo4j.
type priorBriefLoader interface {
	Load(ctx context.Context) (map[summarise.CommunityID]summarise.PriorBrief, error)
}

type communityMaterialiser interface {
	Materialise(ctx context.Context) ([]summarise.Community, error)
}

type frameWriter interface {
	Write(frames []summarise.Frame) error
}

// summariserRunner is the analyze.SummariseStage implementation wired
// by buildAnalyzePipeline. It owns no lifecycle of its own — every
// dependency is provided at construction, and Run is safe to call
// repeatedly (each call is one pass end-to-end).
type summariserRunner struct {
	project        string
	stage          *summarise.Stage
	prior          priorBriefLoader
	comm           communityMaterialiser
	writer         frameWriter
	maxCommunities int // 0 = unlimited; see SummariseConfig.MaxCommunities
}

// Run executes one complete summarisation pass: load prior briefs,
// materialise communities, call Stage.Summarise, append resulting
// frames. A per-community LLM failure is absorbed inside Stage and
// surfaces as CommunityResult.StatusFailed — the pass itself still
// succeeds. A catastrophic failure (Neo4j down, log unwritable) is
// returned up so the analyze pipeline can record it on Result.
func (r *summariserRunner) Run(ctx context.Context) error {
	communities, err := r.comm.Materialise(ctx)
	if err != nil {
		return fmt.Errorf("summarise: materialise: %w", err)
	}
	if len(communities) == 0 {
		// No communities to summarise — probably a fresh graph. Don't
		// treat this as an error; the stitch pass would be empty too.
		return nil
	}
	if r.maxCommunities > 0 && len(communities) > r.maxCommunities {
		communities = communities[:r.maxCommunities]
	}
	prior, err := r.prior.Load(ctx)
	if err != nil {
		return fmt.Errorf("summarise: load prior briefs: %w", err)
	}
	report, err := r.stage.Summarise(ctx, r.project, communities, prior)
	if err != nil {
		return fmt.Errorf("summarise: stage: %w", err)
	}
	if len(report.Frames) == 0 {
		return nil
	}
	if err := r.writer.Write(report.Frames); err != nil {
		return fmt.Errorf("summarise: write frames: %w", err)
	}
	return nil
}
