// cmd/cortex/eval.go wires the `cortex eval` command group. The sole
// subcommand today is `cortex eval calibrate-floors`, which consumes a
// deep-eval dump (eval/deep/runs/*.json) and sweeps the layered
// relevance gate knobs to find the (sim_floor_hard, sim_floor_strict,
// rescue_alpha, composite_floor) combination that maximises F1 on the
// labeled question set. See bead cortex-9ti.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Evaluation and calibration utilities",
		Long:  "cortex eval groups commands that consume deep-eval dumps to report quality metrics or tune retrieval knobs against a labeled question set.",
	}
	cmd.AddCommand(newEvalCalibrateFloorsCmd())
	return cmd
}

func newEvalCalibrateFloorsCmd() *cobra.Command {
	var dumpPath string
	cmd := &cobra.Command{
		Use:   "calibrate-floors",
		Short: "Sweep the layered-gate knobs against a deep-eval dump and emit a recommended config",
		Long: "cortex eval calibrate-floors loads a deep-eval raw recall dump (eval/deep/runs/*.json), " +
			"re-plays the layered relevance gate at a coarse grid of (sim_floor_hard, sim_floor_strict, " +
			"rescue_alpha, composite_floor) values against per-hit similarity/PPR, and emits the " +
			"highest-F1 grid point as a retrieval.relevance_gate YAML block. Labels are derived from " +
			"each record's expected_modules list: a hit whose module is in that list is positive, " +
			"every other hit is negative. The one-line calibration summary goes to stderr.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dumpPath == "" {
				return fmt.Errorf("--dump is required")
			}
			return runCalibrateFloors(dumpPath, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&dumpPath, "dump", "", "path to a deep-eval raw recall dump (eval/deep/runs/*.json)")
	return cmd
}

// evalDumpHit mirrors the per-hit fields the runner persists. We only
// need similarity, ppr, and module for calibration; every other field
// is optional / ignored.
type evalDumpHit struct {
	Module     string  `json:"module"`
	Similarity float64 `json:"similarity"`
	PPRScore   float64 `json:"ppr_score"`
}

type evalDumpRetrieval struct {
	Query string        `json:"query"`
	Hits  []evalDumpHit `json:"hits"`
}

type evalDumpRecord struct {
	ID              int                 `json:"id"`
	Query           string              `json:"q"`
	ExpectedModules []string            `json:"expected_modules"`
	Retrievals      []evalDumpRetrieval `json:"retrievals"`
}

type evalDump struct {
	Records []evalDumpRecord `json:"records"`
}

// gateConfig is a candidate grid point.
type gateConfig struct {
	SimFloorHard   float64
	SimFloorStrict float64
	RescueAlpha    float64
	CompositeFloor float64
}

// labeledHit carries the minimal per-hit state calibration needs.
// Positive=true means the hit's module was in the record's
// expected_modules list.
type labeledHit struct {
	sim      float64
	ppr      float64
	positive bool
}

// simulateLayeredGate mirrors the drop logic in
// internal/recall/pipeline.go (cortex-y6g + cortex-2sg) so the
// calibration script can replay the gate on dump data without pulling
// in the full Pipeline. Duplication is intentional — keeping the
// calibration self-contained avoids a dependency cycle with recall
// and makes it cheap to sweep 108+ grid points per query.
// Returns kept=true for candidates that survive every stage.
func simulateLayeredGate(sim, ppr float64, cfg gateConfig, gateSimWeight, gatePPRWeight float64) bool {
	if sim < cfg.SimFloorHard {
		return false
	}
	if sim < cfg.SimFloorStrict {
		// Option-1 smooth rescue (quantile mode is query-scoped and
		// hard to replay here; Option-1 is the gate's fallback and the
		// more conservative bound, so we sweep against it).
		if sim < cfg.SimFloorHard-cfg.RescueAlpha*ppr {
			return false
		}
	}
	if cfg.CompositeFloor > 0 {
		if gateSimWeight*sim+gatePPRWeight*ppr < cfg.CompositeFloor {
			return false
		}
	}
	return true
}

// f1Score returns the harmonic mean of precision and recall. Zero
// denominators short-circuit to zero so degenerate sweeps still rank
// consistently.
func f1Score(tp, fp, fn int) float64 {
	if tp == 0 {
		return 0
	}
	precision := float64(tp) / float64(tp+fp)
	recall := float64(tp) / float64(tp+fn)
	if precision+recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}

// scoreGrid evaluates one candidate config against a prebuilt slice of
// labeled hits and returns the resulting F1.
func scoreGrid(hits []labeledHit, cfg gateConfig) float64 {
	const gateSimWeight, gatePPRWeight = 0.7, 0.3
	var tp, fp, fn int
	for _, h := range hits {
		kept := simulateLayeredGate(h.sim, h.ppr, cfg, gateSimWeight, gatePPRWeight)
		switch {
		case kept && h.positive:
			tp++
		case kept && !h.positive:
			fp++
		case !kept && h.positive:
			fn++
		}
	}
	return f1Score(tp, fp, fn)
}

