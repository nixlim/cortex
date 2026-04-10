// cmd/cortex/lifecycle.go wires `cortex pin`, `cortex unpin`,
// `cortex evict`, and `cortex unevict` onto internal/pipeline/lifecycle.
//
// All four verbs share one log writer, one state loader, and one
// rendering helper so the command tree stays parallel and the audit
// invariants (single tx per command, sealed datoms, lifecycle Src tag)
// stay enforced in one place.
//
// The state loader is a small log scanner that folds the latest LWW
// values for AttrPinned, AttrPinActivation, AttrBaseActivation,
// AttrEvictedAt, and AttrEvictedAtRetracted into an activation.State.
// Phase 1 has no separate "current state" cache; the log is the
// authoritative source and lifecycle commands run rarely enough that
// a one-shot scan is acceptable.
//
// Spec references:
//
//	docs/spec/cortex-spec.md US-12 (pin/unpin/evict/unevict)
//	docs/spec/cortex-spec.md FR-032
//	bead cortex-4kq.50
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/nixlim/cortex/internal/activation"
	"github.com/nixlim/cortex/internal/config"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
	"github.com/nixlim/cortex/internal/pipeline/lifecycle"
)

// newPinCmdReal returns the wired `cortex pin` command. commands.go
// installs it in place of the notImplemented stub.
func newPinCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "pin <entity-id>",
		Short: "Pin an entity so it resists activation decay",
		Long: "cortex pin marks an entry sticky-pinned: its activation never " +
			"decays below the value captured at pin time. Pinning an evicted " +
			"entry also retracts the eviction in the same transaction.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(cmd, args[0], jsonFlag, "pin")
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// newUnpinCmdReal returns the wired `cortex unpin` command.
func newUnpinCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "unpin <entity-id>",
		Short: "Remove a pin from an entity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(cmd, args[0], jsonFlag, "unpin")
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// newEvictCmdReal returns the wired `cortex evict` command.
func newEvictCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "evict <entity-id>",
		Short: "Force activation to zero and block reinforcement",
		Long: "cortex evict forces base_activation=0 and writes a sticky " +
			"evicted_at marker so the entry disappears from default recall " +
			"and every alternate retrieval mode.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(cmd, args[0], jsonFlag, "evict")
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// newUnevictCmdReal returns the wired `cortex unevict` command.
func newUnevictCmdReal() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "unevict <entity-id>",
		Short: "Re-enable reinforcement for a previously evicted entity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLifecycle(cmd, args[0], jsonFlag, "unevict")
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "emit machine-readable JSON output")
	return cmd
}

// runLifecycle is the shared implementation backing all four verbs.
// It loads config, opens the log writer, builds a lifecycle.Pipeline,
// dispatches to the right method, and renders the outcome.
func runLifecycle(cmd *cobra.Command, entityID string, jsonFlag bool, verb string) error {
	cfgPath := defaultConfigPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return emitAndExit(cmd, errs.Operational("CONFIG_LOAD_FAILED",
			"could not load ~/.cortex/config.yaml", err), jsonFlag)
	}
	segDir := expandHome(cfg.Log.SegmentDir)

	loader, err := newLogStateLoader(segDir)
	if err != nil {
		return emitAndExit(cmd, err, jsonFlag)
	}

	writer, err := log.NewWriter(segDir)
	if err != nil {
		return emitAndExit(cmd, errs.Operational("LOG_OPEN_FAILED",
			"could not open segment directory", err), jsonFlag)
	}
	defer writer.Close()

	p := &lifecycle.Pipeline{
		Log:           writer,
		Loader:        loader,
		DecayExponent: cfg.Retrieval.Activation.DecayExponent,
		Now:           func() time.Time { return time.Now().UTC() },
		Actor:         defaultActor(),
		InvocationID:  ulid.Make().String(),
	}

	var (
		out *lifecycle.Outcome
		opErr error
	)
	switch verb {
	case "pin":
		out, opErr = p.Pin(cmd.Context(), entityID)
	case "unpin":
		out, opErr = p.Unpin(cmd.Context(), entityID)
	case "evict":
		out, opErr = p.Evict(cmd.Context(), entityID)
	case "unevict":
		out, opErr = p.Unevict(cmd.Context(), entityID)
	default:
		return emitAndExit(cmd, errs.Operational("UNKNOWN_LIFECYCLE_VERB",
			fmt.Sprintf("unknown lifecycle verb %q", verb), nil), jsonFlag)
	}
	if opErr != nil {
		return emitAndExit(cmd, opErr, jsonFlag)
	}
	return renderLifecycleOutcome(cmd, verb, out, jsonFlag)
}

