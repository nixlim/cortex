// cmd/cortex/communities.go wires `cortex communities`,
// `cortex communities detect`, and `cortex community show` onto the
// internal/community read + detection API. Each subcommand opens a
// short-lived Neo4j Bolt client and hands it to the appropriate
// community.* entry point.
//
// Replaces the notImplemented stubs in newCommunitiesCmd /
// newCommunityCmd in commands.go. Spec references: docs/spec/cortex-spec.md
// FR-028 (Leiden preferred, Louvain fallback) and FR-029 / SC-013
// (community CLI surfaces hierarchical communities and their summaries).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/community"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/neo4j"
	"github.com/nixlim/cortex/internal/weaviate"
)

// defaultCommunityResolutions are the Leiden γ values used by
// `cortex communities detect` when --resolutions is not supplied.
// One value per hierarchy level (finest → coarsest).
//
// Tuned on 2026-04-15 (cortex-4i2) via an empirical γ sweep on
// the cortex + myagentsgigs graph (5578 projected members). The
// sweep measured detect community counts and reflect --dry-run
// frame-acceptance rates at γ = 3, 4, 5, 6, 8, 10, 15:
//
//   γ=3  →  52 clusters, 100% accepted
//   γ=10 → 193 clusters,  98.4% accepted  (pareto sweet spot)
//   γ=15 → 280 clusters,  95.4% accepted  (first size/cosine rejects)
//
// γ ≈ 10 is the pareto-optimal point: 3.7× more accepted frames
// than γ=3 while rejection stays under 2%. Above γ=10 the graph
// starts fragmenting into sub-3-member and low-cosine clusters,
// quadrupling the rejection rate at γ=15. Levels 1 and 2 scale
// proportionally (5.0, 2.5). Operators should still re-sweep
// per-graph via the --resolutions flag; this measurement covers
// only one graph — a larger, more topically diverse graph may
// push the inflection point lower.
var defaultCommunityResolutions = []float64{10.0, 5.0, 2.5}

// newCommunitiesCmdReal returns the wired `cortex communities` parent.
// commands.go installs it in place of the notImplemented stub.
func newCommunitiesCmdReal() *cobra.Command {
	var (
		level    int
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "communities",
		Short: "List detected knowledge communities",
		Long: "cortex communities lists every persisted Community at the requested " +
			"hierarchy level along with its member count and (when present) " +
			"the LLM-generated summary. Communities below the minimum size " +
			"floor are suppressed; the command exits 0 with an empty list when " +
			"the graph contains no qualifying communities.",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, closeFn, err := openCommunityReader(cmd, jsonFlag)
			if err != nil {
				return err
			}
			defer closeFn()

			out, err := community.ListCommunities(cmd.Context(), r, level)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			renderCommunityList(cmd, level, out)
			return nil
		},
	}
	cmd.Flags().IntVar(&level, "level", 0, "hierarchy level to list (0 = leaves)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	cmd.AddCommand(newCommunitiesDetectCmd())
	return cmd
}

