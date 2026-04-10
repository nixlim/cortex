// cmd/cortex/migrate.go wires `cortex migrate` onto internal/migrate.
// The command opens the MemPalace JSONL export, opens the local log
// for append, builds an ObserveFunc that closes over a write.Pipeline,
// and streams every record through migrate.Run. The report (created/
// reused/retracted/skipped counts plus the synthesized trail id) is
// rendered on stdout.
//
// Replaces the notImplemented stub in commands.go (CRIT-003 fix).
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/migrate"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/write"
)

// newMigrateCmdReal returns the wired `cortex migrate` command.
func newMigrateCmdReal() *cobra.Command {
	var (
		fromMempalace  string
		defaultDomain  string
		defaultProject string
		jsonFlag       bool
	)
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate content from an external knowledge system",
		Long: "cortex migrate --from-mempalace=<path> reads a MemPalace JSONL " +
			"export and writes one Cortex entry per record through the standard " +
			"observe write pipeline. Drawers become Observation entries; diaries " +
			"become SessionReflection entries anchored on a synthesized trail. " +
			"Every migrated entry carries migrated=true and source_system=" +
			"mempalace facets so analyze --find-patterns can exclude them.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(fromMempalace) == "" {
				return emitAndExit(cmd, errs.Validation("MISSING_SOURCE",
					"cortex migrate requires --from-mempalace=<path>", nil), jsonFlag)
			}
			path, err := migrate.CanonicalizePath(fromMempalace)
			if err != nil {
				return emitAndExit(cmd, errs.Validation("INVALID_SOURCE_PATH",
					err.Error(), nil), jsonFlag)
			}

			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			// Open the export file first so a missing/unreadable path
			// fails before we grab the log flock.
			f, err := os.Open(path)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("MEMPALACE_OPEN_FAILED",
					fmt.Sprintf("could not open %s", path), err), jsonFlag)
			}
			defer f.Close()

			writer, err := log.NewWriter(segDir)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("LOG_OPEN_FAILED",
					"could not open log segment directory", err), jsonFlag)
			}
			defer writer.Close()

			detector, err := secrets.LoadBuiltin(0)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("SECRETS_INIT_FAILED",
					"could not initialize secret detector", err), jsonFlag)
			}

			invocationID := ulid.Make().String()
			wp := &write.Pipeline{
				Detector:     detector,
				Registry:     psi.NewRegistry(),
				Log:          writer,
				Actor:        defaultActor(),
				InvocationID: invocationID,
			}

			observe := func(ctx context.Context, req migrate.ObserveRequest) (*migrate.ObserveResult, error) {
				res, err := wp.Observe(ctx, write.ObserveRequest{
					Body:    req.Body,
					Kind:    req.Kind,
					Facets:  req.Facets,
					Subject: req.Subject,
					TrailID: req.TrailID,
				})
				if err != nil {
					return nil, err
				}
				return &migrate.ObserveResult{EntryID: res.EntryID, Tx: res.Tx}, nil
			}

			opts := migrate.RunOptions{
				DefaultDomain:      defaultDomain,
				DefaultProject:     defaultProject,
				SynthesizedTrailID: "trail:migrate:mempalace:" + ulid.Make().String(),
			}

			report, err := migrate.Run(cmd.Context(), f, observe, opts)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("MIGRATE_FAILED",
					"mempalace migration aborted", err), jsonFlag)
			}

			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "cortex migrate ok\n")
			fmt.Fprintf(w, "  source  : %s\n", path)
			fmt.Fprintf(w, "  trail   : %s\n", report.TrailID)
			fmt.Fprintf(w, "  created : %d\n", report.Created)
			fmt.Fprintf(w, "  reused  : %d\n", report.Reused)
			fmt.Fprintf(w, "  skipped : %d\n", report.Skipped)
			if report.Skipped > 0 && len(report.SkippedReasons) > 0 {
				fmt.Fprintf(w, "  first skip reasons:\n")
				for i, reason := range report.SkippedReasons {
					if i >= 5 {
						break
					}
					fmt.Fprintf(w, "    - %s\n", reason)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fromMempalace, "from-mempalace", "", "path to a MemPalace JSONL export (required)")
	cmd.Flags().StringVar(&defaultDomain, "default-domain", "migrated", "domain facet for records that don't carry one")
	cmd.Flags().StringVar(&defaultProject, "default-project", "mempalace", "project facet for records that don't carry one")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON error envelope on failure")
	return cmd
}
