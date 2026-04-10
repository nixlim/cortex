// cmd/cortex/communities.go wires `cortex communities` and
// `cortex community show` onto the internal/community read API.
// Both subcommands open a short-lived Neo4j Bolt client, hand it
// to community.ListCommunities / community.ShowCommunity through
// the QueryGraph seam, and render the result in human or JSON form.
//
// Replaces the notImplemented stubs in newCommunitiesCmd /
// newCommunityCmd in commands.go. Spec references: docs/spec/cortex-spec.md
// FR-029 / SC-013 (community CLI surfaces hierarchical communities and
// their summaries).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/community"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/infra"
	"github.com/nixlim/cortex/internal/neo4j"
)

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
	return cmd
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
