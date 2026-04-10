// cmd/cortex/bench.go wires `cortex bench` onto the internal/bench
// harness. The command constructs a bench.Config from the --profile
// and --corpus flags, registers stub operations (the real pipeline
// closures will be injected once the full stack is wired), runs the
// harness, and renders the report in JSON or human form.
//
// The bench package (adapter-dev, cortex-4kq.55) owns all scoring
// logic; this file is the thin CLI shell (ops-dev).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/bench"
	"github.com/nixlim/cortex/internal/errs"
)

// newBenchCmdReal returns the wired `cortex bench` command.
// commands.go installs it in place of the notImplemented stub.
func newBenchCmdReal() *cobra.Command {
	var (
		profileFlag string
		corpusFlag  string
		jsonFlag    bool
	)
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run the Cortex P1 benchmark suite",
		Long: "cortex bench executes the scripted benchmark sequence under " +
			"the chosen profile (P1-dev or P1-ci) and corpus size (small or " +
			"medium), writing the JSON report to ~/.cortex/bench/latest.json. " +
			"Exit 0 means every operation passed its envelope; exit 1 means " +
			"at least one failed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			corpus := bench.CorpusName(corpusFlag)
			var profile bench.Profile
			switch bench.ProfileName(profileFlag) {
			case bench.ProfileP1Dev:
				profile = bench.P1DevProfile(corpus)
			case bench.ProfileP1CI:
				profile = bench.P1CIProfile(corpus)
			default:
				return emitAndExit(cmd, errs.Validation("BAD_PROFILE",
					fmt.Sprintf("unknown profile %q, expected P1-dev or P1-ci", profileFlag),
					nil), jsonFlag)
			}

			// Stub operations — once the full stack is wired, these will be
			// replaced with closures over the real pipelines. For now each
			// op succeeds instantly so the harness shape can be exercised.
			ops := []bench.Operation{
				bench.OperationFunc{OpName: bench.OpRecall, Fn: func(ctx context.Context) error { return nil }},
				bench.OperationFunc{OpName: bench.OpObserve, Fn: func(ctx context.Context) error { return nil }},
				bench.OperationFunc{OpName: bench.OpReflectDryRun, Fn: func(ctx context.Context) error { return nil }},
				bench.OperationFunc{OpName: bench.OpAnalyzeDryRun, Fn: func(ctx context.Context) error { return nil }},
			}

			home, _ := os.UserHomeDir()
			outDir := filepath.Join(home, ".cortex", "bench")
			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return emitAndExit(cmd, errs.Operational("BENCH_DIR_FAILED",
					"could not create ~/.cortex/bench", err), jsonFlag)
			}
			outPath := filepath.Join(outDir, "latest.json")

			cfg := bench.Config{
				Profile:    profile,
				Operations: ops,
				OutputPath: outPath,
			}

			runner := bench.NewRunner()
			report, err := runner.Run(cmd.Context(), cfg)
			if err != nil {
				return emitAndExit(cmd, errs.Operational("BENCH_RUN_FAILED",
					"bench harness error", err), jsonFlag)
			}

			if jsonFlag {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			renderBenchReport(cmd, report)

			if !report.Passed {
				return &exitCodeErr{code: 1}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&profileFlag, "profile", "P1-dev", "envelope profile: P1-dev or P1-ci")
	cmd.Flags().StringVar(&corpusFlag, "corpus", "small", "fixture corpus size: small or medium")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON")
	return cmd
}

// renderBenchReport prints a terse human-readable bench summary.
func renderBenchReport(cmd *cobra.Command, r *bench.Report) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "cortex bench  profile=%s  corpus=%s\n", r.Profile, r.Corpus)
	fmt.Fprintf(w, "  elapsed: %s\n", r.CompletedAt.Sub(r.StartedAt).Truncate(time.Millisecond))
	for name, op := range r.Operations {
		mark := "PASS"
		if !op.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  %-20s  %s  p95=%s  envelope=%s  n=%d  errors=%d\n",
			name, mark,
			op.P95.Truncate(time.Millisecond),
			op.Envelope.Truncate(time.Millisecond),
			op.Count, op.Errors)
	}
	overall := "PASSED"
	if !r.Passed {
		overall = "FAILED"
	}
	fmt.Fprintf(w, "  overall: %s\n", overall)
	if len(r.FailingOperations) > 0 {
		fmt.Fprintf(w, "  failing: %v\n", r.FailingOperations)
	}
}
