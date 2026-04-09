// cmd/cortex/observe.go wires the `cortex observe` subcommand onto
// the write.Pipeline. The command file is kept intentionally thin:
// everything that can be validated or structured lives in
// internal/write; this file only parses flags, assembles the minimum
// set of dependencies a one-shot invocation needs, and renders the
// final outcome.
//
// The subcommand is registered from commands.go — newObserveCmdReal
// replaces the stub there.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/psi"
	"github.com/nixlim/cortex/internal/security/secrets"
	"github.com/nixlim/cortex/internal/write"
)

// newObserveCmdReal builds the production-ready observe command. It
// replaces the stub in commands.go once features-dev landed the
// write pipeline. The command follows the Cortex error-envelope
// convention: validation failures exit 2 via errs.Emit, operational
// failures exit 1, success prints the entry id on stdout and exits 0.
func newObserveCmdReal() *cobra.Command {
	var (
		kindFlag    string
		facetFlag   []string
		subjectFlag string
		trailFlag   string
		jsonFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "observe <body>",
		Short: "Write an observation entry through the standard pipeline",
		Long: "cortex observe validates the request, scans for secrets, resolves the " +
			"subject PSI, appends a transaction-group to the log, and applies the " +
			"datoms to Neo4j and Weaviate. The command exits 0 and prints the new " +
			"entry id on success, 2 on validation failure, and 1 on operational " +
			"failure. Backend apply errors never roll back the committed log.",
		Args: cobra.ArbitraryArgs, // empty body is a validation error, not a usage error
		RunE: func(cmd *cobra.Command, args []string) error {
			body := strings.Join(args, " ")

			facets, ferr := parseFacetFlag(facetFlag)
			if ferr != nil {
				return emitAndExit(cmd, ferr, jsonFlag)
			}

			// Load config so the log.d path and timeouts match the
			// operator's environment. A missing config file is fine
			// — Load returns the defaults.
			cfgPath := defaultConfigPath()
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
					"could not load ~/.cortex/config.yaml", err), jsonFlag)
			}
			segDir := expandHome(cfg.Log.SegmentDir)

			pipeline, cleanup, err := buildObservePipeline(segDir)
			if err != nil {
				return emitAndExit(cmd, err, jsonFlag)
			}
			defer cleanup()

			req := write.ObserveRequest{
				Body:    body,
				Kind:    kindFlag,
				Facets:  facets,
				Subject: subjectFlag,
				TrailID: trailFlag,
			}
			res, err := pipeline.Observe(context.Background(), req)
			if err != nil {
				// A partial success (log committed but backend apply
				// failed) still prints the entry id so ops tooling
				// can log it. res is non-nil in that case.
				if res != nil && res.EntryID != "" {
					fmt.Fprintln(cmd.OutOrStdout(), res.EntryID)
				}
				return emitAndExit(cmd, err, jsonFlag)
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.EntryID)
			return nil
		},
	}
	cmd.Flags().StringVar(&kindFlag, "kind", "", "frame type (Observation | SessionReflection | ObservedRace)")
	cmd.Flags().StringSliceVar(&facetFlag, "facets", nil, "comma-separated key:value facet list (must include domain, project)")
	cmd.Flags().StringVar(&subjectFlag, "subject", "", "optional PSI subject to attach (canonical or alias)")
	cmd.Flags().StringVar(&trailFlag, "trail", "", "optional trail id to attach this observation to")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON error envelope on failure")
	return cmd
}

// parseFacetFlag converts the --facets=key:value,key:value slice that
// cobra produced into a map. A malformed entry (missing colon, empty
// key, empty value) is surfaced as a validation error so the CLI
// exits 2 with a precise envelope rather than silently dropping the
// bad token.
func parseFacetFlag(raw []string) (map[string]string, error) {
	out := make(map[string]string, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.Index(entry, ":")
		if idx <= 0 || idx == len(entry)-1 {
			return nil, errs.Validation("MALFORMED_FACET",
				fmt.Sprintf("facet %q must be key:value", entry),
				map[string]any{"facet": entry})
		}
		k := strings.TrimSpace(entry[:idx])
		v := strings.TrimSpace(entry[idx+1:])
		if k == "" || v == "" {
			return nil, errs.Validation("MALFORMED_FACET",
				fmt.Sprintf("facet %q has empty key or value", entry),
				map[string]any{"facet": entry})
		}
		out[k] = v
	}
	return out, nil
}

// buildObservePipeline constructs a write.Pipeline with the minimum
// set of dependencies a one-shot observe invocation requires: a log
// writer pointing at segDir, the built-in secret detector, and a
// fresh per-invocation PSI registry (which the pipeline will mint
// into on first sight of a subject). Neo4j, Weaviate, and the
// embedder are intentionally left nil in Phase 1 while adapter
// wiring lands in later beads; the log commit is the authoritative
// step and self-healing replay brings the backends up to date on
// the next command.
func buildObservePipeline(segDir string) (*write.Pipeline, func(), error) {
	detector, err := secrets.LoadBuiltin(0)
	if err != nil {
		return nil, func() {}, errs.Operational("SECRETS_INIT_FAILED",
			"could not initialize secret detector", err)
	}
	writer, err := log.NewWriter(segDir)
	if err != nil {
		return nil, func() {}, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err)
	}
	cleanup := func() { _ = writer.Close() }

	p := &write.Pipeline{
		Detector:     detector,
		Registry:     psi.NewRegistry(),
		Log:          writer,
		Actor:        defaultActor(),
		InvocationID: ulid.Make().String(),
	}
	return p, cleanup, nil
}

// emitAndExit writes err via the errs package (so the envelope shape
// and stderr scrubbing rules are applied uniformly) and returns a
// SilentError that cobra will surface with the right exit code. We
// lean on cobra's Execute -> os.Exit dance by wrapping the exit code
// in a custom error type the main loop checks.
func emitAndExit(cmd *cobra.Command, err error, jsonMode bool) error {
	code := errs.Emit(cmd.ErrOrStderr(), err, jsonMode)
	return &exitCodeErr{code: code}
}

// exitCodeErr is a sentinel error that tells main() which process
// exit code to return. cobra's default behaviour would map any non-
// nil RunE error to exit 1, which is wrong for validation failures
// (exit 2). Handling it in main() keeps the observe command in
// charge of its own exit semantics.
type exitCodeErr struct{ code int }

func (e *exitCodeErr) Error() string { return fmt.Sprintf("exit %d", e.code) }

// exitCodeFromError extracts the desired process exit code from an
// error returned by a subcommand's RunE. Called from main.go before
// os.Exit.
func exitCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	var e *exitCodeErr
	if errors.As(err, &e) {
		return e.code
	}
	return 1
}

// defaultConfigPath returns ~/.cortex/config.yaml. A helper rather
// than a constant because os.UserHomeDir can fail on unusual systems
// and we want to degrade gracefully.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".cortex/config.yaml"
	}
	return filepath.Join(home, ".cortex", "config.yaml")
}

// expandHome rewrites a leading "~" in cfg paths to the user's home
// directory. The config layer stores paths in their raw "~/.cortex/..."
// form so the on-disk config file is portable across machines.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[2:])
}

// defaultActor returns the best available identity for the Actor
// field of a datom: $USER if set, otherwise "cortex".
func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cortex"
}
