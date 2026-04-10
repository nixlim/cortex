// Package trail implements the episodic-trail data model: trail begin
// mints a Trail entity and writes its kind/agent/name/started_at
// datoms in one transaction; trail end appends ended_at and a synthetic
// summary string in another. Read-side helpers (Load, List) materialize
// trails by walking the segmented datom log.
//
// The package owns no persistence of its own — it speaks to a narrow
// LogAppender (the same shape internal/write.Pipeline already targets)
// so cmd/cortex/trail.go can drop in a real *log.Writer in production
// and tests can substitute a fake that captures the datom groups.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Episodic Observation Capture" (trail
//	  membership preserved by IN_TRAIL.order)
//	docs/spec/cortex-spec.md §"Time and Timeout Budgets"
//	  (timeouts.trail_summary_seconds)
//
// Datom shape recap (mirrors internal/write.buildObserveDatoms so the
// two write paths produce structurally compatible groups that the same
// reader can collate):
//
//	A=kind        V="Trail"
//	A=agent       V="<agent name>"
//	A=name        V="<trail label>"
//	A=started_at  V="<rfc3339 nano>"
//	A=ended_at    V="<rfc3339 nano>"
//	A=summary     V="<llm narrative>"
package trail

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
)

// EntityPrefix is the ULID prefix every trail entity ID uses. Keeping
// it as a const lets the read side filter trails out of mixed-entity
// scans without doing a kind=="Trail" lookup first.
const EntityPrefix = "trail:"

// Source is the value placed in every trail datom's Src field. The
// write pipeline uses "observe"; trail uses "trail" so future tooling
// can distinguish trail-management writes from observation writes when
// auditing the log.
const Source = "trail"

// Attribute names. Held as constants because both the writer (Begin /
// End) and the reader (Load / List) reference them and a typo would be
// silently corrupting.
const (
	AttrKind      = "kind"
	AttrAgent     = "agent"
	AttrName      = "name"
	AttrStartedAt = "started_at"
	AttrEndedAt   = "ended_at"
	AttrSummary   = "summary"

	// AttrTrail is the attribute observe uses to attach an entry to a
	// trail. The reader scans for this so it can enumerate the entries
	// that belong to a given trail in their original write order.
	AttrTrail = "trail"

	// KindTrail is the V value placed on the kind datom of a trail
	// entity.
	KindTrail = "Trail"
)

// LogAppender is the narrow contract this package needs from the
// segment log. *log.Writer already satisfies it; tests pass a fake
// that captures every group it is asked to append.
type LogAppender interface {
	Append(group []datom.Datom) (string, error)
}

// Begin mints a fresh trail entity and appends one datom group with
// kind/agent/name/started_at. The returned trailID is the prefixed
// ULID the caller should hand back to the operator (via stdout +
// CORTEX_TRAIL_ID).
//
// agent and name are required. An empty value for either yields a
// validation error so cortex trail begin exits 2 rather than writing
// a half-formed trail.
func Begin(
	appender LogAppender,
	actor, invocationID, agent, name string,
	now func() time.Time,
) (string, error) {
	if appender == nil {
		return "", errs.Operational("NO_LOG_WRITER",
			"trail.Begin called without a log writer", nil)
	}
	if agent == "" {
		return "", errs.Validation("MISSING_AGENT",
			"cortex trail begin requires --agent", nil)
	}
	if name == "" {
		return "", errs.Validation("MISSING_NAME",
			"cortex trail begin requires --name", nil)
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	tx := ulid.Make().String()
	trailID := EntityPrefix + ulid.Make().String()
	ts := now().UTC().Format(time.RFC3339Nano)
	startedAt := ts

	group, err := buildBeginDatoms(tx, ts, actor, invocationID, trailID, agent, name, startedAt)
	if err != nil {
		return "", errs.Operational("DATOM_BUILD_FAILED",
			"could not construct trail begin datoms", err)
	}
	if _, err := appender.Append(group); err != nil {
		return "", errs.Operational("LOG_APPEND_FAILED",
			"failed to append trail begin datoms", err)
	}
	return trailID, nil
}

// End appends ended_at + summary datoms for an existing trail. summary
// is the LLM-synthesized narrative the caller produced; an empty
// summary is a validation error because the spec acceptance criterion
// requires "a non-empty summary string".
func End(
	appender LogAppender,
	actor, invocationID, trailID, summary string,
	now func() time.Time,
) error {
	if appender == nil {
		return errs.Operational("NO_LOG_WRITER",
			"trail.End called without a log writer", nil)
	}
	if trailID == "" {
		return errs.Validation("MISSING_TRAIL_ID",
			"trail.End requires a non-empty trail id", nil)
	}
	if summary == "" {
		return errs.Validation("EMPTY_SUMMARY",
			"trail.End requires a non-empty summary string", nil)
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	tx := ulid.Make().String()
	ts := now().UTC().Format(time.RFC3339Nano)
	endedAt := ts

	group, err := buildEndDatoms(tx, ts, actor, invocationID, trailID, endedAt, summary)
	if err != nil {
		return errs.Operational("DATOM_BUILD_FAILED",
			"could not construct trail end datoms", err)
	}
	if _, err := appender.Append(group); err != nil {
		return errs.Operational("LOG_APPEND_FAILED",
			"failed to append trail end datoms", err)
	}
	return nil
}

// buildBeginDatoms produces the deterministic begin-datom group:
// kind, agent, name, started_at — in that order so tests can compare
// against a fixed slice.
func buildBeginDatoms(tx, ts, actor, invocationID, trailID, agent, name, startedAt string) ([]datom.Datom, error) {
	g := newGroupBuilder(tx, ts, actor, invocationID, trailID)
	if err := g.add(AttrKind, KindTrail); err != nil {
		return nil, err
	}
	if err := g.add(AttrAgent, agent); err != nil {
		return nil, err
	}
	if err := g.add(AttrName, name); err != nil {
		return nil, err
	}
	if err := g.add(AttrStartedAt, startedAt); err != nil {
		return nil, err
	}
	return g.group, nil
}

// buildEndDatoms produces the deterministic end-datom group:
// ended_at, summary — in that order.
func buildEndDatoms(tx, ts, actor, invocationID, trailID, endedAt, summary string) ([]datom.Datom, error) {
	g := newGroupBuilder(tx, ts, actor, invocationID, trailID)
	if err := g.add(AttrEndedAt, endedAt); err != nil {
		return nil, err
	}
	if err := g.add(AttrSummary, summary); err != nil {
		return nil, err
	}
	return g.group, nil
}

// groupBuilder is a small helper that captures the per-group invariants
// (tx, ts, actor, invocationID, entity) so each add call only spells
// out attribute and value.
type groupBuilder struct {
	tx, ts, actor, invocationID, entity string
	group                               []datom.Datom
}

func newGroupBuilder(tx, ts, actor, invocationID, entity string) *groupBuilder {
	return &groupBuilder{
		tx: tx, ts: ts, actor: actor, invocationID: invocationID,
		entity: entity,
	}
}

func (g *groupBuilder) add(attr string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("trail: marshal %s: %w", attr, err)
	}
	d := datom.Datom{
		Tx:           g.tx,
		Ts:           g.ts,
		Actor:        g.actor,
		Op:           datom.OpAdd,
		E:            g.entity,
		A:            attr,
		V:            raw,
		Src:          Source,
		InvocationID: g.invocationID,
	}
	if err := d.Seal(); err != nil {
		return fmt.Errorf("trail: seal %s: %w", attr, err)
	}
	g.group = append(g.group, d)
	return nil
}
