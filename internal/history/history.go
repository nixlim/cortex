// Package history implements the read-side helpers behind cortex
// history and cortex as-of. Both commands walk the segmented datom
// log without applying last-write-wins collapse, satisfying the spec
// invariant that lineage is preserved verbatim and only retrieval
// indexes (Weaviate, Neo4j) ever materialize the LWW projection.
//
// Spec references:
//
//	docs/spec/cortex-spec.md §"Activation Datoms"
//	  ("cortex history <id> still returns the full activation lineage")
//	docs/spec/cortex-spec.md FR-007 / FR-015 (history + as-of contracts)
//	docs/spec/cortex-spec.md §"As-of query excludes later facts"
package history

import (
	"strings"

	"github.com/nixlim/cortex/internal/datom"
	"github.com/nixlim/cortex/internal/errs"
	"github.com/nixlim/cortex/internal/log"
)

// EntryPrefix is the entity-id prefix observe assigns to every entry
// it writes ("entry:<ulid>"). AsOf uses it to enumerate the entries
// visible at a given transaction without needing a kind index.
const EntryPrefix = "entry:"

// History returns every datom whose E field equals entityID, in
// tx-ULID-ascending order. The slice contains the full lineage —
// activation reinforcements, retractions, and any other attributes —
// because the function never collapses by attribute. An empty result
// is not an error: the entity may simply have never been written.
func History(segments []string, entityID string) ([]datom.Datom, error) {
	if entityID == "" {
		return nil, errs.Validation("MISSING_ENTITY_ID",
			"history requires an entity id", nil)
	}

	all, err := log.ReadAll(segments)
	if err != nil {
		return nil, errs.Operational("LOG_READ_FAILED",
			"could not read log segments", err)
	}
	out := make([]datom.Datom, 0, 8)
	for i := range all {
		if all[i].E == entityID {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// AsOfView is one row in an AsOf result: the entity id of an entry
// that existed at or before the requested tx, plus the tx that
// introduced it (i.e. the lowest tx with E==entry id).
type AsOfView struct {
	Entity string `json:"entity"`
	Tx     string `json:"tx"`
}

// AsOf restricts visibility to datoms whose tx is at or before the
// given txID and returns one row per entry-prefixed entity that
// satisfies the cutoff. Entities introduced strictly after txID are
// excluded.
//
// If txID is not present in the log AsOf returns ErrTxNotFound — an
// operational error so the CLI exits 1 per the spec line 656
// invariant ("references a transaction ID not present in the log,
// the command returns exit code 1 with a clear not-found error").
func AsOf(segments []string, txID string) ([]AsOfView, error) {
	if txID == "" {
		return nil, errs.Validation("MISSING_TX",
			"as-of requires a transaction id", nil)
	}

	all, err := log.ReadAll(segments)
	if err != nil {
		return nil, errs.Operational("LOG_READ_FAILED",
			"could not read log segments", err)
	}

	// First pass: confirm the tx exists somewhere in the log. We do
	// this in a separate pass rather than fusing it with the entity
	// scan because the AC explicitly requires a NOT_FOUND error path
	// distinct from "no entries match", and a fused loop would
	// silently return an empty result for an invalid tx.
	txSeen := false
	for i := range all {
		if all[i].Tx == txID {
			txSeen = true
			break
		}
	}
	if !txSeen {
		return nil, ErrTxNotFound
	}

	// Second pass: collect the lowest-tx datom per entry-prefixed
	// entity, capped by txID. Because log.ReadAll yields datoms in
	// global tx-ascending order, the first time we see an entity is
	// also its introduction tx — no extra min-tracking needed.
	seen := make(map[string]string, len(all))
	out := make([]AsOfView, 0, len(all)/4)
	for i := range all {
		d := &all[i]
		if d.Tx > txID {
			break
		}
		if !strings.HasPrefix(d.E, EntryPrefix) {
			continue
		}
		if _, ok := seen[d.E]; ok {
			continue
		}
		seen[d.E] = d.Tx
		out = append(out, AsOfView{Entity: d.E, Tx: d.Tx})
	}
	return out, nil
}

// ErrTxNotFound is returned by AsOf when the requested transaction id
// is not present in any segment. The CLI converts this into exit 1
// per FR-007's "clear not-found error".
var ErrTxNotFound = errs.Operational("NOT_FOUND",
	"no transaction with the requested id is present in the log", nil)