// newCommunitiesDetectCmd is the bootstrap command that closes the
// chicken-and-egg gap between recall, analyze, and reflect: it ensures
// the shared GDS projection is present, runs Leiden (with Louvain
// fallback), and persists the resulting :Community + :IN_COMMUNITY
// schema back to Neo4j. Until something runs this, the analyze and
// reflect cluster sources have nothing to enumerate and both pipelines
// return zero candidates regardless of how much content the log holds.
//
// Detection only — this command does NOT call community.Refresher and
// therefore does not invoke the LLM summarizer. The persisted
// communities have empty Summary fields; cortex analyze --find-patterns
// triggers the summarizer pass on the next refresh after writes land,
// which is the natural place for the LLM cost to accrue.
func newCommunitiesDetectCmd() *cobra.Command {
	var (
		jsonFlag        bool
		resolutionsFlag string
	)
	cmd := &cobra.Command{
		Use:   "detect",
		Short: "Run community detection and persist results to Neo4j",
		Long: "cortex communities detect ensures the cortex.semantic GDS " +
			"projection is present, runs Leiden community detection (Louvain " +
			"fallback), and persists the resulting hierarchy back to Neo4j " +
			"as :Community nodes and :IN_COMMUNITY edges. This is the " +
			"bootstrap step that cortex analyze --find-patterns and cortex " +
			"reflect both depend on; without it their cluster sources find " +
			"no candidates.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resolutions, err := parseResolutionsFlag(resolutionsFlag)
			if err != nil {
				return emitAndExit(cmd, errs.Validation("BAD_RESOLUTIONS",
					err.Error(), nil), jsonFlag)
			}
			return runCommunitiesDetect(cmd, jsonFlag, resolutions)
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	cmd.Flags().StringVar(&resolutionsFlag, "resolutions", "",
		"comma-separated Leiden γ values, finest→coarsest (e.g. 3.0,1.5,0.7); "+
			"number of values determines hierarchy depth")
	return cmd
}

// parseResolutionsFlag turns the --resolutions CLI string into a
// []float64, trimmed and validated. Empty input returns nil so the
// caller falls back to defaultCommunityResolutions; any other input
// must yield a non-empty list of strictly positive floats (Leiden
// requires γ > 0 — γ = 0 degenerates to "one giant community" and
// negative γ is rejected by GDS). Error messages name the bad token
// so a typo is easy to spot in cortex-4i2 sweeps.
func parseResolutionsFlag(s string) ([]float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		token := strings.TrimSpace(p)
		if token == "" {
			continue
		}
		v, err := strconv.ParseFloat(token, 64)
		if err != nil {
			return nil, fmt.Errorf("--resolutions: %q is not a number", token)
		}
		if v <= 0 {
			return nil, fmt.Errorf("--resolutions: %q must be > 0", token)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--resolutions: no values parsed from %q", s)
	}
	return out, nil
}

// communitiesDetectResult is the shape returned by --json. The
// per-level counts let an operator (or a CI gate) verify that
// detection produced a non-empty hierarchy without having to parse
// the human-readable rendering. EnrichedCount / VectorsFetched cover
// the post-detect cosine/MDL enrichment pass (bead cortex-6ef) so an
// operator can confirm that reflect's cluster source will have
// non-null properties to filter on.
type communitiesDetectResult struct {
	Algorithm        string `json:"algorithm"`
	Levels           int    `json:"levels"`
	CommunitiesByLvl []int  `json:"communities_by_level"`
	MembersByLvl     []int  `json:"members_by_level"`
	GraphName        string `json:"graph_name"`
	EnrichedCount    int    `json:"enriched_count"`
	VectorsFetched   int    `json:"vectors_fetched"`
}

// runCommunitiesDetect is the procedural body of the detect command.
// It is split out so the cobra wiring stays declarative and so a
// future test can drive the logic with a fake bolt client without
// dragging in cobra's flag parsing surface.
func runCommunitiesDetect(cmd *cobra.Command, jsonFlag bool, resolutions []float64) error {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err), jsonFlag)
	}
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	bolt, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      30 * time.Second,
		MaxPoolSize:  4,
	})
	if err != nil {
		return emitAndExit(cmd, errs.Operational("NEO4J_UNAVAILABLE",
			"could not connect to neo4j", err), jsonFlag)
	}
	defer func() { _ = bolt.Close(context.Background()) }()

	weaviateClient := newWeaviateClient(cfg)
	res, err := detectAndPersistCommunities(cmd.Context(), bolt, weaviateClient, weaviate.ClassEntry, communityGraphName, resolutions)
	if err != nil {
		return emitAndExit(cmd, err, jsonFlag)
	}

	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	renderCommunitiesDetect(cmd, res)
	return nil
}

