// cmd/summarisesmoke drives the cortex-8sr summariser end-to-end
// against the live ~/.cortex install, bypassing the analyze
// pipeline's accepted-frames gate so we can prove the wiring without
// first landing a cross-project frame. Not shipped — one-shot smoke
// harness.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/claudecli"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/summarise"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := os.ExpandEnv("$HOME/.cortex/config.yaml")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	fmt.Printf("summarise.enabled=%v concurrency=%d max_communities=%d timeout=%ds\n",
		cfg.Summarise.Enabled, cfg.Summarise.Concurrency, cfg.Summarise.MaxCommunities,
		cfg.Summarise.CallTimeoutSeconds)
	if !cfg.Summarise.Enabled {
		return fmt.Errorf("summarise.enabled is false — set it to true in %s", cfgPath)
	}

	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      10 * time.Second,
		MaxPoolSize:  4,
	})
	if err != nil {
		return fmt.Errorf("neo4j: %w", err)
	}
	defer bolt.Close(context.Background())

	segDir := expandHome(cfg.Log.SegmentDir)
	writer, err := log.NewWriter(segDir)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	defer writer.Close()

	stage, err := summarise.New(summarise.Config{
		Runner:      &claudecli.ExecRunner{Command: cfg.Summarise.Command},
		Model:       cfg.Summarise.Model,
		Concurrency: cfg.Summarise.Concurrency,
		CallTimeout: time.Duration(cfg.Summarise.CallTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}

	communities, err := materialise(bolt)
	if err != nil {
		return fmt.Errorf("materialise: %w", err)
	}
	fmt.Printf("materialised %d communities\n", len(communities))
	if cfg.Summarise.MaxCommunities > 0 && len(communities) > cfg.Summarise.MaxCommunities {
		communities = communities[:cfg.Summarise.MaxCommunities]
		fmt.Printf("capped to first %d for smoke test\n", len(communities))
	}
	for _, c := range communities {
		fmt.Printf("  %s  members=%d\n", c.ID, len(c.EntryIDs))
	}

	prior, err := loadPrior(bolt)
	if err != nil {
		return fmt.Errorf("prior: %w", err)
	}
	fmt.Printf("loaded %d prior briefs\n", len(prior))

	start := time.Now()
	report, err := stage.Summarise(context.Background(), "smoke", communities, prior)
	if err != nil {
		return fmt.Errorf("stage.Summarise: %w", err)
	}
	elapsed := time.Since(start)

	fmt.Printf("\n== Report (wall=%v) ==\n", elapsed.Round(time.Millisecond))
	summariseCount, skipCount, failCount := 0, 0, 0
	for _, r := range report.Communities {
		switch r.Status {
		case summarise.StatusSummarised:
			summariseCount++
			fmt.Printf("  %s  summarised  (%dms)\n", r.CommunityID, r.DurationMS)
		case summarise.StatusSkipped:
			skipCount++
			fmt.Printf("  %s  skipped     (hash match, %dms)\n", r.CommunityID, r.DurationMS)
		case summarise.StatusFailed:
			failCount++
			fmt.Printf("  %s  FAILED      %v (%dms)\n", r.CommunityID, r.Err, r.DurationMS)
		}
	}
	fmt.Printf("\nsummarised=%d skipped=%d failed=%d frames=%d project_brief=%v\n",
		summariseCount, skipCount, failCount, len(report.Frames), report.ProjectBrief != nil)
	if report.StitchErr != nil {
		fmt.Printf("stitch_err: %v\n", report.StitchErr)
	}
	if report.ProjectBrief != nil {
		if stitched, ok := report.ProjectBrief.Slots["stitched_narrative"].(string); ok {
			fmt.Printf("\nstitched_narrative:\n  %s\n", truncate(stitched, 400))
		}
	}
	for _, f := range report.Frames {
		if f.Type == "CommunityBrief" {
			theme, _ := f.Slots["theme_label"].(string)
			summary, _ := f.Slots["summary"].(string)
			cid, _ := f.Slots["community_id"].(string)
			fmt.Printf("\nsample CommunityBrief[%s] theme=%q:\n  %s\n", cid, theme, truncate(summary, 400))
			break
		}
	}

	if len(report.Frames) > 0 {
		fmt.Printf("\nwriting %d frames to log...\n", len(report.Frames))
		if err := writeFrames(writer, report.Frames); err != nil {
			return fmt.Errorf("write frames: %w", err)
		}
		fmt.Println("write ok")
	}
	return nil
}

func materialise(c neo4j.Client) ([]summarise.Community, error) {
	cypher := `
MATCH (c:Community {level: 0})<-[:IN_COMMUNITY]-(e)
WHERE e.entry_id IS NOT NULL
WITH c, collect({
  entry_id:    e.entry_id,
  kind:        coalesce(e.kind, ''),
  body:        coalesce(e.body, ''),
  encoding_at: e.encoding_at
}) AS members
WHERE size(members) > 0
RETURN toString(c.community_id) AS community_id, members
ORDER BY community_id
`
	rows, err := c.QueryGraph(context.Background(), cypher, nil)
	if err != nil {
		return nil, err
	}
	out := make([]summarise.Community, 0, len(rows))
	for _, row := range rows {
		cid, _ := row["community_id"].(string)
		members, _ := row["members"].([]any)
		type mv struct {
			id, kind, body string
			ts             time.Time
		}
		ms := make([]mv, 0, len(members))
		for _, raw := range members {
			mm, _ := raw.(map[string]any)
			id, _ := mm["entry_id"].(string)
			if id == "" {
				continue
			}
			kind, _ := mm["kind"].(string)
			body, _ := mm["body"].(string)
			ts, _ := mm["encoding_at"].(string)
			t, _ := time.Parse(time.RFC3339, ts)
			ms = append(ms, mv{id: id, kind: kind, body: body, ts: t})
		}
		if len(ms) == 0 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].id < ms[j].id })
		entryIDs := make([]string, len(ms))
		entries := make([]summarise.Entry, len(ms))
		for i, m := range ms {
			entryIDs[i] = m.id
			entries[i] = summarise.Entry{ID: m.id, Kind: m.kind, Body: m.body, TS: m.ts}
		}
		out = append(out, summarise.Community{ID: summarise.CommunityID(cid), EntryIDs: entryIDs, Entries: entries})
	}
	return out, nil
}

