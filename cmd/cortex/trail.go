// cmd/cortex/trail.go wires the cortex trail subcommands onto the
// internal/trail package and the LLM-backed summary path. The command
// shells are kept thin: flag parsing, dependency assembly, then a
// single call into trail.Begin / trail.End / trail.Load / trail.List.
//
// Replaces the notImplemented stubs in newTrailCmd().
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/ollama"
	"github.com/nixlim/cortex/internal/prompts"
	"github.com/nixlim/cortex/internal/trail"
)

// envActiveTrail is the environment variable cortex trail begin sets
// (via the operator's shell) and cortex trail end reads. Pulling it
// from a constant keeps the contract documented in one place.
const envActiveTrail = "CORTEX_TRAIL_ID"

// newTrailCmdReal returns a fully wired `cortex trail` parent with all
// four subcommands attached. commands.go calls this in place of the
// stub form so each subcommand has a real RunE.
func newTrailCmdReal() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trail",
		Short: "Manage episodic trails",
	}
	cmd.AddCommand(newTrailBeginCmd())
	cmd.AddCommand(newTrailEndCmd())
	cmd.AddCommand(newTrailShowCmd())
	cmd.AddCommand(newTrailListCmd())
	return cmd
}

// ---------------------------------------------------------------------
// trail begin
// ---------------------------------------------------------------------

func newTrailBeginCmd() *cobra.Command {
	var (
		agent    string
		nameFlag string
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "begin",
		Short: "Open a trail and print its id",
		Long: "cortex trail begin mints a new trail entity, writes its kind/agent/" +
			"name/started_at datoms in one transaction group, and prints the " +
			"new trail id on stdout. Capture the printed value in the shell " +
			"variable CORTEX_TRAIL_ID so subsequent observe and trail end calls " +
			"can attach to it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)
			writer, err := log.NewWriter(segDir)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_OPEN_FAILED",
					"could not open segment directory", err), jsonFlag)
			}
			defer writer.Close()

			id, err := trail.Begin(writer,
				defaultActor(),
				ulid.Make().String(),
				agent,
				nameFlag,
				func() time.Time { return time.Now().UTC() },
			)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent identifier (required)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "human-readable trail label (required)")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit JSON envelope on failure")
	return cmd
}

// ---------------------------------------------------------------------
// trail end
// ---------------------------------------------------------------------

func newTrailEndCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "end",
		Short: "Close a trail and synthesize a summary",
		Long: "cortex trail end reads CORTEX_TRAIL_ID from the environment, " +
			"materializes the trail's entries, asks the host generation " +
			"model for a short narrative summary, and appends ended_at + " +
			"summary datoms in one transaction group. Exits 2 with " +
			"NO_ACTIVE_TRAIL when CORTEX_TRAIL_ID is not set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			trailID := os.Getenv(envActiveTrail)
			if trailID == "" {
				return emitAndExit(cmd, errs.Validation("NO_ACTIVE_TRAIL",
					"CORTEX_TRAIL_ID is not set; run 'cortex trail begin' first",
					nil), jsonFlag)
			}

			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			// Build the manifest before opening the writer so the read
			// scan does not race with our own pending append.
			report, err := log.Load(segDir, log.LoadOptions{})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
					"could not enumerate log segments", err), jsonFlag)
			}
			manifest, err := trail.Load(report.Healthy, trailID)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}

			// Build the prompt body from the trail's entries. The trail
			// summary template wraps the body in USER_CONTENT delimiters
			// and forbids in-band instructions, so any malicious entry
			// content is contained.
			body := buildTrailPromptBody(manifest)
			rendered, err := prompts.Render(prompts.NameTrailSummary, prompts.Data{Body: body})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("PROMPT_RENDER_FAILED",
					"could not render trail summary prompt", err), jsonFlag)
			}

			// Generate via the host Ollama. Honour
			// timeouts.trail_summary_seconds as the per-call budget.
			gen := ollama.NewHTTPClient(ollama.Config{
				Endpoint:              cfg.Endpoints.Ollama,
				EmbeddingModel:        defaultEmbeddingModel,
				GenerationModel:       defaultGenerationModel,
				EmbeddingTimeout:      time.Duration(cfg.Timeouts.EmbeddingSeconds) * time.Second,
				LinkDerivationTimeout: time.Duration(cfg.Timeouts.TrailSummarySeconds) * time.Second,
				NumCtx:                cfg.Ollama.NumCtx,
			})
			budget := time.Duration(cfg.Timeouts.TrailSummarySeconds) * time.Second
			if budget <= 0 {
				budget = 60 * time.Second
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), budget)
			defer cancel()
			summary, err := gen.Generate(ctx, rendered)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("TRAIL_SUMMARY_FAILED",
					"could not generate trail summary", err), jsonFlag)
			}
			summary = strings.TrimSpace(summary)
			if summary == "" {
				// The model produced nothing — synthesize a deterministic
				// fallback so the AC "non-empty summary string" still
				// holds. The fallback names the entry count so an
				// operator can tell apart the empty-LLM path from a
				// real narrative.
				summary = fmt.Sprintf("trail closed with %d entries", len(manifest.Entries))
			}

			writer, err := log.NewWriter(segDir)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_OPEN_FAILED",
					"could not open segment directory", err), jsonFlag)
			}
			defer writer.Close()
			if err := trail.End(writer,
				defaultActor(),
				ulid.Make().String(),
				trailID,
				summary,
				func() time.Time { return time.Now().UTC() },
			); err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			fmt.Fprintln(cmd.OutOrStdout(), trailID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit JSON envelope on failure")
	return cmd
}