// detectAndPersistCommunities is the testable core of the detect
// command. It takes a neo4j.Client (so a fake from
// recall_adapters_test.go's pattern can drive it) plus a graph name
// and returns the per-level counts the renderer needs. The Detector
// is constructed inline because it carries no per-call state and the
// Detect/Persist split is the seam analyze.go already exercises.
func detectAndPersistCommunities(ctx context.Context, client neo4j.Client, fetcher community.VectorFetcher, weaviateClass, graphName string, resolutions []float64) (*communitiesDetectResult, error) {
	if err := ensureCommunityProjection(ctx, client, graphName); err != nil {
		return nil, errs.Operational("PROJECTION_FAILED",
			"could not create or refresh GDS projection", err)
	}

	detector := &community.Detector{
		Neo4j:        client,
		LeidenQuery:  neo4j.CommunityLeidenStreamQuery,
		LouvainQuery: neo4j.CommunityLouvainStreamQuery,
		TopNodeCount: 32,
	}
	if len(resolutions) == 0 {
		resolutions = defaultCommunityResolutions
	}
	cfg := community.Config{
		GraphName:     graphName,
		Resolutions:   resolutions,
		Levels:        len(resolutions),
		MaxIterations: 10,
		Tolerance:     0.0001,
	}

	algorithm := community.AlgorithmLeiden
	hierarchy, err := detector.Detect(ctx, algorithm, cfg)
	if err != nil {
		// Leiden missing or otherwise unhappy: fall back to Louvain
		// per FR-028. The error is preserved if Louvain also fails so
		// the operator sees both attempts in the envelope.
		algorithm = community.AlgorithmLouvain
		hierarchy, err = detector.Detect(ctx, algorithm, cfg)
		if err != nil {
			return nil, errs.Operational("COMMUNITY_DETECT_FAILED",
				"both leiden and louvain failed", err)
		}
	}

	if err := detector.Persist(ctx, hierarchy); err != nil {
		return nil, errs.Operational("COMMUNITY_PERSIST_FAILED",
			"could not persist community hierarchy", err)
	}

	// Post-detect enrichment: compute and persist avg_cosine +
	// mdl_ratio on every level-0 :Community node. Without this step
	// cortex reflect's cluster source (reflect_adapters.go) filters
	// every row on `c.avg_cosine IS NOT NULL AND c.mdl_ratio IS NOT
	// NULL` and returns zero candidates. See bead cortex-6ef.
	enrichment, err := detector.EnrichLevel0(ctx, fetcher, weaviateClass)
	if err != nil {
		return nil, errs.Operational("COMMUNITY_ENRICH_FAILED",
			"could not enrich level-0 communities with cosine/mdl", err)
	}

	res := &communitiesDetectResult{
		Algorithm:        algorithm.String(),
		Levels:           len(hierarchy),
		CommunitiesByLvl: make([]int, len(hierarchy)),
		MembersByLvl:     make([]int, len(hierarchy)),
		GraphName:        graphName,
		EnrichedCount:    enrichment.CommunitiesEnriched,
		VectorsFetched:   enrichment.VectorsFetched,
	}
	for i, level := range hierarchy {
		res.CommunitiesByLvl[i] = len(level)
		members := 0
		for _, c := range level {
			members += len(c.Members)
		}
		res.MembersByLvl[i] = members
	}
	return res, nil
}

// renderCommunitiesDetect prints a terse human-readable summary of
// one detect run. The format mirrors cortex rebuild's output: a
// header line followed by per-level rows so an operator can scan it.
func renderCommunitiesDetect(cmd *cobra.Command, r *communitiesDetectResult) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "cortex communities detect  ok  algorithm=%s  graph=%s\n",
		r.Algorithm, r.GraphName)
	for i := 0; i < r.Levels; i++ {
		fmt.Fprintf(w, "  level=%d  communities=%-4d  members=%d\n",
			i, r.CommunitiesByLvl[i], r.MembersByLvl[i])
	}
	if r.Levels == 0 || r.CommunitiesByLvl[0] == 0 {
		fmt.Fprintln(w, "  (no communities formed; the graph may be too small or too sparse)")
	}
	if r.EnrichedCount > 0 {
		fmt.Fprintf(w, "  enriched=%d  vectors_fetched=%d\n", r.EnrichedCount, r.VectorsFetched)
	}
}