// extractLabeledHits walks every record's retrievals and tags each hit
// as positive/negative using the record's expected_modules list. If no
// retrieval carries per-hit similarity data we return a dedicated
// sentinel error — the operator then knows to re-run the deep eval
// against a cortex build that emits sim/ppr in --json output.
func extractLabeledHits(d *evalDump) ([]labeledHit, error) {
	out := make([]labeledHit, 0, 128)
	sawScore := false
	for _, rec := range d.Records {
		expected := make(map[string]struct{}, len(rec.ExpectedModules))
		for _, m := range rec.ExpectedModules {
			expected[m] = struct{}{}
		}
		for _, r := range rec.Retrievals {
			for _, h := range r.Hits {
				if h.Similarity != 0 || h.PPRScore != 0 {
					sawScore = true
				}
				_, ok := expected[h.Module]
				out = append(out, labeledHit{
					sim:      h.Similarity,
					ppr:      h.PPRScore,
					positive: ok,
				})
			}
		}
	}
	if !sawScore {
		return nil, fmt.Errorf("calibration requires per-hit sim/ppr in the dump; re-run deep eval with the latest cortex recall --json")
	}
	return out, nil
}

// runCalibrateFloors is the top-level command entrypoint, exported as
// an unexported helper for testability.
func runCalibrateFloors(dumpPath string, stdout, stderr io.Writer) error {
	raw, err := os.ReadFile(dumpPath)
	if err != nil {
		return fmt.Errorf("read dump: %w", err)
	}
	var d evalDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("parse dump: %w", err)
	}
	hits, err := extractLabeledHits(&d)
	if err != nil {
		return err
	}
	best, baseline, nPoints := sweepCalibrationGrid(hits)
	if _, err := fmt.Fprint(stdout, renderGateYAML(best.cfg)); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "calibration: F1=%.2f (best of N=%d grid points; baseline F1=%.2f)\n",
		best.f1, nPoints, baseline)
	return nil
}

type sweepResult struct {
	cfg gateConfig
	f1  float64
}

// sweepCalibrationGrid evaluates every grid point and returns the
// best config, the current-default baseline F1, and the grid size. The
// grid is deliberately coarse (4x3x3x3 = 108 points) — it's a one-shot
// script so we don't need streaming or pruning.
func sweepCalibrationGrid(hits []labeledHit) (sweepResult, float64, int) {
	hardGrid := []float64{0.30, 0.35, 0.40, 0.45}
	strictGrid := []float64{0.50, 0.55, 0.60}
	alphaGrid := []float64{0.10, 0.15, 0.20}
	compositeGrid := []float64{0.40, 0.45, 0.50}

	baseline := scoreGrid(hits, gateConfig{
		SimFloorHard:   0.40,
		SimFloorStrict: 0.55,
		RescueAlpha:    0.15,
		CompositeFloor: 0.45,
	})

	var best sweepResult
	points := 0
	for _, hard := range hardGrid {
		for _, strict := range strictGrid {
			if strict <= hard {
				continue
			}
			for _, alpha := range alphaGrid {
				for _, comp := range compositeGrid {
					points++
					cfg := gateConfig{
						SimFloorHard:   hard,
						SimFloorStrict: strict,
						RescueAlpha:    alpha,
						CompositeFloor: comp,
					}
					f1 := scoreGrid(hits, cfg)
					if f1 > best.f1 {
						best = sweepResult{cfg: cfg, f1: f1}
					}
				}
			}
		}
	}
	return best, baseline, points
}

// renderGateYAML emits a retrieval.relevance_gate block in the format
// expected by ~/.cortex/config.yaml. The weights/ppr_baseline_min_n
// fields are included at their production defaults so an operator can
// paste the block verbatim without further editing.
func renderGateYAML(cfg gateConfig) string {
	// Deterministic key order: use an explicit slice, not a map.
	keys := []struct {
		name  string
		value string
	}{
		{"sim_floor_hard", fmt.Sprintf("%.2f", cfg.SimFloorHard)},
		{"sim_floor_strict", fmt.Sprintf("%.2f", cfg.SimFloorStrict)},
		{"rescue_alpha", fmt.Sprintf("%.2f", cfg.RescueAlpha)},
		{"composite_floor", fmt.Sprintf("%.2f", cfg.CompositeFloor)},
		{"gate_sim_weight", "0.7"},
		{"gate_ppr_weight", "0.3"},
		{"ppr_baseline_min_n", "25"},
	}
	out := "retrieval:\n  relevance_gate:\n"
	for _, k := range keys {
		out += fmt.Sprintf("    %s: %s\n", k.name, k.value)
	}
	return out
}