// buildTrailPromptBody renders the trail manifest into the opaque text
// the prompt template will wrap in USER_CONTENT delimiters. Each entry
// is one line so the model can reason about a sequence; the trail
// metadata header gives it a label and timestamps.
func buildTrailPromptBody(m *trail.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Trail %s\n", m.ID)
	if m.Name != "" {
		fmt.Fprintf(&b, "Name: %s\n", m.Name)
	}
	if m.Agent != "" {
		fmt.Fprintf(&b, "Agent: %s\n", m.Agent)
	}
	if m.StartedAt != "" {
		fmt.Fprintf(&b, "Started: %s\n", m.StartedAt)
	}
	fmt.Fprintf(&b, "Entries (%d):\n", len(m.Entries))
	for i, e := range m.Entries {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, e.ID)
	}
	return b.String()
}

// ---------------------------------------------------------------------
// trail show
// ---------------------------------------------------------------------

func newTrailShowCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "show <trail-id>",
		Short: "Show a trail with its member entries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)
			report, err := log.Load(segDir, log.LoadOptions{})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
					"could not enumerate log segments", err), jsonFlag)
			}
			manifest, err := trail.Load(report.Healthy, id)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(manifest)
			}
			renderTrailManifest(cmd, manifest)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

func renderTrailManifest(cmd *cobra.Command, m *trail.Manifest) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "id:         %s\n", m.ID)
	if m.Name != "" {
		fmt.Fprintf(w, "name:       %s\n", m.Name)
	}
	if m.Agent != "" {
		fmt.Fprintf(w, "agent:      %s\n", m.Agent)
	}
	if m.StartedAt != "" {
		fmt.Fprintf(w, "started_at: %s\n", m.StartedAt)
	}
	if m.EndedAt != "" {
		fmt.Fprintf(w, "ended_at:   %s\n", m.EndedAt)
	}
	if m.Summary != "" {
		fmt.Fprintf(w, "summary:    %s\n", m.Summary)
	}
	fmt.Fprintf(w, "entries (%d):\n", len(m.Entries))
	for _, e := range m.Entries {
		fmt.Fprintf(w, "  %s  tx=%s\n", e.ID, e.Tx)
	}
}

// ---------------------------------------------------------------------
// trail list
// ---------------------------------------------------------------------

func newTrailListCmd() *cobra.Command {
	var (
		limit    int
		offset   int
		jsonFlag bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List trails in reverse chronological order",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)
			report, err := log.Load(segDir, log.LoadOptions{})
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_LOAD_FAILED",
					"could not enumerate log segments", err), jsonFlag)
			}
			rows, err := trail.List(report.Healthy)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			rows = paginate(rows, offset, limit)
			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			w := cmd.OutOrStdout()
			for _, r := range rows {
				ended := r.EndedAt
				if ended == "" {
					ended = "(in progress)"
				}
				fmt.Fprintf(w, "%s  %-20s  agent=%s  entries=%d  ended=%s\n",
					r.ID, truncate(r.Name, 20), r.Agent, r.EntryCount, ended)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum rows to print (0 = all)")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip from the start")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// paginate trims rows according to offset and limit. Both inputs are
// clamped so an out-of-range request returns an empty slice rather
// than panicking.
func paginate(rows []trail.Summary, offset, limit int) []trail.Summary {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return nil
	}
	rows = rows[offset:]
	if limit > 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	return rows
}

// truncate clips s to n runes for table layout. Used by the human
// list rendering only; the JSON output is unaffected.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