// newCommunityCmdReal returns the wired `cortex community` parent with
// its sole `show` subcommand attached. commands.go installs it in
// place of the notImplemented stub.
func newCommunityCmdReal() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "community",
		Short: "Inspect a specific community",
	}
	cmd.AddCommand(newCommunityShowCmd())
	return cmd
}

// newCommunityShowCmd is the read-side detail command. The single
// positional argument is a canonical "L<level>:C<id>" token (the same
// shape `cortex communities` prints), which keeps level + id together
// so the operator can copy it as one piece.
func newCommunityShowCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "show <community-id>",
		Short: "Show a community with its members and summary",
		Long: "cortex community show fetches one Community node by its " +
			"L<level>:C<id> token (as printed by cortex communities) and " +
			"renders its member count, summary, and the entry-prefixed " +
			"member ids. A missing community exits 1 with NOT_FOUND.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			level, id, parseErr := community.ParseID(args[0])
			if parseErr != nil {
				return emitAndExit(cmd, errs.Validation("BAD_COMMUNITY_ID",
					parseErr.Error(), nil), jsonFlag)
			}
			r, closeFn, err := openCommunityReader(cmd, jsonFlag)
			if err != nil {
				return err
			}
			defer closeFn()

			detail, err := community.ShowCommunity(cmd.Context(), r, level, id)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(detail)
			}
			renderCommunityDetail(cmd, detail)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// openCommunityReader builds a short-lived Bolt client suitable for
// the read-only QueryGraph path. On any failure it routes the error
// through emitAndExit so the caller just propagates it.
func openCommunityReader(cmd *cobra.Command, jsonMode bool) (community.Reader, func(), error) {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err), jsonMode)
	}
	password, _, _ := infra.EnsureNeo4jPassword(cfgPath)
	client, err := neo4j.NewBoltClient(neo4j.Config{
		BoltEndpoint: cfg.Endpoints.Neo4jBolt,
		Username:     "neo4j",
		Password:     password,
		Timeout:      5 * time.Second,
		MaxPoolSize:  2,
	})
	if err != nil {
		return nil, nil, emitAndExit(cmd, errs.Operational("NEO4J_UNAVAILABLE",
			"could not connect to neo4j", err), jsonMode)
	}
	closeFn := func() { _ = client.Close(context.Background()) }
	return client, closeFn, nil
}

// renderCommunityList prints the list output in a column-aligned
// human-readable form. The L<level>:C<id> token is the first column
// so an operator can copy it directly into `cortex community show`.
func renderCommunityList(cmd *cobra.Command, level int, rows []community.Listed) {
	w := cmd.OutOrStdout()
	if len(rows) == 0 {
		fmt.Fprintln(w, "(no communities at this level)")
		return
	}
	for _, row := range rows {
		summary := row.Summary
		if summary == "" {
			summary = "(no summary)"
		}
		fmt.Fprintf(w, "%-12s  members=%-4d  %s\n",
			community.FormatID(level, row.ID), row.MemberCount, summary)
	}
}

// renderCommunityDetail prints the detail output for one community.
// The format mirrors `cortex trail show` so operators see a familiar
// shape: header lines first, then a member id list.
func renderCommunityDetail(cmd *cobra.Command, d *community.Detail) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "id          : %s\n", community.FormatID(d.Level, d.ID))
	fmt.Fprintf(w, "level       : %d\n", d.Level)
	fmt.Fprintf(w, "member_count: %d\n", d.MemberCount)
	if d.Summary != "" {
		fmt.Fprintf(w, "summary     : %s\n", d.Summary)
	}
	if len(d.MemberIDs) == 0 {
		fmt.Fprintln(w, "members     : (none)")
		return
	}
	fmt.Fprintln(w, "members     :")
	for _, id := range d.MemberIDs {
		fmt.Fprintf(w, "  %s\n", id)
	}
}