// renderLifecycleOutcome prints either a human-readable status line or
// a JSON envelope. NoOp outcomes always exit 0 — they are valid by
// design (idempotent re-pin, unevict-on-non-evicted).
func renderLifecycleOutcome(cmd *cobra.Command, verb string, out *lifecycle.Outcome, jsonFlag bool) error {
	if jsonFlag {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{
			"verb":      verb,
			"entity_id": out.EntityID,
			"tx":        out.Tx,
			"no_op":     out.NoOp,
			"reason":    out.Reason,
		})
	}
	if out.NoOp {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: no-op (%s)\n", verb, out.Reason)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: %s tx=%s\n", verb, out.EntityID, out.Tx)
	return nil
}

// logStateLoader is the lifecycle.StateLoader implementation backed by
// a one-shot scan of the segment directory. It folds the latest LWW
// values for the activation-relevant attributes into an
// activation.State. Phase 1 sees rare lifecycle commands; a one-shot
// scan keeps the wiring simple and avoids needing a separate cache.
type logStateLoader struct {
	paths []string
}

// newLogStateLoader enumerates the healthy segments under segDir. A
// missing directory is treated as an empty log (no entries known); a
// scan failure surfaces as an operational error.
func newLogStateLoader(segDir string) (*logStateLoader, error) {
	report, err := log.Load(segDir, log.LoadOptions{})
	if err != nil {
		return nil, errs.Operational("LOG_LOAD_FAILED",
			"could not enumerate log segments", err)
	}
	return &logStateLoader{paths: report.Healthy}, nil
}

// Load streams the log and folds the entry's activation attributes
// into an activation.State. The log reader emits in ascending tx order,
// so naive last-write-wins assignment yields the same result as a full
// LWW collapse. ok=false is returned when the entry has no datoms at
// all.
func (l *logStateLoader) Load(_ context.Context, entityID string) (activation.State, bool, error) {
	if len(l.paths) == 0 {
		return activation.State{}, false, nil
	}
	r, err := log.NewReader(l.paths)
	if err != nil {
		return activation.State{}, false, err
	}
	defer r.Close()

	var (
		state    activation.State
		seen     bool
		evictTs  string
		retractTs string
	)
	for {
		d, ok, err := r.Next()
		if err != nil {
			return activation.State{}, false, err
		}
		if !ok {
			break
		}
		if d.E != entityID {
			continue
		}
		seen = true
		switch d.A {
		case "encoding_at":
			var s string
			if err := json.Unmarshal(d.V, &s); err == nil {
				if t, terr := time.Parse(time.RFC3339Nano, s); terr == nil {
					state.EncodingAt = t
				}
			}
		case lifecycle.AttrBaseActivation:
			var f float64
			if err := json.Unmarshal(d.V, &f); err == nil {
				state.BaseActivation = f
			}
		case lifecycle.AttrPinned:
			var b bool
			if err := json.Unmarshal(d.V, &b); err == nil {
				state.Pinned = b
			}
		case lifecycle.AttrPinActivation:
			var f float64
			if err := json.Unmarshal(d.V, &f); err == nil {
				state.PinActivation = f
			}
		case lifecycle.AttrEvictedAt:
			var s string
			if err := json.Unmarshal(d.V, &s); err == nil {
				evictTs = s
			}
		case lifecycle.AttrEvictedAtRetracted:
			var s string
			if err := json.Unmarshal(d.V, &s); err == nil {
				retractTs = s
			}
		}
	}
	if !seen {
		return activation.State{}, false, nil
	}
	// Eviction is sticky unless a later retraction has been written.
	// Both attributes carry RFC3339Nano timestamps; lexical comparison
	// matches chronological order.
	state.Evicted = evictTs != "" && evictTs > retractTs
	return state, true, nil
}