func loadPrior(c neo4j.Client) (map[summarise.CommunityID]summarise.PriorBrief, error) {
	cypher := `
MATCH (f:Frame)
WHERE f.frame_type = 'CommunityBrief' AND f.frame_slots IS NOT NULL
RETURN coalesce(f.entry_id, f.id) AS frame_id, f.frame_slots AS slots, coalesce(f.last_tx, '') AS tx
ORDER BY tx DESC
`
	rows, err := c.QueryGraph(context.Background(), cypher, nil)
	if err != nil {
		return nil, err
	}
	out := make(map[summarise.CommunityID]summarise.PriorBrief)
	for _, row := range rows {
		fid, _ := row["frame_id"].(string)
		slotsRaw, _ := row["slots"].(string)
		var slots map[string]any
		if err := json.Unmarshal([]byte(slotsRaw), &slots); err != nil {
			continue
		}
		cid, _ := slots["community_id"].(string)
		if cid == "" {
			continue
		}
		if _, ok := out[summarise.CommunityID(cid)]; ok {
			continue
		}
		hash, _ := slots["membership_hash"].(string)
		out[summarise.CommunityID(cid)] = summarise.PriorBrief{
			CommunityID:    summarise.CommunityID(cid),
			MembershipHash: hash,
			FrameID:        fid,
		}
	}
	return out, nil
}

func writeFrames(w *log.Writer, frames []summarise.Frame) error {
	for i := range frames {
		f := &frames[i]
		tx := ulid.Make().String()
		frameID := "frame:" + ulid.Make().String()
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		group := []datom.Datom{}
		add := func(attr string, value any) error {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			d := datom.Datom{
				Tx: tx, Ts: ts, Actor: "smoke", Op: datom.OpAdd,
				E: frameID, A: attr, V: raw, Src: "summarise-smoke", InvocationID: tx,
			}
			if err := d.Seal(); err != nil {
				return err
			}
			group = append(group, d)
			return nil
		}
		if err := add("frame.type", f.Type); err != nil {
			return err
		}
		if err := add("frame.slots", f.Slots); err != nil {
			return err
		}
		if err := add("frame.schema_version", summarise.FrameSchemaVersion); err != nil {
			return err
		}
		if _, err := w.Append(group); err != nil {
			return err
		}
	}
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		return os.ExpandEnv("$HOME") + p[1:]
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
